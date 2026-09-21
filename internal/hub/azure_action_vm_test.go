package hub

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/azure"
	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

const vmA = "/subscriptions/sub/resourcegroups/rg/providers/microsoft.compute/virtualmachines/vm-a"

// vmFake answers the Compute shapes, which the lot 4 fake never had to: a 202
// with an Azure-AsyncOperation header, the operation endpoint behind it, and
// an instance view instead of a properties.state.
type vmFake struct {
	srv *httptest.Server

	mu      sync.Mutex
	actions []string

	polls      atomic.Int32
	pollsUntil int32  // answer InProgress until this many polls have happened
	power      string // what the instance view reports
	opFailure  string // when set, the operation ends Failed with this message
	readStatus int    // non-zero makes the instance view fail
}

func newVMFake(t *testing.T) *vmFake {
	f := &vmFake{pollsUntil: 2, power: "deallocated"}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/oauth2/"):
			io.WriteString(w, `{"token_type":"Bearer","expires_in":3599,"access_token":"tok"}`)

		case r.URL.Path == "/operations/1":
			f.mu.Lock()
			fail := f.opFailure
			f.mu.Unlock()
			if f.polls.Add(1) <= f.pollsUntil {
				io.WriteString(w, `{"status":"InProgress"}`)
				return
			}
			if fail != "" {
				fmt.Fprintf(w, `{"status":"Failed","error":{"code":"OperationNotAllowed","message":%q}}`, fail)
				return
			}
			io.WriteString(w, `{"status":"Succeeded"}`)

		case strings.HasSuffix(r.URL.Path, "/instanceView"):
			f.mu.Lock()
			code, power := f.readStatus, f.power
			f.mu.Unlock()
			if code != 0 {
				w.WriteHeader(code)
				return
			}
			fmt.Fprintf(w, `{"statuses":[{"code":"ProvisioningState/succeeded"},{"code":"PowerState/%s"}]}`, power)

		case r.Method == http.MethodPost:
			f.mu.Lock()
			f.actions = append(f.actions, r.URL.Path)
			f.mu.Unlock()
			w.Header().Set("Azure-AsyncOperation", f.srv.URL+"/operations/1")
			w.WriteHeader(http.StatusAccepted)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *vmFake) actionCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.actions...)
}

// vmHub is azureHub against the Compute fake, with a VM already in the
// inventory. The inventory sweep is not used: lot 3's catalogue reader is Web
// shaped, and this test is about the action path.
func vmHub(t *testing.T, f *vmFake) *Hub {
	t.Helper()
	h := azureHubAt(t, f.srv.URL)
	// The poll paces itself off Azure's Retry-After in production; here it
	// would spend the test's whole budget waiting, and the pacing has its own
	// tests in the azure package.
	h.azureSleep = func(ctx context.Context, _ time.Duration) bool { return ctx.Err() == nil }
	h.ReloadAzure()
	if err := h.st.ReplaceAzureInventory([]string{"rg"}, []store.AzureResource{{
		ID: vmA, ARMID: vmA, Name: "vm-a", Type: "Microsoft.Compute/virtualMachines",
		ResourceGroup: "rg", Location: "westeurope", Tags: map[string]string{},
	}}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestStoppingAVMDeallocatesItAndWaitsForTheOperation(t *testing.T) {
	f := newVMFake(t)
	h := vmHub(t, f)

	id, err := h.StartAction(vmA, azure.ActionStop)
	if err != nil {
		t.Fatal(err)
	}
	a := waitAction(t, h, id)
	if a.Status != "succeeded" {
		t.Fatalf("status = %s, error = %s", a.Status, a.Error)
	}
	// Stop means deallocate: powerOff would leave the machine allocated and
	// billed, which is the opposite of what pressing Stop is for.
	calls := f.actionCalls()
	if len(calls) != 1 || !strings.HasSuffix(calls[0], "/deallocate") {
		t.Fatalf("Azure calls = %v", calls)
	}
	// The row did not finish on the 202. It finished when the operation did.
	if f.polls.Load() < 3 {
		t.Fatalf("polls = %d, want at least 3 — the action must follow the operation", f.polls.Load())
	}
	if a.StateAfter == nil || *a.StateAfter != "deallocated" {
		t.Fatalf("state_after = %v, want deallocated (from the instance view)", show(a.StateAfter))
	}
}

func TestAVMActionStaysRunningWhileTheOperationIs(t *testing.T) {
	// The lot 4 row had a `running` state that no code path could observe: every
	// action finished inside one HTTP call. With a VM it finally means what it
	// says, and the page depends on that to disable the buttons.
	f := newVMFake(t)
	f.pollsUntil = 1 << 30 // never terminates on its own
	h := vmHub(t, f)

	id, err := h.StartAction(vmA, azure.ActionStop)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		as, err := h.st.ListAzureActions(10)
		if err != nil {
			t.Fatal(err)
		}
		if len(as) == 1 && as[0].ID == id && as[0].Status == "running" && f.polls.Load() > 0 {
			return // observed mid-flight, which is the whole point
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the action never showed as running while the operation was in progress")
}

func TestAFailedOperationIsRecordedWithAzuresReason(t *testing.T) {
	f := newVMFake(t)
	f.opFailure = "Operation could not be completed as it results in exceeding quota"
	h := vmHub(t, f)

	id, err := h.StartAction(vmA, azure.ActionStart)
	if err != nil {
		t.Fatal(err)
	}
	a := waitAction(t, h, id)
	if a.Status != "failed" {
		t.Fatalf("status = %s", a.Status)
	}
	if !strings.Contains(a.Error, "exceeding quota") {
		t.Fatalf("the reason must survive the poll: %q", a.Error)
	}
	// And the state was not touched: a failed operation says nothing about
	// whether the machine is running.
	if a.StateAfter != nil {
		t.Fatalf("state_after = %q, want unread", *a.StateAfter)
	}
}

func TestAFailedInstanceViewStillLeavesTheActionSucceeded(t *testing.T) {
	// Lot 4's guarantee, now over a slower path: the action did happen, the
	// read-back did not, and the hub says exactly that.
	f := newVMFake(t)
	f.readStatus = http.StatusInternalServerError
	h := vmHub(t, f)

	id, err := h.StartAction(vmA, azure.ActionStop)
	if err != nil {
		t.Fatal(err)
	}
	a := waitAction(t, h, id)
	if a.Status != "succeeded" || a.StateAfter != nil {
		t.Fatalf("status = %s, state_after = %s; want succeeded with an unread state", a.Status, show(a.StateAfter))
	}
	rs, _ := h.st.ListAzureResources()
	if rs[0].State != nil {
		t.Fatalf("the inventory must be left alone, got %q", *rs[0].State)
	}
}

func TestAVMActionInFlightAtStartupIsInterruptedNotReplayed(t *testing.T) {
	f := newVMFake(t)
	f.pollsUntil = 1 << 30
	h := vmHub(t, f)

	if _, err := h.StartAction(vmA, azure.ActionStop); err != nil {
		t.Fatal(err)
	}
	// Wait until it is genuinely in flight before pretending the hub died.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && f.polls.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	before := len(f.actionCalls())

	h.interruptActions()
	as, err := h.st.ListAzureActions(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(as) != 1 || as[0].Status != "interrupted" {
		t.Fatalf("actions = %+v, want one interrupted", as)
	}
	if got := len(f.actionCalls()); got != before {
		t.Fatalf("the action was replayed: %d calls, was %d", got, before)
	}
}
