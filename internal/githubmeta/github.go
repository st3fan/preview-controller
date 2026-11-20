// Package githubmeta reads display metadata for preview intents.
package githubmeta

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

const query = `query($owner: String!, $repo: String!, $number: Int!) {
  repository(owner: $owner, name: $repo) {
    issue(number: $number) {
      ...IssueFields
      subIssues(first: 20) {
        pageInfo { hasNextPage }
        nodes { ...IssueFields }
      }
    }
  }
}
fragment IssueFields on Issue {
  number title state createdAt repository { nameWithOwner }
  closedByPullRequestsReferences(first: 20, includeClosedPrs: true) {
    pageInfo { hasNextPage }
    nodes {
      number title state createdAt mergedAt
      reviews(last: 10) {
        pageInfo { hasPreviousPage }
        nodes { submittedAt url }
      }
      commits(last: 10) {
        pageInfo { hasPreviousPage }
        nodes { commit { oid committedDate } }
      }
      baseRefName headRefName
      baseRepository { nameWithOwner }
      headRepository { nameWithOwner }
    }
  }
}`

type Client struct {
	token    string
	endpoint string
	http     *http.Client
}

func New(token string) *Client {
	return &Client{token: token, endpoint: "https://api.github.com/graphql", http: &http.Client{
		Timeout:       2 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

type Result struct {
	Intent  *Issue
	Related []Issue
	Pulls   []Pull
	Ordered bool
	Partial bool
}

type repository struct {
	NameWithOwner string
}

type connection[T any] struct {
	PageInfo struct{ HasNextPage, HasPreviousPage bool }
	Nodes    []*T
}

type Issue struct {
	Number                         int64
	Title                          string
	State                          string
	CreatedAt                      time.Time
	Repository                     repository
	ClosedByPullRequestsReferences *connection[Pull]
}

type Review struct {
	SubmittedAt *time.Time
	URL         string
}

type PullCommit struct {
	Commit struct {
		OID           string
		CommittedDate time.Time
	}
}

type Pull struct {
	Number         int64
	Title          string
	State          string
	CreatedAt      time.Time
	MergedAt       *time.Time
	Reviews        *connection[Review]
	Commits        *connection[PullCommit]
	BaseRefName    string
	HeadRefName    string
	BaseRepository repository
	HeadRepository repository
}

func (c *Client) queryIssue(ctx context.Context, repositoryName string, number int64, query string, response any) bool {
	owner, repo, ok := strings.Cut(repositoryName, "/")
	if !ok || owner == "" || repo == "" || number < 1 || number > 1<<31-1 {
		return false
	}
	payload, err := json.Marshal(struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}{query, map[string]any{"owner": owner, "repo": repo, "number": number}})
	if err != nil {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return false
	}
	return json.NewDecoder(io.LimitReader(res.Body, 2<<20)).Decode(response) == nil
}

// LookupTitle reads only the root issue, without its PRs, reviews or sub-issues.
func (c *Client) LookupTitle(ctx context.Context, repositoryName string, number int64) string {
	const titleQuery = `query($owner: String!, $repo: String!, $number: Int!) {
  repository(owner: $owner, name: $repo) { issue(number: $number) { number title repository { nameWithOwner } } }
 }`
	var response struct {
		Data struct{ Repository struct{ Issue *Issue } }
	}
	if !c.queryIssue(ctx, repositoryName, number, titleQuery, &response) {
		return ""
	}
	issue := response.Data.Repository.Issue
	if issue == nil || issue.Number != number || !strings.EqualFold(issue.Repository.NameWithOwner, repositoryName) {
		return ""
	}
	return strings.TrimSpace(issue.Title)
}

func (c *Client) Lookup(ctx context.Context, repositoryName string, number int64) Result {
	var response struct {
		Data struct {
			Repository struct {
				Issue *struct {
					Issue
					SubIssues *connection[Issue]
				}
			}
		}
		Errors []json.RawMessage
	}
	if !c.queryIssue(ctx, repositoryName, number, query, &response) {
		return Result{}
	}
	root := response.Data.Repository.Issue
	if root == nil || root.Number != number || !strings.EqualFold(root.Repository.NameWithOwner, repositoryName) {
		return Result{}
	}
	result := Result{Intent: &root.Issue, Partial: len(response.Errors) > 0}
	seen := make(map[int64]bool)
	addPulls := func(refs *connection[Pull]) {
		if refs == nil {
			result.Partial = true
			return
		}
		result.Partial = result.Partial || refs.PageInfo.HasNextPage
		for _, pr := range refs.Nodes {
			if pr == nil || pr.Number < 1 || pr.BaseRepository.NameWithOwner == "" {
				result.Partial = true
				continue
			}
			if strings.EqualFold(pr.BaseRepository.NameWithOwner, repositoryName) && !seen[pr.Number] {
				result.Pulls = append(result.Pulls, *pr)
				seen[pr.Number] = true
			}
		}
	}
	addPulls(root.ClosedByPullRequestsReferences)
	if root.SubIssues == nil {
		result.Partial = true
	} else {
		result.Partial = result.Partial || root.SubIssues.PageInfo.HasNextPage
		for _, issue := range root.SubIssues.Nodes {
			if issue == nil || issue.Number < 1 {
				result.Partial = true
				continue
			}
			if !strings.EqualFold(issue.Repository.NameWithOwner, repositoryName) {
				continue
			}
			result.Related = append(result.Related, *issue)
			addPulls(issue.ClosedByPullRequestsReferences)
		}
	}
	slices.SortFunc(result.Pulls, func(a, b Pull) int { return cmp.Compare(a.Number, b.Number) })
	if !result.Partial {
		if ordered := stackOrder(result.Pulls); ordered != nil {
			result.Pulls = ordered
			result.Ordered = true
		}
	}
	return result
}

// stackOrder returns a single chain from its lowest PR to its tip, if one exists.
func stackOrder(pulls []Pull) []Pull {
	if len(pulls) < 2 {
		return pulls
	}
	type branch struct{ repository, name string }
	heads := make(map[branch]int)
	for i, pr := range pulls {
		head := branch{strings.ToLower(pr.HeadRepository.NameWithOwner), pr.HeadRefName}
		if head.repository == "" || head.name == "" || pr.BaseRefName == "" {
			return nil
		}
		if _, exists := heads[head]; exists {
			return nil
		}
		heads[head] = i
	}
	next := make(map[int]int)
	root := -1
	for i, pr := range pulls {
		base := branch{strings.ToLower(pr.BaseRepository.NameWithOwner), pr.BaseRefName}
		parent, found := heads[base]
		if !found {
			if root != -1 {
				return nil
			}
			root = i
			continue
		}
		if _, exists := next[parent]; exists {
			return nil
		}
		next[parent] = i
	}
	if root == -1 {
		return nil
	}
	ordered := make([]Pull, 0, len(pulls))
	for i := root; ; {
		ordered = append(ordered, pulls[i])
		child, found := next[i]
		if !found {
			break
		}
		i = child
	}
	if len(ordered) != len(pulls) {
		return nil
	}
	return ordered
}
