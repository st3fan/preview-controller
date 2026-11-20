package githubmeta

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLookup(t *testing.T) {
	for _, tc := range []struct {
		name      string
		upperBase string
		partial   bool
		status    int
		first     int64
		ordered   bool
	}{
		{"stack", "lower", false, 200, 20, true},
		{"independent", "main", false, 200, 10, false},
		{"truncated", "lower", true, 200, 10, false},
		{"rate limited", "lower", false, 429, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Variables struct {
						Owner  string
						Repo   string
						Number int
					}
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
					body.Variables.Owner != "example" || body.Variables.Repo != "repo" || body.Variables.Number != 51 ||
					r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer read-only-token" {
					t.Error("incorrect GitHub request")
				}
				w.WriteHeader(tc.status)
				fmt.Fprintf(w, `{"data":{"repository":{"issue":{
					"number":51,"title":"Intent title","state":"OPEN","repository":{"nameWithOwner":"example/repo"},
					"closedByPullRequestsReferences":{"nodes":[
						{"number":10,"title":"Upper","state":"OPEN","baseRefName":%q,"headRefName":"upper","baseRepository":{"nameWithOwner":"example/repo"},"headRepository":{"nameWithOwner":"example/repo"}}
					]},
					"subIssues":{"pageInfo":{"hasNextPage":%t},"nodes":[
						{"number":52,"title":"Related issue","state":"CLOSED","repository":{"nameWithOwner":"example/repo"},"closedByPullRequestsReferences":{"nodes":[
							{"number":20,"title":"Lower","state":"MERGED","baseRefName":"main","headRefName":"lower","baseRepository":{"nameWithOwner":"example/repo"},"headRepository":{"nameWithOwner":"EXAMPLE/repo"}},
							{"number":10,"baseRepository":{"nameWithOwner":"example/repo"}},
							{"number":30,"baseRepository":{"nameWithOwner":"other/repo"}}
						]}}
					]}
				}}}}`, tc.upperBase, tc.partial)
			}))
			defer server.Close()

			client := New("read-only-token")
			client.endpoint = server.URL
			got := client.Lookup(t.Context(), "example/repo", 51)
			if tc.status != 200 {
				if got.Intent != nil {
					t.Fatalf("expected unavailable metadata, got %+v", got)
				}
				return
			}
			if got.Intent == nil || got.Intent.Title != "Intent title" || len(got.Related) != 1 ||
				got.Related[0].Number != 52 || len(got.Pulls) != 2 || got.Pulls[0].Number != tc.first ||
				got.Ordered != tc.ordered || got.Partial != tc.partial {
				t.Fatalf("unexpected metadata: %+v", got)
			}
		})
	}
}
