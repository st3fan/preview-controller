package dashboard

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/st3fan/preview-controller/internal/config"
	"golang.org/x/oauth2"
)

const (
	callbackPath  = "/dashboard/auth/callback"
	loginCookie   = "__Host-preview-login"
	sessionCookie = "__Host-preview-session"
	roleView      = "view"
	roleOperate   = "operate"
)

type accountKey struct{}

type dashboardAccount struct {
	role  string
	Email string
}

func requestAccount(r *http.Request) *dashboardAccount {
	account, _ := r.Context().Value(accountKey{}).(*dashboardAccount)
	return account
}

type browserSession struct {
	account dashboardAccount
	ctx     context.Context
	cancel  context.CancelFunc
}

type dashboardAccess struct {
	settings config.DashboardAuth
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
	origin   string
	sessions sync.Map // random session ID -> browserSession
}

// WithAccess applies the browser policy to every route on the dashboard mux.
func WithAccess(ctx context.Context, cfg config.Config, next http.Handler) (http.Handler, error) {
	if cfg.DashboardAuth == nil {
		return next, nil
	}
	discovery, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	provider, err := oidc.NewProvider(discovery, cfg.DashboardAuth.Issuer)
	if err != nil {
		return nil, fmt.Errorf("discover dashboard issuer: %w", err)
	}
	endpoint := provider.Endpoint()
	var metadata struct {
		JWKS string `json:"jwks_uri"`
	}
	if err := provider.Claims(&metadata); err != nil {
		return nil, err
	}
	for _, addr := range []string{endpoint.AuthURL, endpoint.TokenURL, metadata.JWKS} {
		if !strings.HasPrefix(addr, "https://") {
			return nil, errors.New("dashboard issuer endpoints must use HTTPS")
		}
	}
	// The dashboard is registered as a public PKCE client.
	endpoint.AuthStyle = oauth2.AuthStyleInParams
	a := &dashboardAccess{
		settings: *cfg.DashboardAuth,
		origin:   cfg.DashboardOrigin,
		oauth: oauth2.Config{
			ClientID:    cfg.DashboardAuth.ClientID,
			Endpoint:    endpoint,
			RedirectURL: cfg.DashboardOrigin + callbackPath,
			Scopes:      []string{oidc.ScopeOpenID, "groups", "email"},
		},
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.DashboardAuth.ClientID}),
	}
	return a.handler(next), nil
}

func browserCookie(w http.ResponseWriter, name, value string, seconds int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: seconds})
}

func (a *dashboardAccess) handler(next http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+callbackPath, a.callback)
	mux.HandleFunc("POST /dashboard/logout", func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookie); err == nil {
			if value, ok := a.sessions.LoadAndDelete(c.Value); ok {
				value.(browserSession).cancel()
			}
		}
		browserCookie(w, sessionCookie, "", -1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<p>Signed out of Preview Controller.</p><a href="/">Sign in again</a>`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s, ok := a.session(r)
		if !ok {
			if r.Method == "GET" && dashboardDestination(r.URL.EscapedPath()) {
				a.login(w, r)
				return
			}
			http.Error(w, "Dashboard sign-in required", http.StatusUnauthorized)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" && s.account.role != roleOperate {
			http.Error(w, "Operate access required", http.StatusForbidden)
			return
		}
		ctx, cancel := context.WithCancel(context.WithValue(r.Context(), accountKey{}, &s.account))
		defer cancel()
		stop := context.AfterFunc(s.ctx, cancel)
		defer stop()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "same-origin")
		if r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("Origin") != a.origin {
			http.Error(w, "Same-origin request required", http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (a *dashboardAccess) session(r *http.Request) (browserSession, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return browserSession{}, false
	}
	value, ok := a.sessions.Load(c.Value)
	if !ok {
		return browserSession{}, false
	}
	session := value.(browserSession)
	return session, session.ctx.Err() == nil
}

func dashboardDestination(path string) bool {
	return path == "/" || strings.HasPrefix(path, "/dashboard/previews/") || strings.HasPrefix(path, "/dashboard/logs/")
}

func (a *dashboardAccess) login(w http.ResponseWriter, r *http.Request) {
	state, nonce, verifier := rand.Text(), rand.Text(), oauth2.GenerateVerifier()
	browserCookie(w, loginCookie, strings.Join([]string{state, nonce, verifier, base64.RawURLEncoding.EncodeToString([]byte(r.URL.EscapedPath()))}, "."), 600)
	http.Redirect(w, r, a.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), http.StatusSeeOther)
}

func (a *dashboardAccess) callback(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(loginCookie)
	browserCookie(w, loginCookie, "", -1)
	if err != nil {
		http.Error(w, "Sign-in expired. Open the dashboard to retry.", http.StatusUnauthorized)
		return
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) != 4 || parts[0] == "" || parts[1] == "" || parts[2] == "" || parts[0] != r.URL.Query().Get("state") || r.URL.Query().Get("code") == "" {
		http.Error(w, "Invalid sign-in response", http.StatusUnauthorized)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	token, err := a.oauth.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(parts[2]))
	if err != nil {
		http.Error(w, "Pocket ID sign-in failed", http.StatusUnauthorized)
		return
	}
	raw, _ := token.Extra("id_token").(string)
	id, err := a.verifier.Verify(ctx, raw)
	if err != nil || id.Subject == "" || id.Nonce != parts[1] {
		http.Error(w, "Invalid Pocket ID identity", http.StatusUnauthorized)
		return
	}
	var claims struct {
		Email  string   `json:"email"`
		Groups []string `json:"groups"`
	}
	if err := id.Claims(&claims); err != nil {
		http.Error(w, "Invalid group claims", http.StatusForbidden)
		return
	}
	role := roleView
	switch {
	case slices.Contains(claims.Groups, a.settings.OperateGroup):
		role = roleOperate
	case slices.Contains(claims.Groups, a.settings.ViewGroup):
	default:
		http.Error(w, "Dashboard group membership required", http.StatusForbidden)
		return
	}
	lifetime := min(time.Hour, time.Until(id.Expiry))
	if lifetime < time.Second {
		http.Error(w, "Sign-in expired", http.StatusUnauthorized)
		return
	}
	key := rand.Text()
	sessionCtx, end := context.WithTimeout(context.Background(), lifetime)
	a.sessions.Store(key, browserSession{account: dashboardAccount{role: role, Email: claims.Email}, ctx: sessionCtx, cancel: end})
	context.AfterFunc(sessionCtx, func() { a.sessions.Delete(key) })
	browserCookie(w, sessionCookie, key, int(lifetime.Seconds()))
	destination, err := base64.RawURLEncoding.DecodeString(parts[3])
	returnPath := "/"
	if err == nil && dashboardDestination(string(destination)) && !strings.ContainsAny(string(destination), "\\\r\n") && path.Clean(string(destination)) == string(destination) {
		returnPath = string(destination)
	}
	http.Redirect(w, r, returnPath, http.StatusSeeOther)
}
