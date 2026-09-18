package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/shirou/gopsutil/v4/mem"
	"github.com/vincentlauriat/serversmonitor/internal/agent/collect"
	"github.com/vincentlauriat/serversmonitor/internal/agent/link"
	"github.com/vincentlauriat/serversmonitor/internal/proto"
)

var version = "dev"

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("smagent", version)
		return
	}
	hubURL := flag.String("hub", envOr("SM_HUB", ""), "hub url, e.g. wss://hub.example (env SM_HUB)")
	token := flag.String("token", envOr("SM_TOKEN", ""), "host token from the hub (env SM_TOKEN)")
	insecure := flag.Bool("insecure", envOr("SM_INSECURE", "") == "1", "allow plain ws:// to a remote host (env SM_INSECURE=1)")
	dockerSock := flag.String("docker-socket", envOr("SM_DOCKER_SOCKET", "/var/run/docker.sock"), "docker socket path; empty disables (env SM_DOCKER_SOCKET)")
	debug := flag.Bool("debug", false, "debug logging")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if *hubURL == "" || *token == "" {
		fmt.Fprintln(os.Stderr, "smagent: --hub and --token are required (or SM_HUB / SM_TOKEN)")
		os.Exit(2)
	}
	if err := link.ValidateURL(*hubURL, *insecure); err != nil {
		fmt.Fprintln(os.Stderr, "smagent:", err)
		os.Exit(2)
	}

	var docker collect.DockerReader
	caps := []string{}
	if *dockerSock != "" {
		if _, err := os.Stat(*dockerSock); err == nil {
			docker = collect.NewDocker(*dockerSock)
			caps = append(caps, "docker")
		} else {
			log.Info("docker socket not found, container metrics disabled", "path", *dockerSock)
		}
	}
	collector := collect.New(collect.NewGopsutil(), docker, nil, log)

	hostname, _ := os.Hostname()
	var memTotal int64
	if vm, err := mem.VirtualMemory(); err == nil {
		memTotal = int64(vm.Total)
	}
	hello := proto.Hello{AgentVersion: version, OS: runtime.GOOS, Arch: runtime.GOARCH, Hostname: hostname,
		Cores: runtime.NumCPU(), MemTotal: memTotal, Capabilities: caps}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("smagent starting", "version", version, "hub", *hubURL, "docker", docker != nil)
	link.Run(ctx, link.Config{HubURL: *hubURL, Token: *token, Insecure: *insecure, Hello: hello, Log: log,
		OnWelcome: func(w *proto.Welcome) { collector.SetIgnoreMounts(w.IgnoreMounts) }}, collector.Collect)
	log.Info("smagent stopped")
}
