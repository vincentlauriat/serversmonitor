package serve

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/config"
)

// selfSigned writes a certificate for 127.0.0.1 with the given common name
// and returns its DER bytes, so a test can tell two certificates apart.
func selfSigned(t *testing.T, certPath, keyPath, cn string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		t.Fatal(err)
	}
	return der
}

func TestRedirect(t *testing.T) {
	for _, tc := range []struct {
		name, listen, method, target, want string
		code                               int
	}{
		{"443 needs no port", ":443", "GET", "http://hub.example.com/hosts/3?x=1", "https://hub.example.com/hosts/3?x=1", 301},
		{"another port is kept", ":8443", "GET", "http://hub.example.com:80/", "https://hub.example.com:8443/", 301},
		{"a POST keeps its method", ":443", "POST", "http://hub.example.com/api/v1/login", "https://hub.example.com/api/v1/login", 308},
	} {
		rec := httptest.NewRecorder()
		Redirect(tc.listen).ServeHTTP(rec, httptest.NewRequest(tc.method, tc.target, nil))
		if rec.Code != tc.code || rec.Header().Get("Location") != tc.want {
			t.Errorf("%s: %d %q, want %d %q", tc.name, rec.Code, rec.Header().Get("Location"), tc.code, tc.want)
		}
	}
}

func TestReloaderPicksUpARenewal(t *testing.T) {
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	first := selfSigned(t, cert, key, "first")
	r, err := NewReloader(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	r.now = func() time.Time { return now }
	got := func() []byte {
		c, err := r.GetCertificate(nil)
		if err != nil {
			t.Fatal(err)
		}
		return c.Certificate[0]
	}

	second := selfSigned(t, cert, key, "second")
	later := time.Now().Add(time.Minute)
	os.Chtimes(cert, later, later)
	os.Chtimes(key, later, later)
	if !slices.Equal(got(), first) {
		t.Fatal("the files are not looked at more than every ten seconds")
	}
	now = now.Add(11 * time.Second)
	if !slices.Equal(got(), second) {
		t.Fatal("a renewed certificate must be served without a restart")
	}

	// A renewal caught half written keeps the certificate that works.
	os.WriteFile(cert, []byte("not a certificate"), 0o600)
	evenLater := later.Add(time.Minute)
	os.Chtimes(cert, evenLater, evenLater)
	now = now.Add(11 * time.Second)
	if !slices.Equal(got(), second) {
		t.Fatal("an unreadable renewal must not replace a working certificate")
	}
}

func TestAMissingCertificateFailsAtStartup(t *testing.T) {
	cfg := config.Config{Listen: ":443", TLS: config.TLS{CertFile: "/nope/c.pem", KeyFile: "/nope/k.pem"}}
	if _, err := Build(cfg, http.NotFoundHandler()); err == nil || !strings.Contains(err.Error(), "SM_TLS_CERT") {
		t.Fatalf("err = %v", err)
	}
}

// A real handshake against the certificate on disk, and the plain listener
// sending a browser there.
func TestFilesModeServesHTTPS(t *testing.T) {
	dir := t.TempDir()
	cert, key := filepath.Join(dir, "c.pem"), filepath.Join(dir, "k.pem")
	der := selfSigned(t, cert, key, "hub")
	cfg := config.Config{Listen: "127.0.0.1:0", TLS: config.TLS{CertFile: cert, KeyFile: key, HTTPListen: "127.0.0.1:0"}}
	s, err := Build(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "hub") }))
	if err != nil {
		t.Fatal(err)
	}
	if !s.TLS || s.Plain == nil {
		t.Fatalf("servers = %+v", s)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", s.Main.TLSConfig)
	if err != nil {
		t.Fatal(err)
	}
	go s.Main.Serve(ln)
	t.Cleanup(func() { s.Main.Close() })

	parsed, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	resp, err := client.Get("https://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "hub" {
		t.Fatalf("body = %q", body)
	}
}

func TestACMEModeWiring(t *testing.T) {
	cfg := config.Config{Listen: ":443", DataDir: t.TempDir(),
		TLS: config.TLS{Domains: []string{"hub.example.com"}, HTTPListen: ":80"}}
	s, err := Build(cfg, http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}
	if !s.TLS || !slices.Contains(s.Main.TLSConfig.NextProtos, "acme-tls/1") {
		t.Fatalf("TLS-ALPN-01 must be served on the HTTPS port: %+v", s.Main.TLSConfig.NextProtos)
	}
	// The plain listener answers the HTTP-01 path itself and redirects the rest.
	rec := httptest.NewRecorder()
	s.Plain.Handler.ServeHTTP(rec, httptest.NewRequest("GET", "http://hub.example.com/hosts", nil))
	if rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "https://hub.example.com/hosts" {
		t.Fatalf("redirect = %d %q", rec.Code, rec.Header().Get("Location"))
	}
	rec = httptest.NewRecorder()
	s.Plain.Handler.ServeHTTP(rec, httptest.NewRequest("GET", "http://hub.example.com/.well-known/acme-challenge/token", nil))
	if rec.Code == http.StatusMovedPermanently {
		t.Fatal("an ACME challenge must be answered, not redirected")
	}
	// A name nobody listed is never asked for.
	if _, err := s.Main.TLSConfig.GetCertificate(&tls.ClientHelloInfo{ServerName: "evil.example.com"}); err == nil {
		t.Fatal("a certificate must only be requested for the configured domains")
	}
}

func TestPlainModeIsUnchanged(t *testing.T) {
	s, err := Build(config.Config{Listen: ":8090"}, http.NotFoundHandler())
	if err != nil || s.TLS || s.Plain != nil || s.Main.TLSConfig != nil {
		t.Fatalf("servers = %+v %v", s, err)
	}
}
