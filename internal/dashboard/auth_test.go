package dashboard

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/st3fan/preview-controller/internal/config"
	"github.com/st3fan/preview-controller/internal/health"
	"github.com/st3fan/preview-controller/internal/state"
	"golang.org/x/oauth2"
)

func TestBrowserSignIn(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, nil)
	if err != nil {
		t.Fatal(err)
	}
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		issuer := "https://" + r.Host
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys", "id_token_signing_alg_values_supported": []string{"ES256"}})
		case "/keys":
			json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, Algorithm: "ES256", Use: "sig"}}})
		case "/token":
			// The fixture code binds the signed ID token to the original PKCE challenge.
			challenge, raw, ok := strings.Cut(r.FormValue("code"), ":")
			if !ok || oauth2.S256ChallengeFromVerifier(r.FormValue("code_verifier")) != challenge || r.FormValue("client_id") != "dashboard" {
				http.Error(w, "invalid code", 400)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"access_token": "unused", "id_token": raw})
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	ctx := oidc.ClientContext(t.Context(), provider.Client())
	cfg := config.Config{DashboardOrigin: "https://preview.test", DashboardAuth: &config.DashboardAuth{Issuer: provider.URL, ClientID: "dashboard", ViewGroup: "readers", OperateGroup: "operators"}}
	mux := http.NewServeMux()
	store, err := state.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(state.Environment{ID: state.ID("alpha", 1), Project: "alpha", Intent: 1, ActiveDeploymentID: "active", Attempts: []state.Attempt{{ID: "active", Backend: config.BackendDocker, Status: state.Healthy}}}); err != nil {
		t.Fatal(err)
	}
	Register(mux, ctx, cfg, store.Load, store.List, store.Changes, health.New(store, nil, nil))
	RegisterLogs(mux, ctx, store.Load, func(context.Context, state.Attempt) (func(io.Writer) error, error) {
		return func(w io.Writer) error { _, err := io.WriteString(w, "preview output"); return err }, nil
	})
	actions := 0
	action := func(string) error { actions++; return nil }
	RegisterArchive(mux, action, action)
	RegisterDelete(mux, action)
	h, err := WithAccess(ctx, cfg, mux)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	request := func(method, path string, c *http.Cookie, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(ctx, method, cfg.DashboardOrigin+path, nil)
		if c != nil {
			r.AddCookie(c)
		}
		r.Header.Set("Origin", origin)
		r.Header.Set("X-Preview-Action", path[strings.LastIndex(path, "/")+1:])
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/dashboard/host", "/dashboard/events", "/dashboard/logs/alpha/1/active", "/dashboard/previews/alpha/1"} {
		if w := request("HEAD", path, &http.Cookie{Name: sessionCookie, Value: "unknown"}, ""); w.Code != 401 {
			t.Fatal(path, w.Code)
		}
	}
	for _, tc := range []struct {
		name    string
		groups  []string
		status  int
		operate bool
	}{
		{"viewer", []string{"readers"}, 303, false}, {"operator", []string{"operators"}, 303, true}, {"both", []string{"readers", "operators"}, 303, true}, {"neither", []string{"elsewhere"}, 403, false}, {"no groups", nil, 403, false}, {"wrong nonce", []string{"operators"}, 401, false}, {"wrong state", []string{"operators"}, 401, false}, {"expired identity", []string{"operators"}, 401, false}, {"wrong audience", []string{"operators"}, 401, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const detail = "/dashboard/previews/alpha/1"
			login := request("GET", detail, nil, "")
			u, err := login.Result().Location()
			if err != nil {
				t.Fatal(err)
			}
			q := u.Query()
			if login.Code != http.StatusSeeOther || u.Path != "/authorize" || q.Get("scope") != "openid groups" || q.Get("code_challenge_method") != "S256" || q.Get("redirect_uri") != cfg.DashboardOrigin+callbackPath {
				t.Fatal(q)
			}
			claims := map[string]any{"aud": "dashboard", "iss": provider.URL, "sub": "human", "nonce": q.Get("nonce"), "exp": time.Now().Add(time.Hour).Unix(), "groups": tc.groups}
			stateValue := q.Get("state")
			switch tc.name {
			case "wrong nonce":
				claims["nonce"] = "other"
			case "expired identity":
				claims["exp"] = time.Now().Add(-time.Minute).Unix()
			case "wrong audience":
				claims["aud"] = "mcp"
			case "wrong state":
				stateValue = "wrong"
			}
			raw, err := jwt.Signed(signer).Claims(claims).Serialize()
			if err != nil {
				t.Fatal(err)
			}
			callback := callbackPath + "?" + url.Values{"code": {q.Get("code_challenge") + ":" + raw}, "state": {stateValue}}.Encode()
			result := request("GET", callback, login.Result().Cookies()[0], "")
			if result.Code != tc.status {
				t.Fatal(result.Code, result.Body.String())
			}
			var session *http.Cookie
			for _, c := range result.Result().Cookies() {
				if c.Name == sessionCookie {
					session = c
				}
			}
			if tc.status != 303 {
				if session != nil {
					t.Fatal("granted session")
				}
				return
			}
			if result.Header().Get("Location") != detail || session == nil || !session.Secure || !session.HttpOnly || session.SameSite != http.SameSiteLaxMode || session.Path != "/" || session.Domain != "" {
				t.Fatal("invalid session or return path")
			}
			page := request("GET", "/", session, "")
			if page.Code != 200 || !strings.Contains(page.Body.String(), "Sign out") || strings.Contains(page.Body.String(), "data-delete-url=") != tc.operate || strings.Contains(page.Body.String(), `data-action="archive"`) != tc.operate {
				t.Fatal("wrong dashboard controls", page.Code)
			}
			logRequest, err := http.NewRequestWithContext(ctx, "GET", server.URL+"/dashboard/logs/alpha/1/active", nil)
			if err != nil {
				t.Fatal(err)
			}
			logRequest.AddCookie(session)
			logs, err := server.Client().Do(logRequest)
			if err != nil {
				t.Fatal(err)
			}
			output, err := io.ReadAll(logs.Body)
			logs.Body.Close()
			if logs.StatusCode != 200 || err != nil || string(output) != "preview output" {
				t.Fatal("could not read authorized logs", logs.StatusCode, err)
			}
			streamRequest, err := http.NewRequestWithContext(ctx, "GET", server.URL+"/dashboard/events", nil)
			if err != nil {
				t.Fatal(err)
			}
			streamRequest.AddCookie(session)
			stream, err := server.Client().Do(streamRequest)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Body.Close()
			fragment := readEvent(t, bufio.NewScanner(stream.Body))
			stream.Body.Close()
			if !strings.Contains(fragment, "#1") || strings.Contains(fragment, "data-delete-url=") != tc.operate {
				t.Fatal("wrong controls in live update")
			}
			for _, path := range []string{detail, "/dashboard/host", "/dashboard/events"} {
				if w := request("HEAD", path, session, ""); w.Code != 200 {
					t.Fatal(path, w.Code)
				}
			}
			for _, action := range []string{"archive", "restore", "delete"} {
				path := "/dashboard/environments/alpha/1/" + action
				before := actions
				if w := request("POST", path, session, "https://other.test"); w.Code != 403 || actions != before {
					t.Fatal("cross-origin action")
				}
				w := request("POST", path, session, cfg.DashboardOrigin)
				if tc.operate && (w.Code != 204 || actions != before+1) || !tc.operate && (w.Code != 403 || actions != before) {
					t.Fatal(action, w.Code, actions)
				}
			}
			if request("POST", "/dashboard/logout", session, cfg.DashboardOrigin).Code != 200 || request("GET", "/dashboard/host", session, "").Code != 401 {
				t.Fatal("logout did not revoke session")
			}
		})
	}
}

func TestDashboardSessionExpiryStopsStream(t *testing.T) {
	a := new(dashboardAccess)
	session, end := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer end()
	a.sessions.Store("test", browserSession{roleView, session, end})
	started := make(chan struct{})
	done := make(chan struct{})
	handler := a.handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	r := httptest.NewRequest("GET", "/dashboard/events", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "test"})
	go func() { defer close(done); handler.ServeHTTP(httptest.NewRecorder(), r) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("stream did not start")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream remained open")
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
}

func TestDashboardOpenWithMCPOAuth(t *testing.T) {
	mux := http.NewServeMux()
	calls := 0
	RegisterDelete(mux, func(id string) error { calls++; return nil })
	h, err := WithAccess(t.Context(), config.Config{MCPOAuth: &config.MCPOAuth{}}, mux)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/dashboard/environments/alpha/1/delete", nil)
	r.Header.Set("X-Preview-Action", "delete")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 204 || calls != 1 {
		t.Fatal("open dashboard must allow anonymous operator actions")
	}
}
