// Package azure reads an Azure subscription: inventory, state and cost.
// It writes nothing to Azure. Acting on Azure is lots 4 to 6.
//
// The lot 1 invariant holds here too: silence is never zero. A state nobody
// read is nil, a cost Azure has not reported has no row, and a sync that failed
// changes nothing rather than emptying the inventory.
package azure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
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

	entraBase       = "https://login.microsoftonline.com"
	defaultIMDSBase = "http://169.254.169.254"

	// refreshMargin keeps a request from racing the expiry boundary.
	refreshMargin = 5 * time.Minute
)

// Token is an access token and the instant it stops being usable.
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

// MarkRetryable says this failure is worth another attempt: throttling, a 5xx,
// a refused connection. A bad secret is not, and will fail identically forever.
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
// is a string epoch and is the only one App Service sends.
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

// azureError extracts error_description, the only part of an Azure failure that
// tells anyone what to do about it.
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
		msg = string(body)
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

// WithEndpoint points the source at another token host. Tests only.
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
		e := azureError(resp.StatusCode, body)
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return Token{}, MarkRetryable(e)
		}
		return Token{}, e
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
	return &ManagedIdentitySource{miClientID: miClientID, env: env, imdsBase: defaultIMDSBase,
		client: &http.Client{Timeout: 20 * time.Second}}
}

func (s *ManagedIdentitySource) Name() string { return "managed_identity" }

func (s *ManagedIdentitySource) Token(ctx context.Context) (Token, error) {
	endpoint, header, appService := s.appService()
	apiVersion := "2019-08-01"
	if !appService {
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
	if appService {
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
		e := azureError(resp.StatusCode, body)
		// Documented IMDS guidance: 404 and 410 mean the service is updating,
		// 429 is throttling, 5xx is transient. Other 4xx are design-time errors.
		switch {
		case resp.StatusCode == http.StatusNotFound,
			resp.StatusCode == http.StatusGone,
			resp.StatusCode == http.StatusTooManyRequests,
			resp.StatusCode >= 500:
			return Token{}, MarkRetryable(e)
		}
		return Token{}, e
	}
	return parseTokenResponse(body, time.Now().UTC())
}

// appService reports the App Service / Container Apps endpoint when the
// platform exposes it. Both variables must be present and non-empty.
func (s *ManagedIdentitySource) appService() (endpoint, header string, ok bool) {
	e, hasE := s.env("IDENTITY_ENDPOINT")
	h, hasH := s.env("IDENTITY_HEADER")
	if hasE && hasH && e != "" && h != "" {
		return e, h, true
	}
	return "", "", false
}

// ---------- caching ----------

// Cached holds a token and refreshes it before it expires, so a request never
// races the boundary.
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

// WithEndpointForTests points a cached client-secret source at another token
// host. Only the hub's own tests call it.
func (c *Cached) WithEndpointForTests(base string) {
	if s, ok := c.src.(*ClientSecretSource); ok {
		s.WithEndpoint(base)
	}
}
