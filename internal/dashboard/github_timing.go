package dashboard

import (
	"time"

	"github.com/st3fan/preview-controller/internal/githubmeta"
)

func (d detailView) FirstLinkedPull() *githubmeta.Pull {
	if d.GitHub.Intent == nil || d.GitHub.Intent.CreatedAt.IsZero() || d.GitHub.Partial {
		return nil
	}
	var first *githubmeta.Pull
	for i := range d.GitHub.Pulls {
		pull := &d.GitHub.Pulls[i]
		if pull.CreatedAt.IsZero() {
			return nil
		}
		if first == nil || pull.CreatedAt.Before(first.CreatedAt) ||
			(pull.CreatedAt.Equal(first.CreatedAt) && pull.Number < first.Number) {
			first = pull
		}
	}
	if first == nil || first.CreatedAt.Before(d.GitHub.Intent.CreatedAt) {
		return nil
	}
	return first
}

func pullMergeTiming(pull githubmeta.Pull) string {
	if pull.CreatedAt.IsZero() {
		return "Timing unavailable"
	}
	switch pull.State {
	case "OPEN":
		return "Pending"
	case "CLOSED":
		return "Closed without merge"
	case "MERGED":
		if pull.MergedAt != nil && !pull.MergedAt.IsZero() && !pull.MergedAt.Before(pull.CreatedAt) {
			return elapsedTime(pull.CreatedAt, *pull.MergedAt)
		}
	}
	return "Timing unavailable"
}

func (d detailView) IntentCompletionTiming() string {
	issue := d.GitHub.Intent
	if issue == nil {
		return "Timing unavailable"
	}
	switch issue.State {
	case "OPEN":
		return "Pending"
	case "CLOSED":
		switch issue.StateReason {
		case "NOT_PLANNED":
			return "Closed as not planned"
		case "DUPLICATE":
			return "Closed as duplicate"
		case "COMPLETED":
			if !issue.CreatedAt.IsZero() && issue.ClosedAt != nil &&
				!issue.ClosedAt.IsZero() && !issue.ClosedAt.Before(issue.CreatedAt) {
				return elapsedTime(issue.CreatedAt, *issue.ClosedAt)
			}
		}
	}
	return "Timing unavailable"
}

func elapsedTime(start, end time.Time) string {
	return end.Sub(start).Round(time.Millisecond).String()
}
