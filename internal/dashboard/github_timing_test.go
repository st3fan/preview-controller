package dashboard

import (
	"testing"
	"time"

	"github.com/st3fan/preview-controller/internal/githubmeta"
)

func TestFirstLinkedPull(t *testing.T) {
	opened := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	earliest := githubmeta.Pull{Number: 52, CreatedAt: opened}
	tied := githubmeta.Pull{Number: 53, CreatedAt: opened}
	later := githubmeta.Pull{Number: 51, CreatedAt: opened.Add(time.Hour)}
	for _, tc := range []struct {
		name    string
		opened  time.Time
		pulls   []githubmeta.Pull
		partial bool
		want    int64
	}{
		{name: "earliest with tie", opened: opened, pulls: []githubmeta.Pull{later, tied, earliest}, want: 52},
		{name: "missing PR timestamp", opened: opened, pulls: []githubmeta.Pull{earliest, {Number: 53}}},
		{name: "missing issue timestamp", pulls: []githubmeta.Pull{earliest}},
		{name: "PR predates issue", opened: opened.Add(time.Minute), pulls: []githubmeta.Pull{earliest, later}},
		{name: "incomplete links", opened: opened, pulls: []githubmeta.Pull{earliest}, partial: true},
		{name: "no links", opened: opened},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := detailView{GitHub: githubmeta.Result{
				Intent: &githubmeta.Issue{CreatedAt: tc.opened}, Pulls: tc.pulls, Partial: tc.partial,
			}}
			var got int64
			if pull := view.FirstLinkedPull(); pull != nil {
				got = pull.Number
			}
			if got != tc.want {
				t.Errorf("first linked PR = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestPullMergeTiming(t *testing.T) {
	opened := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		pull githubmeta.Pull
		want string
	}{
		{"merged", githubmeta.Pull{State: "MERGED", CreatedAt: opened, MergedAt: new(opened.Add(time.Minute))}, "1m0s"},
		{"same timestamp", githubmeta.Pull{State: "MERGED", CreatedAt: opened, MergedAt: &opened}, "0s"},
		{"missing opening", githubmeta.Pull{State: "MERGED", MergedAt: &opened}, "Timing unavailable"},
		{"missing merge", githubmeta.Pull{State: "MERGED", CreatedAt: opened}, "Timing unavailable"},
		{"zero merge", githubmeta.Pull{State: "MERGED", CreatedAt: opened, MergedAt: new(time.Time)}, "Timing unavailable"},
		{"out of order", githubmeta.Pull{State: "MERGED", CreatedAt: opened, MergedAt: new(opened.Add(-time.Minute))}, "Timing unavailable"},
		{"open", githubmeta.Pull{State: "OPEN", CreatedAt: opened}, "Pending"},
		{"closed", githubmeta.Pull{State: "CLOSED", CreatedAt: opened}, "Closed without merge"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pullMergeTiming(tc.pull); got != tc.want {
				t.Errorf("PR merge timing = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestIntentCompletionTiming(t *testing.T) {
	opened := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		issue *githubmeta.Issue
		want  string
	}{
		{"reclosed as completed", &githubmeta.Issue{State: "CLOSED", StateReason: "COMPLETED", CreatedAt: opened, ClosedAt: new(opened.Add(time.Hour))}, "1h0m0s"},
		{"same timestamp", &githubmeta.Issue{State: "CLOSED", StateReason: "COMPLETED", CreatedAt: opened, ClosedAt: &opened}, "0s"},
		{"reopened", &githubmeta.Issue{State: "OPEN", StateReason: "REOPENED", CreatedAt: opened, ClosedAt: &opened}, "Pending"},
		{"not planned", &githubmeta.Issue{State: "CLOSED", StateReason: "NOT_PLANNED", CreatedAt: opened, ClosedAt: &opened}, "Closed as not planned"},
		{"duplicate", &githubmeta.Issue{State: "CLOSED", StateReason: "DUPLICATE", CreatedAt: opened, ClosedAt: &opened}, "Closed as duplicate"},
		{"unknown reason", &githubmeta.Issue{State: "CLOSED", CreatedAt: opened, ClosedAt: &opened}, "Timing unavailable"},
		{"missing opening", &githubmeta.Issue{State: "CLOSED", StateReason: "COMPLETED", ClosedAt: &opened}, "Timing unavailable"},
		{"missing closure", &githubmeta.Issue{State: "CLOSED", StateReason: "COMPLETED", CreatedAt: opened}, "Timing unavailable"},
		{"out of order", &githubmeta.Issue{State: "CLOSED", StateReason: "COMPLETED", CreatedAt: opened, ClosedAt: new(opened.Add(-time.Minute))}, "Timing unavailable"},
		{"GitHub unavailable", nil, "Timing unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Partial timeline or PR history does not invalidate the root issue fields.
			view := detailView{GitHub: githubmeta.Result{Intent: tc.issue, Partial: true}}
			if got := view.IntentCompletionTiming(); got != tc.want {
				t.Errorf("Intent completion timing = %q, want %q", got, tc.want)
			}
		})
	}
}
