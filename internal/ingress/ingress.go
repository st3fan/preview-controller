package ingress

import (
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"github.com/st3fan/preview-controller/internal/config"
	"github.com/st3fan/preview-controller/internal/state"
)

type Router interface {
	PreviewDeployment(string, int64) (state.Attempt, state.Status, error)
}

type handler struct {
	cfg       config.Config
	router    Router
	transport *http.Transport
}

func Handler(cfg config.Config, router Router) http.Handler {
	return &handler{cfg: cfg, router: router, transport: &http.Transport{Proxy: nil}}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	project, intent, ok := h.parseHost(r.Host)
	if !ok {
		http.NotFound(w, r)
		return
	}
	attempt, status, err := h.router.PreviewDeployment(project, intent)
	port := attempt.Port
	if err == nil && port == 0 {
		serveStatus(w, status)
		return
	}
	if errors.Is(err, state.ErrNotFound) {
		http.Error(w, "preview is not available", http.StatusServiceUnavailable)
		return
	}
	if err != nil || port < 1 || port > 65535 {
		http.Error(w, "preview routing failed", http.StatusBadGateway)
		return
	}
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(port))}
	proxy := &httputil.ReverseProxy{
		Transport: h.transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
			if pr.In.Header.Get("X-Forwarded-Proto") == "https" {
				pr.Out.Header.Set("X-Forwarded-Proto", "https")
			}
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "preview backend is unavailable", http.StatusBadGateway)
		},
	}
	settings := h.cfg.Projects[project]
	if settings.PreviewInspector && inspectorNavigation(r) {
		proxy.ModifyResponse = func(response *http.Response) error {
			return injectInspector(response, inspectorData(settings.Repository, h.cfg.DashboardOrigin, project, intent, attempt))
		}
	}
	proxy.ServeHTTP(w, r)
}

func (h *handler) parseHost(host string) (string, int64, bool) {
	suffix := "." + h.cfg.PreviewDomain
	if !strings.HasSuffix(host, suffix) {
		return "", 0, false
	}
	label := strings.TrimSuffix(host, suffix)
	separator := strings.LastIndexByte(label, '-')
	if separator < 1 {
		return "", 0, false
	}
	project, intentText := label[:separator], label[separator+1:]
	if _, ok := h.cfg.Projects[project]; !ok {
		return "", 0, false
	}
	intent, err := strconv.ParseInt(intentText, 10, 64)
	if err != nil || intent < 1 || strconv.FormatInt(intent, 10) != intentText {
		return "", 0, false
	}
	if expected, err := h.cfg.URL(project, intent); err != nil || expected != fmt.Sprintf("https://%s", host) {
		return "", 0, false
	}
	return project, intent, true
}

var statusPage = template.Must(template.New("preview status").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
{{if .Refresh}}<meta http-equiv="refresh" content="3">{{end}}
<title>{{.Title}}</title>
</head>
<body>
<main>
<h1>{{.Title}}</h1>
<p>{{.Message}}</p>
</main>
</body>
</html>
`))

func serveStatus(w http.ResponseWriter, status state.Status) {
	page := struct {
		Title, Message string
		Refresh        bool
	}{Title: "Preview deployment in progress", Refresh: true}
	switch status {
	case state.Queued:
		page.Message = "Queued for deployment."
	case state.Fetching:
		page.Message = "Fetching source code."
	case state.Verified:
		page.Message = "Source verified."
	case state.Building:
		page.Message = "Building the preview."
	case state.Starting:
		page.Message = "Starting the preview and checking its health."
	case state.Failed:
		page.Title = "Preview deployment failed"
		page.Message = "The preview will be available after a successful deployment."
		page.Refresh = false
	default:
		http.Error(w, "preview is not available", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = statusPage.Execute(w, page)
}
