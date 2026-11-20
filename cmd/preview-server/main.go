package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/st3fan/preview-controller/internal/api"
	"github.com/st3fan/preview-controller/internal/config"
	"github.com/st3fan/preview-controller/internal/deploy"
	"github.com/st3fan/preview-controller/internal/gitverify"
	"github.com/st3fan/preview-controller/internal/runtime"
	"github.com/st3fan/preview-controller/internal/state"
)

func main() {
	settings := flag.String("config", config.DefaultPath, "path to global settings JSON")
	flag.Parse()
	if err := run(*settings); err != nil {
		slog.Error("preview server stopped", "error", err)
		os.Exit(1)
	}
}

func run(settings string) error {
	cfg, err := config.Load(settings)
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
	server := &http.Server{
		Addr:              cfg.ManagementListen,
		Handler:           api.Handler(service, cfg.Token()),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			slog.Error("shutdown failed", "error", err)
		}
	}()
	slog.Info("management API listening", "address", cfg.ManagementListen)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
