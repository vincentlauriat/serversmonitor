package azure

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// armRecorder remembers every request, method included. The method matters:
// a rollback would DELETE the very id the create PUT used, so a test that only
// looked at paths would pass against the bug it exists to catch.
type armRecorder struct {
	mu   sync.Mutex
	reqs []recorded
	// fail, when set for a path suffix, answers that status instead.
	fail map[string]int
	srv  *httptest.Server
}

type recorded struct {
	Method string
	Path   string
	Body   map[string]any
}

func newARM(t *testing.T) *armRecorder {
	a := &armRecorder{fail: map[string]int{}}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if len(raw) > 0 {
			json.Unmarshal(raw, &body)
		}
		a.mu.Lock()
		a.reqs = append(a.reqs, recorded{Method: r.Method, Path: r.URL.Path, Body: body})
		var status int
		for suffix, code := range a.fail {
			if strings.HasSuffix(r.URL.Path, suffix) {
				status = code
			}
		}
		a.mu.Unlock()

		if status != 0 {
			w.WriteHeader(status)
			fmt.Fprint(w, `{"error":{"code":"AuthorizationFailed","message":"no Virtual Machine Contributor"}}`)
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "/oauth2/"):
			io.WriteString(w, `{"token_type":"Bearer","expires_in":3599,"access_token":"tok"}`)
		case strings.HasSuffix(r.URL.Path, "/virtualNetworks/vnet-sandbox"):
			io.WriteString(w, `{"location":"westeurope"}`)
		default:
			io.WriteString(w, `{"id":"`+r.URL.Path+`","properties":{"provisioningState":"Succeeded"}}`)
		}
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *armRecorder) all() []recorded {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]recorded, 0, len(a.reqs))
	for _, r := range a.reqs {
		if strings.Contains(r.Path, "/oauth2/") {
			continue
		}
		out = append(out, r)
	}
	return out
}

func (a *armRecorder) client() *Client {
	return NewClient(staticSource("tok"), Options{Base: a.srv.URL,
		Sleep: func(ctx context.Context, _ time.Duration) bool { return ctx.Err() == nil }})
}

func createReq() CreateRequest {
	return CreateRequest{Name: "vm-test", HostID: 7, Token: sentinelToken}
}

// collect returns an onCreated that records what landed.
func collect(out *[]Created) func(Created) error {
	return func(c Created) error { *out = append(*out, c); return nil }
}

func TestCreateVMPutsTheNICThenTheVM(t *testing.T) {
	a := newARM(t)
	var created []Created
	if err := CreateVM(context.Background(), a.client(), readyConfig(), createReq(), collect(&created)); err != nil {
		t.Fatal(err)
	}

	reqs := a.all()
	// The VNet is read first: both bodies need a location, and the subnet has
	// none of its own.
	if len(reqs) < 3 {
		t.Fatalf("requests = %+v", reqs)
	}
	if reqs[0].Method != http.MethodGet || !strings.HasSuffix(reqs[0].Path, "/virtualNetworks/vnet-sandbox") {
		t.Fatalf("first request = %s %s, want a GET on the VNet", reqs[0].Method, reqs[0].Path)
	}
	if reqs[1].Method != http.MethodPut || !strings.Contains(reqs[1].Path, "/networkInterfaces/") {
		t.Fatalf("second request = %s %s, want the NIC PUT", reqs[1].Method, reqs[1].Path)
	}
	if reqs[2].Method != http.MethodPut || !strings.Contains(reqs[2].Path, "/virtualMachines/") {
		t.Fatalf("third request = %s %s, want the VM PUT", reqs[2].Method, reqs[2].Path)
	}
	if len(created) != 2 || created[0].Kind != "nic" || created[1].Kind != "vm" {
		t.Fatalf("created = %+v", created)
	}
	// Both in the subnet's own resource group, which is where the hub can see
	// them: a VM the hub creates must be a VM the hub can show.
	for _, r := range reqs[1:] {
		if !strings.Contains(r.Path, "/resourceGroups/rg-dev-vincent-sandbox/") {
			t.Fatalf("%s went to %s", r.Method, r.Path)
		}
	}
}

func TestEveryCreatedResourceCarriesTheTag(t *testing.T) {
	// Lot 6 finds orphans by this tag, and the delete path refuses anything
	// without it. A resource created untagged is one nothing can clean up.
	a := newARM(t)
	if err := CreateVM(context.Background(), a.client(), readyConfig(), createReq(), collect(new([]Created))); err != nil {
		t.Fatal(err)
	}
	for _, r := range a.all() {
		if r.Method != http.MethodPut {
			continue
		}
		tags, _ := r.Body["tags"].(map[string]any)
		if tags["createdBy"] != "ServersMonitor" {
			t.Fatalf("%s has tags %v", r.Path, tags)
		}
		if strings.Contains(r.Path, "/virtualMachines/") && tags["smHostId"] != "7" {
			t.Fatalf("the VM must carry its host id, got %v", tags)
		}
	}
}

func TestTheNICHasNoPublicIPAndJoinsTheConfiguredSubnet(t *testing.T) {
	// Not an omission: Virtual Machine Contributor cannot create a public IP,
	// and the agent dials out so the VM does not need one. A body copied from
	// a tutorial would carry one and fail with an authorization error that
	// reads like a credential problem.
	a := newARM(t)
	if err := CreateVM(context.Background(), a.client(), readyConfig(), createReq(), collect(new([]Created))); err != nil {
		t.Fatal(err)
	}
	var nic recorded
	for _, r := range a.all() {
		if strings.Contains(r.Path, "/networkInterfaces/") {
			nic = r
		}
	}
	raw, _ := json.Marshal(nic.Body)
	if strings.Contains(strings.ToLower(string(raw)), "publicip") {
		t.Fatalf("the NIC asks for a public IP:\n%s", raw)
	}
	if !strings.Contains(string(raw), goodSubnet) {
		t.Fatalf("the NIC must join the configured subnet verbatim:\n%s", raw)
	}
}

func TestTheVMCarriesCloudInitAndRefusesPasswords(t *testing.T) {
	a := newARM(t)
	if err := CreateVM(context.Background(), a.client(), readyConfig(), createReq(), collect(new([]Created))); err != nil {
		t.Fatal(err)
	}
	var vm recorded
	for _, r := range a.all() {
		if strings.Contains(r.Path, "/virtualMachines/") {
			vm = r
		}
	}
	props, _ := vm.Body["properties"].(map[string]any)
	os, _ := props["osProfile"].(map[string]any)
	data, _ := os["customData"].(string)
	doc, err := base64.StdEncoding.DecodeString(data)
	if err != nil || !strings.HasPrefix(string(doc), "#cloud-config") {
		t.Fatalf("customData = %q (%v)", data, err)
	}
	lin, _ := os["linuxConfiguration"].(map[string]any)
	if lin["disablePasswordAuthentication"] != true {
		t.Fatalf("password login must be off: %v", lin)
	}
	// The OS disk cannot outlive the VM. See the comment in provision.go: it
	// is why the disk is not a third recorded resource.
	storage, _ := props["storageProfile"].(map[string]any)
	disk, _ := storage["osDisk"].(map[string]any)
	if disk["deleteOption"] != "Delete" {
		t.Fatalf("the OS disk must be deleted with the VM: %v", disk)
	}
}

func TestAFailedVMPutLeavesTheNICAndDeletesNothing(t *testing.T) {
	// §4 as an executable statement. A rollback here would be the hub's first
	// automatic delete, placed in the least exercised path in the lot, at the
	// moment its view of what it created is least trustworthy.
	a := newARM(t)
	a.fail["/virtualMachines/vm-test"] = http.StatusForbidden
	var created []Created
	err := CreateVM(context.Background(), a.client(), readyConfig(), createReq(), collect(&created))
	if err == nil {
		t.Fatal("want an error")
	}
	if StatusOf(err) != http.StatusForbidden {
		t.Fatalf("StatusOf = %d, want 403", StatusOf(err))
	}
	if len(created) != 1 || created[0].Kind != "nic" {
		t.Fatalf("created = %+v, want just the NIC", created)
	}
	for _, r := range a.all() {
		if r.Method == http.MethodDelete {
			t.Fatalf("the hub deleted something on its own: %s %s", r.Method, r.Path)
		}
	}
}

func TestAFailedVNetReadStopsBeforeTheFirstPut(t *testing.T) {
	// Without the location both PUTs are unsendable, and defaulting one would
	// create the NIC in a region the subnet is not in — a leftover produced by
	// a guess.
	a := newARM(t)
	a.fail["/virtualNetworks/vnet-sandbox"] = http.StatusForbidden
	var created []Created
	if err := CreateVM(context.Background(), a.client(), readyConfig(), createReq(), collect(&created)); err == nil {
		t.Fatal("want an error")
	}
	if len(created) != 0 {
		t.Fatalf("created = %+v, want nothing", created)
	}
	for _, r := range a.all() {
		if r.Method != http.MethodGet {
			t.Fatalf("something was written before the location was known: %s %s", r.Method, r.Path)
		}
	}
}

func TestAnUnrecordableResourceStopsTheRun(t *testing.T) {
	// If the hub cannot write down what it just created, it must not create
	// more: the second resource would be an orphan nothing could name.
	a := newARM(t)
	err := CreateVM(context.Background(), a.client(), readyConfig(), createReq(),
		func(Created) error { return fmt.Errorf("disk full") })
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v", err)
	}
	for _, r := range a.all() {
		if strings.Contains(r.Path, "/virtualMachines/") {
			t.Fatal("the VM was created after the NIC could not be recorded")
		}
	}
}

func TestAnUnconfiguredProvisionIsRefusedBeforeAnyCall(t *testing.T) {
	a := newARM(t)
	if err := CreateVM(context.Background(), a.client(), ProvisionConfig{}, createReq(),
		collect(new([]Created))); err == nil {
		t.Fatal("want an error")
	}
	if n := len(a.all()); n != 0 {
		t.Fatalf("%d requests were sent", n)
	}
}
