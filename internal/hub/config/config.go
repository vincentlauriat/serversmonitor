// Package config reads hub settings from the environment.
package config

import (
	"fmt"
	"log/slog"
	"strings"
)

type Config struct {
	Listen   string
	DataDir  string
	Secure   bool
	LogLevel slog.Level
}

func FromEnv(lookup func(string) (string, bool)) (Config, error) {
	get := func(key, def string) string {
		if v, ok := lookup(key); ok && v != "" {
			return v
		}
		return def
	}
	c := Config{Listen: get("SM_LISTEN", ":8090"), DataDir: get("SM_DATA_DIR", "./data")}
	switch strings.ToLower(get("SM_SECURE_COOKIES", "false")) {
	case "1", "true", "yes":
		c.Secure = true
	}
	switch strings.ToLower(get("SM_LOG_LEVEL", "info")) {
	case "debug":
		c.LogLevel = slog.LevelDebug
	case "info":
		c.LogLevel = slog.LevelInfo
	case "warn":
		c.LogLevel = slog.LevelWarn
	case "error":
		c.LogLevel = slog.LevelError
	default:
		return c, fmt.Errorf("SM_LOG_LEVEL must be debug, info, warn or error")
	}
	return c, nil
}
