package dashboard

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/st3fan/preview-controller/internal/deploy"
)

func TestDashboardDelete(t *testing.T) {
	const path = "/dashboard/environments/demo/18/delete"
	for _, tc := range []struct {
		name, method, site, origin, action string
		removeError                        error
		wantStatus                         int
		wantCall                           bool
	}{
		{name: "non-mutating GET", method: "GET", wantStatus: 405},
		{name: "cross-site", site: "cross-site", action: "delete", wantStatus: 403},
		{name: "sibling preview origin", site: "same-site", action: "delete", wantStatus: 403},
		{name: "legacy cross-origin", origin: "https://preview.example.com", action: "delete", wantStatus: 403},
		{name: "cross-origin form without metadata", wantStatus: 403},
		{name: "busy environment", site: "same-origin", action: "delete", removeError: deploy.ErrConflict, wantStatus: 409, wantCall: true},
		{name: "cleanup error", site: "same-origin", action: "delete", removeError: errors.New("private cleanup details"), wantStatus: 500, wantCall: true},
		{name: "successful deletion or retry", site: "same-origin", action: "delete", wantStatus: 204, wantCall: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			called := false
			RegisterDelete(mux, func(id string) error {
				called = true
				if id != "demo:18" {
					t.Errorf("unexpected environment %q", id)
				}
				return tc.removeError
			})
			method := http.MethodPost
			if tc.method != "" {
				method = tc.method
			}
			request := httptest.NewRequest(method, "https://operator.example.com"+path, nil)
			request.Header.Set("Sec-Fetch-Site", tc.site)
			request.Header.Set("Origin", tc.origin)
			request.Header.Set("X-Preview-Action", tc.action)
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)
			if response.Code != tc.wantStatus || called != tc.wantCall {
				t.Fatalf("status %d, deletion called %t", response.Code, called)
			}
			if strings.Contains(response.Body.String(), "private cleanup details") {
				t.Fatal("response exposes internal cleanup details")
			}
		})
	}
}
