package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub"
	"github.com/vincentlauriat/serversmonitor/internal/hub/config"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("smhub", version)
		return
	}
	cfg, err := config.FromEnv(os.LookupEnv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "smhub:", err)
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	h, err := hub.New(cfg, version, log)
	if err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}
	defer h.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{Addr: cfg.Listen, Handler: h.Handler(), ReadHeaderTimeout: 10 * time.Second}
	// serveErr carries a listen failure back to main: exiting 0 on a port that
	// is already taken would look like a clean stop to systemd or Docker.
	serveErr := make(chan error, 1)
	go func() {
		log.Info("smhub listening", "addr", cfg.Listen, "version", version, "data", cfg.DataDir)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			stop()
			return
		}
		serveErr <- nil
	}()
	go h.Run(ctx)

	var failed error
	select {
	case <-ctx.Done():
	case failed = <-serveErr:
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
	if failed != nil {
		log.Error("http server", "err", failed)
		h.Close()
		os.Exit(1)
	}
	log.Info("smhub stopped")
}
