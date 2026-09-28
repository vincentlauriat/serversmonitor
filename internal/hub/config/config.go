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
	TLS      TLS
}

// TLS is how the hub serves HTTPS itself (lot 10). Off unless a certificate
// or ACME domains are given; a reverse proxy in front stays a supported way.
type TLS struct {
	// CertFile and KeyFile are a certificate someone else renews. The hub
	// re-reads them when they change, without a restart.
	CertFile string
	KeyFile  string
	// Domains turns on ACME (Let's Encrypt by default) for these names.
	Domains []string
	Email   string
	// Directory is the ACME directory URL; empty is Let's Encrypt
	// production. Set it to the staging URL to try without rate limits.
	Directory string
	// HTTPListen is the plain HTTP listener that redirects to HTTPS and
	// answers ACME HTTP-01 challenges. "" means none.
	HTTPListen string
}

// Mode says which of the three the hub runs: "" (plain HTTP), "files" or
// "acme".
func (t TLS) Mode() string {
	switch {
	case t.CertFile != "":
		return "files"
	case len(t.Domains) > 0:
		return "acme"
	}
	return ""
}

func FromEnv(lookup func(string) (string, bool)) (Config, error) {
	get := func(key, def string) string {
		if v, ok := lookup(key); ok && v != "" {
			return v
		}
		return def
	}
	c := Config{DataDir: get("SM_DATA_DIR", "./data")}
	switch strings.ToLower(get("SM_SECURE_COOKIES", "false")) {
	case "1", "true", "yes":
		c.Secure = true
	}
	c.TLS = TLS{CertFile: get("SM_TLS_CERT", ""), KeyFile: get("SM_TLS_KEY", ""),
		Email: get("SM_TLS_EMAIL", ""), Directory: get("SM_ACME_DIRECTORY", "")}
	for _, d := range strings.Split(get("SM_TLS_DOMAINS", ""), ",") {
		if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
			c.TLS.Domains = append(c.TLS.Domains, d)
		}
	}
	if (c.TLS.CertFile == "") != (c.TLS.KeyFile == "") {
		return c, fmt.Errorf("SM_TLS_CERT and SM_TLS_KEY go together: set both or neither")
	}
	if c.TLS.CertFile != "" && len(c.TLS.Domains) > 0 {
		return c, fmt.Errorf("SM_TLS_CERT and SM_TLS_DOMAINS are two ways to get a certificate; set one")
	}
	if c.TLS.Mode() == "" {
		c.Listen = get("SM_LISTEN", ":8090")
		if v, ok := lookup("SM_HTTP_LISTEN"); ok && v != "" && v != "off" {
			return c, fmt.Errorf("SM_HTTP_LISTEN redirects to HTTPS, which needs SM_TLS_CERT or SM_TLS_DOMAINS")
		}
	} else {
		// HTTPS on its standard port, the redirect and the HTTP-01 challenge
		// on theirs. A cookie sent over HTTPS only is no longer a setting:
		// the hub knows it is serving HTTPS.
		c.Listen = get("SM_LISTEN", ":443")
		c.TLS.HTTPListen = get("SM_HTTP_LISTEN", ":80")
		if c.TLS.HTTPListen == "off" {
			c.TLS.HTTPListen = ""
		}
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
