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
	_ "time/tzdata" // the runtime image is FROM scratch and has no zone database

	"github.com/vincentlauriat/serversmonitor/internal/hub"
	"github.com/vincentlauriat/serversmonitor/internal/hub/config"
	"github.com/vincentlauriat/serversmonitor/internal/hub/serve"
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
	servers, err := serve.Build(cfg, h.Handler())
	if err != nil {
		log.Error("startup failed", "err", err)
		h.Close()
		os.Exit(1)
	}
	// serveErr carries a listen failure back to main: exiting 0 on a port that
	// is already taken would look like a clean stop to systemd or Docker. It
	// holds one slot per server, so neither goroutine blocks on the other.
	serveErr := make(chan error, 2)
	run := func(srv *http.Server, tlsOn bool) {
		var err error
		if tlsOn {
			// The certificate comes from TLSConfig.GetCertificate, never from
			// these two arguments.
			err = srv.ListenAndServeTLS("", "")
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			stop()
			return
		}
		serveErr <- nil
	}
	log.Info("smhub listening", "addr", cfg.Listen, "tls", cfg.TLS.Mode(), "version", version, "data", cfg.DataDir)
	go run(servers.Main, servers.TLS)
	if servers.Plain != nil {
		log.Info("redirecting plain HTTP to HTTPS", "addr", servers.Plain.Addr)
		go run(servers.Plain, false)
	}
	go h.Run(ctx)

	var failed error
	select {
	case <-ctx.Done():
	case failed = <-serveErr:
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	servers.Main.Shutdown(shutdownCtx)
	if servers.Plain != nil {
		servers.Plain.Shutdown(shutdownCtx)
	}
	if failed != nil {
		log.Error("http server", "err", failed)
		h.Close()
		os.Exit(1)
	}
	log.Info("smhub stopped")
}
