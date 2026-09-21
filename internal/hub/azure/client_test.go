package azure

import (
	"context"
	"encoding/json"
	"errors"
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
	var page atomic.Int32
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q", got)
		}
		switch page.Add(1) {
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
		t.Fatalf("got %d items across %d pages, want 4", len(items), page.Load())
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

// --- lot 4: the status has to survive as a number ---

func TestErrorKeepsTheStatusAsANumber(t *testing.T) {
	// Lot 4 branches on 403 vs 404 vs 409. Recovering that by matching
	// http.StatusText out of a message string would be parsing English.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":{"code":"AuthorizationFailed","message":"no role"}}`)
	}))
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	_, err := c.GetAll(context.Background(), "/x", nil)
	if StatusOf(err) != http.StatusForbidden {
		t.Fatalf("StatusOf = %d, want 403", StatusOf(err))
	}
	var ae *Error
	if !errors.As(err, &ae) {
		t.Fatalf("want an *Error, got %T", err)
	}
	if ae.Code != "AuthorizationFailed" {
		t.Fatalf("Code = %q, want AuthorizationFailed", ae.Code)
	}
}

func TestStatusSurvivesTheRetryableWrapper(t *testing.T) {
	// A 429 is wrapped by MarkRetryable. errors.As traverses Unwrap today;
	// this test is here so a later refactor cannot quietly drop that.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"code":"TooManyRequests","message":"slow down"}}`)
	}))
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL, Attempts: 1})
	_, err := c.GetAll(context.Background(), "/x", nil)
	if StatusOf(err) != http.StatusTooManyRequests {
		t.Fatalf("StatusOf = %d, want 429", StatusOf(err))
	}
	if !Retryable(err) {
		t.Fatal("a 429 must still read as retryable")
	}
}

func TestStatusOfANonHTTPFailureIsZero(t *testing.T) {
	// A refused connection is not a 500. Reporting one would invent a server
	// answer that never came.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := srv.URL
	srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: base, Attempts: 1})
	_, err := c.GetAll(context.Background(), "/x", nil)
	if err == nil {
		t.Fatal("want an error")
	}
	if StatusOf(err) != 0 {
		t.Fatalf("StatusOf = %d, want 0", StatusOf(err))
	}
}

func TestGetReturnsOneObjectAndGetAllCannot(t *testing.T) {
	const body = `{"id":"/x","properties":{"state":"Running"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL})

	got, err := c.Get(context.Background(), "/x", nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != body {
		t.Fatalf("Get = %s, want the body verbatim", got)
	}

	// Why Get has to exist: a single object unmarshals into the page envelope
	// without error and yields nothing at all. A silent empty is the failure
	// mode this project refuses everywhere else.
	items, err := c.GetAll(context.Background(), "/x", nil)
	if err != nil || len(items) != 0 {
		t.Fatalf("GetAll on one object = %d items, %v; want 0, nil — that is the trap", len(items), err)
	}
}

func TestStatusSurvivesABodyThatIsNotTheArmEnvelope(t *testing.T) {
	// A proxy between the hub and ARM can answer 403 with an HTML page or
	// nothing at all. The status is then the only fact left; losing it would
	// turn "you lack the role" into an unclassifiable failure.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	_, err := c.GetAll(context.Background(), "/x", nil)
	if StatusOf(err) != http.StatusForbidden {
		t.Fatalf("StatusOf = %d, want 403", StatusOf(err))
	}
}

// --- lot 5, task 1: the call path returns a response, not just a body -------

func TestAnAsyncAnswerKeepsItsStatusAndHeaders(t *testing.T) {
	// A 202 is indistinguishable from a 200 when only the body comes back, and
	// Azure-AsyncOperation is the only way to learn how the operation ended.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		w.Header().Set("Azure-AsyncOperation", "https://management.azure.com/op/1")
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusAccepted)
		io.WriteString(w, `{"properties":{"provisioningState":"Creating"}}`)
	}))
	defer srv.Close()

	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	resp, err := c.PutAsync(context.Background(), "/subscriptions/s/…/nic", nil, map[string]any{"location": "westeurope"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.Status)
	}
	if got := resp.Header.Get("Azure-AsyncOperation"); got != "https://management.azure.com/op/1" {
		t.Fatalf("Azure-AsyncOperation = %q", got)
	}
	if got := resp.Header.Get("Retry-After"); got != "3" {
		t.Fatalf("Retry-After = %q", got)
	}
	if !strings.Contains(string(resp.Body), "Creating") {
		t.Fatalf("body = %q", resp.Body)
	}
}

func TestASynchronousAnswerStillCarriesItsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":"/subscriptions/s/…/nic"}`)
	}))
	defer srv.Close()

	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	resp, err := c.PutAsync(context.Background(), "/subscriptions/s/…/nic", nil, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "/nic") {
		t.Fatalf("body = %q", resp.Body)
	}
}

func TestAPutRepeatsA5xxAndAnActionDoesNot(t *testing.T) {
	// The two policies, against one fake, because the difference is the whole
	// reason both exist: a PUT on a named id converges, a POST …/stop does not.
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, `{"error":{"code":"BadGateway","message":"upstream"}}`)
	}))
	defer srv.Close()

	var mu sync.Mutex
	var slept []time.Duration
	c := NewClient(staticSource("tok"), Options{Base: srv.URL, Attempts: 3, Sleep: noSleep(&slept, &mu)})

	if _, err := c.PutAsync(context.Background(), "/put", nil, map[string]any{}); err == nil {
		t.Fatal("want an error")
	}
	if got := calls.Swap(0); got != 3 {
		t.Fatalf("PUT attempts = %d, want 3 (a PUT on a named id is idempotent)", got)
	}

	if _, err := c.PostActionAsync(context.Background(), "/act", nil); err == nil {
		t.Fatal("want an error")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("action attempts = %d, want exactly 1 (a 5xx on an action may have landed)", got)
	}
}

func TestADeleteRepeatsA5xxToo(t *testing.T) {
	// Deleting a named id is idempotent in the same way: the second DELETE of a
	// resource already gone is a 404, not a second deletion.
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	var mu sync.Mutex
	var slept []time.Duration
	c := NewClient(staticSource("tok"), Options{Base: srv.URL, Attempts: 2, Sleep: noSleep(&slept, &mu)})
	if _, err := c.DeleteAsync(context.Background(), "/gone", nil); err == nil {
		t.Fatal("want an error")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("DELETE attempts = %d, want 2", got)
	}
}
