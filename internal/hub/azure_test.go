package hub

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/azure"
	"github.com/vincentlauriat/serversmonitor/internal/hub/config"
)

// azureFake serves a token, a catalogue, the two enrichment passes and a cost
// query. failAfter makes every call fail once that many have succeeded.
type azureFake struct {
	srv       *httptest.Server
	calls     int
	failAfter int
	sites     []string
}

func newAzureFake(t *testing.T, sites ...string) *azureFake {
	f := &azureFake{failAfter: -1, sites: sites}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls++
		if f.failAfter >= 0 && f.calls > f.failAfter {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"error":{"code":"AuthorizationFailed","message":"does not have authorization"}}`)
			return
		}
		switch {
		case strings.Contains(r.URL.Path, "/oauth2/"):
			io.WriteString(w, `{"token_type":"Bearer","expires_in":3599,"access_token":"tok"}`)
		case strings.HasSuffix(r.URL.Path, "/resources"):
			var parts []string
			for _, s := range f.sites {
				parts = append(parts, fmt.Sprintf(
					`{"id":"/subscriptions/SUB/resourceGroups/RG/providers/Microsoft.Web/sites/%s","name":%q,"type":"Microsoft.Web/sites","location":"westeurope","properties":null}`, s, s))
			}
			fmt.Fprintf(w, `{"value":[%s]}`, strings.Join(parts, ","))
		case strings.Contains(r.URL.Path, "/Microsoft.Web/sites"):
			var parts []string
			for _, s := range f.sites {
				parts = append(parts, fmt.Sprintf(
					`{"id":"/subscriptions/SUB/resourceGroups/RG/providers/Microsoft.Web/sites/%s","name":%q,"properties":{"state":"Running"}}`, s, s))
			}
			fmt.Fprintf(w, `{"value":[%s]}`, strings.Join(parts, ","))
		case strings.Contains(r.URL.Path, "/Microsoft.Web/serverfarms"):
			io.WriteString(w, `{"value":[]}`)
		// The real path is ".../providers/Microsoft.CostManagement/query", so the
		// character before CostManagement is a dot, not a slash. Matching
		// "/CostManagement/query" answers 404 and the sync reads as broken.
		case strings.Contains(r.URL.Path, "CostManagement/query"):
			io.WriteString(w, `{"properties":{"columns":[{"name":"Cost"},{"name":"ResourceId"},{"name":"Currency"}],
			 "rows":[[3.5,"/subscriptions/sub/resourcegroups/rg/providers/microsoft.web/sites/a","EUR"],
			         [9.0,"/subscriptions/sub/resourcegroups/rg/providers/microsoft.insights/components/long-gone","EUR"]]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func azureHub(t *testing.T, f *azureFake) *Hub {
	t.Helper()
	h, err := New(config.Config{DataDir: t.TempDir()}, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	if err := azure.SaveConfig(h.st, azure.Config{Mode: "client_secret", TenantID: "t",
		ClientID: "c", ClientSecret: "s", SubscriptionID: "SUB", ResourceGroups: []string{"RG"},
		InventoryEveryMin: 15, CostEveryMin: 60}); err != nil {
		t.Fatal(err)
	}
	h.azureBase = f.srv.URL
	h.ReloadAzure()
	return h
}

func TestAzureSyncStoresInventoryAndState(t *testing.T) {
	f := newAzureFake(t, "a", "b")
	h := azureHub(t, f)
	h.syncAzureInventory(context.Background())

	rs, err := h.st.ListAzureResources()
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 {
		t.Fatalf("got %d resources", len(rs))
	}
	if rs[0].State == nil || *rs[0].State != "Running" {
		t.Fatalf("state = %v, the enrichment pass must have run", rs[0].State)
	}
	st, _ := h.st.AzureSyncState()
	if !st["inventory"].OK {
		t.Fatalf("sync state = %+v", st["inventory"])
	}
}

func TestAFailedSyncKeepsTheLastGoodInventory(t *testing.T) {
	// The property this whole lot exists to protect: "I cannot see Azure" must
	// never render as "the sandbox is empty".
	f := newAzureFake(t, "a", "b")
	h := azureHub(t, f)
	h.syncAzureInventory(context.Background())
	if rs, _ := h.st.ListAzureResources(); len(rs) != 2 {
		t.Fatalf("fixture did not land: %d", len(rs))
	}

	f.failAfter = 0 // everything fails from now on
	f.calls = 0
	h.syncAzureInventory(context.Background())

	rs, _ := h.st.ListAzureResources()
	if len(rs) != 2 {
		t.Fatalf("a failed sync must change nothing, got %d rows", len(rs))
	}
	for _, r := range rs {
		if r.DeletedAt != nil {
			t.Fatalf("%s was marked deleted by a failed sync", r.Name)
		}
	}
	st, _ := h.st.AzureSyncState()
	if st["inventory"].OK {
		t.Fatal("the sync failed and the state must say so")
	}
	if !strings.Contains(st["inventory"].Message, "authorization") {
		t.Fatalf("Azure's own message must be kept: %q", st["inventory"].Message)
	}
}

func TestAPartialSyncIsAFailedSync(t *testing.T) {
	// Token, catalogue, then the enrichment pass fails. If that counted as a
	// success the sweep would mark every resource deleted.
	f := newAzureFake(t, "a", "b")
	h := azureHub(t, f)
	h.syncAzureInventory(context.Background())
	f.calls = 0
	f.failAfter = 2 // token and catalogue succeed, enrichment does not
	h.syncAzureInventory(context.Background())
	rs, _ := h.st.ListAzureResources()
	for _, r := range rs {
		if r.DeletedAt != nil {
			t.Fatalf("%s was deleted by a half-read sync", r.Name)
		}
	}
	if st, _ := h.st.AzureSyncState(); st["inventory"].OK {
		t.Fatal("a half-read sync is a failed sync")
	}
}

func TestAResourceThatVanishesIsSoftDeleted(t *testing.T) {
	f := newAzureFake(t, "a", "b")
	h := azureHub(t, f)
	h.syncAzureInventory(context.Background())
	f.sites = []string{"a"}
	h.syncAzureInventory(context.Background())
	rs, _ := h.st.ListAzureResources()
	var deleted int
	for _, r := range rs {
		if r.DeletedAt != nil {
			deleted++
			if r.Name != "b" {
				t.Fatalf("%s should not be deleted", r.Name)
			}
		}
	}
	if deleted != 1 || len(rs) != 2 {
		t.Fatalf("rows = %d, deleted = %d", len(rs), deleted)
	}
}

func TestCostRowsSurviveWithoutAResource(t *testing.T) {
	// A resource deleted mid-month still cost money. Dropping its row would
	// understate the bill.
	f := newAzureFake(t, "a")
	h := azureHub(t, f)
	h.syncAzureCosts(context.Background())
	cs, err := h.st.ListAzureCosts(currentPeriod(time.Now().UTC()))
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 {
		t.Fatalf("got %d cost rows, want 2 including the one with no resource", len(cs))
	}
	var orphan bool
	for _, c := range cs {
		if strings.Contains(c.ResourceID, "long-gone") {
			orphan = true
			if c.Amount != 9.0 {
				t.Fatalf("orphan amount = %v", c.Amount)
			}
		}
	}
	if !orphan {
		t.Fatal("the orphan cost row must be stored")
	}
}

func TestTestAzureReturnsAzuresOwnWords(t *testing.T) {
	f := newAzureFake(t, "a")
	h := azureHub(t, f)
	if err := h.TestAzure(context.Background()); err != nil {
		t.Fatalf("a working configuration must test clean: %v", err)
	}
	f.failAfter = 1 // the token works, the catalogue does not
	f.calls = 0
	err := h.TestAzure(context.Background())
	if err == nil || !strings.Contains(err.Error(), "authorization") {
		t.Fatalf("err = %v", err)
	}
}

func TestAzureOffDoesNothing(t *testing.T) {
	h, err := New(config.Config{DataDir: t.TempDir()}, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	h.ReloadAzure()
	h.syncAzureInventory(context.Background())
	h.syncAzureCosts(context.Background())
	if rs, _ := h.st.ListAzureResources(); len(rs) != 0 {
		t.Fatal("azure is off; nothing may be written")
	}
	if st, _ := h.st.AzureSyncState(); len(st) != 0 {
		t.Fatal("azure is off; not even a sync row")
	}
	if err := h.TestAzure(context.Background()); err == nil {
		t.Fatal("testing a disabled integration is an error, not a success")
	}
}

func TestChangingTheScopePurgesTheOldOne(t *testing.T) {
	f := newAzureFake(t, "a", "b")
	h := azureHub(t, f)
	h.syncAzureInventory(context.Background())
	// A different resource group: what was collected for the old one would
	// otherwise linger on the page forever, never swept because never synced.
	azure.SaveConfig(h.st, azure.Config{Mode: "client_secret", TenantID: "t", ClientID: "c",
		ClientSecret: "s", SubscriptionID: "SUB", ResourceGroups: []string{"OTHER"},
		InventoryEveryMin: 15, CostEveryMin: 60})
	h.ReloadAzure()
	if rs, _ := h.st.ListAzureResources(); len(rs) != 0 {
		t.Fatalf("changing the scope must purge, %d rows left", len(rs))
	}
}

func TestCurrentPeriod(t *testing.T) {
	if got := currentPeriod(time.Date(2026, 9, 18, 3, 0, 0, 0, time.UTC)); got != "2026-09" {
		t.Fatalf("period = %q", got)
	}
	if got := currentPeriod(time.Date(2026, 12, 31, 23, 59, 0, 0, time.UTC)); got != "2026-12" {
		t.Fatalf("period = %q", got)
	}
}

// Saving a configuration must sync now, not at the next tick. Without this the
// page shows "no inventory has run yet" with an empty table for a quarter of an
// hour after the user configured Azure — and longer, because the tickers were
// built at boot with the one-hour cadence an off integration uses, and nothing
// resets them until they first fire.
func TestSavingAConfigurationSyncsWithoutWaitingForTheTicker(t *testing.T) {
	f := newAzureFake(t, "a", "b")
	h, err := New(config.Config{DataDir: t.TempDir()}, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	h.azureBase = f.srv.URL

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx) //nolint:errcheck // Run returns ctx.Err() on cancel

	// Azure is off at boot, so the tickers are on the one-hour fallback.
	if err := azure.SaveConfig(h.st, azure.Config{Mode: "client_secret", TenantID: "t",
		ClientID: "c", ClientSecret: "s", SubscriptionID: "SUB", ResourceGroups: []string{"RG"},
		InventoryEveryMin: 15, CostEveryMin: 60}); err != nil {
		t.Fatal(err)
	}
	h.ReloadAzure()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st, _ := h.st.AzureSyncState(); st["inventory"].OK {
			if rs, _ := h.st.ListAzureResources(); len(rs) == 2 {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	st, _ := h.st.AzureSyncState()
	rs, _ := h.st.ListAzureResources()
	t.Fatalf("no inventory within 5s after the save: sync = %+v, rows = %d", st, len(rs))
}

// A failed sync is the outcome the open pages most need to hear about: an Azure
// read can take minutes to time out, and a page told only about successes shows
// "no inventory has run yet" for the whole of it.
func TestAFailedSyncIsBroadcastToo(t *testing.T) {
	f := newAzureFake(t, "a")
	h := azureHub(t, f)
	events, stop := h.bus.Subscribe()
	defer stop()

	f.failAfter = 0
	h.syncAzureInventory(context.Background())

	for {
		select {
		case e := <-events:
			if e.Type == "azure" {
				return
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no azure event after a failed sync")
		}
	}
}
