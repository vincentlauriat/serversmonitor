package azure

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var now0 = time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)

type sourceFunc struct {
	name string
	fn   func(context.Context) (Token, error)
}

func (s sourceFunc) Name() string                             { return s.name }
func (s sourceFunc) Token(ctx context.Context) (Token, error) { return s.fn(ctx) }

func TestParseTokenResponseAcceptsAllThreeShapes(t *testing.T) {
	// The three shapes are not interchangeable and getting this wrong yields a
	// token that is either never refreshed or refreshed on every single call.
	cases := map[string]struct {
		body string
		want time.Time
	}{
		"entra, expires_in as a number": {
			`{"token_type":"Bearer","expires_in":3599,"access_token":"t"}`,
			now0.Add(3599 * time.Second),
		},
		"imds, expires_in as a string": {
			`{"access_token":"t","refresh_token":"","expires_in":"3599","expires_on":"1789725599","not_before":"1789721999","resource":"https://management.azure.com/","token_type":"Bearer"}`,
			now0.Add(3599 * time.Second),
		},
		"app service, no expires_in at all": {
			`{"access_token":"t","expires_on":"1789725599","resource":"https://management.azure.com/","token_type":"Bearer","client_id":"c"}`,
			time.Unix(1789725599, 0).UTC(),
		},
	}
	for name, c := range cases {
		got, err := parseTokenResponse([]byte(c.body), now0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.Value != "t" {
			t.Errorf("%s: value = %q", name, got.Value)
		}
		if !got.ExpiresAt.Equal(c.want) {
			t.Errorf("%s: expires = %s, want %s", name, got.ExpiresAt, c.want)
		}
	}
}

func TestParseTokenResponseRejectsATokenWithNoExpiry(t *testing.T) {
	// A token with no expiry would be cached forever and start failing silently.
	if _, err := parseTokenResponse([]byte(`{"access_token":"t"}`), now0); err == nil {
		t.Fatal("a token with neither expires_in nor expires_on must be an error")
	}
	if _, err := parseTokenResponse([]byte(`{"expires_in":3599}`), now0); err == nil {
		t.Fatal("a response with no access_token must be an error")
	}
}

func TestClientSecretSendsTheDocumentedForm(t *testing.T) {
	var got url.Values
	var path, ctype string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		ctype = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		got, _ = url.ParseQuery(string(raw))
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"token_type":"Bearer","expires_in":3599,"access_token":"abc"}`)
	}))
	defer srv.Close()

	s := NewClientSecret("tenant-1", "client-1", "s3cr3t").WithEndpoint(srv.URL)
	if s.Name() != "client_secret" {
		t.Fatalf("name = %q", s.Name())
	}
	tok, err := s.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok.Value != "abc" {
		t.Fatalf("token = %q", tok.Value)
	}
	if path != "/tenant-1/oauth2/v2.0/token" {
		t.Fatalf("path = %q", path)
	}
	if ctype != "application/x-www-form-urlencoded" {
		t.Fatalf("content-type = %q", ctype)
	}
	for k, want := range map[string]string{
		"grant_type":    "client_credentials",
		"client_id":     "client-1",
		"client_secret": "s3cr3t",
		"scope":         "https://management.azure.com/.default",
	} {
		if got.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, got.Get(k), want)
		}
	}
}

func TestClientSecretSurfacesAzuresOwnError(t *testing.T) {
	// AADSTS7000215 tells Vincent his secret is wrong. "authentication failed"
	// tells him nothing.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"invalid_client","error_description":"AADSTS7000215: Invalid client secret provided.","error_codes":[7000215]}`)
	}))
	defer srv.Close()
	_, err := NewClientSecret("t", "c", "hunter2").WithEndpoint(srv.URL).Token(context.Background())
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "AADSTS7000215") {
		t.Fatalf("Azure's own words must reach the caller: %v", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("the secret must never appear in an error: %v", err)
	}
	if Retryable(err) {
		t.Fatalf("a bad secret fails identically forever: %v", err)
	}
}

func TestManagedIdentityPrefersTheAppServiceEndpoint(t *testing.T) {
	var gotHeader, gotResource, gotAPIVersion, gotClientID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-IDENTITY-HEADER")
		gotResource = r.URL.Query().Get("resource")
		gotAPIVersion = r.URL.Query().Get("api-version")
		gotClientID = r.URL.Query().Get("client_id")
		io.WriteString(w, `{"access_token":"mi","expires_on":"1789725599","token_type":"Bearer"}`)
	}))
	defer srv.Close()
	env := map[string]string{"IDENTITY_ENDPOINT": srv.URL + "/MSI/token", "IDENTITY_HEADER": "hdr"}
	s := NewManagedIdentity("", func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if s.Name() != "managed_identity" {
		t.Fatalf("name = %q", s.Name())
	}
	tok, err := s.Token(context.Background())
	if err != nil || tok.Value != "mi" {
		t.Fatalf("token = %+v err = %v", tok, err)
	}
	if gotHeader != "hdr" {
		t.Errorf("X-IDENTITY-HEADER = %q", gotHeader)
	}
	if gotResource != ARMResource {
		t.Errorf("resource = %q", gotResource)
	}
	if gotAPIVersion != "2019-08-01" {
		t.Errorf("api-version = %q", gotAPIVersion)
	}
	if gotClientID != "" {
		t.Errorf("no user-assigned id configured means no client_id, got %q", gotClientID)
	}
}

func TestManagedIdentitySelectsAUserAssignedIdentity(t *testing.T) {
	var gotClientID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotClientID = r.URL.Query().Get("client_id")
		io.WriteString(w, `{"access_token":"mi","expires_on":"1789725599","token_type":"Bearer"}`)
	}))
	defer srv.Close()
	env := map[string]string{"IDENTITY_ENDPOINT": srv.URL, "IDENTITY_HEADER": "hdr"}
	s := NewManagedIdentity("uami-client-id", func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if _, err := s.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotClientID != "uami-client-id" {
		t.Fatalf("client_id = %q", gotClientID)
	}
}

func TestManagedIdentityFallsBackToIMDS(t *testing.T) {
	var gotMetadata, gotAPIVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMetadata = r.Header.Get("Metadata")
		gotAPIVersion = r.URL.Query().Get("api-version")
		io.WriteString(w, `{"access_token":"imds","expires_in":"3599","expires_on":"1789725599","token_type":"Bearer"}`)
	}))
	defer srv.Close()
	// No IDENTITY_ENDPOINT: the IMDS path is taken, pointed at the fake.
	s := NewManagedIdentity("", func(string) (string, bool) { return "", false })
	s.imdsBase = srv.URL
	tok, err := s.Token(context.Background())
	if err != nil || tok.Value != "imds" {
		t.Fatalf("token = %+v err = %v", tok, err)
	}
	if gotMetadata != "true" {
		t.Errorf("Metadata header = %q, must be exactly \"true\"", gotMetadata)
	}
	if gotAPIVersion != "2018-02-01" {
		t.Errorf("api-version = %q", gotAPIVersion)
	}
}

func TestManagedIdentityRetryClassification(t *testing.T) {
	// From the IMDS docs: 404, 410, 429 and 5xx are retryable; other 4xx are
	// design-time errors.
	for _, c := range []struct {
		status int
		retry  bool
	}{{404, true}, {410, true}, {429, true}, {500, true}, {400, false}, {401, false}, {403, false}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			io.WriteString(w, `{"error":"x","error_description":"y"}`)
		}))
		s := NewManagedIdentity("", func(string) (string, bool) { return "", false })
		s.imdsBase = srv.URL
		_, err := s.Token(context.Background())
		srv.Close()
		if err == nil {
			t.Fatalf("status %d must be an error", c.status)
		}
		if Retryable(err) != c.retry {
			t.Errorf("status %d: retryable = %v, want %v", c.status, Retryable(err), c.retry)
		}
	}
}

func TestCachedRefreshesFiveMinutesEarly(t *testing.T) {
	var calls atomic.Int32
	clock := now0
	src := sourceFunc{name: "fake", fn: func(context.Context) (Token, error) {
		calls.Add(1)
		return Token{Value: "t", ExpiresAt: clock.Add(time.Hour)}, nil
	}}
	c := NewCached(src, func() time.Time { return clock })
	if _, err := c.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("a fresh token must be reused, %d calls", calls.Load())
	}
	clock = clock.Add(56 * time.Minute) // 4 minutes of life left
	if _, err := c.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("a token inside the 5-minute margin must be refreshed, %d calls", calls.Load())
	}
}

func TestCachedDoesNotCacheAFailure(t *testing.T) {
	var calls atomic.Int32
	src := sourceFunc{name: "fake", fn: func(context.Context) (Token, error) {
		calls.Add(1)
		return Token{}, errors.New("boom")
	}}
	c := NewCached(src, func() time.Time { return now0 })
	c.Token(context.Background())
	c.Token(context.Background())
	if calls.Load() != 2 {
		t.Fatalf("a failure must be retried, not cached, %d calls", calls.Load())
	}
}
