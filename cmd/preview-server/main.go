package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/st3fan/preview-controller/internal/api"
	"github.com/st3fan/preview-controller/internal/config"
	"github.com/st3fan/preview-controller/internal/dashboard"
	"github.com/st3fan/preview-controller/internal/deploy"
	"github.com/st3fan/preview-controller/internal/gitverify"
	"github.com/st3fan/preview-controller/internal/health"
	"github.com/st3fan/preview-controller/internal/ingress"
	"github.com/st3fan/preview-controller/internal/runtime"
	"github.com/st3fan/preview-controller/internal/state"
)

func main() {
	projects := flag.String("projects", config.DefaultProjectsPath, "path to project definitions JSON")
	verbose := flag.Bool("verbose", false, "log controller activity and deployment progress")
	flag.Parse()
	if *verbose {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
	}
	if err := run(*projects); err != nil {
		slog.Error("preview server stopped", "error", err)
		os.Exit(1)
	}
}

func run(projects string) error {
	cfg, err := config.Load(projects)
	if err != nil {
		return err
	}
	store, err := state.New(cfg.StateRoot)
	if err != nil {
		return err
	}
	runner, err := runtime.New(cfg.WorkspaceRoot)
	if err != nil {
		return err
	}
	service, err := deploy.New(cfg, store, gitverify.Verifier{}, runner)
	if err != nil {
		return err
	}
	defer service.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	monitor := health.New(store, cfg.Projects, runner.ProbeHealth)
	managementMux := http.NewServeMux()
	dashboardMux := http.NewServeMux()
	dashboard.Register(dashboardMux, ctx, cfg, store.Load, service.ListEnvironments, store.Changes, monitor)
	dashboard.RegisterLogs(dashboardMux, ctx, store.Load, runner.OpenLogs)
	dashboard.RegisterDelete(dashboardMux, service.DeleteEnvironment)
	dashboard.RegisterArchive(dashboardMux, service.ArchiveEnvironment, service.RestoreEnvironment)
	dashboardHandler, err := dashboard.WithAccess(ctx, cfg, dashboardMux)
	if err != nil {
		return err
	}
	managementMux.Handle("GET /{$}", dashboardHandler)
	managementMux.Handle("/dashboard/", dashboardHandler)
	api.RegisterMCP(managementMux, service, cfg.Projects, cfg.Token(), cfg.MCPOAuth, runner.LogExcerpt)
	managementMux.Handle("/", api.Handler(service, cfg.Token()))
	management := &http.Server{
		Addr:              cfg.ManagementListen,
		Handler:           managementMux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	preview := &http.Server{
		Addr:              cfg.PreviewListen,
		Handler:           ingress.Handler(cfg, service),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	managementListener, err := net.Listen("tcp", cfg.ManagementListen)
	if err != nil {
		return err
	}
	defer managementListener.Close()
	previewListener, err := net.Listen("tcp", cfg.PreviewListen)
	if err != nil {
		return err
	}
	defer previewListener.Close()
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		monitor.Run(ctx)
	}()
	errorsFromServers := make(chan error, 2)
	go func() { errorsFromServers <- management.Serve(managementListener) }()
	go func() { errorsFromServers <- preview.Serve(previewListener) }()
	slog.Info("management API listening", "address", cfg.ManagementListen)
	slog.Info("preview ingress listening", "address", cfg.PreviewListen)
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errorsFromServers:
	}
	stop()
	<-monitorDone
	slog.Debug("shutting down preview server")
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := management.Shutdown(shutdown); err != nil {
		slog.Error("management shutdown failed", "error", err)
	}
	if err := preview.Shutdown(shutdown); err != nil {
		slog.Error("preview shutdown failed", "error", err)
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	return nil
}
