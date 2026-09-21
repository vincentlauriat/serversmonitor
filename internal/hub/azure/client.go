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

// Client calls Azure Resource Manager. Hand-written over net/http rather than
// through the Azure SDK, in the taste already set by the Docker reader and the
// mail sender: the surface used here is four GET shapes and one POST.
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

// GetAll follows nextLink to the end. A partial read is an error, never a short
// list: the caller's sweep would take the missing rows for deleted resources.
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

// Get reads one object. ARM answers a single resource with the object itself,
// not with a {"value":[…]} page, and GetAll would unmarshal that without error
// and return nothing at all — a silent empty, which is the one failure this
// project refuses everywhere else.
func (c *Client) Get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	return c.do(ctx, http.MethodGet, c.url(path, query), nil)
}

func (c *Client) Post(ctx context.Context, path string, query url.Values, body any) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return c.do(ctx, http.MethodPost, c.url(path, query), raw)
}

// PostAction sends an action. It shares the client with the read path but not
// the retry policy: see retryThrottlingOnly.
func (c *Client) PostAction(ctx context.Context, path string, query url.Values) ([]byte, error) {
	return c.doWith(ctx, http.MethodPost, c.url(path, query), nil, retryThrottlingOnly)
}

func (c *Client) url(path string, query url.Values) string {
	u := strings.TrimSuffix(c.opt.Base, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return u
}

func (c *Client) do(ctx context.Context, method, rawURL string, body []byte) ([]byte, error) {
	return c.doWith(ctx, method, rawURL, body, Retryable)
}

// doWith takes the retry policy per call rather than per client. One
// *Client is shared by the inventory sweep, the cost sweep and the actions
// (see hub.go), so weakening its Options for one call would be a race against
// a sweep already in flight.
func (c *Client) doWith(ctx context.Context, method, rawURL string, body []byte, retry func(error) bool) ([]byte, error) {
	var last error
	for attempt := 1; attempt <= c.opt.Attempts; attempt++ {
		out, wait, err := c.attempt(ctx, method, rawURL, body)
		if err == nil {
			return out, nil
		}
		last = err
		if !retry(err) || attempt == c.opt.Attempts {
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

// Error is an ARM failure that kept its status as a number. The text is
// unchanged from what lot 3 produced; what is new is that a caller can branch
// on Status without reading English back out of the message.
type Error struct {
	Status  int
	Code    string // ARM's own code, e.g. "AuthorizationFailed"
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", http.StatusText(e.Status), truncate(e.Message, 400))
}

// StatusOf returns the HTTP status behind a failure, or 0 when there was no
// answer at all. A refused connection is not a 500, and must not read as one.
func StatusOf(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
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
	if e.Error.Message == "" {
		return azureError(status, body)
	}
	return &Error{Status: status, Code: e.Error.Code, Message: e.Error.Message}
}
