package azure

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	delNIC = "/subscriptions/SUB/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/vm1-nic"
	delVM  = "/subscriptions/SUB/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1"
)

// tagFake answers reads with whatever tags a test asks for, and records every
// request by method — a delete that went out is only detectable that way.
type tagFake struct {
	srv *httptest.Server

	mu       sync.Mutex
	reqs     []string
	tags     map[string]string // by path suffix: the createdBy value to answer
	readFail map[string]int
	missing  map[string]bool
}

func newTagFake(t *testing.T) *tagFake {
	f := &tagFake{
		tags:     map[string]string{delNIC: CreatedByValue, delVM: CreatedByValue},
		readFail: map[string]int{},
		missing:  map[string]bool{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/oauth2/") {
			io.WriteString(w, `{"token_type":"Bearer","expires_in":3599,"access_token":"tok"}`)
			return
		}
		f.mu.Lock()
		f.reqs = append(f.reqs, r.Method+" "+r.URL.Path)
		tag, hasTag := f.tags[r.URL.Path]
		fail := f.readFail[r.URL.Path]
		missing := f.missing[r.URL.Path]
		f.mu.Unlock()

		if missing {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":{"code":"ResourceNotFound","message":"gone"}}`)
			return
		}
		if r.Method == http.MethodGet && fail != 0 {
			w.WriteHeader(fail)
			return
		}
		if r.Method == http.MethodGet {
			if !hasTag {
				fmt.Fprint(w, `{"tags":{}}`)
				return
			}
			fmt.Fprintf(w, `{"tags":{"createdBy":%q}}`, tag)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *tagFake) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reqs...)
}

func (f *tagFake) deletes() []string {
	var out []string
	for _, r := range f.requests() {
		if strings.HasPrefix(r, http.MethodDelete+" ") {
			out = append(out, strings.TrimPrefix(r, http.MethodDelete+" "))
		}
	}
	return out
}

func (f *tagFake) client() *Client {
	return NewClient(staticSource("tok"), Options{Base: f.srv.URL, Attempts: 1,
		Sleep: func(ctx context.Context, _ time.Duration) bool { return ctx.Err() == nil }})
}

func bothResources() []Deletable {
	// Deliberately in the wrong order: the caller's order must not decide.
	return []Deletable{{ARMID: delNIC, Kind: "nic"}, {ARMID: delVM, Kind: "vm"}}
}

func TestDeleteRemovesTheVMBeforeTheNIC(t *testing.T) {
	// Azure refuses to delete a NIC that is still attached, so this ordering
	// is a requirement, not a habit.
	f := newTagFake(t)
	var gone []Deletable
	err := DeleteCreated(context.Background(), f.client(), bothResources(),
		func(d Deletable) error { gone = append(gone, d); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if got := f.deletes(); len(got) != 2 || got[0] != delVM || got[1] != delNIC {
		t.Fatalf("deletes = %v", got)
	}
	if len(gone) != 2 || gone[0].Kind != "vm" || gone[1].Kind != "nic" {
		t.Fatalf("recorded = %+v", gone)
	}
}

func TestDeleteUsesEachProvidersOwnAPIVersion(t *testing.T) {
	var versions []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/oauth2/") {
			io.WriteString(w, `{"token_type":"Bearer","expires_in":3599,"access_token":"tok"}`)
			return
		}
		if r.Method == http.MethodDelete {
			versions = append(versions, r.URL.Path+"@"+r.URL.Query().Get("api-version"))
		}
		fmt.Fprintf(w, `{"tags":{"createdBy":%q}}`, CreatedByValue)
	}))
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	if err := DeleteCreated(context.Background(), c, bothResources(), func(Deletable) error { return nil }); err != nil {
		t.Fatal(err)
	}
	want := []string{delVM + "@" + computeAPIVersion, delNIC + "@" + networkAPIVersion}
	if len(versions) != 2 || versions[0] != want[0] || versions[1] != want[1] {
		t.Fatalf("deletes = %v\nwant %v", versions, want)
	}
}

func TestAnUntaggedResourceIsRefusedAndNothingIsDeleted(t *testing.T) {
	// The whole set is refused, not just that one. Deleting the VM and then
	// refusing the NIC would be the worst of both outcomes.
	f := newTagFake(t)
	f.tags[delVM] = "somebody-else"
	err := DeleteCreated(context.Background(), f.client(), bothResources(), func(Deletable) error { return nil })
	if !errors.Is(err, ErrNotOurs) {
		t.Fatalf("err = %v, want ErrNotOurs", err)
	}
	if got := f.deletes(); len(got) != 0 {
		t.Fatalf("something was deleted anyway: %v", got)
	}
}

func TestAResourceWithNoTagsAtAllIsRefused(t *testing.T) {
	f := newTagFake(t)
	delete(f.tags, delNIC)
	if err := DeleteCreated(context.Background(), f.client(), bothResources(),
		func(Deletable) error { return nil }); !errors.Is(err, ErrNotOurs) {
		t.Fatalf("err = %v, want ErrNotOurs", err)
	}
	if got := f.deletes(); len(got) != 0 {
		t.Fatalf("something was deleted anyway: %v", got)
	}
}

func TestAFailedTagReadIsARefusal(t *testing.T) {
	// Not knowing whether a resource is ours is not permission to delete it.
	f := newTagFake(t)
	f.readFail[delVM] = http.StatusInternalServerError
	err := DeleteCreated(context.Background(), f.client(), bothResources(), func(Deletable) error { return nil })
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "check it is ours") {
		t.Fatalf("the error must say why: %v", err)
	}
	if got := f.deletes(); len(got) != 0 {
		t.Fatalf("something was deleted anyway: %v", got)
	}
}

func TestAResourceAlreadyGoneIsRecordedAsDeleted(t *testing.T) {
	// Idempotence: the second press of a Delete button must not report a
	// failure, and the record has to catch up rather than stay stale.
	f := newTagFake(t)
	f.missing[delVM] = true
	var gone []Deletable
	if err := DeleteCreated(context.Background(), f.client(), bothResources(),
		func(d Deletable) error { gone = append(gone, d); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(gone) != 2 {
		t.Fatalf("recorded = %+v, want both", gone)
	}
	if got := f.deletes(); len(got) != 2 {
		t.Fatalf("the NIC must still be attempted: %v", got)
	}
}

func TestAnUnknownKindIsRefusedBeforeAnyCall(t *testing.T) {
	// "disk" no longer stands in for an unknown kind: DeleteAny's tests below
	// need apiVersionFor to know it. A kind genuinely absent from the switch
	// is what this test is about.
	f := newTagFake(t)
	err := DeleteCreated(context.Background(), f.client(),
		[]Deletable{{ARMID: "/x", Kind: "storageAccount"}}, func(Deletable) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "storageAccount") {
		t.Fatalf("err = %v", err)
	}
	if n := len(f.requests()); n != 0 {
		t.Fatalf("%d requests were sent", n)
	}
}

func TestKindOfAndWhatTheRoleCanDelete(t *testing.T) {
	cases := map[string][2]any{
		"Microsoft.Compute/disks":             {"disk", true},
		"Microsoft.Network/networkInterfaces": {"nic", true},
		"Microsoft.Web/serverfarms":           {"plan", true},
		"Microsoft.Compute/virtualMachines":   {"vm", true},
		"Microsoft.Network/publicIPAddresses": {"ip", false},
		"Microsoft.Web/sites":                 {"", false},
	}
	for typ, want := range cases {
		kind, ok := KindOf(typ)
		if kind != want[0].(string) || ok != want[1].(bool) {
			t.Errorf("%s → %q %v, want %q %v", typ, kind, ok, want[0], want[1])
		}
	}
}

func TestDeleteAnyDoesNotCheckOwnershipAndTreats404AsDone(t *testing.T) {
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/oauth2/") {
			io.WriteString(w, `{"token_type":"Bearer","expires_in":3599,"access_token":"tok"}`)
			return
		}
		if r.Method == http.MethodGet {
			t.Errorf("no GET expected (no ownership check), got %s", r.URL.Path)
		}
		if r.Method == http.MethodDelete {
			deleted = append(deleted, r.URL.Path)
			if strings.HasSuffix(r.URL.Path, "/gone") {
				w.WriteHeader(404)
				io.WriteString(w, `{"error":{"code":"ResourceNotFound","message":"gone"}}`)
				return
			}
			w.WriteHeader(200)
		}
	}))
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	if err := DeleteAny(context.Background(), c, "/subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/disks/d1", "disk"); err != nil {
		t.Fatal(err)
	}
	if err := DeleteAny(context.Background(), c, "/subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/disks/gone", "disk"); err != nil {
		t.Fatalf("404 is done: %v", err)
	}
	if err := DeleteAny(context.Background(), c, "/x/ip1", "ip"); !errors.Is(err, ErrUndeletableKind) {
		t.Fatalf("ip must be refused before any call: %v", err)
	}
	if len(deleted) != 2 {
		t.Fatalf("deletes = %v", deleted)
	}
}
