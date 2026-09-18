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
