package api

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"github.com/st3fan/preview-controller/internal/config"
)

type fixtureIssuer struct {
	cfg config.MCPOAuth
	key atomic.Pointer[jose.JSONWebKey]
}

func newFixtureIssuer(t *testing.T) *fixtureIssuer {
	f := new(fixtureIssuer)
	f.rotate(t, "first")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{f.key.Load().Public()}})
	}))
	t.Cleanup(server.Close)
	f.cfg = config.MCPOAuth{Issuer: "https://issuer.test", Resource: "https://preview.test/mcp", JWKSURI: server.URL}
	return f
}

func (f *fixtureIssuer) rotate(t *testing.T, id string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f.key.Store(&jose.JSONWebKey{Key: key, KeyID: id, Algorithm: "ES256", Use: "sig"})
}

func (f *fixtureIssuer) claims() map[string]any {
	return map[string]any{
		"iss": f.cfg.Issuer, "aud": f.cfg.Resource, "sub": "operator",
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"client_id": "fixture", "jti": "fixture-id", "scope": operatorScope,
	}
}

func (f *fixtureIssuer) sign(t *testing.T, typ string, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: f.key.Load()}, new(jose.SignerOptions).WithType(jose.ContentType(typ)))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func (f *fixtureIssuer) accessToken(t *testing.T) string { return f.sign(t, "at+jwt", f.claims()) }

func TestOAuthAccess(t *testing.T) {
	f := newFixtureIssuer(t)
	mux := http.NewServeMux()
	RegisterMCP(mux, nil, nil, "legacy", &f.cfg, nil)
	request := func(path, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if strings.HasPrefix(path, "/") {
			r.Host = "preview.test"
		}
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	if w := request("https://other.test/mcp", f.accessToken(t)); w.Code != 403 {
		t.Fatal(w)
	}
	missing := request("/mcp", "")
	if missing.Code != 401 || !strings.Contains(missing.Header().Get("WWW-Authenticate"), `resource_metadata="https://preview.test`+metadataPath+`"`) {
		t.Fatal(missing)
	}
	var metadata oauthex.ProtectedResourceMetadata
	w := request(metadataPath, "")
	if err := json.Unmarshal(w.Body.Bytes(), &metadata); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || metadata.Resource != f.cfg.Resource ||
		!slices.Equal(metadata.AuthorizationServers, []string{f.cfg.Issuer}) ||
		!slices.Equal(metadata.ScopesSupported, []string{operatorScope}) {
		t.Fatalf("unexpected metadata: %+v", metadata)
	}
	for _, tc := range []struct {
		name, key string
		value     any
		status    int
	}{
		{"expired", "exp", time.Now().Add(-time.Minute).Unix(), 401},
		{"missing expiry", "exp", nil, 401},
		{"wrong issuer", "iss", "https://other.test", 401},
		{"wrong audience", "aud", "https://preview.test/", 401},
		{"missing subject", "sub", "", 401},
		{"machine subject", "sub", "client-fixture", 401},
		{"missing permission", "scope", "openid", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := f.claims()
			claims[tc.key] = tc.value
			w := request("/mcp", f.sign(t, "at+jwt", claims))
			if w.Code != tc.status {
				t.Fatal(w)
			}
		})
	}
	other := newFixtureIssuer(t)
	for _, raw := range []string{"legacy", f.sign(t, "JWT", f.claims()), other.accessToken(t)} {
		if w := request("/mcp", raw); w.Code != 401 {
			t.Fatal(w)
		}
	}
	f.rotate(t, "next")
	token := f.accessToken(t)
	// A verified GET reaches the stateless MCP transport.
	if w := request("/mcp", token); w.Code != http.StatusMethodNotAllowed {
		t.Fatal(w)
	}
	// Pocket ID controls eligibility. A different human subject with the
	// API permission reaches the transport without a local account entry.
	claims := f.claims()
	claims["sub"] = "another-operator"
	if w := request("/mcp", f.sign(t, "at+jwt", claims)); w.Code != http.StatusMethodNotAllowed {
		t.Fatal(w)
	}
}
