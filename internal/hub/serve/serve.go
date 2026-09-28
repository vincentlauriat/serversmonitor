// Package serve builds the hub's HTTP servers from its configuration: plain
// HTTP as before lot 10, or HTTPS from a certificate on disk or from ACME,
// with an optional plain listener that redirects and answers ACME HTTP-01.
package serve

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/vincentlauriat/serversmonitor/internal/hub/config"
)

// Servers is what main starts. Plain is nil when there is no redirect
// listener; TLS says whether Main serves HTTPS.
type Servers struct {
	Main  *http.Server
	Plain *http.Server
	TLS   bool
}

// Build wires the handler into the servers the configuration asks for. It
// reads nothing from the network: a certificate on disk is loaded here so a
// wrong path fails at startup, not at the first handshake.
func Build(cfg config.Config, h http.Handler) (*Servers, error) {
	main := &http.Server{Addr: cfg.Listen, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	out := &Servers{Main: main}
	var challenge func(http.Handler) http.Handler

	switch cfg.TLS.Mode() {
	case "":
		return out, nil
	case "files":
		r, err := NewReloader(cfg.TLS.CertFile, cfg.TLS.KeyFile)
		if err != nil {
			return nil, err
		}
		main.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: r.GetCertificate}
	case "acme":
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(cfg.TLS.Domains...),
			// The account key and every certificate live beside the
			// database: a restarted container must not ask Let's Encrypt
			// again and run into its rate limits.
			Cache: autocert.DirCache(filepath.Join(cfg.DataDir, "acme")),
			Email: cfg.TLS.Email,
		}
		if cfg.TLS.Directory != "" {
			m.Client = &acme.Client{DirectoryURL: cfg.TLS.Directory}
		}
		// TLSConfig carries the acme-tls/1 protocol, so TLS-ALPN-01 works on
		// the HTTPS port alone, even with the plain listener turned off.
		main.TLSConfig = m.TLSConfig()
		main.TLSConfig.MinVersion = tls.VersionTLS12
		challenge = m.HTTPHandler
	}
	out.TLS = true

	if cfg.TLS.HTTPListen != "" {
		var plain http.Handler = Redirect(cfg.Listen)
		if challenge != nil {
			// Answers /.well-known/acme-challenge/ and hands the rest to
			// the redirect.
			plain = challenge(plain)
		}
		out.Plain = &http.Server{Addr: cfg.TLS.HTTPListen, Handler: plain, ReadHeaderTimeout: 10 * time.Second}
	}
	return out, nil
}

// Redirect sends a plain request to the same host and path over HTTPS. The
// port is kept only when HTTPS is not on 443: a browser adds nothing for
// 443, and an explicit ":443" in every link is noise.
func Redirect(tlsListen string) http.Handler {
	_, port, _ := net.SplitHostPort(tlsListen)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(r.Host); err == nil {
			host = h
		}
		if host == "" {
			http.Error(w, "HTTPS only", http.StatusBadRequest)
			return
		}
		if port != "" && port != "443" {
			host = net.JoinHostPort(host, port)
		}
		code := http.StatusMovedPermanently
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// 301 lets a client turn a POST into a GET; 308 keeps the method
			// and the body, so an old agent or script that posts fails loudly
			// at the right address instead of quietly doing something else.
			code = http.StatusPermanentRedirect
		}
		http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), code)
	})
}

// Reloader serves a certificate from disk and picks up a renewal without a
// restart: certbot, a Kubernetes secret or a hand copy all end with new files
// in place, and nobody should have to remember to bounce the hub after.
type Reloader struct {
	cert, key string

	mu      sync.Mutex
	current *tls.Certificate
	stamp   [2]time.Time
	checked time.Time
	now     func() time.Time
}

func NewReloader(certFile, keyFile string) (*Reloader, error) {
	r := &Reloader{cert: certFile, key: keyFile, now: time.Now}
	if err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

// GetCertificate checks the files at most every ten seconds, so a busy hub
// does not stat twice per handshake. A renewal that cannot be read keeps the
// certificate already loaded: a half-written file must not take HTTPS down.
func (r *Reloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now := r.now(); now.Sub(r.checked) >= 10*time.Second {
		r.checked = now
		if changed, err := r.changed(); err == nil && changed {
			_ = r.loadLocked()
		}
	}
	return r.current, nil
}

func (r *Reloader) load() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checked = r.now()
	return r.loadLocked()
}

func (r *Reloader) loadLocked() error {
	stamp, err := r.stat()
	if err != nil {
		return err
	}
	c, err := tls.LoadX509KeyPair(r.cert, r.key)
	if err != nil {
		return fmt.Errorf("SM_TLS_CERT/SM_TLS_KEY: %w", err)
	}
	r.current, r.stamp = &c, stamp
	return nil
}

func (r *Reloader) changed() (bool, error) {
	stamp, err := r.stat()
	if err != nil {
		return false, err
	}
	return stamp != r.stamp, nil
}

func (r *Reloader) stat() ([2]time.Time, error) {
	var out [2]time.Time
	for i, p := range []string{r.cert, r.key} {
		fi, err := os.Stat(p)
		if err != nil {
			return out, fmt.Errorf("SM_TLS_CERT/SM_TLS_KEY: %w", err)
		}
		out[i] = fi.ModTime()
	}
	return out, nil
}
