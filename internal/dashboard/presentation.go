package dashboard

import (
	"net/url"
	"runtime/debug"
	"strings"

	"github.com/st3fan/preview-controller/internal/state"
)

const controllerRepository = "https://github.com/st3fan/preview-controller"

// Container builds do not include .git. The release workflow supplies these at link time.
var buildRevision, buildRelease string

type buildIdentity struct {
	Label string
	URL   string
}

func controllerBuild() buildIdentity {
	revision := buildRevision
	if revision == "" {
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, setting := range info.Settings {
				if setting.Key == "vcs.revision" {
					revision = setting.Value
					break
				}
			}
		}
	}
	if !commitOID(revision) {
		return buildIdentity{Label: "Development build", URL: controllerRepository}
	}
	if buildRelease == "" {
		return buildIdentity{Label: revision[:7], URL: controllerRepository}
	}
	return buildIdentity{Label: buildRelease + " / " + revision[:7], URL: controllerRepository + "/releases/tag/" + url.PathEscape(buildRelease)}
}

func attemptOutcome(status state.Status) string {
	switch status {
	case state.Healthy, state.Retired:
		return "Succeeded"
	case "":
		return "Unknown"
	default:
		return strings.ToUpper(string(status[:1])) + string(status[1:])
	}
}
