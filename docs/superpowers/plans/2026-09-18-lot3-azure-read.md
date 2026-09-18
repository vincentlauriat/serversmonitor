# ServersMonitor Lot 3 (Azure read) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development or superpowers:executing-plans to implement this plan task-by-task.

**Goal:** ServersMonitor shows, without being asked, every resource in the configured Azure scope: what it is, whether it is running, and what it has cost this month.

**Architecture:** `internal/hub/azure` owns everything Azure. `Source` produces a bearer token; `Client` calls ARM over `net/http` with pagination and retry; `Inventory` and `Cost` turn responses into rows; the hub runs the periodic sync. Read only — nothing writes to Azure.

**Tech Stack:** Go standard library only. No Azure SDK, in the taste already set by `docker.go` and `net/smtp`.

**Spec:** `docs/superpowers/specs/2026-09-18-lot3-azure-read-design.md`

## Global Constraints

- Branch `feat/lot3-azure-read`; never commit to `main`. Conventional commits, English, no `Co-Authored-By` trailer.
- **Silence is never zero.** A state not read is `NULL`, a cost not reported has no row, a failed sync changes nothing but the `azure_sync` row.
- **A sync is successful only when every page of every call succeeded.** Any failure anywhere aborts the whole sweep.
- **Resource ids are lowercased** before storage and before any join. ARM returns mixed case, Cost Management returns lowercase.
- **Never log or return the client secret.** Reads expose `azure_client_secret_set` only.
- Every task boundary leaves `go build ./...` green. `go vet ./...` clean, `svelte-check` zero errors.
- No test may reach the network. Every Azure endpoint is an `httptest` server.

---

## File structure

| Path | Responsibility |
|---|---|
| `internal/hub/azure/token.go` | `Source`, `ClientSecret`, `ManagedIdentity`, the caching wrapper |
| `internal/hub/azure/client.go` | ARM HTTP: GET with pagination, POST, retry, classification |
| `internal/hub/azure/inventory.go` | Catalogue pass plus `Microsoft.Web` enrichment |
| `internal/hub/azure/cost.go` | The columnar cost query |
| `internal/hub/azure/config.go` | Settings load, save and validation |
| `internal/hub/store/migrations/0003_azure.sql` | `azure_resources`, `azure_costs`, `azure_sync` |
| `internal/hub/store/azure.go` | Upsert, sweep, costs, sync state, the display join |
| `internal/hub/azure_sync.go` | The hub's periodic sync and `TestAzure` |
| `internal/hub/server/api_azure.go` | `/api/v1/azure`, its settings and its test button |
| `web/src/routes/azure/+page.svelte` | The Azure page |
| `web/src/lib/azure.ts` | Pure helpers, unit-tested |

---

### Task 1: Token sources

**Files:**
- Create: `internal/hub/azure/token.go`
- Test: `internal/hub/azure/token_test.go`

**Interfaces:**
- Produces:
```go
const ARMResource = "https://management.azure.com/"
const ARMScope = ARMResource + ".default"

type Token struct{ Value string; ExpiresAt time.Time }
type Source interface {
    Name() string                                // "client_secret" | "managed_identity"
    Token(ctx context.Context) (Token, error)
}

type ClientSecretSource struct{ … }
func NewClientSecret(tenantID, clientID, secret string) *ClientSecretSource
func (s *ClientSecretSource) WithEndpoint(base string) *ClientSecretSource   // tests only

type ManagedIdentitySource struct{ … }
// env is os.LookupEnv in production; tests inject a map.
func NewManagedIdentity(miClientID string, env func(string) (string, bool)) *ManagedIdentitySource

type Cached struct{ … }
func NewCached(src Source, now func() time.Time) *Cached
func (c *Cached) Token(ctx context.Context) (Token, error)
func (c *Cached) Name() string

// parseTokenResponse accepts all three shapes Azure returns.
func parseTokenResponse(body []byte, now time.Time) (Token, error)
```

**Read before implementing.** §5 of the spec carries the three contracts, verified against Microsoft
Learn on 2026-09-18. The response shapes differ and that difference is the whole point of
`parseTokenResponse`:

| Endpoint | `expires_in` | `expires_on` |
|---|---|---|
| `login.microsoftonline.com` | JSON **number** | absent |
| IMDS `169.254.169.254` | JSON **string** | JSON string, Unix epoch |
| App Service `IDENTITY_ENDPOINT` | **absent** | JSON string, Unix epoch |

- [ ] **Step 1: Write the failing tests**

`internal/hub/azure/token_test.go`:
```go
package azure

import (
	"context"
	"encoding/json"
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
	_, err := NewClientSecret("t", "c", "wrong").WithEndpoint(srv.URL).Token(context.Background())
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "AADSTS7000215") {
		t.Fatalf("Azure's own words must reach the caller: %v", err)
	}
	if strings.Contains(err.Error(), "wrong") {
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

type sourceFunc struct {
	name string
	fn   func(context.Context) (Token, error)
}

func (s sourceFunc) Name() string                              { return s.name }
func (s sourceFunc) Token(ctx context.Context) (Token, error)  { return s.fn(ctx) }

var _ = json.Marshal // keep the import honest if the file is trimmed
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/hub/azure/`
Expected: FAIL, the package does not exist.

- [ ] **Step 3: Write token.go**

```go
// Package azure reads an Azure subscription: inventory, state and cost.
// It writes nothing. Acting on Azure is lots 4 to 6.
package azure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// ARMResource is what the managed identity endpoints call the audience.
	ARMResource = "https://management.azure.com/"
	// ARMScope is what the v2 token endpoint calls the same thing.
	ARMScope = ARMResource + ".default"

	entraBase = "https://login.microsoftonline.com"
	imdsBase  = "http://169.254.169.254"

	// refreshMargin keeps a request from racing the expiry boundary.
	refreshMargin = 5 * time.Minute
)

type Token struct {
	Value     string
	ExpiresAt time.Time
}

// Source produces a bearer token for Azure Resource Manager.
type Source interface {
	Name() string
	Token(ctx context.Context) (Token, error)
}

type retryableError struct{ err error }

func (r retryableError) Error() string { return r.err.Error() }
func (r retryableError) Unwrap() error { return r.err }

// MarkRetryable says this failure is worth another attempt.
func MarkRetryable(err error) error {
	if err == nil {
		return nil
	}
	return retryableError{err}
}

func Retryable(err error) bool {
	if err == nil {
		return false
	}
	var r retryableError
	return errors.As(err, &r)
}

// tokenResponse covers all three shapes Azure returns. ExpiresIn is a
// json.Number because Entra sends a number and IMDS sends a string; ExpiresOn
// is a string epoch and is the only field App Service sends.
type tokenResponse struct {
	AccessToken string      `json:"access_token"`
	ExpiresIn   json.Number `json:"expires_in"`
	ExpiresOn   string      `json:"expires_on"`
}

func parseTokenResponse(body []byte, now time.Time) (Token, error) {
	var r tokenResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return Token{}, fmt.Errorf("token response is not json: %w", err)
	}
	if r.AccessToken == "" {
		return Token{}, errors.New("token response carries no access_token")
	}
	if r.ExpiresIn != "" {
		if secs, err := strconv.ParseInt(r.ExpiresIn.String(), 10, 64); err == nil && secs > 0 {
			return Token{Value: r.AccessToken, ExpiresAt: now.Add(time.Duration(secs) * time.Second)}, nil
		}
	}
	if r.ExpiresOn != "" {
		if epoch, err := strconv.ParseInt(r.ExpiresOn, 10, 64); err == nil {
			return Token{Value: r.AccessToken, ExpiresAt: time.Unix(epoch, 0).UTC()}, nil
		}
	}
	// A token with no expiry would be cached forever and start failing silently.
	return Token{}, errors.New("token response carries neither expires_in nor expires_on")
}

// azureError extracts error_description, which is the only part of an Azure
// failure that tells anyone what to do about it.
func azureError(status int, body []byte) error {
	var e struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	json.Unmarshal(body, &e)
	msg := e.Description
	if msg == "" {
		msg = e.Error
	}
	if msg == "" {
		msg = strings.TrimSpace(string(body))
	}
	return fmt.Errorf("%s: %s", http.StatusText(status), truncate(msg, 300))
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------- client secret ----------

type ClientSecretSource struct {
	tenantID, clientID, secret string
	base                       string
	client                     *http.Client
}

func NewClientSecret(tenantID, clientID, secret string) *ClientSecretSource {
	return &ClientSecretSource{tenantID: tenantID, clientID: clientID, secret: secret,
		base: entraBase, client: &http.Client{Timeout: 20 * time.Second}}
}

// WithEndpoint points the source at a different token host. Tests only.
func (s *ClientSecretSource) WithEndpoint(base string) *ClientSecretSource {
	s.base = base
	return s
}

func (s *ClientSecretSource) Name() string { return "client_secret" }

func (s *ClientSecretSource) Token(ctx context.Context) (Token, error) {
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {s.clientID},
		"client_secret": {s.secret},
		"scope":         {ARMScope},
	}
	endpoint := strings.TrimSuffix(s.base, "/") + "/" + s.tenantID + "/oauth2/v2.0/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Token{}, err
		}
		return Token{}, MarkRetryable(fmt.Errorf("token endpoint unreachable: %w", err))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		err := azureError(resp.StatusCode, body)
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return Token{}, MarkRetryable(err)
		}
		return Token{}, err
	}
	return parseTokenResponse(body, time.Now().UTC())
}

// ---------- managed identity ----------

type ManagedIdentitySource struct {
	miClientID string
	env        func(string) (string, bool)
	imdsBase   string
	client     *http.Client
}

func NewManagedIdentity(miClientID string, env func(string) (string, bool)) *ManagedIdentitySource {
	if env == nil {
		env = os.LookupEnv
	}
	return &ManagedIdentitySource{miClientID: miClientID, env: env, imdsBase: imdsBase,
		client: &http.Client{Timeout: 20 * time.Second}}
}

func (s *ManagedIdentitySource) Name() string { return "managed_identity" }

func (s *ManagedIdentitySource) Token(ctx context.Context) (Token, error) {
	endpoint, header, ok := s.appService()
	apiVersion := "2019-08-01"
	if !ok {
		endpoint = strings.TrimSuffix(s.imdsBase, "/") + "/metadata/identity/oauth2/token"
		apiVersion = "2018-02-01"
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return Token{}, err
	}
	q := u.Query()
	q.Set("resource", ARMResource)
	q.Set("api-version", apiVersion)
	if s.miClientID != "" {
		q.Set("client_id", s.miClientID)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Token{}, err
	}
	if ok {
		req.Header.Set("X-IDENTITY-HEADER", header)
	} else {
		req.Header.Set("Metadata", "true") // exactly "true", lower case
	}
	resp, err := s.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Token{}, err
		}
		return Token{}, MarkRetryable(fmt.Errorf("managed identity endpoint unreachable: %w", err))
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		err := azureError(resp.StatusCode, body)
		// Documented IMDS guidance: 404 and 410 mean the service is updating,
		// 429 is throttling, 5xx is transient. Other 4xx are design-time errors.
		switch {
		case resp.StatusCode == http.StatusNotFound,
			resp.StatusCode == http.StatusGone,
			resp.StatusCode == http.StatusTooManyRequests,
			resp.StatusCode >= 500:
			return Token{}, MarkRetryable(err)
		}
		return Token{}, err
	}
	return parseTokenResponse(body, time.Now().UTC())
}

// appService reports the App Service / Container Apps endpoint when the
// platform exposes it. Both variables must be present.
func (s *ManagedIdentitySource) appService() (endpoint, header string, ok bool) {
	e, hasE := s.env("IDENTITY_ENDPOINT")
	h, hasH := s.env("IDENTITY_HEADER")
	if hasE && hasH && e != "" && h != "" {
		return e, h, true
	}
	return "", "", false
}

// ---------- caching ----------

type Cached struct {
	src Source
	now func() time.Time
	mu  sync.Mutex
	tok Token
}

func NewCached(src Source, now func() time.Time) *Cached {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Cached{src: src, now: now}
}

func (c *Cached) Name() string { return c.src.Name() }

func (c *Cached) Token(ctx context.Context) (Token, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tok.Value != "" && c.now().Add(refreshMargin).Before(c.tok.ExpiresAt) {
		return c.tok, nil
	}
	tok, err := c.src.Token(ctx)
	if err != nil {
		return Token{}, err // a failure is never cached
	}
	c.tok = tok
	return tok, nil
}
```

Add `"os"` to the imports. The test file needs `"errors"`; drop the
`json.Marshal` keep-alive line once the imports settle.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/hub/azure/ -race -v`
Expected: PASS, 9 tests.

- [ ] **Step 5: Commit**

```bash
git add internal/hub/azure
git commit -m "feat(azure): token sources for client secret and managed identity"
```

---

### Task 2: The ARM client

**Files:**
- Create: `internal/hub/azure/client.go`
- Test: `internal/hub/azure/client_test.go`

**Interfaces:**
- Consumes: `Source`, `Retryable`, `azureError`, `truncate` (Task 1)
- Produces:
```go
type Client struct{ … }
type Options struct {
    Base     string        // ARM base, default https://management.azure.com
    Attempts int           // default 4
    Sleep    func(ctx context.Context, d time.Duration) bool
    Now      func() time.Time
}
func NewClient(src Source, opt Options) *Client

// GetAll follows nextLink to the end and returns every element of value.
func (c *Client) GetAll(ctx context.Context, path string, query url.Values) ([]json.RawMessage, error)
// Post sends a JSON body and returns the raw response.
func (c *Client) Post(ctx context.Context, path string, query url.Values, body any) ([]byte, error)
```

- [ ] **Step 1: Write the failing tests**

`internal/hub/azure/client_test.go`:
```go
package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func staticSource(v string) Source {
	return sourceFunc{name: "fake", fn: func(context.Context) (Token, error) {
		return Token{Value: v, ExpiresAt: time.Now().Add(time.Hour)}, nil
	}}
}

// noSleep records the backoff without spending it.
func noSleep(slept *[]time.Duration, mu *sync.Mutex) func(context.Context, time.Duration) bool {
	return func(ctx context.Context, d time.Duration) bool {
		mu.Lock()
		*slept = append(*slept, d)
		mu.Unlock()
		return true
	}
}

func TestGetAllFollowsNextLinkToTheEnd(t *testing.T) {
	// A client that reads only the first page silently truncates the inventory,
	// which the sweep then reads as "these resources were deleted".
	var srv *httptest.Server
	page := 0
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		page++
		switch page {
		case 1:
			fmt.Fprintf(w, `{"value":[{"n":1},{"n":2}],"nextLink":%q}`, srv.URL+"/next?page=2")
		case 2:
			fmt.Fprintf(w, `{"value":[{"n":3}],"nextLink":%q}`, srv.URL+"/next?page=3")
		default:
			io.WriteString(w, `{"value":[{"n":4}]}`)
		}
	}))
	defer srv.Close()

	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	items, err := c.GetAll(context.Background(), "/subscriptions/s/resources", url.Values{"api-version": {"2021-04-01"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 {
		t.Fatalf("got %d items across %d pages, want 4", len(items), page)
	}
	var last struct{ N int }
	json.Unmarshal(items[3], &last)
	if last.N != 4 {
		t.Fatalf("last item = %+v", last)
	}
}

func TestGetAllRefusesAnEndlessNextLink(t *testing.T) {
	// A server that always points at itself must not hang the sync forever.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"value":[{"n":1}],"nextLink":"%s/loop"}`, "http://"+r.Host)
	}))
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	if _, err := c.GetAll(context.Background(), "/x", nil); err == nil {
		t.Fatal("an unbounded page chain must be an error, not an infinite loop")
	}
}

func TestGetAllHonoursRetryAfterOn429(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":{"code":"429","message":"Too many requests."}}`)
			return
		}
		io.WriteString(w, `{"value":[{"n":1}]}`)
	}))
	defer srv.Close()
	var mu sync.Mutex
	var slept []time.Duration
	c := NewClient(staticSource("tok"), Options{Base: srv.URL, Sleep: noSleep(&slept, &mu)})
	items, err := c.GetAll(context.Background(), "/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || calls.Load() != 2 {
		t.Fatalf("items = %d, calls = %d", len(items), calls.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(slept) != 1 || slept[0] != 7*time.Second {
		t.Fatalf("backoff = %v, want the server's own Retry-After of 7s", slept)
	}
}

func TestGetAllBacksOffWhenRetryAfterIsAbsent(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, `{"value":[]}`)
	}))
	defer srv.Close()
	var mu sync.Mutex
	var slept []time.Duration
	c := NewClient(staticSource("tok"), Options{Base: srv.URL, Sleep: noSleep(&slept, &mu)})
	if _, err := c.GetAll(context.Background(), "/x", nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(slept) != 2 || slept[0] != time.Second || slept[1] != 5*time.Second {
		t.Fatalf("backoff = %v, want [1s 5s]", slept)
	}
}

func TestGetAllSurfacesA403Verbatim(t *testing.T) {
	// The missing role assignment is the single most likely failure on first
	// setup, and Azure's own message names the action and the scope.
	const msg = "The client 'x' with object id 'y' does not have authorization to perform action 'Microsoft.Resources/subscriptions/resourceGroups/resources/read' over scope '/subscriptions/s/resourceGroups/rg'."
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, `{"error":{"code":"AuthorizationFailed","message":%q}}`, msg)
	}))
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	_, err := c.GetAll(context.Background(), "/x", nil)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "does not have authorization") {
		t.Fatalf("Azure's own message must reach the interface: %v", err)
	}
	if Retryable(err) {
		t.Fatalf("a missing role assignment will fail identically forever: %v", err)
	}
}

func TestGetAllGivesUpAfterTheAttemptBudget(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	var mu sync.Mutex
	var slept []time.Duration
	c := NewClient(staticSource("tok"), Options{Base: srv.URL, Sleep: noSleep(&slept, &mu)})
	if _, err := c.GetAll(context.Background(), "/x", nil); err == nil {
		t.Fatal("want an error")
	}
	if calls.Load() != 4 {
		t.Fatalf("calls = %d, want the 4-attempt budget", calls.Load())
	}
}

func TestATokenFailureFailsTheCall(t *testing.T) {
	// No credential means no inventory. It must never mean an empty inventory.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("ARM must not be called without a token")
	}))
	defer srv.Close()
	bad := sourceFunc{name: "fake", fn: func(context.Context) (Token, error) {
		return Token{}, errors.New("AADSTS7000215: Invalid client secret provided.")
	}}
	c := NewClient(bad, Options{Base: srv.URL})
	_, err := c.GetAll(context.Background(), "/x", nil)
	if err == nil || !strings.Contains(err.Error(), "AADSTS7000215") {
		t.Fatalf("err = %v", err)
	}
}

func TestPostSendsJSONAndReturnsTheBody(t *testing.T) {
	var gotBody, gotCT, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotCT = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	out, err := c.Post(context.Background(), "/q", url.Values{"api-version": {"2023-11-01"}},
		map[string]any{"type": "ActualCost"})
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != "POST" || gotCT != "application/json" {
		t.Fatalf("%s %s", gotMethod, gotCT)
	}
	if !strings.Contains(gotBody, `"ActualCost"`) {
		t.Fatalf("body = %s", gotBody)
	}
	if !strings.Contains(string(out), `"ok":true`) {
		t.Fatalf("out = %s", out)
	}
}
```

Add `"errors"` to the test imports.

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/hub/azure/ -run "GetAll|Post|Token failure"`
Expected: FAIL, `undefined: NewClient`.

- [ ] **Step 3: Write client.go**

```go
package azure

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const armBase = "https://management.azure.com"

// maxPages bounds a page chain. A server that keeps pointing at itself must not
// hang the sync forever.
const maxPages = 200

type Options struct {
	Base     string
	Attempts int
	Sleep    func(ctx context.Context, d time.Duration) bool
	Now      func() time.Time
}

type Client struct {
	src  Source
	opt  Options
	http *http.Client
}

func NewClient(src Source, opt Options) *Client {
	if opt.Base == "" {
		opt.Base = armBase
	}
	if opt.Attempts <= 0 {
		opt.Attempts = 4
	}
	if opt.Sleep == nil {
		opt.Sleep = sleepCtx
	}
	if opt.Now == nil {
		opt.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Client{src: src, opt: opt, http: &http.Client{Timeout: 60 * time.Second}}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// backoff is the wait before attempt+1: 1 s, 5 s, 25 s, the same schedule the
// notification dispatcher uses.
func backoff(attempt int) time.Duration {
	w := time.Second
	for i := 1; i < attempt; i++ {
		w *= 5
	}
	return w
}

type page struct {
	Value    []json.RawMessage `json:"value"`
	NextLink string            `json:"nextLink"`
}

// GetAll follows nextLink to the end. A partial read is an error, never a
// short list: the caller would take the missing rows for deleted resources.
func (c *Client) GetAll(ctx context.Context, path string, query url.Values) ([]json.RawMessage, error) {
	next := c.url(path, query)
	var out []json.RawMessage
	for n := 0; next != ""; n++ {
		if n >= maxPages {
			return nil, fmt.Errorf("azure: more than %d pages, giving up", maxPages)
		}
		body, err := c.do(ctx, http.MethodGet, next, nil)
		if err != nil {
			return nil, err
		}
		var p page
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, fmt.Errorf("azure: response is not a page: %w", err)
		}
		out = append(out, p.Value...)
		next = p.NextLink // absolute, and already carries its own query
	}
	return out, nil
}

func (c *Client) Post(ctx context.Context, path string, query url.Values, body any) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return c.do(ctx, http.MethodPost, c.url(path, query), raw)
}

func (c *Client) url(path string, query url.Values) string {
	u := strings.TrimSuffix(c.opt.Base, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return u
}

func (c *Client) do(ctx context.Context, method, rawURL string, body []byte) ([]byte, error) {
	var last error
	for attempt := 1; attempt <= c.opt.Attempts; attempt++ {
		out, wait, err := c.attempt(ctx, method, rawURL, body)
		if err == nil {
			return out, nil
		}
		last = err
		if !Retryable(err) || attempt == c.opt.Attempts {
			break
		}
		if wait <= 0 {
			wait = backoff(attempt)
		}
		if !c.opt.Sleep(ctx, wait) {
			return nil, err // shutting down
		}
	}
	return nil, last
}

// attempt returns the body, or an error plus the wait the server asked for.
func (c *Client) attempt(ctx context.Context, method, rawURL string, body []byte) ([]byte, time.Duration, error) {
	tok, err := c.src.Token(ctx)
	if err != nil {
		// No credential means no inventory. It must never mean an empty one.
		return nil, 0, err
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+tok.Value)
	req.Header.Set("User-Agent", "ServersMonitor")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, err
		}
		return nil, 0, MarkRetryable(fmt.Errorf("azure request failed: %w", err))
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return out, 0, nil
	}
	e := armError(resp.StatusCode, out)
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return nil, retryAfter(resp.Header.Get("Retry-After")), MarkRetryable(e)
	}
	return nil, 0, e
}

// retryAfter reads the header Azure sends with a 429. Only the seconds form is
// used; a date form falls back to the computed backoff.
func retryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	return 0
}

// armError unwraps ARM's {"error":{"code":…,"message":…}} envelope, which is
// shaped differently from the token endpoint's flat one.
func armError(status int, body []byte) error {
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	json.Unmarshal(body, &e)
	msg := e.Error.Message
	if msg == "" {
		return azureError(status, body)
	}
	return fmt.Errorf("%s: %s", http.StatusText(status), truncate(msg, 400))
}

var _ = errors.New
```

Drop the `errors` keep-alive line once imports settle.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/hub/azure/ -race -v`
Expected: PASS, the whole package.

- [ ] **Step 5: Commit**

```bash
git add internal/hub/azure
git commit -m "feat(azure): arm client with pagination, retry and verbatim errors"
```

---

### Task 3: Inventory, two passes

**Files:**
- Create: `internal/hub/azure/inventory.go`
- Test: `internal/hub/azure/inventory_test.go`

**Interfaces:**
- Produces:
```go
type Resource struct {
    ID, Name, Type, ResourceGroup, Location, Kind, SKU string
    State             *string          // nil = not read, never "unknown"
    ProvisioningState string
    Host              string
    Tags              map[string]string
}
// NormalizeID lowercases an ARM resource id. ARM returns mixed case,
// Cost Management returns lowercase, and they must join.
func NormalizeID(id string) string
func Inventory(ctx context.Context, c *Client, subscription string, groups []string) ([]Resource, error)
```

- [ ] **Step 1: Write the failing tests**

`internal/hub/azure/inventory_test.go`:
```go
package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

// A real mixed-case ARM id. Building the fixture lowercase would make the
// normalisation test pass vacuously.
const armSiteID = "/subscriptions/SUB/resourceGroups/rg-dev-vincent-sandbox/providers/Microsoft.Web/sites/visualrami"

func TestNormalizeIDLowercasesEverything(t *testing.T) {
	got := NormalizeID(armSiteID)
	if got != strings.ToLower(armSiteID) {
		t.Fatalf("normalize = %q", got)
	}
	if strings.Contains(got, "resourceGroups") || strings.Contains(got, "Microsoft.Web") {
		t.Fatalf("mixed case survived: %q", got)
	}
	if NormalizeID("  "+armSiteID+"  ") != strings.ToLower(armSiteID) {
		t.Fatal("surrounding blanks must be trimmed")
	}
}

func inventoryServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/resources"):
			// The catalogue pass: properties is null, exactly as ARM returns it.
			fmt.Fprintf(w, `{"value":[
			 {"id":%q,"name":"visualrami","type":"Microsoft.Web/sites","location":"westeurope","kind":"app,linux","tags":{"env":"dev"},"properties":null},
			 {"id":"/subscriptions/SUB/resourceGroups/RG/providers/Microsoft.Web/serverFarms/asp-urban","name":"asp-urban","type":"Microsoft.Web/serverfarms","location":"westeurope","sku":{"name":"B1"},"properties":null},
			 {"id":"/subscriptions/SUB/resourceGroups/RG/providers/Microsoft.KeyVault/vaults/kv-x","name":"kv-x","type":"Microsoft.KeyVault/vaults","location":"westeurope","properties":null}
			]}`, armSiteID)
		case strings.Contains(r.URL.Path, "/Microsoft.Web/sites"):
			fmt.Fprintf(w, `{"value":[
			 {"id":%q,"name":"visualrami","type":"Microsoft.Web/sites","kind":"app,linux",
			  "properties":{"state":"Running","defaultHostName":"visualrami.azurewebsites.net","httpsOnly":true}}
			]}`, armSiteID)
		case strings.Contains(r.URL.Path, "/Microsoft.Web/serverfarms"):
			io.WriteString(w, `{"value":[
			 {"id":"/subscriptions/SUB/resourceGroups/RG/providers/Microsoft.Web/serverFarms/asp-urban","name":"asp-urban",
			  "sku":{"name":"B1"},"properties":{"status":"Ready"}}
			]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
}

func TestInventoryEnrichesWebResources(t *testing.T) {
	srv := inventoryServer(t)
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	rs, err := Inventory(context.Background(), c, "SUB", []string{"rg-dev-vincent-sandbox"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 3 {
		t.Fatalf("got %d resources, want 3", len(rs))
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].Name < rs[j].Name })
	byName := map[string]Resource{}
	for _, r := range rs {
		byName[r.Name] = r
	}

	site := byName["visualrami"]
	if site.ID != strings.ToLower(armSiteID) {
		t.Fatalf("id not normalised: %q", site.ID)
	}
	if site.State == nil || *site.State != "Running" {
		t.Fatalf("state = %v, the enrichment pass must fill it", site.State)
	}
	if site.Host != "visualrami.azurewebsites.net" {
		t.Fatalf("host = %q", site.Host)
	}
	if site.Kind != "app,linux" || site.Tags["env"] != "dev" {
		t.Fatalf("site = %+v", site)
	}
	if site.ResourceGroup != "rg-dev-vincent-sandbox" {
		t.Fatalf("resource group = %q, it is parsed from the id", site.ResourceGroup)
	}

	if plan := byName["asp-urban"]; plan.SKU != "B1" || plan.State == nil || *plan.State != "Ready" {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestInventoryKeepsTypesItCannotEnrich(t *testing.T) {
	// An inventory that silently omits what it does not understand is worse
	// than one that admits the gap.
	srv := inventoryServer(t)
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	rs, _ := Inventory(context.Background(), c, "SUB", []string{"rg-dev-vincent-sandbox"})
	for _, r := range rs {
		if r.Name == "kv-x" {
			if r.State != nil {
				t.Fatalf("an unenriched type has no state, got %q", *r.State)
			}
			return
		}
	}
	t.Fatal("the key vault must still be listed")
}

func TestInventoryFailsWhenAnyCallFails(t *testing.T) {
	// A sync is successful only when every call in it succeeded. A half-read
	// inventory would mark the missing resources deleted.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/resources") {
			fmt.Fprintf(w, `{"value":[{"id":%q,"name":"visualrami","type":"Microsoft.Web/sites","location":"we","properties":null}]}`, armSiteID)
			return
		}
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":{"code":"AuthorizationFailed","message":"no authorization to read sites"}}`)
	}))
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	if _, err := Inventory(context.Background(), c, "SUB", []string{"RG"}); err == nil {
		t.Fatal("a failed enrichment pass must fail the whole inventory")
	}
}

func TestInventoryCoversEveryConfiguredGroup(t *testing.T) {
	seen := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, part := range strings.Split(r.URL.Path, "/") {
			if strings.HasPrefix(part, "rg-") {
				seen[part] = true
			}
		}
		io.WriteString(w, `{"value":[]}`)
	}))
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	if _, err := Inventory(context.Background(), c, "SUB", []string{"rg-one", "rg-two"}); err != nil {
		t.Fatal(err)
	}
	if !seen["rg-one"] || !seen["rg-two"] {
		t.Fatalf("both groups must be visited, saw %v", seen)
	}
}

func TestInventoryRefusesAnEmptyGroupList(t *testing.T) {
	// Subscription-wide scope needs a subscription-scope role assignment that
	// this lot does not ask anyone for, and would answer 403.
	c := NewClient(staticSource("tok"), Options{Base: "http://unused"})
	if _, err := Inventory(context.Background(), c, "SUB", nil); err == nil {
		t.Fatal("an empty group list must be refused, not turned into a subscription sweep")
	}
}

var _ = json.Marshal
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/hub/azure/ -run Inventory`
Expected: FAIL, `undefined: Inventory`.

- [ ] **Step 3: Write inventory.go**

```go
package azure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Resource is one Azure resource as ServersMonitor stores it.
// State is a pointer: nil means nobody read it, which is never "stopped".
type Resource struct {
	ID                string
	Name              string
	Type              string
	ResourceGroup     string
	Location          string
	Kind              string
	SKU               string
	State             *string
	ProvisioningState string
	Host              string
	Tags              map[string]string
}

// NormalizeID lowercases an ARM resource id. ARM returns `resourceGroups` and
// `Microsoft.Web`; Cost Management returns `resourcegroups` and `microsoft.web`
// for the same resource. Every id is normalised on the way in so the two join.
func NormalizeID(id string) string { return strings.ToLower(strings.TrimSpace(id)) }

// resourceGroupOf pulls the group out of an id, which is more reliable than the
// generic list's own field and works for the typed passes too.
func resourceGroupOf(id string) string {
	parts := strings.Split(strings.Trim(id, "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		if strings.EqualFold(parts[i], "resourceGroups") {
			return parts[i+1]
		}
	}
	return ""
}

type armResource struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Type     string            `json:"type"`
	Location string            `json:"location"`
	Kind     string            `json:"kind"`
	Tags     map[string]string `json:"tags"`
	SKU      struct {
		Name string `json:"name"`
	} `json:"sku"`
	Properties struct {
		State             string `json:"state"`
		Status            string `json:"status"`
		ProvisioningState string `json:"provisioningState"`
		DefaultHostName   string `json:"defaultHostName"`
	} `json:"properties"`
}

// Inventory runs the catalogue pass over every configured group, then one
// typed enrichment pass per provider it understands.
//
// It returns an error if any call fails. A partial inventory is worse than
// none: the caller's sweep would read the missing rows as deletions.
func Inventory(ctx context.Context, c *Client, subscription string, groups []string) ([]Resource, error) {
	if len(groups) == 0 {
		return nil, errors.New("azure: at least one resource group is required; " +
			"subscription-wide scope needs a subscription-scope role assignment this hub does not request")
	}
	byID := map[string]*Resource{}
	var order []string

	for _, g := range groups {
		base := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s", subscription, g)
		items, err := c.GetAll(ctx, base+"/resources", url.Values{"api-version": {"2021-04-01"}})
		if err != nil {
			return nil, fmt.Errorf("catalogue pass on %s: %w", g, err)
		}
		for _, raw := range items {
			var a armResource
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, fmt.Errorf("catalogue entry in %s: %w", g, err)
			}
			id := NormalizeID(a.ID)
			r := &Resource{ID: id, Name: a.Name, Type: a.Type, Location: a.Location,
				Kind: a.Kind, SKU: a.SKU.Name, Tags: a.Tags, ResourceGroup: resourceGroupOf(a.ID)}
			if r.ResourceGroup == "" {
				r.ResourceGroup = g
			}
			if r.Tags == nil {
				r.Tags = map[string]string{}
			}
			byID[id] = r
			order = append(order, id)
		}
		// Enrichment. One entry per provider path we understand; an unknown
		// type simply keeps a nil state.
		for _, path := range []string{"/providers/Microsoft.Web/sites", "/providers/Microsoft.Web/serverfarms"} {
			items, err := c.GetAll(ctx, base+path, url.Values{"api-version": {"2023-12-01"}})
			if err != nil {
				return nil, fmt.Errorf("enrichment pass %s on %s: %w", path, g, err)
			}
			for _, raw := range items {
				var a armResource
				if err := json.Unmarshal(raw, &a); err != nil {
					return nil, err
				}
				r, ok := byID[NormalizeID(a.ID)]
				if !ok {
					continue // enriched something the catalogue did not list
				}
				state := a.Properties.State
				if state == "" {
					state = a.Properties.Status
				}
				if state != "" {
					s := state
					r.State = &s
				}
				r.ProvisioningState = a.Properties.ProvisioningState
				if a.Properties.DefaultHostName != "" {
					r.Host = a.Properties.DefaultHostName
				}
				if a.SKU.Name != "" {
					r.SKU = a.SKU.Name
				}
				if a.Kind != "" {
					r.Kind = a.Kind
				}
			}
		}
	}

	out := make([]Resource, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out, nil
}
```

- [ ] **Step 4: Run the tests, then the package**

Run: `go test ./internal/hub/azure/ -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/hub/azure
git commit -m "feat(azure): inventory with catalogue and enrichment passes"
```

---

### Task 4: Cost

**Files:**
- Create: `internal/hub/azure/cost.go`
- Test: `internal/hub/azure/cost_test.go`

**Interfaces:**
- Produces:
```go
type Cost struct{ ResourceID, Currency string; Amount float64 }
func Costs(ctx context.Context, c *Client, subscription string, groups []string) ([]Cost, error)
```

The response is columnar. **Column order is read from `columns`, never assumed by position.**
Verified live on 2026-09-18: the columns came back as `Cost`, `ResourceId`, `Currency`, which is not
the order the request lists them in.

- [ ] **Step 1: Write the failing tests**

`internal/hub/azure/cost_test.go`:
```go
package azure

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const costBody = `{"properties":{
 "columns":[{"name":"Cost","type":"Number"},{"name":"ResourceId","type":"String"},{"name":"Currency","type":"String"}],
 "rows":[
  [0.000838612982998454,"/subscriptions/sub/resourcegroups/rg/providers/microsoft.web/serverfarms/asp-urban","EUR"],
  [0.0,"/subscriptions/sub/resourcegroups/rg/providers/microsoft.web/sites/visualrami","EUR"],
  [1.5,"/subscriptions/sub/resourcegroups/rg/providers/microsoft.operationalinsights/workspaces/gone","EUR"]
 ]}}`

func TestCostsReadColumnsByName(t *testing.T) {
	// Reading by position would silently swap the amount and the id the day
	// Azure reorders its columns, which it already does not do in request order.
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		io.WriteString(w, costBody)
	}))
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	cs, err := Costs(context.Background(), c, "sub", []string{"rg"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 3 {
		t.Fatalf("got %d cost rows, want 3", len(cs))
	}
	if cs[0].Amount != 0.000838612982998454 || cs[0].Currency != "EUR" {
		t.Fatalf("row 0 = %+v", cs[0])
	}
	if !strings.HasSuffix(cs[0].ResourceID, "/asp-urban") {
		t.Fatalf("resource id = %q", cs[0].ResourceID)
	}
	for _, want := range []string{`"ActualCost"`, `"MonthToDate"`, `"ResourceId"`, `"Sum"`} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("request body is missing %s: %s", want, gotBody)
		}
	}
}

func TestCostsSurviveAReorderedResponse(t *testing.T) {
	reordered := `{"properties":{
	 "columns":[{"name":"Currency"},{"name":"ResourceId"},{"name":"Cost"}],
	 "rows":[["USD","/subscriptions/sub/resourcegroups/rg/providers/x/y/z",42.5]]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, reordered)
	}))
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	cs, err := Costs(context.Background(), c, "sub", []string{"rg"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].Amount != 42.5 || cs[0].Currency != "USD" {
		t.Fatalf("cost = %+v", cs)
	}
}

func TestCostsNormaliseTheResourceID(t *testing.T) {
	mixed := `{"properties":{"columns":[{"name":"Cost"},{"name":"ResourceId"},{"name":"Currency"}],
	 "rows":[[1.0,"/subscriptions/SUB/resourceGroups/RG/providers/Microsoft.Web/sites/VisualRami","EUR"]]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, mixed)
	}))
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	cs, _ := Costs(context.Background(), c, "sub", []string{"rg"})
	if cs[0].ResourceID != strings.ToLower(cs[0].ResourceID) {
		t.Fatalf("cost ids must be normalised too: %q", cs[0].ResourceID)
	}
}

func TestCostsRejectAResponseMissingAColumn(t *testing.T) {
	// Guessing a missing column would invent numbers, which is the one thing a
	// cost feature must never do.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"properties":{"columns":[{"name":"Cost"},{"name":"Currency"}],"rows":[[1.0,"EUR"]]}}`)
	}))
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	if _, err := Costs(context.Background(), c, "sub", []string{"rg"}); err == nil {
		t.Fatal("a response with no ResourceId column must be an error")
	}
}

func TestCostsQueryEveryGroup(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		io.WriteString(w, `{"properties":{"columns":[{"name":"Cost"},{"name":"ResourceId"},{"name":"Currency"}],"rows":[]}}`)
	}))
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	if _, err := Costs(context.Background(), c, "sub", []string{"rg-one", "rg-two"}); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || !strings.Contains(paths[0], "rg-one") || !strings.Contains(paths[1], "rg-two") {
		t.Fatalf("paths = %v", paths)
	}
}

var _ = json.Marshal
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/hub/azure/ -run Cost`
Expected: FAIL, `undefined: Costs`.

- [ ] **Step 3: Write cost.go**

```go
package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// Cost is one resource's spend over the queried period.
type Cost struct {
	ResourceID string
	Currency   string
	Amount     float64
}

// costQuery is the documented month-to-date query, grouped by resource.
func costQuery() map[string]any {
	return map[string]any{
		"type":      "ActualCost",
		"timeframe": "MonthToDate",
		"dataset": map[string]any{
			"granularity": "None",
			"aggregation": map[string]any{
				"total": map[string]any{"name": "Cost", "function": "Sum"},
			},
			"grouping": []any{
				map[string]any{"type": "Dimension", "name": "ResourceId"},
			},
		},
	}
}

type costResponse struct {
	Properties struct {
		Columns []struct {
			Name string `json:"name"`
		} `json:"columns"`
		Rows [][]json.RawMessage `json:"rows"`
	} `json:"properties"`
}

// Costs returns month-to-date spend per resource, for every configured group.
//
// The response is columnar and its column order is not the request's order —
// verified live, the request lists Cost then ResourceId then Currency and the
// response came back in that order only by luck. Columns are therefore located
// by name, and a missing one is an error rather than a guess.
func Costs(ctx context.Context, c *Client, subscription string, groups []string) ([]Cost, error) {
	if len(groups) == 0 {
		return nil, fmt.Errorf("azure: at least one resource group is required for a cost query")
	}
	var out []Cost
	for _, g := range groups {
		path := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.CostManagement/query", subscription, g)
		raw, err := c.Post(ctx, path, url.Values{"api-version": {"2023-11-01"}}, costQuery())
		if err != nil {
			return nil, fmt.Errorf("cost query on %s: %w", g, err)
		}
		var r costResponse
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, fmt.Errorf("cost response for %s is not json: %w", g, err)
		}
		idx := map[string]int{}
		for i, col := range r.Properties.Columns {
			idx[col.Name] = i
		}
		iCost, okCost := idx["Cost"]
		iID, okID := idx["ResourceId"]
		iCur, okCur := idx["Currency"]
		if !okCost || !okID || !okCur {
			return nil, fmt.Errorf("cost response for %s is missing a column, got %v", g, idx)
		}
		for _, row := range r.Properties.Rows {
			if len(row) <= iCost || len(row) <= iID || len(row) <= iCur {
				return nil, fmt.Errorf("cost row for %s is shorter than its columns", g)
			}
			var amount float64
			var id, currency string
			if err := json.Unmarshal(row[iCost], &amount); err != nil {
				return nil, fmt.Errorf("cost amount for %s: %w", g, err)
			}
			json.Unmarshal(row[iID], &id)
			json.Unmarshal(row[iCur], &currency)
			out = append(out, Cost{ResourceID: NormalizeID(id), Currency: currency, Amount: amount})
		}
	}
	return out, nil
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/hub/azure/ -race`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/hub/azure
git commit -m "feat(azure): month-to-date cost query read by column name"
```

---

### Task 5: Storage

**Files:**
- Create: `internal/hub/store/migrations/0003_azure.sql`, `internal/hub/store/azure.go`
- Modify: `internal/hub/store/store_test.go` (schema version 2 → 3, table list)
- Test: `internal/hub/store/azure_test.go`

**Interfaces:**
- Produces:
```go
type AzureResource struct {
    ID, Name, Type, ResourceGroup, Location, Kind, SKU string
    State             *string
    ProvisioningState string
    Host              string
    Tags              map[string]string
    FirstSeen, LastSeen time.Time
    DeletedAt         *time.Time
}
type AzureCost struct{ ResourceID, Period, Currency string; Amount float64; AsOf time.Time }
type AzureSync struct{ Scope string; OK bool; Message string; StartedAt, EndedAt time.Time }

// ReplaceAzureInventory upserts every row and sweeps what was not seen, in one
// transaction. Call it only when the whole sync succeeded.
func (s *Store) ReplaceAzureInventory(groups []string, rs []AzureResource, now time.Time) error
func (s *Store) ListAzureResources() ([]AzureResource, error)
func (s *Store) UpsertAzureCosts(cs []AzureCost) error
func (s *Store) ListAzureCosts(period string) ([]AzureCost, error)
func (s *Store) SetAzureSync(a AzureSync) error
func (s *Store) AzureSyncState() (map[string]AzureSync, error)
func (s *Store) PurgeAzure() error   // used when the scope changes
```

- [ ] **Step 1: Write the migration**

`internal/hub/store/migrations/0003_azure.sql`:
```sql
CREATE TABLE azure_resources (
  id                 TEXT PRIMARY KEY,   -- ARM resource id, lowercased
  name               TEXT NOT NULL,
  type               TEXT NOT NULL,
  resource_group     TEXT NOT NULL,
  location           TEXT NOT NULL,
  kind               TEXT NOT NULL DEFAULT '',
  sku                TEXT NOT NULL DEFAULT '',
  state              TEXT,               -- NULL = not read, never "unknown"
  provisioning_state TEXT NOT NULL DEFAULT '',
  host               TEXT NOT NULL DEFAULT '',
  tags               TEXT NOT NULL DEFAULT '{}',
  first_seen         TEXT NOT NULL,
  last_seen          TEXT NOT NULL,
  deleted_at         TEXT                -- NULL = still there
) WITHOUT ROWID;

CREATE INDEX azure_resources_group ON azure_resources(resource_group, id);

-- No foreign key to azure_resources, deliberately: a resource deleted before
-- ServersMonitor first ran still has cost rows and they must still be stored.
CREATE TABLE azure_costs (
  resource_id TEXT NOT NULL,
  period      TEXT NOT NULL,             -- "2026-09"
  amount      REAL NOT NULL,
  currency    TEXT NOT NULL,
  as_of       TEXT NOT NULL,
  PRIMARY KEY (resource_id, period)
) WITHOUT ROWID;

CREATE TABLE azure_sync (
  scope      TEXT PRIMARY KEY,           -- "inventory" | "cost"
  ok         INTEGER NOT NULL,
  message    TEXT NOT NULL DEFAULT '',
  started_at TEXT NOT NULL,
  ended_at   TEXT NOT NULL
) WITHOUT ROWID;
```

- [ ] **Step 2: Write the failing tests**

`internal/hub/store/azure_test.go`:
```go
package store

import (
	"testing"
	"time"
)

func res(id, name string, state *string) AzureResource {
	return AzureResource{ID: id, Name: name, Type: "Microsoft.Web/sites",
		ResourceGroup: "rg", Location: "westeurope", State: state,
		Tags: map[string]string{"env": "dev"}}
}

func strp(s string) *string { return &s }

func TestAzureInventoryRoundTrip(t *testing.T) {
	s := openTest(t)
	in := []AzureResource{
		res("/subscriptions/sub/resourcegroups/rg/providers/microsoft.web/sites/a", "a", strp("Running")),
		res("/subscriptions/sub/resourcegroups/rg/providers/microsoft.web/sites/b", "b", nil),
	}
	if err := s.ReplaceAzureInventory([]string{"rg"}, in, t0); err != nil {
		t.Fatal(err)
	}
	out, err := s.ListAzureResources()
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d rows", len(out))
	}
	byName := map[string]AzureResource{}
	for _, r := range out {
		byName[r.Name] = r
	}
	if got := byName["a"]; got.State == nil || *got.State != "Running" {
		t.Fatalf("a.state = %v", got.State)
	}
	if got := byName["b"]; got.State != nil {
		// A state nobody read is NULL, and must come back nil rather than "".
		t.Fatalf("b.state = %q, want nil", *got.State)
	}
	if byName["a"].Tags["env"] != "dev" {
		t.Fatalf("tags = %v", byName["a"].Tags)
	}
	if byName["a"].FirstSeen.IsZero() || byName["a"].LastSeen.IsZero() {
		t.Fatal("first_seen and last_seen must be set")
	}
	if byName["a"].DeletedAt != nil {
		t.Fatal("a resource that was just seen is not deleted")
	}
}

func TestAzureSweepSoftDeletesWhatVanished(t *testing.T) {
	s := openTest(t)
	a := res("/subs/rg/a", "a", strp("Running"))
	b := res("/subs/rg/b", "b", strp("Running"))
	s.ReplaceAzureInventory([]string{"rg"}, []AzureResource{a, b}, t0)

	later := t0.Add(time.Hour)
	if err := s.ReplaceAzureInventory([]string{"rg"}, []AzureResource{a}, later); err != nil {
		t.Fatal(err)
	}
	out, _ := s.ListAzureResources()
	if len(out) != 2 {
		t.Fatalf("a vanished resource is soft-deleted, not removed: %d rows", len(out))
	}
	for _, r := range out {
		switch r.Name {
		case "a":
			if r.DeletedAt != nil {
				t.Fatal("a is still there")
			}
			if !r.LastSeen.Equal(later) {
				t.Fatalf("a.last_seen = %s", r.LastSeen)
			}
			if !r.FirstSeen.Equal(t0) {
				t.Fatalf("first_seen must not move on a re-sync: %s", r.FirstSeen)
			}
		case "b":
			if r.DeletedAt == nil {
				t.Fatal("b vanished and must carry deleted_at")
			}
			if r.Name != "b" {
				t.Fatal("a deleted resource keeps its name so its cost row can be labelled")
			}
		}
	}
}

func TestAzureResourceComingBackClearsDeletedAt(t *testing.T) {
	s := openTest(t)
	a := res("/subs/rg/a", "a", strp("Running"))
	s.ReplaceAzureInventory([]string{"rg"}, []AzureResource{a}, t0)
	s.ReplaceAzureInventory([]string{"rg"}, nil, t0.Add(time.Hour))
	s.ReplaceAzureInventory([]string{"rg"}, []AzureResource{a}, t0.Add(2*time.Hour))
	out, _ := s.ListAzureResources()
	if len(out) != 1 || out[0].DeletedAt != nil {
		t.Fatalf("a recreated resource is alive again: %+v", out)
	}
}

func TestAzureSweepOnlyTouchesTheGroupsItSynced(t *testing.T) {
	// Syncing one group must not mark another group's resources deleted.
	s := openTest(t)
	one := res("/subs/one/a", "a", nil)
	one.ResourceGroup = "rg-one"
	two := res("/subs/two/b", "b", nil)
	two.ResourceGroup = "rg-two"
	s.ReplaceAzureInventory([]string{"rg-one", "rg-two"}, []AzureResource{one, two}, t0)

	s.ReplaceAzureInventory([]string{"rg-one"}, []AzureResource{one}, t0.Add(time.Hour))
	out, _ := s.ListAzureResources()
	for _, r := range out {
		if r.Name == "b" && r.DeletedAt != nil {
			t.Fatal("rg-two was not synced, so its resources must be left alone")
		}
	}
}

func TestAzureCostsRoundTripAndUpsert(t *testing.T) {
	s := openTest(t)
	cs := []AzureCost{
		{ResourceID: "/subs/rg/a", Period: "2026-09", Amount: 1.5, Currency: "EUR", AsOf: t0},
		{ResourceID: "/subs/rg/gone", Period: "2026-09", Amount: 4.0, Currency: "EUR", AsOf: t0},
	}
	if err := s.UpsertAzureCosts(cs); err != nil {
		t.Fatal(err)
	}
	// A cost row for a resource that is not in the inventory must survive. The
	// absence of a foreign key is the point.
	out, err := s.ListAzureCosts("2026-09")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d cost rows, want 2 including the orphan", len(out))
	}
	cs[0].Amount = 2.5
	cs[0].AsOf = t0.Add(time.Hour)
	if err := s.UpsertAzureCosts(cs[:1]); err != nil {
		t.Fatal(err)
	}
	out, _ = s.ListAzureCosts("2026-09")
	for _, c := range out {
		if c.ResourceID == "/subs/rg/a" && c.Amount != 2.5 {
			t.Fatalf("a second sync updates the amount, got %v", c.Amount)
		}
	}
	if got, _ := s.ListAzureCosts("2026-08"); len(got) != 0 {
		t.Fatal("periods are separate")
	}
}

func TestAzureSyncStateRoundTrip(t *testing.T) {
	s := openTest(t)
	if err := s.SetAzureSync(AzureSync{Scope: "inventory", OK: false,
		Message: "Forbidden: does not have authorization", StartedAt: t0, EndedAt: t0.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	st, err := s.AzureSyncState()
	if err != nil {
		t.Fatal(err)
	}
	if st["inventory"].OK || st["inventory"].Message == "" {
		t.Fatalf("sync state = %+v", st["inventory"])
	}
	s.SetAzureSync(AzureSync{Scope: "inventory", OK: true, StartedAt: t0, EndedAt: t0})
	st, _ = s.AzureSyncState()
	if !st["inventory"].OK || st["inventory"].Message != "" {
		t.Fatalf("a success clears the message: %+v", st["inventory"])
	}
}

func TestPurgeAzureClearsEverything(t *testing.T) {
	s := openTest(t)
	s.ReplaceAzureInventory([]string{"rg"}, []AzureResource{res("/a", "a", nil)}, t0)
	s.UpsertAzureCosts([]AzureCost{{ResourceID: "/a", Period: "2026-09", Amount: 1, Currency: "EUR", AsOf: t0}})
	if err := s.PurgeAzure(); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, "azure_resources"); n != 0 {
		t.Fatalf("%d resources left", n)
	}
	if n := count(t, s, "azure_costs"); n != 0 {
		t.Fatalf("%d cost rows left", n)
	}
}
```

- [ ] **Step 3: Run them and watch them fail**

Run: `go test ./internal/hub/store/ -run Azure`
Expected: FAIL, `undefined: ReplaceAzureInventory`.

- [ ] **Step 4: Update the two lot 1 tests the migration invalidates**

`internal/hub/store/store_test.go` asserts the schema version and lists the tables. Both are now
false, and a test that asserts yesterday's schema blocks every future migration.

```go
	if v != 3 {
		t.Fatalf("schema version = %d, want 3", v)
	}
	for _, table := range []string{"hosts", "samples", "samples_10m", "samples_1h", "samples_1d", "containers", "container_samples", "alert_rules", "alert_events", "deliveries", "azure_resources", "azure_costs", "azure_sync", "users", "sessions", "settings"} {
```

and in `TestOpenTwiceIsIdempotent`, `v != 3`.

`TestSchemaKeepsForeignKeys` iterates a fixed list and is unaffected: the three Azure tables have no
foreign key at all, which is deliberate and already asserted by
`TestAzureCostsRoundTripAndUpsert`.

- [ ] **Step 5: Write azure.go**

```go
package store

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

type AzureResource struct {
	ID                string
	Name              string
	Type              string
	ResourceGroup     string
	Location          string
	Kind              string
	SKU               string
	State             *string
	ProvisioningState string
	Host              string
	Tags              map[string]string
	FirstSeen         time.Time
	LastSeen          time.Time
	DeletedAt         *time.Time
}

type AzureCost struct {
	ResourceID string
	Period     string
	Amount     float64
	Currency   string
	AsOf       time.Time
}

type AzureSync struct {
	Scope     string
	OK        bool
	Message   string
	StartedAt time.Time
	EndedAt   time.Time
}

// ReplaceAzureInventory upserts what the sync saw and soft-deletes, within the
// synced groups only, what it did not. One transaction: a half-applied sweep
// would leave resources marked deleted that are not.
//
// Call this only when the whole sync succeeded. A partial inventory would read
// as a batch of deletions.
func (s *Store) ReplaceAzureInventory(groups []string, rs []AzureResource, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	at := fmtTime(now)
	for _, r := range rs {
		tags, err := json.Marshal(r.Tags)
		if err != nil {
			return err
		}
		var state any
		if r.State != nil {
			state = *r.State
		}
		// first_seen is kept from the existing row; last_seen always moves and
		// deleted_at is cleared, so a recreated resource comes back to life.
		if _, err := tx.Exec(`INSERT INTO azure_resources
			(id, name, type, resource_group, location, kind, sku, state, provisioning_state, host, tags, first_seen, last_seen, deleted_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,NULL)
			ON CONFLICT(id) DO UPDATE SET
			  name=excluded.name, type=excluded.type, resource_group=excluded.resource_group,
			  location=excluded.location, kind=excluded.kind, sku=excluded.sku,
			  state=excluded.state, provisioning_state=excluded.provisioning_state,
			  host=excluded.host, tags=excluded.tags, last_seen=excluded.last_seen, deleted_at=NULL`,
			r.ID, r.Name, r.Type, r.ResourceGroup, r.Location, r.Kind, r.SKU, state,
			r.ProvisioningState, r.Host, string(tags), at, at); err != nil {
			return err
		}
	}

	// The sweep, scoped to the groups this sync actually covered.
	if len(groups) > 0 {
		q := `UPDATE azure_resources SET deleted_at = ?
		      WHERE deleted_at IS NULL AND last_seen < ? AND lower(resource_group) IN (` +
			strings.TrimSuffix(strings.Repeat("?,", len(groups)), ",") + `)`
		args := []any{at, at}
		for _, g := range groups {
			args = append(args, strings.ToLower(g))
		}
		if _, err := tx.Exec(q, args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const azureResourceCols = `id, name, type, resource_group, location, kind, sku, state,
	provisioning_state, host, tags, first_seen, last_seen, deleted_at`

func (s *Store) ListAzureResources() ([]AzureResource, error) {
	rows, err := s.db.Query(`SELECT ` + azureResourceCols + ` FROM azure_resources ORDER BY resource_group, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AzureResource
	for rows.Next() {
		var r AzureResource
		var state, deleted sql.NullString
		var tags, first, last string
		if err := rows.Scan(&r.ID, &r.Name, &r.Type, &r.ResourceGroup, &r.Location, &r.Kind, &r.SKU,
			&state, &r.ProvisioningState, &r.Host, &tags, &first, &last, &deleted); err != nil {
			return nil, err
		}
		if state.Valid {
			v := state.String
			r.State = &v
		}
		if deleted.Valid {
			t, err := parseTime(deleted.String)
			if err != nil {
				return nil, err
			}
			r.DeletedAt = &t
		}
		if r.FirstSeen, err = parseTime(first); err != nil {
			return nil, err
		}
		if r.LastSeen, err = parseTime(last); err != nil {
			return nil, err
		}
		r.Tags = map[string]string{}
		json.Unmarshal([]byte(tags), &r.Tags)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) UpsertAzureCosts(cs []AzureCost) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, c := range cs {
		if _, err := tx.Exec(`INSERT INTO azure_costs(resource_id, period, amount, currency, as_of)
			VALUES (?,?,?,?,?)
			ON CONFLICT(resource_id, period) DO UPDATE SET
			  amount=excluded.amount, currency=excluded.currency, as_of=excluded.as_of`,
			c.ResourceID, c.Period, c.Amount, c.Currency, fmtTime(c.AsOf)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListAzureCosts(period string) ([]AzureCost, error) {
	rows, err := s.db.Query(`SELECT resource_id, period, amount, currency, as_of
		FROM azure_costs WHERE period = ? ORDER BY amount DESC`, period)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AzureCost
	for rows.Next() {
		var c AzureCost
		var asOf string
		if err := rows.Scan(&c.ResourceID, &c.Period, &c.Amount, &c.Currency, &asOf); err != nil {
			return nil, err
		}
		if c.AsOf, err = parseTime(asOf); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) SetAzureSync(a AzureSync) error {
	ok := 0
	if a.OK {
		ok = 1
	}
	_, err := s.db.Exec(`INSERT INTO azure_sync(scope, ok, message, started_at, ended_at)
		VALUES (?,?,?,?,?)
		ON CONFLICT(scope) DO UPDATE SET ok=excluded.ok, message=excluded.message,
		  started_at=excluded.started_at, ended_at=excluded.ended_at`,
		a.Scope, ok, a.Message, fmtTime(a.StartedAt), fmtTime(a.EndedAt))
	return err
}

func (s *Store) AzureSyncState() (map[string]AzureSync, error) {
	rows, err := s.db.Query(`SELECT scope, ok, message, started_at, ended_at FROM azure_sync`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]AzureSync{}
	for rows.Next() {
		var a AzureSync
		var ok int
		var started, ended string
		if err := rows.Scan(&a.Scope, &ok, &a.Message, &started, &ended); err != nil {
			return nil, err
		}
		a.OK = ok == 1
		if a.StartedAt, err = parseTime(started); err != nil {
			return nil, err
		}
		if a.EndedAt, err = parseTime(ended); err != nil {
			return nil, err
		}
		out[a.Scope] = a
	}
	return out, rows.Err()
}

// PurgeAzure drops everything Azure. Used when the configured scope changes,
// so resources from a group nobody watches any more stop being displayed.
func (s *Store) PurgeAzure() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, t := range []string{"azure_resources", "azure_costs", "azure_sync"} {
		if _, err := tx.Exec(`DELETE FROM ` + t); err != nil {
			return err
		}
	}
	return tx.Commit()
}
```

- [ ] **Step 6: Run the store package**

Run: `go test ./internal/hub/store/ -race`
Expected: PASS, including the lot 1 and lot 2 tests.

- [ ] **Step 7: Commit**

```bash
git add internal/hub/store
git commit -m "feat(store): azure inventory, costs and sync state"
```

---

### Task 6: Configuration

**Files:**
- Create: `internal/hub/azure/config.go`
- Test: `internal/hub/azure/config_test.go`

**Interfaces:**
- Produces:
```go
type Config struct {
    Mode              string   // off | client_secret | managed_identity
    TenantID, ClientID, ClientSecret string
    MIClientID        string
    SubscriptionID    string
    ResourceGroups    []string
    InventoryEveryMin int      // default 15
    CostEveryMin      int      // default 60
    BudgetMonthly     float64  // 0 = none
}
func LoadConfig(g Getter) Config
func SaveConfig(s Setter, c Config) error
func (c Config) Validate() error
func (c Config) Enabled() bool
// NewSource builds the source the mode asks for. env is os.LookupEnv in production.
func NewSource(c Config, env func(string) (string, bool)) (Source, error)
```

`Getter` and `Setter` are the same two one-method interfaces the `notify` package declares. Declare
them again here rather than importing `notify`: a monitoring package importing a notification
package to reach a settings table would be a dependency nobody wants to explain later.

- [ ] **Step 1: Write the failing tests**

`internal/hub/azure/config_test.go`:
```go
package azure

import (
	"reflect"
	"strings"
	"testing"
)

type fakeSettings map[string]string

func (f fakeSettings) GetSetting(k string) (string, bool, error) { v, ok := f[k]; return v, ok, nil }
func (f fakeSettings) SetSetting(k, v string) error              { f[k] = v; return nil }

func validConfig() Config {
	return Config{Mode: "client_secret", TenantID: "t", ClientID: "c", ClientSecret: "s",
		SubscriptionID: "sub", ResourceGroups: []string{"rg-one", "rg-two"},
		InventoryEveryMin: 15, CostEveryMin: 60, BudgetMonthly: 50}
}

func TestConfigRoundTrip(t *testing.T) {
	st := fakeSettings{}
	want := validConfig()
	if err := SaveConfig(st, want); err != nil {
		t.Fatal(err)
	}
	if got := LoadConfig(st); !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip lost data:\n got %+v\nwant %+v", got, want)
	}
}

func TestLoadConfigOnAnEmptyStoreIsOff(t *testing.T) {
	c := LoadConfig(fakeSettings{})
	if c.Mode != "off" || c.Enabled() {
		t.Fatalf("a fresh install reads no Azure: %+v", c)
	}
	if c.InventoryEveryMin != 15 || c.CostEveryMin != 60 {
		t.Fatalf("defaults = %d / %d", c.InventoryEveryMin, c.CostEveryMin)
	}
}

func TestLoadConfigSurvivesGarbage(t *testing.T) {
	st := fakeSettings{
		"azure_inventory_interval_min": "not-a-number",
		"azure_budget_monthly":         "beaucoup",
		"azure_resource_groups":        " , rg-one , ,rg-two,",
	}
	c := LoadConfig(st)
	if c.InventoryEveryMin != 15 {
		t.Fatalf("interval = %d, want the default", c.InventoryEveryMin)
	}
	if c.BudgetMonthly != 0 {
		t.Fatalf("budget = %v, want 0", c.BudgetMonthly)
	}
	if !reflect.DeepEqual(c.ResourceGroups, []string{"rg-one", "rg-two"}) {
		t.Fatalf("groups = %q, blanks must be dropped", c.ResourceGroups)
	}
}

func TestValidateRejectsWhatWouldFailAtTheNextSync(t *testing.T) {
	cases := map[string]func(*Config){
		"client secret mode with no tenant":  func(c *Config) { c.TenantID = "" },
		"client secret mode with no client":  func(c *Config) { c.ClientID = "" },
		"client secret mode with no secret":  func(c *Config) { c.ClientSecret = "" },
		"no subscription":                    func(c *Config) { c.SubscriptionID = "" },
		"no resource group":                  func(c *Config) { c.ResourceGroups = nil },
		"an interval of zero":                func(c *Config) { c.InventoryEveryMin = 0 },
		"a negative budget":                  func(c *Config) { c.BudgetMonthly = -1 },
		"a mode nobody implements":           func(c *Config) { c.Mode = "device_code" },
	}
	for name, break_ := range cases {
		c := validConfig()
		break_(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: must be rejected", name)
		}
	}
	if err := validConfig().Validate(); err != nil {
		t.Errorf("a valid config was rejected: %v", err)
	}
	if err := (Config{Mode: "off"}).Validate(); err != nil {
		t.Errorf("off is always valid: %v", err)
	}
}

func TestEmptyGroupListIsRejectedWithItsReason(t *testing.T) {
	// The role assignment being asked for is on a resource group. A
	// subscription-wide sweep would answer 403, which is worth saying now
	// rather than at three in the morning.
	c := validConfig()
	c.ResourceGroups = nil
	err := c.Validate()
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "resource group") {
		t.Fatalf("the reason must name resource groups: %v", err)
	}
}

func TestManagedIdentityModeNeedsNoSecret(t *testing.T) {
	c := Config{Mode: "managed_identity", SubscriptionID: "sub",
		ResourceGroups: []string{"rg"}, InventoryEveryMin: 15, CostEveryMin: 60}
	if err := c.Validate(); err != nil {
		t.Fatalf("managed identity carries no credential of its own: %v", err)
	}
}

func TestNewSourceBuildsWhatTheModeAsksFor(t *testing.T) {
	cs, err := NewSource(validConfig(), nil)
	if err != nil || cs.Name() != "client_secret" {
		t.Fatalf("source = %v err = %v", cs, err)
	}
	mi, err := NewSource(Config{Mode: "managed_identity", SubscriptionID: "s",
		ResourceGroups: []string{"rg"}, InventoryEveryMin: 15, CostEveryMin: 60},
		func(string) (string, bool) { return "", false })
	if err != nil || mi.Name() != "managed_identity" {
		t.Fatalf("source = %v err = %v", mi, err)
	}
	if _, err := NewSource(Config{Mode: "off"}, nil); err == nil {
		t.Fatal("off builds no source")
	}
}

func TestSecretNeverAppearsInAnError(t *testing.T) {
	c := validConfig()
	c.ClientSecret = "super-secret-value"
	c.SubscriptionID = ""
	err := c.Validate()
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "super-secret-value") {
		t.Fatalf("the secret leaked into an error message: %v", err)
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/hub/azure/ -run "Config|Validate|Source|Secret"`
Expected: FAIL, `undefined: LoadConfig`.

- [ ] **Step 3: Write config.go**

```go
package azure

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Getter and Setter are the two halves of the settings table this package uses.
// Declared here rather than imported from notify: a package that reads Azure
// has no business depending on the one that sends mail.
type Getter interface {
	GetSetting(key string) (string, bool, error)
}

type Setter interface {
	SetSetting(key, value string) error
}

type Config struct {
	Mode              string
	TenantID          string
	ClientID          string
	ClientSecret      string
	MIClientID        string
	SubscriptionID    string
	ResourceGroups    []string
	InventoryEveryMin int
	CostEveryMin      int
	BudgetMonthly     float64
}

func (c Config) Enabled() bool { return c.Mode == "client_secret" || c.Mode == "managed_identity" }

func str(g Getter, key, def string) string {
	v, ok, err := g.GetSetting(key)
	if err != nil || !ok {
		return def
	}
	return v
}

func integer(g Getter, key string, def int) int {
	n, err := strconv.Atoi(str(g, key, ""))
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func number(g Getter, key string) float64 {
	f, err := strconv.ParseFloat(str(g, key, ""), 64)
	if err != nil || f < 0 {
		return 0
	}
	return f
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// LoadConfig never fails: a hub that cannot parse its own settings still has to
// boot and still has to monitor its servers.
func LoadConfig(g Getter) Config {
	mode := str(g, "azure_mode", "off")
	switch mode {
	case "off", "client_secret", "managed_identity":
	default:
		mode = "off"
	}
	return Config{
		Mode:              mode,
		TenantID:          str(g, "azure_tenant_id", ""),
		ClientID:          str(g, "azure_client_id", ""),
		ClientSecret:      str(g, "azure_client_secret", ""),
		MIClientID:        str(g, "azure_mi_client_id", ""),
		SubscriptionID:    str(g, "azure_subscription_id", ""),
		ResourceGroups:    splitList(str(g, "azure_resource_groups", "")),
		InventoryEveryMin: integer(g, "azure_inventory_interval_min", 15),
		CostEveryMin:      integer(g, "azure_cost_interval_min", 60),
		BudgetMonthly:     number(g, "azure_budget_monthly"),
	}
}

func SaveConfig(s Setter, c Config) error {
	for k, v := range map[string]string{
		"azure_mode":                   c.Mode,
		"azure_tenant_id":              c.TenantID,
		"azure_client_id":              c.ClientID,
		"azure_client_secret":          c.ClientSecret,
		"azure_mi_client_id":           c.MIClientID,
		"azure_subscription_id":        c.SubscriptionID,
		"azure_resource_groups":        strings.Join(c.ResourceGroups, ","),
		"azure_inventory_interval_min": strconv.Itoa(c.InventoryEveryMin),
		"azure_cost_interval_min":      strconv.Itoa(c.CostEveryMin),
		"azure_budget_monthly":         strconv.FormatFloat(c.BudgetMonthly, 'f', -1, 64),
	} {
		if err := s.SetSetting(k, v); err != nil {
			return err
		}
	}
	return nil
}

// Validate refuses a configuration at save time rather than at the next sync.
// No message ever contains the secret.
func (c Config) Validate() error {
	switch c.Mode {
	case "off":
		return nil
	case "client_secret":
		if c.TenantID == "" || c.ClientID == "" || c.ClientSecret == "" {
			return errors.New("client secret mode needs a tenant id, a client id and a secret")
		}
	case "managed_identity":
		// Nothing of its own: the platform supplies the credential.
	default:
		return fmt.Errorf("mode must be off, client_secret or managed_identity")
	}
	if c.SubscriptionID == "" {
		return errors.New("a subscription id is required")
	}
	if len(c.ResourceGroups) == 0 {
		return errors.New("at least one resource group is required; reading a whole subscription " +
			"needs a subscription-scope role assignment this hub does not request")
	}
	if c.InventoryEveryMin <= 0 || c.CostEveryMin <= 0 {
		return errors.New("both intervals must be at least one minute")
	}
	if c.BudgetMonthly < 0 {
		return errors.New("a budget cannot be negative")
	}
	return nil
}

// NewSource builds the token source the mode asks for, wrapped in the cache.
func NewSource(c Config, env func(string) (string, bool)) (Source, error) {
	if env == nil {
		env = os.LookupEnv
	}
	switch c.Mode {
	case "client_secret":
		return NewCached(NewClientSecret(c.TenantID, c.ClientID, c.ClientSecret), nil), nil
	case "managed_identity":
		return NewCached(NewManagedIdentity(c.MIClientID, env), nil), nil
	}
	return nil, errors.New("azure is off")
}
```

- [ ] **Step 4: Run the package**

Run: `go test ./internal/hub/azure/ -race` then `go build ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/hub/azure
git commit -m "feat(azure): settings-backed configuration and source selection"
```

---

### Task 7: The sync job

**Files:**
- Create: `internal/hub/azure_sync.go`
- Modify: `internal/hub/hub.go`
- Test: `internal/hub/azure_test.go`

**Interfaces:**
- Produces:
```go
// in package hub
func (h *Hub) ReloadAzure()
func (h *Hub) syncAzureInventory(ctx context.Context)
func (h *Hub) syncAzureCosts(ctx context.Context)
func (h *Hub) TestAzure(ctx context.Context) error
func currentPeriod(now time.Time) string   // "2026-09"
```

`Hub` gains `acfg atomic.Pointer[azure.Config]` and `aclient atomic.Pointer[azure.Client]`, set by
`ReloadAzure`, read by the sync. Same shape as the notification configuration.

- [ ] **Step 1: Write the failing tests**

`internal/hub/azure_test.go`:
```go
package hub

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/azure"
	"github.com/vincentlauriat/serversmonitor/internal/hub/config"
)

// azureFake serves a token, a catalogue, the two enrichment passes and a cost
// query. failAfter makes every call fail once that many have succeeded.
type azureFake struct {
	srv       *httptest.Server
	calls     int
	failAfter int
	sites     []string // site names the catalogue and enrichment return
}

func newAzureFake(t *testing.T, sites ...string) *azureFake {
	f := &azureFake{failAfter: -1, sites: sites}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls++
		if f.failAfter >= 0 && f.calls > f.failAfter {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"error":{"code":"AuthorizationFailed","message":"does not have authorization"}}`)
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "/oauth2/"):
			io.WriteString(w, `{"token_type":"Bearer","expires_in":3599,"access_token":"tok"}`)
		case strings.HasSuffix(r.URL.Path, "/resources"):
			var parts []string
			for _, s := range f.sites {
				parts = append(parts, fmt.Sprintf(
					`{"id":"/subscriptions/SUB/resourceGroups/RG/providers/Microsoft.Web/sites/%s","name":%q,"type":"Microsoft.Web/sites","location":"westeurope","properties":null}`, s, s))
			}
			fmt.Fprintf(w, `{"value":[%s]}`, strings.Join(parts, ","))
		case strings.Contains(r.URL.Path, "/Microsoft.Web/sites"):
			var parts []string
			for _, s := range f.sites {
				parts = append(parts, fmt.Sprintf(
					`{"id":"/subscriptions/SUB/resourceGroups/RG/providers/Microsoft.Web/sites/%s","name":%q,"properties":{"state":"Running"}}`, s, s))
			}
			fmt.Fprintf(w, `{"value":[%s]}`, strings.Join(parts, ","))
		case strings.Contains(r.URL.Path, "/Microsoft.Web/serverfarms"):
			io.WriteString(w, `{"value":[]}`)
		// The real path is ".../providers/Microsoft.CostManagement/query", so the
		// character before CostManagement is a dot, not a slash. Matching
		// "/CostManagement/query" answers 404 and the sync reads as broken.
		case strings.Contains(r.URL.Path, "CostManagement/query"):
			io.WriteString(w, `{"properties":{"columns":[{"name":"Cost"},{"name":"ResourceId"},{"name":"Currency"}],
			 "rows":[[3.5,"/subscriptions/sub/resourcegroups/rg/providers/microsoft.web/sites/a","EUR"],
			         [9.0,"/subscriptions/sub/resourcegroups/rg/providers/microsoft.insights/components/long-gone","EUR"]]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func azureHub(t *testing.T, f *azureFake, dir string) *Hub {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	h, err := New(config.Config{DataDir: dir}, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	if err := azure.SaveConfig(h.st, azure.Config{Mode: "client_secret", TenantID: "t",
		ClientID: "c", ClientSecret: "s", SubscriptionID: "SUB", ResourceGroups: []string{"RG"},
		InventoryEveryMin: 15, CostEveryMin: 60}); err != nil {
		t.Fatal(err)
	}
	h.azureBase = f.srv.URL // both ARM and the token endpoint, for the test only
	h.ReloadAzure()
	return h
}

func TestAzureSyncStoresInventoryAndState(t *testing.T) {
	f := newAzureFake(t, "a", "b")
	h := azureHub(t, f, "")
	h.syncAzureInventory(context.Background())

	rs, err := h.st.ListAzureResources()
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 {
		t.Fatalf("got %d resources", len(rs))
	}
	if rs[0].State == nil || *rs[0].State != "Running" {
		t.Fatalf("state = %v, the enrichment pass must have run", rs[0].State)
	}
	st, _ := h.st.AzureSyncState()
	if !st["inventory"].OK {
		t.Fatalf("sync state = %+v", st["inventory"])
	}
}

func TestAFailedSyncKeepsTheLastGoodInventory(t *testing.T) {
	// The property this whole lot exists to protect: "I cannot see Azure" must
	// never render as "the sandbox is empty".
	f := newAzureFake(t, "a", "b")
	h := azureHub(t, f, "")
	h.syncAzureInventory(context.Background())
	if rs, _ := h.st.ListAzureResources(); len(rs) != 2 {
		t.Fatalf("fixture did not land: %d", len(rs))
	}

	f.failAfter = 0 // everything fails from now on
	f.calls = 0
	h.syncAzureInventory(context.Background())

	rs, _ := h.st.ListAzureResources()
	if len(rs) != 2 {
		t.Fatalf("a failed sync must change nothing, got %d rows", len(rs))
	}
	for _, r := range rs {
		if r.DeletedAt != nil {
			t.Fatalf("%s was marked deleted by a failed sync", r.Name)
		}
	}
	st, _ := h.st.AzureSyncState()
	if st["inventory"].OK {
		t.Fatal("the sync failed and the state must say so")
	}
	if !strings.Contains(st["inventory"].Message, "authorization") {
		t.Fatalf("Azure's own message must be kept: %q", st["inventory"].Message)
	}
}

func TestAPartialSyncIsAFailedSync(t *testing.T) {
	// Token, catalogue, then the enrichment pass fails. If that counted as a
	// success the sweep would mark every resource deleted.
	f := newAzureFake(t, "a", "b")
	h := azureHub(t, f, "")
	h.syncAzureInventory(context.Background())
	f.calls = 0
	f.failAfter = 2 // token and catalogue succeed, enrichment does not
	h.syncAzureInventory(context.Background())
	rs, _ := h.st.ListAzureResources()
	for _, r := range rs {
		if r.DeletedAt != nil {
			t.Fatalf("%s was deleted by a half-read sync", r.Name)
		}
	}
	if st, _ := h.st.AzureSyncState(); st["inventory"].OK {
		t.Fatal("a half-read sync is a failed sync")
	}
}

func TestAResourceThatVanishesIsSoftDeleted(t *testing.T) {
	f := newAzureFake(t, "a", "b")
	h := azureHub(t, f, "")
	h.syncAzureInventory(context.Background())
	f.sites = []string{"a"}
	h.syncAzureInventory(context.Background())
	rs, _ := h.st.ListAzureResources()
	var deleted int
	for _, r := range rs {
		if r.DeletedAt != nil {
			deleted++
			if r.Name != "b" {
				t.Fatalf("%s should not be deleted", r.Name)
			}
		}
	}
	if deleted != 1 || len(rs) != 2 {
		t.Fatalf("rows = %d, deleted = %d", len(rs), deleted)
	}
}

func TestCostRowsSurviveWithoutAResource(t *testing.T) {
	// A resource deleted mid-month still cost money. Dropping its row would
	// understate the bill.
	f := newAzureFake(t, "a")
	h := azureHub(t, f, "")
	h.syncAzureCosts(context.Background())
	cs, err := h.st.ListAzureCosts(currentPeriod(time.Now().UTC()))
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 {
		t.Fatalf("got %d cost rows, want 2 including the one with no resource", len(cs))
	}
	var orphan bool
	for _, c := range cs {
		if strings.Contains(c.ResourceID, "long-gone") {
			orphan = true
			if c.Amount != 9.0 {
				t.Fatalf("orphan amount = %v", c.Amount)
			}
		}
	}
	if !orphan {
		t.Fatal("the orphan cost row must be stored")
	}
}

func TestTestAzureReturnsAzuresOwnWords(t *testing.T) {
	f := newAzureFake(t, "a")
	h := azureHub(t, f, "")
	if err := h.TestAzure(context.Background()); err != nil {
		t.Fatalf("a working configuration must test clean: %v", err)
	}
	f.failAfter = 1 // the token works, the catalogue does not
	f.calls = 0
	err := h.TestAzure(context.Background())
	if err == nil || !strings.Contains(err.Error(), "authorization") {
		t.Fatalf("err = %v", err)
	}
}

func TestAzureOffDoesNothing(t *testing.T) {
	h, err := New(config.Config{DataDir: t.TempDir()}, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	h.ReloadAzure()
	h.syncAzureInventory(context.Background())
	h.syncAzureCosts(context.Background())
	if rs, _ := h.st.ListAzureResources(); len(rs) != 0 {
		t.Fatal("azure is off; nothing may be written")
	}
	if st, _ := h.st.AzureSyncState(); len(st) != 0 {
		t.Fatal("azure is off; not even a sync row")
	}
	if err := h.TestAzure(context.Background()); err == nil {
		t.Fatal("testing a disabled integration is an error, not a success")
	}
}

func TestChangingTheScopePurgesTheOldOne(t *testing.T) {
	f := newAzureFake(t, "a", "b")
	h := azureHub(t, f, "")
	h.syncAzureInventory(context.Background())
	// A different resource group: what was collected for the old one would
	// otherwise linger on the page forever, never swept because never synced.
	azure.SaveConfig(h.st, azure.Config{Mode: "client_secret", TenantID: "t", ClientID: "c",
		ClientSecret: "s", SubscriptionID: "SUB", ResourceGroups: []string{"OTHER"},
		InventoryEveryMin: 15, CostEveryMin: 60})
	h.ReloadAzure()
	if rs, _ := h.st.ListAzureResources(); len(rs) != 0 {
		t.Fatalf("changing the scope must purge, %d rows left", len(rs))
	}
}

func TestCurrentPeriod(t *testing.T) {
	if got := currentPeriod(time.Date(2026, 9, 18, 3, 0, 0, 0, time.UTC)); got != "2026-09" {
		t.Fatalf("period = %q", got)
	}
	if got := currentPeriod(time.Date(2026, 12, 31, 23, 59, 0, 0, time.UTC)); got != "2026-12" {
		t.Fatalf("period = %q", got)
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/hub/ -run Azure`
Expected: FAIL, `h.azureBase undefined`.

- [ ] **Step 3: Write azure_sync.go**

```go
package hub

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/azure"
	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

// currentPeriod is the month a cost row belongs to.
func currentPeriod(now time.Time) string { return now.UTC().Format("2006-01") }

// ReloadAzure rebuilds the client from the settings. Called at boot and
// whenever the Azure settings are saved.
//
// Changing the scope purges what was collected for the old one: resources from
// a group nobody watches any more would otherwise linger forever, never swept
// because never synced.
func (h *Hub) ReloadAzure() {
	c := azure.LoadConfig(h.st)
	if prev := h.acfg.Load(); prev != nil && scopeChanged(*prev, c) {
		if err := h.st.PurgeAzure(); err != nil {
			h.log.Error("purge azure after a scope change", "err", err)
		}
	}
	h.acfg.Store(&c)
	if !c.Enabled() {
		h.aclient.Store(nil)
		return
	}
	src, err := azure.NewSource(c, nil)
	if err != nil {
		h.log.Error("azure source", "err", err)
		h.aclient.Store(nil)
		return
	}
	opt := azure.Options{Log: nil}
	if h.azureBase != "" { // tests point both ARM and the token endpoint at a fake
		opt.Base = h.azureBase
		if cs, ok := src.(*azure.Cached); ok {
			_ = cs
		}
	}
	h.aclient.Store(azure.NewClient(src, opt))
}

func scopeChanged(a, b azure.Config) bool {
	if a.SubscriptionID != b.SubscriptionID || len(a.ResourceGroups) != len(b.ResourceGroups) {
		return true
	}
	for i := range a.ResourceGroups {
		if a.ResourceGroups[i] != b.ResourceGroups[i] {
			return true
		}
	}
	return false
}

// azureReady returns the config and client, or false when Azure is off.
func (h *Hub) azureReady() (azure.Config, *azure.Client, bool) {
	cp := h.acfg.Load()
	cl := h.aclient.Load()
	if cp == nil || cl == nil || !cp.Enabled() {
		return azure.Config{}, nil, false
	}
	return *cp, cl, true
}

// syncAzureInventory replaces the inventory, but only when every call in the
// sweep succeeded. A partial read would be swept as a batch of deletions, and
// "I cannot see Azure" would render as "the sandbox is empty".
func (h *Hub) syncAzureInventory(ctx context.Context) {
	cfg, client, ok := h.azureReady()
	if !ok {
		return
	}
	started := time.Now().UTC()
	rs, err := azure.Inventory(ctx, client, cfg.SubscriptionID, cfg.ResourceGroups)
	if err != nil {
		h.log.Warn("azure inventory sync failed", "err", err)
		h.recordAzureSync("inventory", false, err.Error(), started)
		return
	}
	now := time.Now().UTC()
	rows := make([]store.AzureResource, 0, len(rs))
	for _, r := range rs {
		rows = append(rows, store.AzureResource{ID: r.ID, Name: r.Name, Type: r.Type,
			ResourceGroup: r.ResourceGroup, Location: r.Location, Kind: r.Kind, SKU: r.SKU,
			State: r.State, ProvisioningState: r.ProvisioningState, Host: r.Host, Tags: r.Tags})
	}
	if err := h.st.ReplaceAzureInventory(cfg.ResourceGroups, rows, now); err != nil {
		h.log.Error("store azure inventory", "err", err)
		h.recordAzureSync("inventory", false, err.Error(), started)
		return
	}
	h.recordAzureSync("inventory", true, "", started)
	h.bus.Publish("azure", map[string]any{"scope": "inventory", "count": len(rows)})
}

func (h *Hub) syncAzureCosts(ctx context.Context) {
	cfg, client, ok := h.azureReady()
	if !ok {
		return
	}
	started := time.Now().UTC()
	cs, err := azure.Costs(ctx, client, cfg.SubscriptionID, cfg.ResourceGroups)
	if err != nil {
		h.log.Warn("azure cost sync failed", "err", err)
		h.recordAzureSync("cost", false, err.Error(), started)
		return
	}
	now := time.Now().UTC()
	period := currentPeriod(now)
	rows := make([]store.AzureCost, 0, len(cs))
	for _, c := range cs {
		// No filtering against the inventory: a resource deleted mid-month
		// still cost money, and dropping its row understates the bill.
		rows = append(rows, store.AzureCost{ResourceID: c.ResourceID, Period: period,
			Amount: c.Amount, Currency: c.Currency, AsOf: now})
	}
	if err := h.st.UpsertAzureCosts(rows); err != nil {
		h.log.Error("store azure costs", "err", err)
		h.recordAzureSync("cost", false, err.Error(), started)
		return
	}
	h.recordAzureSync("cost", true, "", started)
	h.bus.Publish("azure", map[string]any{"scope": "cost", "count": len(rows)})
}

func (h *Hub) recordAzureSync(scope string, ok bool, msg string, started time.Time) {
	if err := h.st.SetAzureSync(store.AzureSync{Scope: scope, OK: ok, Message: msg,
		StartedAt: started, EndedAt: time.Now().UTC()}); err != nil {
		h.log.Error("record azure sync", "err", err)
	}
}

// TestAzure acquires a token and runs one catalogue call, returning Azure's own
// error. It does not go through the periodic job: the button must answer now.
func (h *Hub) TestAzure(ctx context.Context) error {
	cfg, client, ok := h.azureReady()
	if !ok {
		return errors.New("azure is off; choose a mode and save first")
	}
	if _, err := azure.Inventory(ctx, client, cfg.SubscriptionID, cfg.ResourceGroups[:1]); err != nil {
		return fmt.Errorf("%w", err)
	}
	return nil
}
```

**Simplify `ReloadAzure`.** The `azureBase` handling above is muddled: the token endpoint and the
ARM base are two different things. Write it as two fields instead — `h.azureBase` overrides the ARM
base, and the client-secret source gets its own override through `azure.Config`:

```go
	src, err := azure.NewSource(c, nil)
	if err != nil { … }
	if h.azureBase != "" {
		if cs, ok := src.(*azure.Cached); ok {
			cs.WithEndpointForTests(h.azureBase)
		}
	}
	h.aclient.Store(azure.NewClient(src, azure.Options{Base: h.azureBase}))
```

and add to `internal/hub/azure/token.go`:

```go
// WithEndpointForTests points a cached client-secret source at another token
// host. Only the hub's own tests call it.
func (c *Cached) WithEndpointForTests(base string) {
	if s, ok := c.src.(*ClientSecretSource); ok {
		s.WithEndpoint(base)
	}
}
```

`azure.Options` has no `Log` field; drop that line.

- [ ] **Step 4: Wire the hub**

In `internal/hub/hub.go`, add to the struct:
```go
	acfg      atomic.Pointer[azure.Config]
	aclient   atomic.Pointer[azure.Client]
	azureBase string // tests only; empty means the real Azure
```

In `New`, after `h.ReloadNotify()`:
```go
	h.ReloadAzure()
```

In `Run`, add two tickers driven by the configuration, next to the existing ones:
```go
	azureInv := time.NewTicker(h.azureInterval("inventory"))
	azureCost := time.NewTicker(h.azureInterval("cost"))
	defer azureInv.Stop()
	defer azureCost.Stop()
	go func() { // catch up at boot without delaying the first metric tick
		h.syncAzureInventory(ctx)
		h.syncAzureCosts(ctx)
	}()
```
and in the select:
```go
		case <-azureInv.C:
			h.syncAzureInventory(ctx)
			azureInv.Reset(h.azureInterval("inventory"))
		case <-azureCost.C:
			h.syncAzureCosts(ctx)
			azureCost.Reset(h.azureInterval("cost"))
```
with:
```go
// azureInterval reads the configured cadence, falling back to a long one when
// Azure is off so a disabled integration costs one wake-up an hour, not one a
// minute.
func (h *Hub) azureInterval(scope string) time.Duration {
	c := h.acfg.Load()
	if c == nil || !c.Enabled() {
		return time.Hour
	}
	if scope == "cost" {
		return time.Duration(c.CostEveryMin) * time.Minute
	}
	return time.Duration(c.InventoryEveryMin) * time.Minute
}
```

New imports in `hub.go`: `github.com/vincentlauriat/serversmonitor/internal/hub/azure`.

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/hub/... -race`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/hub
git commit -m "feat(hub): periodic azure inventory and cost sync"
```

---

### Task 8: The Azure API

**Files:**
- Create: `internal/hub/server/api_azure.go`
- Modify: `internal/hub/server/server.go` (routes, `Deps.Azure`), `internal/hub/server/server_test.go` (rig)
- Test: `internal/hub/server/api_azure_test.go`

**Interfaces:**
- Produces:
```go
// Azurer is the hub seen from the server.
type Azurer interface {
    ReloadAzure()
    TestAzure(ctx context.Context) error
}
// GET  /api/v1/azure                  -> azureView
// GET  /api/v1/azure/settings         -> azureSettingsView
// PUT  /api/v1/azure/settings         -> 204
// POST /api/v1/azure/test             -> 204 or 502 with Azure's own reason
```

`azureView` joins inventory and cost **for display only**, as §3 of the spec requires:

```go
type azureRow struct {
    ID        string            `json:"id"`
    Name      string            `json:"name"`
    Type      string            `json:"type"`
    Group     string            `json:"resource_group"`
    Location  string            `json:"location"`
    State     *string           `json:"state"`      // null, never "unknown"
    Host      string            `json:"host,omitempty"`
    Tags      map[string]string `json:"tags"`
    Cost      *float64          `json:"cost"`       // null when Azure has not reported
    Currency  string            `json:"currency,omitempty"`
    Deleted   bool              `json:"deleted"`
}
type azureTotal struct {
    Currency string   `json:"currency"`
    Spent    float64  `json:"spent"`
    Budget   float64  `json:"budget"`   // 0 = none
}
type azureView struct {
    Mode      string       `json:"mode"`
    Period    string       `json:"period"`
    Rows      []azureRow   `json:"rows"`
    Totals    []azureTotal `json:"totals"`   // one per currency, never summed across
    CostAsOf  *time.Time   `json:"cost_as_of"`
    Sync      map[string]syncView `json:"sync"`
}
```

- [ ] **Step 1: Write the failing tests**

`internal/hub/server/api_azure_test.go`:
```go
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

type fakeAzurer struct {
	reloads int
	tested  int
	err     error
}

func (f *fakeAzurer) ReloadAzure()                        { f.reloads++ }
func (f *fakeAzurer) TestAzure(context.Context) error     { f.tested++; return f.err }

func (r *rig) azurer(t *testing.T) *fakeAzurer {
	t.Helper()
	f, ok := r.azure.(*fakeAzurer)
	if !ok {
		t.Fatalf("rig azurer is %T", r.azure)
	}
	return f
}

func strp(s string) *string { return &s }

func seedAzure(t *testing.T, r *rig) {
	t.Helper()
	rows := []store.AzureResource{
		{ID: "/subs/rg/a", Name: "visualrami", Type: "Microsoft.Web/sites", ResourceGroup: "rg",
			Location: "westeurope", State: strp("Running"), Tags: map[string]string{"env": "dev"}},
		{ID: "/subs/rg/b", Name: "kv-x", Type: "Microsoft.KeyVault/vaults", ResourceGroup: "rg",
			Location: "westeurope", State: nil},
	}
	if err := r.st.ReplaceAzureInventory([]string{"rg"}, rows, r.now); err != nil {
		t.Fatal(err)
	}
	costs := []store.AzureCost{
		{ResourceID: "/subs/rg/a", Period: "2026-09", Amount: 3.5, Currency: "EUR", AsOf: r.now},
		{ResourceID: "/subs/rg/long-gone", Period: "2026-09", Amount: 9, Currency: "EUR", AsOf: r.now},
	}
	if err := r.st.UpsertAzureCosts(costs); err != nil {
		t.Fatal(err)
	}
	r.st.SetAzureSync(store.AzureSync{Scope: "inventory", OK: true, StartedAt: r.now, EndedAt: r.now})
}

// azureAt reads the view for a fixed period, so the test does not depend on
// the real calendar.
func azureView(t *testing.T, r *rig) map[string]any {
	t.Helper()
	_, data := r.do(t, "GET", "/api/v1/azure?period=2026-09", nil)
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("view is not json: %s", data)
	}
	return v
}

func TestAzureViewJoinsCostOntoInventory(t *testing.T) {
	r := newNotifyRig(t)
	seedAzure(t, r)
	v := azureView(t, r)
	rows, _ := v["rows"].([]any)
	if len(rows) != 3 {
		t.Fatalf("two resources plus one orphan cost row = 3, got %d: %v", len(rows), v["rows"])
	}
	byName := map[string]map[string]any{}
	for _, raw := range rows {
		m := raw.(map[string]any)
		byName[m["name"].(string)] = m
	}
	if got := byName["visualrami"]; got["cost"] != 3.5 || got["currency"] != "EUR" {
		t.Fatalf("visualrami = %v", got)
	}
	if got := byName["kv-x"]; got["cost"] != nil {
		// Azure has reported nothing for it. That is not zero.
		t.Fatalf("a resource with no cost row carries null, got %v", got["cost"])
	}
	if got := byName["kv-x"]; got["state"] != nil {
		t.Fatalf("a state nobody read is null, got %v", got["state"])
	}
}

func TestOrphanCostRowIsShownAsDeleted(t *testing.T) {
	// Dropping it would understate the bill, which is the one number this
	// feature exists to get right.
	r := newNotifyRig(t)
	seedAzure(t, r)
	v := azureView(t, r)
	var found bool
	for _, raw := range v["rows"].([]any) {
		m := raw.(map[string]any)
		if strings.Contains(m["id"].(string), "long-gone") {
			found = true
			if m["deleted"] != true {
				t.Fatalf("an orphan cost row is shown as deleted: %v", m)
			}
			if m["cost"] != 9.0 {
				t.Fatalf("orphan cost = %v", m["cost"])
			}
			if m["name"] == "" {
				t.Fatal("with no inventory row, the last id segment stands in for a name")
			}
		}
	}
	if !found {
		t.Fatal("the orphan cost row must appear")
	}
}

func TestTotalsAreOnePerCurrencyAndNeverSummed(t *testing.T) {
	// Adding euros to dollars produces a number that is wrong in a way nobody
	// notices.
	r := newNotifyRig(t)
	seedAzure(t, r)
	r.st.UpsertAzureCosts([]store.AzureCost{
		{ResourceID: "/subs/rg/usd", Period: "2026-09", Amount: 2, Currency: "USD", AsOf: r.now}})
	v := azureView(t, r)
	totals, _ := v["totals"].([]any)
	if len(totals) != 2 {
		t.Fatalf("two currencies means two totals, got %v", totals)
	}
	byCur := map[string]float64{}
	for _, raw := range totals {
		m := raw.(map[string]any)
		byCur[m["currency"].(string)] = m["spent"].(float64)
	}
	if byCur["EUR"] != 12.5 || byCur["USD"] != 2 {
		t.Fatalf("totals = %v", byCur)
	}
}

func TestBudgetInheritsTheCostCurrency(t *testing.T) {
	r := newNotifyRig(t)
	seedAzure(t, r)
	body := map[string]any{"mode": "managed_identity", "subscription_id": "sub",
		"resource_groups": []string{"rg"}, "inventory_every_min": 15, "cost_every_min": 60,
		"budget_monthly": 100}
	if resp, data := r.do(t, "PUT", "/api/v1/azure/settings", body); resp.StatusCode != 204 {
		t.Fatalf("put = %d %s", resp.StatusCode, data)
	}
	v := azureView(t, r)
	for _, raw := range v["totals"].([]any) {
		m := raw.(map[string]any)
		if m["currency"] == "EUR" && m["budget"] != 100.0 {
			t.Fatalf("the budget carries the cost rows' currency: %v", m)
		}
	}
}

func TestAzureSettingsHideTheSecret(t *testing.T) {
	r := newNotifyRig(t)
	body := map[string]any{"mode": "client_secret", "tenant_id": "t", "client_id": "c",
		"client_secret": "s3cr3t", "subscription_id": "sub", "resource_groups": []string{"rg"},
		"inventory_every_min": 15, "cost_every_min": 60}
	if resp, data := r.do(t, "PUT", "/api/v1/azure/settings", body); resp.StatusCode != 204 {
		t.Fatalf("put = %d %s", resp.StatusCode, data)
	}
	if n := r.azurer(t).reloads; n != 1 {
		t.Fatalf("saving must reload the client, reloads = %d", n)
	}
	_, data := r.do(t, "GET", "/api/v1/azure/settings", nil)
	var v map[string]any
	json.Unmarshal(data, &v)
	if v["client_secret_set"] != true {
		t.Fatal("client_secret_set must say a secret exists")
	}
	if _, leaked := v["client_secret"]; leaked {
		t.Fatalf("the secret leaked: %s", data)
	}
	if !strings.Contains(string(data), `"tenant_id":"t"`) {
		t.Fatalf("the rest of the settings must come back: %s", data)
	}
}

func TestAbsentSecretKeepsTheStoredOne(t *testing.T) {
	r := newNotifyRig(t)
	base := map[string]any{"mode": "client_secret", "tenant_id": "t", "client_id": "c",
		"subscription_id": "sub", "resource_groups": []string{"rg"},
		"inventory_every_min": 15, "cost_every_min": 60}
	with := map[string]any{}
	for k, v := range base {
		with[k] = v
	}
	with["client_secret"] = "s3cr3t"
	r.do(t, "PUT", "/api/v1/azure/settings", with)
	base["client_id"] = "c2"
	if resp, _ := r.do(t, "PUT", "/api/v1/azure/settings", base); resp.StatusCode != 204 {
		t.Fatal("the second save must succeed without the secret")
	}
	if got, _, _ := r.st.GetSetting("azure_client_secret"); got != "s3cr3t" {
		t.Fatalf("secret = %q, want the stored one kept", got)
	}
	if got, _, _ := r.st.GetSetting("azure_client_id"); got != "c2" {
		t.Fatalf("the rest of the save must apply, client id = %q", got)
	}
}

func TestInvalidAzureSettingsAreRejectedAndWriteNothing(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		key  string
	}{
		{"no resource group", map[string]any{"mode": "managed_identity", "subscription_id": "sub",
			"resource_groups": []string{}, "inventory_every_min": 15, "cost_every_min": 60}, "azure_subscription_id"},
		{"client secret with no tenant", map[string]any{"mode": "client_secret", "client_id": "c",
			"client_secret": "s", "subscription_id": "sub", "resource_groups": []string{"rg"},
			"inventory_every_min": 15, "cost_every_min": 60}, "azure_client_id"},
		{"an interval of zero", map[string]any{"mode": "managed_identity", "subscription_id": "sub",
			"resource_groups": []string{"rg"}, "inventory_every_min": 0, "cost_every_min": 60}, "azure_subscription_id"},
	}
	for _, c := range cases {
		r := newNotifyRig(t) // a fresh store per case
		resp, data := r.do(t, "PUT", "/api/v1/azure/settings", c.body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: code = %d, want 400 (%s)", c.name, resp.StatusCode, data)
			continue
		}
		if v, ok, _ := r.st.GetSetting(c.key); ok && v != "" {
			t.Errorf("%s: a rejected save writes nothing, %s = %q", c.name, c.key, v)
		}
		if n := r.azurer(t).reloads; n != 0 {
			t.Errorf("%s: a rejected save must not reload, reloads = %d", c.name, n)
		}
	}
}

func TestAzureTestButtonReportsTheRealFailure(t *testing.T) {
	r := newNotifyRig(t)
	r.azurer(t).err = errors.New("Forbidden: does not have authorization to perform action")
	resp, data := r.do(t, "POST", "/api/v1/azure/test", nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502", resp.StatusCode)
	}
	if !strings.Contains(string(data), "does not have authorization") {
		t.Fatalf("Azure's own words are the whole point: %s", data)
	}
}

func TestAzureViewSurfacesAFailedSync(t *testing.T) {
	// An empty table with no explanation is the failure this lot exists to
	// avoid.
	r := newNotifyRig(t)
	r.st.SetAzureSync(store.AzureSync{Scope: "inventory", OK: false,
		Message: "Forbidden: does not have authorization", StartedAt: r.now, EndedAt: r.now})
	v := azureView(t, r)
	sync := v["sync"].(map[string]any)["inventory"].(map[string]any)
	if sync["ok"] != false || !strings.Contains(sync["message"].(string), "authorization") {
		t.Fatalf("sync = %v", sync)
	}
}

func TestAzureRequiresAuth(t *testing.T) {
	r := newRig(t)
	for _, p := range []string{"/api/v1/azure", "/api/v1/azure/settings"} {
		if resp, _ := r.do(t, "GET", p, nil); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s anonymous = %d", p, resp.StatusCode)
		}
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/hub/server/ -run Azure`
Expected: FAIL, the routes do not exist.

- [ ] **Step 3: Write api_azure.go**

Mirror `api_notify.go` exactly in shape: a view type, an input type with a `*string` secret, a
validate-before-write handler, and a test endpoint returning `502` with the underlying message.

The join, which is the part worth writing out:

```go
func (s *server) handleGetAzure(w http.ResponseWriter, r *http.Request, _ store.User) {
	cfg := azure.LoadConfig(s.Store)
	period := r.URL.Query().Get("period")
	if period == "" {
		period = time.Now().UTC().Format("2006-01")
	}
	resources, err := s.Store.ListAzureResources()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	costs, err := s.Store.ListAzureCosts(period)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	costByID := map[string]store.AzureCost{}
	var costAsOf *time.Time
	for _, c := range costs {
		costByID[c.ResourceID] = c
		if costAsOf == nil || c.AsOf.After(*costAsOf) {
			t := c.AsOf
			costAsOf = &t
		}
	}

	rows := make([]azureRow, 0, len(resources)+len(costs))
	seen := map[string]bool{}
	for _, r := range resources {
		row := azureRow{ID: r.ID, Name: r.Name, Type: r.Type, Group: r.ResourceGroup,
			Location: r.Location, State: r.State, Host: r.Host, Tags: r.Tags,
			Deleted: r.DeletedAt != nil}
		if c, ok := costByID[r.ID]; ok {
			amount := c.Amount
			row.Cost, row.Currency = &amount, c.Currency
		}
		// No cost row means Azure has not reported one. That is not zero, so
		// Cost stays nil and the table shows a dash.
		rows = append(rows, row)
		seen[r.ID] = true
	}
	// A cost row with no inventory row is a resource deleted before this hub
	// ever looked. Showing it is the difference between a right bill and a
	// wrong one.
	for _, c := range costs {
		if seen[c.ResourceID] {
			continue
		}
		amount := c.Amount
		rows = append(rows, azureRow{ID: c.ResourceID, Name: lastSegment(c.ResourceID),
			Type: typeFromID(c.ResourceID), Group: groupFromID(c.ResourceID),
			Cost: &amount, Currency: c.Currency, Deleted: true, Tags: map[string]string{}})
	}

	// One total per currency. Never summed across: adding euros to dollars
	// produces a number that is wrong in a way nobody notices.
	spent := map[string]float64{}
	for _, c := range costs {
		spent[c.Currency] += c.Amount
	}
	currencies := make([]string, 0, len(spent))
	for cur := range spent {
		currencies = append(currencies, cur)
	}
	sort.Strings(currencies)
	totals := make([]azureTotal, 0, len(currencies))
	for _, cur := range currencies {
		// The budget carries no currency of its own; it is read in whichever
		// currency the cost rows came back in.
		totals = append(totals, azureTotal{Currency: cur, Spent: spent[cur], Budget: cfg.BudgetMonthly})
	}

	syncs := map[string]syncView{}
	if st, err := s.Store.AzureSyncState(); err == nil {
		for k, v := range st {
			syncs[k] = syncView{OK: v.OK, Message: v.Message, At: v.EndedAt}
		}
	}
	writeJSON(w, http.StatusOK, azureView{Mode: cfg.Mode, Period: period, Rows: rows,
		Totals: totals, CostAsOf: costAsOf, Sync: syncs})
}

// lastSegment stands in for a name when a cost row has no inventory row,
// because "/subscriptions/…/components/long-gone" is not a name anyone reads.
func lastSegment(id string) string {
	if i := strings.LastIndex(id, "/"); i >= 0 && i+1 < len(id) {
		return id[i+1:]
	}
	return id
}

// typeFromID and groupFromID recover what they can from a bare resource id.
func typeFromID(id string) string {
	parts := strings.Split(strings.Trim(id, "/"), "/")
	for i, p := range parts {
		if strings.EqualFold(p, "providers") && i+2 < len(parts) {
			return parts[i+1] + "/" + parts[i+2]
		}
	}
	return ""
}

func groupFromID(id string) string {
	parts := strings.Split(strings.Trim(id, "/"), "/")
	for i := 0; i+1 < len(parts); i++ {
		if strings.EqualFold(parts[i], "resourcegroups") {
			return parts[i+1]
		}
	}
	return ""
}
```

`syncView` is `{ok bool, message string, at time.Time}` with the obvious JSON tags.

In `server.go`, add `Azure Azurer` to `Deps`, carry it onto `server`, and register:
```go
	mux.Handle("GET /api/v1/azure", s.auth(s.handleGetAzure))
	mux.Handle("GET /api/v1/azure/settings", s.auth(s.handleGetAzureSettings))
	mux.Handle("PUT /api/v1/azure/settings", s.auth(s.handlePutAzureSettings))
	mux.Handle("POST /api/v1/azure/test", s.auth(s.handleTestAzure))
```

In `server_test.go`, add `azure Azurer` to the `rig` struct and `azure: &fakeAzurer{}` plus
`Azure: r.azure` in `newRig`, exactly as `notify` was added in lot 2.

In `internal/hub/hub.go`, pass the hub itself: `server.Deps{… Notify: h, Azure: h …}`.

- [ ] **Step 4: Run everything**

Run: `go test ./... -race` then `go vet ./...`
Expected: PASS.

- [ ] **Step 5: Verify the tests bite, by mutation**

Three assertions here are load-bearing and easy to satisfy vacuously. Break each, confirm the suite
goes red, restore:

1. Drop the orphan loop (`for _, c := range costs { if seen… }`).
   `TestOrphanCostRowIsShownAsDeleted` must fail.
2. Sum every currency into one total.
   `TestTotalsAreOnePerCurrencyAndNeverSummed` must fail.
3. Default a missing cost to `0` instead of leaving `Cost` nil.
   `TestAzureViewJoinsCostOntoInventory` must fail.

A mutation the suite survives means the test is decoration. Fix the test, not the mutation.

- [ ] **Step 6: Commit**

```bash
git add internal/hub
git commit -m "feat(api): azure inventory, cost and settings endpoints"
```

---

### Task 9: The Azure page

**Files:**
- Modify: `web/src/lib/api.ts`, `web/src/routes/settings/+page.svelte`, the nav component
- Create: `web/src/lib/azure.ts`, `web/src/lib/azure.test.ts`, `web/src/routes/azure/+page.svelte`

**Interfaces:**
- Produces:
```ts
export interface AzureRow { id: string; name: string; type: string; resource_group: string;
  location: string; state: string | null; host?: string; tags: Record<string,string>;
  cost: number | null; currency?: string; deleted: boolean }
export interface AzureTotal { currency: string; spent: number; budget: number }
export interface AzureSync { ok: boolean; message: string; at: string }
export interface AzureView { mode: string; period: string; rows: AzureRow[];
  totals: AzureTotal[]; cost_as_of: string | null; sync: Record<string, AzureSync> }
// web/src/lib/azure.ts
export function money(amount: number | null, currency: string | undefined): string
export function shortType(t: string): string
export function budgetShare(t: AzureTotal): number | null
export function sortRows(rows: AzureRow[]): AzureRow[]
```

- [ ] **Step 1: Write the failing front-end tests**

`web/src/lib/azure.test.ts`:
```ts
import { describe, expect, it } from 'vitest';
import { budgetShare, money, shortType, sortRows } from './azure';
import type { AzureRow } from './api';

const row = (over: Partial<AzureRow>): AzureRow => ({
  id: 'x', name: 'x', type: 'Microsoft.Web/sites', resource_group: 'rg', location: 'we',
  state: 'Running', tags: {}, cost: null, deleted: false, ...over
});

describe('money', () => {
  it('renders a dash when Azure has reported nothing', () => {
    // A cost Azure has not reported is not zero.
    expect(money(null, 'EUR')).toBe('—');
  });
  it('renders zero as zero', () => {
    expect(money(0, 'EUR')).not.toBe('—');
  });
  it('keeps small amounts visible instead of rounding them away', () => {
    // The sandbox's real numbers are fractions of a cent; 0,00 € reads as free.
    expect(money(0.000838, 'EUR')).not.toMatch(/^0[.,]00\s*€?$/);
  });
  it('carries the currency', () => {
    expect(money(3.5, 'USD')).toContain('3');
  });
});

describe('shortType', () => {
  it('drops the provider prefix', () => {
    expect(shortType('Microsoft.Web/sites')).toBe('sites');
    expect(shortType('Microsoft.Web/serverfarms')).toBe('serverfarms');
  });
  it('survives a type it has never seen', () => {
    expect(shortType('')).toBe('');
    expect(shortType('weird')).toBe('weird');
  });
});

describe('budgetShare', () => {
  it('is null when no budget is set', () => {
    expect(budgetShare({ currency: 'EUR', spent: 5, budget: 0 })).toBeNull();
  });
  it('is a fraction of the budget', () => {
    expect(budgetShare({ currency: 'EUR', spent: 25, budget: 100 })).toBeCloseTo(0.25);
  });
  it('does not cap at one, because going over budget is the thing worth seeing', () => {
    expect(budgetShare({ currency: 'EUR', spent: 150, budget: 100 })).toBeCloseTo(1.5);
  });
});

describe('sortRows', () => {
  it('puts deleted resources last whatever their cost', () => {
    const rows = [
      row({ name: 'gone', deleted: true, cost: 100 }),
      row({ name: 'alive', cost: 1 })
    ];
    expect(sortRows(rows).map((r) => r.name)).toEqual(['alive', 'gone']);
  });
  it('orders the living by cost, most expensive first', () => {
    const rows = [row({ name: 'cheap', cost: 1 }), row({ name: 'dear', cost: 9 })];
    expect(sortRows(rows).map((r) => r.name)).toEqual(['dear', 'cheap']);
  });
  it('puts a resource with no reported cost after the ones that have one', () => {
    const rows = [row({ name: 'unknown', cost: null }), row({ name: 'cheap', cost: 0.01 })];
    expect(sortRows(rows).map((r) => r.name)).toEqual(['cheap', 'unknown']);
  });
  it('does not mutate its input', () => {
    const rows = [row({ name: 'b', cost: 1 }), row({ name: 'a', cost: 2 })];
    sortRows(rows);
    expect(rows[0].name).toBe('b');
  });
});
```

- [ ] **Step 2: Run them and watch them fail**

Run: `cd web && npx vitest run src/lib/azure.test.ts`
Expected: FAIL, the module does not exist.

- [ ] **Step 3: Write `web/src/lib/azure.ts`**

```ts
import type { AzureRow, AzureTotal } from './api';

/**
 * Renders an amount, or a dash when Azure has reported nothing for it.
 * A missing cost is not a zero cost, and the two must not look alike.
 *
 * Small amounts keep enough digits to stay visible: the sandbox's real numbers
 * are fractions of a cent, and "0,00 €" reads as free.
 */
export function money(amount: number | null, currency: string | undefined): string {
  if (amount === null || amount === undefined) return '—';
  const digits = amount !== 0 && Math.abs(amount) < 0.01 ? 4 : 2;
  try {
    return new Intl.NumberFormat(undefined, {
      style: currency ? 'currency' : 'decimal',
      currency: currency || undefined,
      minimumFractionDigits: digits,
      maximumFractionDigits: digits
    }).format(amount);
  } catch {
    return `${amount.toFixed(digits)} ${currency ?? ''}`.trim();
  }
}

/** Drops the provider prefix: Microsoft.Web/sites reads as sites. */
export function shortType(t: string): string {
  const i = t.indexOf('/');
  return i >= 0 ? t.slice(i + 1) : t;
}

/** The share of the budget spent, or null when no budget is set. Not capped:
 *  going over is exactly the thing worth seeing. */
export function budgetShare(t: AzureTotal): number | null {
  if (!t.budget) return null;
  return t.spent / t.budget;
}

/** Living resources first, dearest first; a resource with no reported cost
 *  sorts after the ones that have one, then deleted resources last. */
export function sortRows(rows: AzureRow[]): AzureRow[] {
  return [...rows].sort((a, b) => {
    if (a.deleted !== b.deleted) return a.deleted ? 1 : -1;
    if ((a.cost === null) !== (b.cost === null)) return a.cost === null ? 1 : -1;
    if (a.cost !== null && b.cost !== null && a.cost !== b.cost) return b.cost - a.cost;
    return a.name.localeCompare(b.name);
  });
}
```

- [ ] **Step 4: Run the tests**

Run: `cd web && npx vitest run`
Expected: PASS, the lot 1 and lot 2 tests plus these 13.

- [ ] **Step 5: Write the page and the settings tab**

`web/src/routes/azure/+page.svelte`, following the existing pages' markup and the `$state` pattern:

- When `mode === 'off'`, no table. One short paragraph instead:
  > ServersMonitor is not reading Azure yet. It needs a credential a tenant administrator has to
  > create once: either an app registration with a client secret, or a managed identity, in both
  > cases with the **Reader** role on the resource group. Configure it under Settings → Azure.
- A header line: the period, the number of resources, when the cost figures were last refreshed and
  that Azure's cost data lags by hours, so a low number means "not all reported yet".
- When `sync.inventory.ok === false`, a red line above the table carrying Azure's own message and
  the time, **with the previous table still shown underneath**.
- The table: name, type (`shortType`), group, location, state (a dash when null), month-to-date
  cost (`money`), tags. Deleted rows struck through and last.
- A total block per currency: spent, budget when set, and a neutral bar at `budgetShare`. No colour
  threshold and no alert; acting on it is lot 6.

Add `Azure` to the nav next to Hosts and Alerts, and an `azure` tab in Settings holding mode, the
credential fields, subscription, resource groups, the two intervals, the budget, and a **Test
connection** button that shows Azure's own error.

The secret input binds through a handler rather than directly, so `null` and `''` stay distinct,
exactly as the SMTP password does:
```svelte
  <input type="password" autocomplete="new-password"
    placeholder={settings.client_secret_set ? '•••••••• (unchanged)' : 'no secret set'}
    value={clientSecret ?? ''}
    oninput={(e) => (clientSecret = (e.currentTarget as HTMLInputElement).value)} />
```

- [ ] **Step 6: Build and check**

Run: `cd web && npx svelte-check --threshold error && npm run build`, then `make web && go build ./...`
Expected: zero errors.

- [ ] **Step 7: Commit**

```bash
git add web internal
git commit -m "feat(web): azure page with inventory, cost and budget"
```

---

### Task 10: Proof, documentation and the branch

**Files:**
- Modify: `README.md`, `docs/ARCHITECTURE_EN.md`, `docs/ARCHITECTURE.md`, `CHANGES.md`, `TODOS.md`, `MEMORY.md`

- [ ] **Step 1: Run the whole suite**

```bash
go test ./... -race
go vet ./...
cd web && npx vitest run && npx svelte-check --threshold error && npm run build
```

- [ ] **Step 2: Verify against a running hub, by hand**

The habit that caught eight defects across lots 1 and 2, and which no test replaced.

1. Build and start the hub on a port `lsof -nP -iTCP:<port> -sTCP:LISTEN` shows to be free. **Not
   8090 on this machine**: a Python process holds `127.0.0.1:8090`, a bind on `:8090` succeeds and
   `localhost:8090` reaches the other process.
2. With Azure off, open the Azure page. It must explain what to ask for, not show an empty table.
3. Configure client-secret mode against a deliberately wrong secret. Press **Test connection**.
   Azure's own `AADSTS…` message must appear.
4. Point the hub at a local fake ARM (the one from Task 7's tests, run as a small `main`), let one
   sync land, confirm the table fills, then make the fake return `403` and confirm the table stays
   and a red line appears above it.
5. Check the browser console on every page.

Record what each step produced. A step not run is reported as not run.

- [ ] **Step 3: The one thing only Vincent can do**

The credential does not exist yet. Ask him to send the administrator request from §1 of the spec,
and to press **Test connection** once it is granted. Until then the README says Azure is untested
against a real subscription, and no green suite is allowed to imply otherwise — the same honesty
lot 2 applied to Microsoft Teams.

- [ ] **Step 4: Documentation**

- `README.md`: an Azure section covering the two credential options, the **Reader** role and the
  counter-intuitive fact that it covers cost despite the query being a POST, the resource-group
  requirement, and the note that a failed sync keeps the last good inventory. Tick lot 3.
- `docs/ARCHITECTURE_EN.md`: the `azure` package, the three tables, the two-pass inventory, the
  all-or-nothing sweep, and why cost and inventory are separate facts.
  `docs/ARCHITECTURE.md` mirrors it in French **in the same commit**.
- `CHANGES.md`, `TODOS.md`, `MEMORY.md`: per the repository's maintenance rule, updated in the same
  turn as each task rather than collected here. This step is the final reconciliation.

- [ ] **Step 5: Finish the branch**

**REQUIRED SUB-SKILL:** use `superpowers:finishing-a-development-branch`. Push
`feat/lot3-azure-read`, open the pull request, wait for CI on `go`, `web` and `docker`, then merge.
Never push to `main` directly.

---

## Self-review

**Spec coverage.** §1 the precondition → Task 6 validation and Task 9's off-state copy. §2 the
invariant → Tasks 5, 7 and 8, with the property tested in `TestAFailedSyncKeepsTheLastGoodInventory`.
§3 cost and inventory separate → Tasks 5 and 8. §4 two passes → Task 3. §5 authentication → Task 1.
§6 ARM → Tasks 2 and 4. §7 storage → Task 5. §8 configuration → Tasks 6 and 8. §9 interface →
Task 9. §10 what is not proved → Task 10 step 3.

**Ordering.** Every task compiles on its own. Task 7 needs `Deps.Azure` from Task 8 to build the
hub; the plan handles this by having Task 7 add the field to `Deps` as part of its own wiring step,
and Task 8 only fills in the handlers. Run `go build ./...` at each boundary and add the field
early if the compiler asks.

**Names checked across tasks.** `Source`, `Token`, `Retryable` and `MarkRetryable` are defined once
in Task 1 and used in 2, 4, 6, 7. `NormalizeID` is defined in Task 3 and used in Task 4.
`store.AzureResource` and `store.AzureCost` keep the same fields in Tasks 5, 7 and 8. `ReloadAzure`
and `TestAzure` are exported from Task 7, which is what the `Azurer` interface of Task 8 requires.

**What this plan does not prove.** That the credential works, because none exists. Every Azure
endpoint in the suite is an `httptest` server, and the first real sync is Vincent's.
