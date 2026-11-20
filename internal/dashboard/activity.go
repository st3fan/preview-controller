package dashboard

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/st3fan/preview-controller/internal/githubmeta"
	"github.com/st3fan/preview-controller/internal/state"
)

type activityEvent struct {
	At     time.Time
	Source string
	Label  string
	URL    string
	Detail string
}

func (d *detailView) addActivity(at time.Time, source, label, url, detail string) {
	if !at.IsZero() {
		d.Activity = append(d.Activity, activityEvent{at, source, label, url, detail})
	}
}

func (d *detailView) buildActivity() {
	for i, attempt := range d.Attempts {
		link := ""
		if i >= len(d.Attempts)-len(d.Recent) {
			link = "#attempt-" + attempt.ID
		} else if d.RepositoryURL != "" && commitOID(attempt.Commit) {
			link = d.RepositoryURL + "/commit/" + attempt.Commit
		}
		detail := attempt.ID + " · " + attempt.Commit
		d.DeploymentActivityPartial = d.DeploymentActivityPartial || attempt.CreatedAt.IsZero()
		d.addActivity(attempt.CreatedAt, "Controller", "Deployment requested", link, detail)
		if attempt.ActivatedAt != nil {
			d.addActivity(*attempt.ActivatedAt, "Controller", "Preview activated", link, detail)
		}
		if attempt.Status == state.Failed {
			d.DeploymentActivityPartial = d.DeploymentActivityPartial || attempt.UpdatedAt.IsZero()
			label := "Deployment failed"
			if attempt.ActivatedAt != nil {
				label = "Preview failed after activation"
			}
			d.addActivity(attempt.UpdatedAt, "Controller", label, link, detail)
		}
	}
	if d.GitHub.Intent != nil {
		d.addIssueActivity(*d.GitHub.Intent)
		for _, issue := range d.GitHub.Related {
			d.addIssueActivity(issue)
		}
		for _, pull := range d.GitHub.Pulls {
			d.addPullActivity(pull)
		}
	}
	d.ActivityPartial = d.ActivityPartial || d.GitHub.Partial
	slices.SortStableFunc(d.Activity, func(a, b activityEvent) int {
		return a.At.Compare(b.At)
	})
}

func (d *detailView) addIssueActivity(issue githubmeta.Issue) {
	d.ActivityPartial = d.ActivityPartial || issue.CreatedAt.IsZero()
	d.addActivity(issue.CreatedAt, "GitHub", fmt.Sprintf("Issue #%d opened", issue.Number),
		fmt.Sprintf("%s/issues/%d", d.RepositoryURL, issue.Number), issue.Title)
}

func (d *detailView) addPullActivity(pull githubmeta.Pull) {
	link := fmt.Sprintf("%s/pull/%d", d.RepositoryURL, pull.Number)
	prefix := fmt.Sprintf("PR #%d", pull.Number)
	d.ActivityPartial = d.ActivityPartial || pull.CreatedAt.IsZero() || (pull.State == "MERGED" && pull.MergedAt == nil)
	d.addActivity(pull.CreatedAt, "GitHub", prefix+" opened", link, pull.Title)
	if pull.MergedAt != nil {
		d.addActivity(*pull.MergedAt, "GitHub", prefix+" merged", link, pull.Title)
	}
	if pull.Reviews == nil {
		d.ActivityPartial = true
	} else {
		d.ActivityPartial = d.ActivityPartial || pull.Reviews.PageInfo.HasPreviousPage
		for _, review := range pull.Reviews.Nodes {
			if review == nil {
				d.ActivityPartial = true
				continue
			}
			if review.SubmittedAt == nil {
				continue // Pending review.
			}
			reviewLink := link
			reviewPath := fmt.Sprintf("/pull/%d#pullrequestreview-", pull.Number)
			if owner, suffix, ok := strings.Cut(review.URL, reviewPath); ok &&
				strings.EqualFold(owner, d.RepositoryURL) && suffix != "" &&
				strings.Trim(suffix, "0123456789") == "" {
				reviewLink = link + "#pullrequestreview-" + suffix
			}
			d.addActivity(*review.SubmittedAt, "GitHub", prefix+" review submitted", reviewLink, pull.Title)
		}
	}
	if pull.Commits == nil {
		d.ActivityPartial = true
	} else {
		d.ActivityPartial = d.ActivityPartial || pull.Commits.PageInfo.HasPreviousPage
		for _, node := range pull.Commits.Nodes {
			if node == nil || node.Commit.CommittedDate.IsZero() || !commitOID(node.Commit.OID) {
				d.ActivityPartial = true
				continue
			}
			d.addActivity(node.Commit.CommittedDate, "GitHub", prefix+" commit recorded", link+"/commits/"+node.Commit.OID, node.Commit.OID)
		}
	}
}

func commitOID(oid string) bool {
	return len(oid) == 40 && strings.Trim(oid, "0123456789abcdefABCDEF") == ""
}
