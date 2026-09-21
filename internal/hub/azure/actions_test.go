package azure

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const siteID = "/subscriptions/s/resourceGroups/rg/providers/Microsoft.Web/sites/app"

func TestDoCallsTheRightURL(t *testing.T) {
	for _, a := range []Action{ActionStart, ActionStop, ActionRestart} {
		t.Run(string(a), func(t *testing.T) {
			var gotMethod, gotPath, gotVersion string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod, gotPath = r.Method, r.URL.Path
				gotVersion = r.URL.Query().Get("api-version")
			}))
			defer srv.Close()
			c := NewClient(staticSource("tok"), Options{Base: srv.URL})
			if err := Do(context.Background(), c, siteID, "microsoft.web/sites", a); err != nil {
				t.Fatalf("Do: %v", err)
			}
			if gotMethod != http.MethodPost {
				t.Fatalf("method = %s, want POST", gotMethod)
			}
			// ARM's own casing, taken from the id it gave us.
			if want := siteID + "/" + string(a); gotPath != want {
				t.Fatalf("path = %s, want %s", gotPath, want)
			}
			if gotVersion != webAPIVersion {
				t.Fatalf("api-version = %s, want %s", gotVersion, webAPIVersion)
			}
		})
	}
}

func TestUnknownTypeIsNotActionable(t *testing.T) {
	// An App Service Plan is not a site. Stopping one is not a thing, and the
	// hub must say so instead of inventing a URL for it.
	if Supports("microsoft.web/serverfarms", ActionStop) {
		t.Fatal("a server farm must not be actionable")
	}
	var called atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called.Add(1) }))
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	err := Do(context.Background(), c, "/subscriptions/s/resourceGroups/rg/providers/Microsoft.Web/serverfarms/p",
		"microsoft.web/serverfarms", ActionStop)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "serverfarms") {
		t.Fatalf("the error must name the type: %v", err)
	}
	if called.Load() != 0 {
		t.Fatalf("no connection must be opened, got %d", called.Load())
	}
}

func TestA429IsRetried(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"code":"TooManyRequests","message":"slow down"}}`)
		}
	}))
	defer srv.Close()
	var mu sync.Mutex
	var slept []time.Duration
	c := NewClient(staticSource("tok"), Options{Base: srv.URL, Sleep: noSleep(&slept, &mu)})
	if err := Do(context.Background(), c, siteID, "microsoft.web/sites", ActionStop); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3", calls.Load())
	}
	if len(slept) != 2 || slept[0] != time.Second || slept[1] != 5*time.Second {
		t.Fatalf("backoff = %v, want [1s 5s]", slept)
	}
}

func TestA500IsNotRetried(t *testing.T) {
	// The load-bearing test of this lot. A POST …/stop that answers 500 may
	// have stopped the site already; sending it again is the double-action the
	// startup rule refuses, only faster. The read-back is what settles it.
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"code":"InternalServerError","message":"boom"}}`)
	}))
	defer srv.Close()
	var mu sync.Mutex
	var slept []time.Duration
	c := NewClient(staticSource("tok"), Options{Base: srv.URL, Sleep: noSleep(&slept, &mu)})
	err := Do(context.Background(), c, siteID, "microsoft.web/sites", ActionStop)
	if err == nil {
		t.Fatal("want an error")
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want exactly 1 — an action is not replayed", calls.Load())
	}
	if StatusOf(err) != http.StatusInternalServerError {
		t.Fatalf("StatusOf = %d, want 500", StatusOf(err))
	}
}

func TestReadsKeepTheirOwn5xxRetry(t *testing.T) {
	// The action policy must not leak onto the read path: a GET is safe to
	// repeat, and lot 3 depends on that.
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, `{"value":[]}`)
	}))
	defer srv.Close()
	var mu sync.Mutex
	var slept []time.Duration
	c := NewClient(staticSource("tok"), Options{Base: srv.URL, Sleep: noSleep(&slept, &mu)})
	if _, err := c.GetAll(context.Background(), "/x", nil); err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3 — reads still retry a 5xx", calls.Load())
	}
}

func TestA403IsNotRetriedAndKeepsItsStatus(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":{"code":"AuthorizationFailed","message":"no Website Contributor"}}`)
	}))
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	err := Do(context.Background(), c, siteID, "microsoft.web/sites", ActionStop)
	if StatusOf(err) != http.StatusForbidden || calls.Load() != 1 {
		t.Fatalf("StatusOf = %d after %d calls, want 403 after 1", StatusOf(err), calls.Load())
	}
}

func TestReadStateReturnsNilWhenAzureOmitsIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"/x","properties":{}}`)
	}))
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	got, err := ReadState(context.Background(), c, siteID, "microsoft.web/sites")
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if got != nil {
		t.Fatalf("state = %q, want nil — not knowing is not Stopped", *got)
	}
}

func TestReadStateReadsTheState(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		fmt.Fprint(w, `{"id":"/x","properties":{"state":"Stopped"}}`)
	}))
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	got, err := ReadState(context.Background(), c, siteID, "microsoft.web/sites")
	if err != nil || got == nil || *got != "Stopped" {
		t.Fatalf("ReadState = %v, %v; want Stopped", got, err)
	}
	if gotPath != siteID {
		t.Fatalf("path = %s, want %s", gotPath, siteID)
	}
}

func TestReadStateFailureIsAnError(t *testing.T) {
	// The caller has to tell "Azure says nothing" from "Azure did not answer".
	// One leaves the state alone; the other would be a lie if it did not.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL, Attempts: 1})
	if _, err := ReadState(context.Background(), c, siteID, "microsoft.web/sites"); err == nil {
		t.Fatal("a failed read-back must be an error, not a silent nil")
	}
}
