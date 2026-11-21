package ingress

import (
	"bytes"
	_ "embed"
	"fmt"
	"html/template"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/st3fan/preview-controller/internal/config"
	"github.com/st3fan/preview-controller/internal/state"
	"golang.org/x/net/html"
)

//go:embed inspector.js
var inspectorScript string

var inspectorTemplate = template.Must(template.New("preview inspector").Parse(
	"<script>(() => {const deployment = {{.}};\n" + inspectorScript + "\n})();</script>",
))

const maxInspectorDocument = 2 << 20

type inspectorDetails struct {
	Repository, Commit, CommitURL, Issue, IssueURL, LogsURL string
}

func inspectorData(repository, dashboardOrigin, project string, intent int64, attempt state.Attempt) inspectorDetails {
	repository = strings.TrimSuffix(strings.TrimRight(repository, "/"), ".git")
	details := inspectorDetails{
		Repository: repository,
		Commit:     attempt.Commit, CommitURL: repository + "/commit/" + url.PathEscape(attempt.Commit),
		Issue: fmt.Sprintf("#%d", intent), IssueURL: fmt.Sprintf("%s/issues/%d", repository, intent),
	}
	if attempt.IssueTitle != "" {
		details.Issue += " " + attempt.IssueTitle
	}
	if dashboardOrigin != "" && (attempt.Backend == config.BackendDocker || attempt.ComposeProject != "") {
		details.LogsURL = fmt.Sprintf("%s/dashboard/logs/%s/%d/%s", dashboardOrigin, project, intent, url.PathEscape(attempt.ID))
	}
	return details
}

func inspectorNavigation(r *http.Request) bool {
	return r.Method == http.MethodGet &&
		r.Header.Get("Sec-Fetch-Dest") == "document" &&
		r.Header.Get("Sec-Fetch-Mode") == "navigate" &&
		r.Header.Get("Range") == ""
}

func injectInspector(response *http.Response, details inspectorDetails) error {
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "text/html" || response.StatusCode != http.StatusOK ||
		response.ContentLength <= 0 || response.ContentLength > maxInspectorDocument ||
		response.Header.Get("Content-Encoding") != "" || response.Header.Get("Content-Range") != "" ||
		response.Header.Get("Content-Disposition") != "" || response.Header.Get("Signature") != "" ||
		response.Header.Get("Signature-Input") != "" ||
		len(response.Trailer) != 0 {
		return nil
	}

	for _, value := range response.Header.Values("Cache-Control") {
		for directive := range strings.SplitSeq(value, ",") {
			if strings.EqualFold(strings.TrimSpace(directive), "no-transform") {
				return nil
			}
		}
	}

	var script bytes.Buffer
	if err := inspectorTemplate.Execute(&script, details); err != nil {
		return err
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxInspectorDocument+1))
	original := response.Body
	if err != nil {
		original.Close()
		return err
	}
	if len(body) > maxInspectorDocument {
		response.Body = &prefixedBody{Reader: io.MultiReader(bytes.NewReader(body), original), Closer: original}
		return nil
	}
	original.Close()

	if offset := bodyEnd(body); offset >= 0 {
		var document bytes.Buffer
		document.Grow(len(body) + script.Len())
		document.Write(body[:offset])
		document.Write(script.Bytes())
		document.Write(body[offset:])
		body = document.Bytes()
		response.ContentLength = int64(len(body))
		response.Header.Set("Content-Length", fmt.Sprint(len(body)))
		for _, header := range []string{"ETag", "Last-Modified", "Accept-Ranges", "Content-MD5", "Digest", "Content-Digest", "Repr-Digest"} {
			response.Header.Del(header)
		}
		response.Header.Set("Cache-Control", "no-store")
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	return nil
}

type prefixedBody struct {
	io.Reader
	io.Closer
}

func bodyEnd(body []byte) int {
	tokens := html.NewTokenizer(bytes.NewReader(body))
	offset := 0
	depth := 0
	for {
		switch tokens.Next() {
		case html.ErrorToken:
			return -1
		case html.StartTagToken:
			name, _ := tokens.TagName()
			if string(name) == "template" {
				depth++
			}
		case html.EndTagToken:
			name, _ := tokens.TagName()
			if string(name) == "body" && depth == 0 {
				return offset
			}
			if string(name) == "template" && depth > 0 {
				depth--
			}
		}
		offset += len(tokens.Raw())
	}
}
