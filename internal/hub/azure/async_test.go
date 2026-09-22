package azure

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// accepted builds the 202 a caller would have received, pointing at srv.
func accepted(header, url string, retryAfter string) Response {
	h := http.Header{}
	h.Set(header, url)
	if retryAfter != "" {
		h.Set("Retry-After", retryAfter)
	}
	return Response{Status: http.StatusAccepted, Header: h}
}

func TestAwaitFollowsAzureAsyncOperationToSucceeded(t *testing.T) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n := polls.Add(1); n <= 2 {
			fmt.Fprint(w, `{"status":"InProgress"}`)
			return
		}
		fmt.Fprint(w, `{"status":"Succeeded"}`)
	}))
	defer srv.Close()

	var mu sync.Mutex
	var slept []time.Duration
	c := NewClient(staticSource("tok"), Options{Base: srv.URL, Sleep: noSleep(&slept, &mu)})
	if err := Await(context.Background(), c, accepted("Azure-AsyncOperation", srv.URL+"/op/1", "")); err != nil {
		t.Fatalf("Await: %v", err)
	}
	if polls.Load() != 3 {
		t.Fatalf("polls = %d, want 3", polls.Load())
	}
}

func TestAwaitReportsAFailedOperationWithAzuresOwnCode(t *testing.T) {
	// A 202 followed by Failed is the shape a quota refusal or a bad image
	// takes. The hub records the reason, so it has to survive the poll.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"Failed","error":{"code":"OperationNotAllowed",
			"message":"Operation could not be completed as it results in exceeding quota"}}`)
	}))
	defer srv.Close()

	var mu sync.Mutex
	var slept []time.Duration
	c := NewClient(staticSource("tok"), Options{Base: srv.URL, Sleep: noSleep(&slept, &mu)})
	err := Await(context.Background(), c, accepted("Azure-AsyncOperation", srv.URL+"/op/1", ""))
	if err == nil {
		t.Fatal("want an error")
	}
	var oe *OperationError
	if !errors.As(err, &oe) {
		t.Fatalf("err = %T %v, want *OperationError", err, err)
	}
	if oe.Code != "OperationNotAllowed" || oe.Status != "Failed" {
		t.Fatalf("code = %q, status = %q", oe.Code, oe.Status)
	}
	if !strings.Contains(err.Error(), "exceeding quota") {
		t.Fatalf("the message must survive: %v", err)
	}
	// It is not an HTTP failure, and must not read as one: the poll answered
	// 200 and StatusOf would otherwise report success.
	if StatusOf(err) != 0 {
		t.Fatalf("StatusOf = %d, want 0 — an operation failure is not an HTTP status", StatusOf(err))
	}
}

func TestAwaitTreatsCanceledAsAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"Canceled"}`)
	}))
	defer srv.Close()
	var mu sync.Mutex
	var slept []time.Duration
	c := NewClient(staticSource("tok"), Options{Base: srv.URL, Sleep: noSleep(&slept, &mu)})
	if err := Await(context.Background(), c, accepted("Azure-AsyncOperation", srv.URL+"/op/1", "")); err == nil {
		t.Fatal("a cancelled operation did not succeed")
	}
}

func TestAwaitHonoursRetryAfter(t *testing.T) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if polls.Add(1) == 1 {
			w.Header().Set("Retry-After", "7")
			fmt.Fprint(w, `{"status":"InProgress"}`)
			return
		}
		fmt.Fprint(w, `{"status":"Succeeded"}`)
	}))
	defer srv.Close()

	var mu sync.Mutex
	var slept []time.Duration
	c := NewClient(staticSource("tok"), Options{Base: srv.URL, Sleep: noSleep(&slept, &mu)})
	// The 202 itself asked for 3 s; the first poll then asked for 7 s.
	if err := Await(context.Background(), c, accepted("Azure-AsyncOperation", srv.URL+"/op/1", "3")); err != nil {
		t.Fatalf("Await: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(slept) != 2 || slept[0] != 3*time.Second || slept[1] != 7*time.Second {
		t.Fatalf("waits = %v, want [3s 7s] — Azure's pacing, not ours", slept)
	}
}

func TestAwaitFallsBackToLocation(t *testing.T) {
	// Location has no status field at all: it answers 202 until it answers the
	// result. Treating it like an async-operation body would read an empty
	// status and never terminate.
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if polls.Add(1) <= 2 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		fmt.Fprint(w, `{"id":"/subscriptions/s/…/vm1"}`)
	}))
	defer srv.Close()

	var mu sync.Mutex
	var slept []time.Duration
	c := NewClient(staticSource("tok"), Options{Base: srv.URL, Sleep: noSleep(&slept, &mu)})
	if err := Await(context.Background(), c, accepted("Location", srv.URL+"/loc/1", "")); err != nil {
		t.Fatalf("Await: %v", err)
	}
	if polls.Load() != 3 {
		t.Fatalf("polls = %d, want 3", polls.Load())
	}
}

func TestAzureAsyncOperationWinsOverLocation(t *testing.T) {
	// Azure sends both on a VM PUT. Only one of them carries a status, and
	// picking Location would report success the moment the result exists,
	// which for a create is before the machine is running.
	var opPolled, locPolled atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/op/1":
			opPolled.Add(1)
			fmt.Fprint(w, `{"status":"Succeeded"}`)
		default:
			locPolled.Add(1)
		}
	}))
	defer srv.Close()

	h := http.Header{}
	h.Set("Azure-AsyncOperation", srv.URL+"/op/1")
	h.Set("Location", srv.URL+"/loc/1")
	var mu sync.Mutex
	var slept []time.Duration
	c := NewClient(staticSource("tok"), Options{Base: srv.URL, Sleep: noSleep(&slept, &mu)})
	if err := Await(context.Background(), c, Response{Status: http.StatusAccepted, Header: h}); err != nil {
		t.Fatalf("Await: %v", err)
	}
	if opPolled.Load() != 1 || locPolled.Load() != 0 {
		t.Fatalf("op polled %d, location polled %d", opPolled.Load(), locPolled.Load())
	}
}

func TestANonAcceptedAnswerIsAlreadyDone(t *testing.T) {
	c := NewClient(staticSource("tok"), Options{Base: "http://127.0.0.1:1"})
	if err := Await(context.Background(), c, Response{Status: http.StatusOK}); err != nil {
		t.Fatalf("a 200 is finished: %v", err)
	}
}

func TestA202WithNoHeaderIsAnError(t *testing.T) {
	// Not a silent success. If Azure accepted the work and gave no way to
	// follow it, the hub does not know how it ended and must say so.
	c := NewClient(staticSource("tok"), Options{Base: "http://127.0.0.1:1"})
	err := Await(context.Background(), c, Response{Status: http.StatusAccepted, Header: http.Header{}})
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{"Azure-AsyncOperation", "Location"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error must name %s: %v", want, err)
		}
	}
}

func TestAShutdownMidPollIsNotAnOperationFailure(t *testing.T) {
	// The hub stopping is not Azure refusing. Recording it as a failure would
	// tell Vincent a VM creation failed when it is very likely still running.
	ctx, cancel := context.WithCancel(context.Background())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel()
		fmt.Fprint(w, `{"status":"InProgress"}`)
	}))
	defer srv.Close()

	var mu sync.Mutex
	var slept []time.Duration
	c := NewClient(staticSource("tok"), Options{Base: srv.URL, Sleep: noSleep(&slept, &mu)})
	err := Await(ctx, c, accepted("Azure-AsyncOperation", srv.URL+"/op/1", ""))
	if err == nil {
		t.Fatal("want an error")
	}
	var oe *OperationError
	if errors.As(err, &oe) {
		t.Fatalf("a shutdown was recorded as an Azure failure: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// created builds what Compute answers to a VM PUT: 201 Created, the
// provisioning still running, and an Azure-AsyncOperation header to follow.
func created(url string) Response {
	h := http.Header{}
	h.Set("Azure-AsyncOperation", url)
	return Response{Status: http.StatusCreated, Header: h}
}

// The first real VM (2026-09-22) was recorded "succeeded" two seconds after
// the request, while Azure was still building it. Compute answers a VM PUT
// with 201 rather than 202, and Await took every non-202 as already finished.
// A 201 that carries an Azure-AsyncOperation header is not finished: the
// header is Azure saying so.
func TestA201WithAnAsyncOperationIsFollowed(t *testing.T) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n := polls.Add(1); n <= 2 {
			fmt.Fprint(w, `{"status":"InProgress"}`)
			return
		}
		fmt.Fprint(w, `{"status":"Succeeded"}`)
	}))
	defer srv.Close()

	var mu sync.Mutex
	var slept []time.Duration
	c := NewClient(staticSource("tok"), Options{Base: srv.URL, Sleep: noSleep(&slept, &mu)})
	if err := Await(context.Background(), c, created(srv.URL+"/op/1")); err != nil {
		t.Fatalf("Await: %v", err)
	}
	if polls.Load() != 3 {
		t.Fatalf("polls = %d, want 3: a 201 with an operation to follow must be followed", polls.Load())
	}
}

func TestA201WhoseOperationFailsIsAFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"Failed","error":{"code":"AllocationFailed","message":"no capacity"}}`)
	}))
	defer srv.Close()

	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	err := Await(context.Background(), c, created(srv.URL+"/op/1"))
	if err == nil {
		t.Fatal("a 201 whose operation failed must not read as success")
	}
	if !strings.Contains(err.Error(), "AllocationFailed") {
		t.Fatalf("the error must carry Azure's code: %v", err)
	}
}
