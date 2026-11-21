package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"github.com/st3fan/preview-controller/internal/config"
)

const metadataPath = "/.well-known/oauth-protected-resource/mcp"
const operatorScope = "preview:operate"

func protectOAuthMCP(mux *http.ServeMux, cfg config.MCPOAuth, next http.Handler) http.Handler {
	metadata := &oauthex.ProtectedResourceMetadata{
		Resource:               cfg.Resource,
		AuthorizationServers:   []string{cfg.Issuer},
		ScopesSupported:        []string{operatorScope},
		BearerMethodsSupported: []string{"header"},
	}
	mux.Handle(metadataPath, auth.ProtectedResourceMetadataHandler(metadata))
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	keys := oidc.NewRemoteKeySet(oidc.ClientContext(context.Background(), client), cfg.JWKSURI)
	options := &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: strings.TrimSuffix(cfg.Resource, "/mcp") + metadataPath,
		Scopes:              []string{operatorScope},
	}
	authenticated := auth.RequireBearerToken(accessTokenVerifier(cfg, keys), options)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !slices.Contains(cfg.OperatorSubjects, auth.TokenInfoFromContext(r.Context()).UserID) {
			http.Error(w, "operator access denied", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	}))
	// Config validation requires an HTTPS URL with exactly the /mcp path.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold("https://"+r.Host+"/mcp", cfg.Resource) {
			http.Error(w, "invalid MCP host", http.StatusForbidden)
			return
		}
		authenticated.ServeHTTP(w, r)
	})
}

func accessTokenVerifier(cfg config.MCPOAuth, keys oidc.KeySet) auth.TokenVerifier {
	verifier := oidc.NewVerifier(cfg.Issuer, keys, &oidc.Config{
		ClientID:             cfg.Resource,
		SupportedSigningAlgs: []string{oidc.RS256, oidc.ES256, oidc.EdDSA},
	})
	return func(ctx context.Context, raw string, _ *http.Request) (*auth.TokenInfo, error) {
		// Pocket ID v2.19.3 uses the RFC 9068 protected type for access tokens.
		jws, err := jose.ParseSignedCompact(raw, []jose.SignatureAlgorithm{jose.RS256, jose.ES256, jose.EdDSA})
		if err != nil || jws.Signatures[0].Protected.ExtraHeaders["typ"] != "at+jwt" {
			return nil, auth.ErrInvalidToken
		}
		token, err := verifier.Verify(ctx, raw)
		if err != nil || token.Issuer != cfg.Issuer || token.Subject == "" || strings.HasPrefix(token.Subject, "client-") {
			return nil, auth.ErrInvalidToken
		}
		var claims struct {
			Scope    string `json:"scope"`
			ClientID string `json:"client_id"`
			ID       string `json:"jti"`
		}
		if err := token.Claims(&claims); err != nil || claims.ClientID == "" || claims.ID == "" || token.IssuedAt.IsZero() {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{
			UserID: token.Subject, Expiration: token.Expiry,
			Scopes: strings.Fields(claims.Scope), Extra: map[string]any{"issuer": token.Issuer},
		}, nil
	}
}
