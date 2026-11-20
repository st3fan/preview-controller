package ingress

import (
	"errors"
	"fmt"
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
	ActivePort(string, int64) (int, error)
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
	port, err := h.router.ActivePort(project, intent)
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
