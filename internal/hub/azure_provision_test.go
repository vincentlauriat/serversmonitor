package hub

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/azure"
	"github.com/vincentlauriat/serversmonitor/internal/hub/config"
	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

const provisionSubnet = "/subscriptions/SUB/resourceGroups/RG/providers/" +
	"Microsoft.Network/virtualNetworks/vnet-sandbox/subnets/default"

// provisionFake is an ARM that can create a VM, and can be told to stop
// halfway or to never finish.
type provisionFake struct {
	srv *httptest.Server

	mu       sync.Mutex
	reqs     []string // "METHOD path"
	failVM   int      // non-zero: the VM PUT answers this status
	blockVM  chan struct{}
	bodies   []string
	vnetFail int
}

func newProvisionFake(t *testing.T) *provisionFake {
	f := &provisionFake{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		if !strings.Contains(r.URL.Path, "/oauth2/") {
			f.reqs = append(f.reqs, r.Method+" "+r.URL.Path)
			f.bodies = append(f.bodies, string(raw))
		}
		failVM, block, vnetFail := f.failVM, f.blockVM, f.vnetFail
		f.mu.Unlock()

		switch {
		case strings.Contains(r.URL.Path, "/oauth2/"):
			io.WriteString(w, `{"token_type":"Bearer","expires_in":3599,"access_token":"tok"}`)
		case strings.HasSuffix(r.URL.Path, "/virtualNetworks/vnet-sandbox"):
			if vnetFail != 0 {
				w.WriteHeader(vnetFail)
				return
			}
			io.WriteString(w, `{"location":"westeurope"}`)
		case strings.Contains(r.URL.Path, "/virtualMachines/"):
			if block != nil {
				<-block
			}
			if failVM != 0 {
				w.WriteHeader(failVM)
				fmt.Fprint(w, `{"error":{"code":"OperationNotAllowed","message":"quota exceeded"}}`)
				return
			}
			fmt.Fprintf(w, `{"id":%q}`, r.URL.Path)
		default:
			fmt.Fprintf(w, `{"id":%q}`, r.URL.Path)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *provisionFake) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.reqs...)
}

func (f *provisionFake) allBodies() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.bodies, "\n")
}

// provisionHub is a hub wired to the fake, in a directory the caller can
// reopen — which is how a restart is tested honestly.
func provisionHub(t *testing.T, f *provisionFake, dir string, configure bool) (*Hub, context.CancelFunc) {
	t.Helper()
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
	if configure {
		if err := azure.SaveProvisionConfig(h.st, azure.ProvisionConfig{
			SubnetID: provisionSubnet, HubURL: "https://monitor.example.net",
			Size: "Standard_B1s", Image: "Canonical:ubuntu-24_04-lts:server:latest",
			AdminUser: "azureuser", SSHKey: "ssh-ed25519 AAAA… vincent@mac",
		}); err != nil {
			t.Fatal(err)
		}
	}
	h.azureBase = f.srv.URL
	h.azureSleep = func(ctx context.Context, _ time.Duration) bool { return ctx.Err() == nil }
	h.readBack = 200 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	h.runCtx.Store(&ctx)
	h.ReloadAzure()
	return h, cancel
}

func waitProvision(t *testing.T, h *Hub, id int64) store.AzureProvision {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ps, err := h.ListProvisions(10)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range ps {
			if p.ID == id && p.FinishedAt != nil {
				return p
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("provision %d never finished", id)
	return store.AzureProvision{}
}

func TestAProvisionCreatesTheHostFirstThenTheResources(t *testing.T) {
	f := newProvisionFake(t)
	h, cancel := provisionHub(t, f, t.TempDir(), true)
	defer cancel()

	id, err := h.StartProvision("vm-test")
	if err != nil {
		t.Fatal(err)
	}
	// The host exists before Azure is touched: a VM that boots fast and dials
	// in must find itself expected, not rejected.
	hosts, _ := h.st.ListHosts()
	if len(hosts) != 1 || hosts[0].Name != "vm-test" {
		t.Fatalf("hosts = %+v", hosts)
	}

	p := waitProvision(t, h, id)
	if p.Status != "succeeded" {
		t.Fatalf("status = %s, error = %s", p.Status, p.Error)
	}
	if p.HostID == nil || *p.HostID != hosts[0].ID {
		t.Fatalf("the provision must point at its host, got %v", p.HostID)
	}
	if len(p.Resources) != 2 || p.Resources[0].Kind != "nic" || p.Resources[1].Kind != "vm" {
		t.Fatalf("resources = %+v", p.Resources)
	}
}

func TestAProvisionIsRefusedWhenAzureIsOff(t *testing.T) {
	f := newProvisionFake(t)
	h, cancel := provisionHub(t, f, t.TempDir(), true)
	defer cancel()
	if err := azure.SaveConfig(h.st, azure.Config{Mode: "off"}); err != nil {
		t.Fatal(err)
	}
	h.ReloadAzure()

	if _, err := h.StartProvision("vm-test"); !errors.Is(err, ErrAzureOff) {
		t.Fatalf("err = %v, want ErrAzureOff", err)
	}
	assertNothingHappened(t, h, f)
}

func TestAnUnconfiguredProvisionLeavesNoHostRow(t *testing.T) {
	// The refusal comes before anything exists. A phantom host row for a VM
	// that was never created would show as a machine that has never reported.
	f := newProvisionFake(t)
	h, cancel := provisionHub(t, f, t.TempDir(), false)
	defer cancel()

	_, err := h.StartProvision("vm-test")
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "subnet") {
		t.Fatalf("the message must name what is missing: %v", err)
	}
	assertNothingHappened(t, h, f)
}

func TestASubnetOutsideTheWatchedGroupsIsRefused(t *testing.T) {
	// A VM the hub creates must be a VM the hub can show. One in an unwatched
	// group would never appear in the inventory or the costs, and would be
	// remembered only by this table.
	f := newProvisionFake(t)
	h, cancel := provisionHub(t, f, t.TempDir(), true)
	defer cancel()
	if err := azure.SaveProvisionConfig(h.st, azure.ProvisionConfig{
		SubnetID: strings.Replace(provisionSubnet, "/resourceGroups/RG/", "/resourceGroups/other-rg/", 1),
		HubURL:   "https://monitor.example.net", Size: "Standard_B1s",
		Image: "Canonical:ubuntu-24_04-lts:server:latest", AdminUser: "azureuser", SSHKey: "ssh-ed25519 AAAA…",
	}); err != nil {
		t.Fatal(err)
	}

	_, err := h.StartProvision("vm-test")
	if err == nil || !strings.Contains(err.Error(), "other-rg") {
		t.Fatalf("err = %v, want it to name the group", err)
	}
	assertNothingHappened(t, h, f)
}

func TestAnInvalidNameIsRefusedWithTheRule(t *testing.T) {
	f := newProvisionFake(t)
	h, cancel := provisionHub(t, f, t.TempDir(), true)
	defer cancel()
	_, err := h.StartProvision("vm test-")
	if err == nil || !strings.Contains(err.Error(), "64 characters") {
		t.Fatalf("err = %v, want the rule spelled out", err)
	}
	assertNothingHappened(t, h, f)
}

func TestASecondProvisionOfTheSameNameIsRefused(t *testing.T) {
	f := newProvisionFake(t)
	f.blockVM = make(chan struct{})
	h, cancel := provisionHub(t, f, t.TempDir(), true)
	defer cancel()
	defer close(f.blockVM)

	if _, err := h.StartProvision("vm-test"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.StartProvision("vm-test"); !errors.Is(err, ErrProvisionInFlight) {
		t.Fatalf("err = %v, want ErrProvisionInFlight", err)
	}
	// And the refused one created no second host row.
	hosts, _ := h.st.ListHosts()
	if len(hosts) != 1 {
		t.Fatalf("hosts = %d, want 1", len(hosts))
	}
}

func TestAMidRunFailureKeepsTheNICAndPublishesIt(t *testing.T) {
	f := newProvisionFake(t)
	f.failVM = http.StatusForbidden
	h, cancel := provisionHub(t, f, t.TempDir(), true)
	defer cancel()

	events, stop := h.bus.Subscribe()
	defer stop()

	id, err := h.StartProvision("vm-test")
	if err != nil {
		t.Fatal(err)
	}
	p := waitProvision(t, h, id)
	if p.Status != "failed" {
		t.Fatalf("status = %s", p.Status)
	}
	if len(p.Resources) != 1 || p.Resources[0].Kind != "nic" {
		t.Fatalf("the NIC must stay in the record: %+v", p.Resources)
	}
	if p.Resources[0].DeletedAt != nil {
		t.Fatal("the hub deleted the leftover on its own")
	}
	// The host row stays too: it is the record of a machine that was meant to
	// exist, and it is visible as one that never connected.
	if hosts, _ := h.st.ListHosts(); len(hosts) != 1 {
		t.Fatalf("hosts = %d, want the row to survive the failure", len(hosts))
	}
	// A failure nobody published was a lot 3 defect; it is not repeated here.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case e := <-events:
			if e.Type == "azure_provision" {
				return
			}
		case <-deadline:
			t.Fatal("no azure_provision event was published for the failure")
		}
	}
}

func TestAProvisionInFlightWhenTheHubDiesIsInterruptedAtTheNextStart(t *testing.T) {
	// A real restart: the first hub is stopped, its store closed, and a second
	// hub opens the same directory. Calling the interrupt on the same live hub
	// would prove less — the goroutine would still be running behind it.
	dir := t.TempDir()
	f := newProvisionFake(t)
	f.blockVM = make(chan struct{})

	h1, cancel1 := provisionHub(t, f, dir, true)
	id, err := h1.StartProvision("vm-test")
	if err != nil {
		t.Fatal(err)
	}
	// Wait until the NIC exists, so there is a leftover to preserve.
	waitFor(t, func() bool {
		ps, _ := h1.ListProvisions(10)
		return len(ps) == 1 && len(ps[0].Resources) == 1
	}, "the NIC to be recorded")
	cancel1()        // the hub's lifetime ends; the run goroutine unwinds
	close(f.blockVM) // release the blocked VM PUT so nothing is stuck
	h1.Close()

	h2, cancel2 := provisionHub(t, f, dir, true)
	defer cancel2()
	h2.interruptProvisions()

	ps, err := h2.ListProvisions(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || ps[0].ID != id || ps[0].Status != "interrupted" {
		t.Fatalf("provisions = %+v, want one interrupted", ps)
	}
	if len(ps[0].Resources) != 1 || ps[0].Resources[0].ARMID == "" {
		t.Fatalf("the leftover must be named after a restart: %+v", ps[0].Resources)
	}
}

func TestTheTokenNeverLeavesTheHub(t *testing.T) {
	// §5 as a test. The token reaches exactly one place — the cloud-init
	// document inside customData — and the test takes it from there, which is
	// the only way to get it: the store keeps a hash, never the token itself.
	f := newProvisionFake(t)
	h, cancel := provisionHub(t, f, t.TempDir(), true)
	defer cancel()

	id, err := h.StartProvision("vm-test")
	if err != nil {
		t.Fatal(err)
	}
	p := waitProvision(t, h, id)

	token := tokenFromCustomData(t, f)
	if token == "" {
		t.Fatal("no token reached the machine, so the VM could never have connected")
	}
	// It is base64 inside customData, so it must not appear in clear text
	// anywhere in what went over the wire either.
	for _, body := range strings.Split(f.allBodies(), "\n") {
		if strings.Contains(body, token) {
			t.Fatalf("the token is in a request body in clear text:\n%s", body)
		}
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), token) {
		t.Fatalf("the token is in the provision record:\n%s", raw)
	}
	hosts, _ := h.st.ListHosts()
	rawHosts, _ := json.Marshal(hosts)
	if strings.Contains(string(rawHosts), token) {
		t.Fatalf("the token is in the host record:\n%s", rawHosts)
	}
}

// tokenFromCustomData pulls the token back out of the cloud-init document the
// VM PUT carried. Going through the wire rather than through the store is the
// point: the store never had it.
func tokenFromCustomData(t *testing.T, f *provisionFake) string {
	t.Helper()
	for _, body := range strings.Split(f.allBodies(), "\n") {
		var v struct {
			Properties struct {
				OSProfile struct {
					CustomData string `json:"customData"`
				} `json:"osProfile"`
			} `json:"properties"`
		}
		if json.Unmarshal([]byte(body), &v) != nil || v.Properties.OSProfile.CustomData == "" {
			continue
		}
		doc, err := base64.StdEncoding.DecodeString(v.Properties.OSProfile.CustomData)
		if err != nil {
			t.Fatalf("customData is not base64: %v", err)
		}
		_, rest, ok := strings.Cut(string(doc), "content: ")
		if !ok {
			t.Fatalf("no token in the document:\n%s", doc)
		}
		line, _, _ := strings.Cut(rest, "\n")
		return strings.Trim(strings.TrimSpace(line), `"`)
	}
	return ""
}

// assertNothingHappened is the shape every refusal shares: no provision row,
// no host row, no call to Azure.
func assertNothingHappened(t *testing.T, h *Hub, f *provisionFake) {
	t.Helper()
	if ps, _ := h.ListProvisions(10); len(ps) != 0 {
		t.Fatalf("a provision row was written: %+v", ps)
	}
	if hosts, _ := h.st.ListHosts(); len(hosts) != 0 {
		t.Fatalf("a host row was written: %+v", hosts)
	}
	for _, r := range f.requests() {
		if !strings.HasPrefix(r, "GET") {
			t.Fatalf("Azure was written to: %s", r)
		}
	}
}

// --- task 10: deleting ----------------------------------------------------

// deletingFake serves reads with the hub's own tag and records every DELETE.
type deletingFake struct {
	*provisionFake
	untagged bool
}

func newDeletingFake(t *testing.T, untagged bool) *deletingFake {
	f := &deletingFake{provisionFake: &provisionFake{}, untagged: untagged}
	tag := "ServersMonitor"
	if untagged {
		tag = "somebody-else"
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/oauth2/") {
			io.WriteString(w, `{"token_type":"Bearer","expires_in":3599,"access_token":"tok"}`)
			return
		}
		f.mu.Lock()
		f.reqs = append(f.reqs, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/virtualNetworks/vnet-sandbox"):
			io.WriteString(w, `{"location":"westeurope"}`)
		case r.Method == http.MethodGet:
			fmt.Fprintf(w, `{"tags":{"createdBy":%q}}`, tag)
		default:
			fmt.Fprintf(w, `{"id":%q}`, r.URL.Path)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *deletingFake) deletes() []string {
	var out []string
	for _, r := range f.requests() {
		if strings.HasPrefix(r, "DELETE ") {
			out = append(out, strings.TrimPrefix(r, "DELETE "))
		}
	}
	return out
}

func provisionThen(t *testing.T, f *deletingFake) (*Hub, int64, context.CancelFunc) {
	t.Helper()
	h, cancel := provisionHub(t, f.provisionFake, t.TempDir(), true)
	id, err := h.StartProvision("vm-test")
	if err != nil {
		t.Fatal(err)
	}
	if p := waitProvision(t, h, id); p.Status != "succeeded" {
		t.Fatalf("setup: status = %s, error = %s", p.Status, p.Error)
	}
	return h, id, cancel
}

func TestDeletingAProvisionNeedsTheNameTypedRight(t *testing.T) {
	f := newDeletingFake(t, false)
	h, id, cancel := provisionThen(t, f)
	defer cancel()

	before := len(f.deletes())
	if err := h.DeleteProvision(id, "vm-tset"); !errors.Is(err, ErrWrongName) {
		t.Fatalf("err = %v, want ErrWrongName", err)
	}
	if got := len(f.deletes()); got != before {
		t.Fatalf("a mistyped confirmation reached Azure: %v", f.deletes())
	}
	// And the resources are still recorded as present.
	p, _ := h.st.AzureProvision(id)
	for _, r := range p.Resources {
		if r.DeletedAt != nil {
			t.Fatalf("%s was marked deleted", r.ARMID)
		}
	}
}

func TestDeletingAProvisionRemovesTheVMThenTheNIC(t *testing.T) {
	f := newDeletingFake(t, false)
	h, id, cancel := provisionThen(t, f)
	defer cancel()

	if err := h.DeleteProvision(id, "vm-test"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		p, _ := h.st.AzureProvision(id)
		for _, r := range p.Resources {
			if r.DeletedAt == nil {
				return false
			}
		}
		return len(p.Resources) == 2
	}, "both resources to be recorded as deleted")

	got := f.deletes()
	if len(got) != 2 || !strings.Contains(got[0], "/virtualMachines/") || !strings.Contains(got[1], "/networkInterfaces/") {
		t.Fatalf("deletes = %v, want the VM then the NIC", got)
	}
	// The host row survives: its history is the record of a machine that
	// existed, and removing it is a separate, explicit act.
	if hosts, _ := h.st.ListHosts(); len(hosts) != 1 {
		t.Fatalf("hosts = %d, want the row to survive the deletion", len(hosts))
	}
}

func TestDeletingSomethingTheHubDidNotCreateIsRefusedAndRecorded(t *testing.T) {
	f := newDeletingFake(t, true) // Azure reports somebody else's tag
	h, id, cancel := provisionThen(t, f)
	defer cancel()

	if err := h.DeleteProvision(id, "vm-test"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		p, _ := h.st.AzureProvision(id)
		return p.DeleteError != ""
	}, "the refusal to be recorded")

	if got := f.deletes(); len(got) != 0 {
		t.Fatalf("something was deleted anyway: %v", got)
	}
	p, _ := h.st.AzureProvision(id)
	if !strings.Contains(p.DeleteError, "createdBy") {
		t.Fatalf("delete_error = %q, want it to name the tag", p.DeleteError)
	}
	// And the provision itself still reads as the success it was.
	if p.Status != "succeeded" {
		t.Fatalf("a failed deletion rewrote how the provision ended: %s", p.Status)
	}
}

func TestDeletingAnUnknownProvisionIsRefused(t *testing.T) {
	f := newDeletingFake(t, false)
	h, _, cancel := provisionThen(t, f)
	defer cancel()
	if err := h.DeleteProvision(9999, "vm-test"); !errors.Is(err, store.ErrNoSuchProvision) {
		t.Fatalf("err = %v, want ErrNoSuchProvision", err)
	}
}
