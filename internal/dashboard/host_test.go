package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/st3fan/preview-controller/internal/config"
	"github.com/st3fan/preview-controller/internal/health"
	"github.com/st3fan/preview-controller/internal/state"
)

func TestHostProcReadings(t *testing.T) {
	proc := fstest.MapFS{
		"meminfo": {Data: []byte("MemTotal: 4194304 kB\nMemFree: 1 kB\nMemAvailable: 1048576 kB\n")},
		"loadavg": {Data: []byte("2.41 1.90 1.30 1/128 217")},
		"stat":    {Data: []byte("cpu  10 20\ncpu0 5 10\ncpu3 5 10\nintr 10\n")},
	}
	mem := readMemory(proc)
	if mem == nil || *mem != (capacity{"3.0", "4.0", "1.0", 75}) {
		t.Fatalf("memory: %+v", mem)
	}
	load := readLoad(proc)
	if load == nil || *load != (cpuLoad{"2.41", "1.90", "1.30", 2}) {
		t.Fatalf("load: %+v", load)
	}
	delete(proc, "meminfo")
	if readMemory(proc) != nil || readLoad(proc) == nil {
		t.Fatal("memory failure affected load")
	}
	proc["loadavg"].Data = []byte("NaN 1.0 0.0")
	if readLoad(proc) != nil {
		t.Fatal("accepted invalid load")
	}
	if got := usage(10<<30, 3<<30, 2<<30); got == nil || got.Used != "7.0" || got.Available != "2.0" {
		t.Fatalf("reserved disk blocks: %+v", got)
	}
}

func TestHostBarOnDashboardPages(t *testing.T) {
	store, err := state.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env := state.Environment{ID: state.ID("demo", 69), Project: "demo", Intent: 69}
	if err := store.Save(env); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	Register(mux, t.Context(), config.Config{WorkspaceRoot: t.TempDir()}, store.Load, store.List, store.Changes, health.New(store, nil, nil))
	render := func(path string) string {
		t.Helper()
		r := httptest.NewRecorder()
		mux.ServeHTTP(r, httptest.NewRequest("GET", path, nil))
		if r.Code != 200 {
			t.Fatalf("%s returned %d", path, r.Code)
		}
		return r.Body.String()
	}
	values := render("/dashboard/host")
	requireText(t, values, "Memory", "Workspace disk", "CPU load average", "Sampled")
	for _, path := range []string{"/", "/dashboard/previews/demo/69"} {
		page := render(path)
		header, bar, main := strings.Index(page, "</header>"), strings.Index(page, "id=\"host-bar\""), strings.Index(page, "<main")
		if header < 0 || bar < header || main < bar {
			t.Fatalf("host bar misplaced on %s", path)
		}
		requireText(t, page, "/dashboard/assets/host.js", "CPU load average")
	}
}
