package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

type fakeAzurer struct {
	reloads int
	tested  int
	err     error
}

func (f *fakeAzurer) ReloadAzure() { f.reloads++ }

func (f *fakeAzurer) TestAzure(_ context.Context) error {
	f.tested++
	return f.err
}

func newAzureRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	r.setupAndLogin(t)
	return r
}

func (r *rig) azurer(t *testing.T) *fakeAzurer {
	t.Helper()
	f, ok := r.azure.(*fakeAzurer)
	if !ok {
		t.Fatalf("rig azurer is %T", r.azure)
	}
	return f
}

func (r *rig) azureView(t *testing.T, query string) azureView {
	t.Helper()
	resp, data := r.do(t, "GET", "/api/v1/azure"+query, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET azure = %d %s", resp.StatusCode, data)
	}
	var v azureView
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("decode: %v — %s", err, data)
	}
	return v
}

func ptr[T any](v T) *T { return &v }

const (
	vmID   = "/subscriptions/s1/resourcegroups/rg-sandbox/providers/microsoft.compute/virtualmachines/vm-a"
	goneID = "/subscriptions/s1/resourcegroups/rg-sandbox/providers/microsoft.compute/disks/disk-gone"
)

func seedInventory(t *testing.T, r *rig) {
	t.Helper()
	err := r.st.ReplaceAzureInventory([]string{"rg-sandbox"}, []store.AzureResource{
		{ID: vmID, Name: "vm-a", Type: "Microsoft.Compute/virtualMachines",
			ResourceGroup: "rg-sandbox", Location: "westeurope", State: ptr("running"),
			Tags: map[string]string{"env": "dev"}},
	}, r.now)
	if err != nil {
		t.Fatal(err)
	}
}

func TestAzureViewJoinsCostOntoInventory(t *testing.T) {
	r := newAzureRig(t)
	seedInventory(t, r)
	if err := r.st.UpsertAzureCosts([]store.AzureCost{
		{ResourceID: vmID, Period: "2026-09", Amount: 12.5, Currency: "EUR", AsOf: r.now},
	}); err != nil {
		t.Fatal(err)
	}
	v := r.azureView(t, "?period=2026-09")
	if len(v.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(v.Rows))
	}
	row := v.Rows[0]
	if row.Cost == nil || *row.Cost != 12.5 || row.Currency != "EUR" {
		t.Fatalf("cost = %v %q, want 12.5 EUR", row.Cost, row.Currency)
	}
	if row.State == nil || *row.State != "running" {
		t.Fatalf("state = %v, want running", row.State)
	}
}

// The invariant, at the API boundary: a resource Azure has not billed is not a
// resource that cost zero. Nothing else in the stack can make this distinction
// once the handler has flattened it to a number.
func TestResourceWithNoCostRowReadsNullNotZero(t *testing.T) {
	r := newAzureRig(t)
	seedInventory(t, r)
	resp, data := r.do(t, "GET", "/api/v1/azure?period=2026-09", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET azure = %d %s", resp.StatusCode, data)
	}
	var raw struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw.Rows) != 1 {
		t.Fatalf("rows = %d", len(raw.Rows))
	}
	// Asserted against the JSON rather than the Go struct: the browser is what
	// has to see null, and a float64 zero and a null decode the same into an
	// untyped map only if the key is absent.
	cost, present := raw.Rows[0]["cost"]
	if !present {
		t.Fatal("cost key absent; the front cannot tell missing from zero")
	}
	if cost != nil {
		t.Fatalf("cost = %#v, want null", cost)
	}
}

// A cost row whose resource no longer exists is money that was spent. Dropping
// it makes the total disagree with the Azure bill.
func TestCostWithoutInventoryStillAppears(t *testing.T) {
	r := newAzureRig(t)
	seedInventory(t, r)
	if err := r.st.UpsertAzureCosts([]store.AzureCost{
		{ResourceID: vmID, Period: "2026-09", Amount: 10, Currency: "EUR", AsOf: r.now},
		{ResourceID: goneID, Period: "2026-09", Amount: 4, Currency: "EUR", AsOf: r.now},
	}); err != nil {
		t.Fatal(err)
	}
	v := r.azureView(t, "?period=2026-09")
	var orphan *azureRow
	for i := range v.Rows {
		if v.Rows[i].ID == goneID {
			orphan = &v.Rows[i]
		}
	}
	if orphan == nil {
		t.Fatalf("the orphan cost row is missing; rows = %d", len(v.Rows))
	}
	if !orphan.Deleted {
		t.Error("an orphan cost row must be marked deleted")
	}
	if orphan.Name != "disk-gone" {
		t.Errorf("name = %q, want disk-gone", orphan.Name)
	}
	if orphan.Type != "microsoft.compute/disks" {
		t.Errorf("type = %q", orphan.Type)
	}
	if orphan.Group != "rg-sandbox" {
		t.Errorf("group = %q", orphan.Group)
	}
	if len(v.Totals) != 1 || v.Totals[0].Spent != 14 {
		t.Errorf("totals = %+v, want one of 14", v.Totals)
	}
}

// Adding euros to dollars produces a number that is wrong in a way nobody
// notices, so the API never produces it.
func TestTotalsAreOnePerCurrencyNeverSummed(t *testing.T) {
	r := newAzureRig(t)
	if err := r.st.UpsertAzureCosts([]store.AzureCost{
		{ResourceID: "a", Period: "2026-09", Amount: 10, Currency: "EUR", AsOf: r.now},
		{ResourceID: "b", Period: "2026-09", Amount: 3, Currency: "USD", AsOf: r.now},
		{ResourceID: "c", Period: "2026-09", Amount: 2, Currency: "EUR", AsOf: r.now},
	}); err != nil {
		t.Fatal(err)
	}
	v := r.azureView(t, "?period=2026-09")
	if len(v.Totals) != 2 {
		t.Fatalf("totals = %+v, want two", v.Totals)
	}
	got := map[string]float64{}
	for _, tt := range v.Totals {
		got[tt.Currency] = tt.Spent
	}
	if got["EUR"] != 12 || got["USD"] != 3 {
		t.Fatalf("totals = %+v, want EUR 12 and USD 3", got)
	}
	if v.Totals[0].Currency != "EUR" || v.Totals[1].Currency != "USD" {
		t.Errorf("order = %q %q, want a stable alphabetical order", v.Totals[0].Currency, v.Totals[1].Currency)
	}
}

func TestPeriodDefaultsToTheCurrentMonth(t *testing.T) {
	r := newAzureRig(t) // the rig's clock is 2026-09-17
	v := r.azureView(t, "")
	if v.Period != "2026-09" {
		t.Fatalf("period = %q, want 2026-09", v.Period)
	}
}

func TestEmptyAzureIsAnEmptyListNotAnError(t *testing.T) {
	r := newAzureRig(t)
	v := r.azureView(t, "")
	if v.Mode != "off" {
		t.Fatalf("mode = %q", v.Mode)
	}
	if len(v.Rows) != 0 || len(v.Totals) != 0 {
		t.Fatalf("rows = %d, totals = %d", len(v.Rows), len(v.Totals))
	}
	if v.CostAsOf != nil {
		t.Errorf("cost_as_of = %v, want null when nothing was ever read", v.CostAsOf)
	}
}

// A failed sync must read as a failed sync, never as an empty sandbox.
func TestFailedSyncIsReportedAlongsideTheStaleRows(t *testing.T) {
	r := newAzureRig(t)
	seedInventory(t, r)
	if err := r.st.SetAzureSync(store.AzureSync{Scope: "inventory", OK: false,
		Message: "403 AuthorizationFailed", StartedAt: r.now, EndedAt: r.now}); err != nil {
		t.Fatal(err)
	}
	v := r.azureView(t, "")
	inv, ok := v.Sync["inventory"]
	if !ok {
		t.Fatal("no inventory sync state reported")
	}
	if inv.OK || !strings.Contains(inv.Message, "AuthorizationFailed") {
		t.Fatalf("sync = %+v, want the failure and its reason", inv)
	}
	if len(v.Rows) != 1 {
		t.Errorf("rows = %d; the last good inventory must survive a failed sync", len(v.Rows))
	}
}

func TestSettingsNeverReturnTheSecret(t *testing.T) {
	r := newAzureRig(t)
	resp, _ := r.do(t, "PUT", "/api/v1/azure/settings", map[string]any{
		"mode": "client_secret", "tenant_id": "t", "client_id": "c",
		"client_secret": "sh-hunter22", "subscription_id": "s",
		"resource_groups": []string{"rg-sandbox"},
		"inventory_every_min": 15, "cost_every_min": 60,
	})
	if resp.StatusCode != 204 {
		t.Fatalf("PUT = %d", resp.StatusCode)
	}
	resp, data := r.do(t, "GET", "/api/v1/azure/settings", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("GET = %d %s", resp.StatusCode, data)
	}
	if strings.Contains(string(data), "hunter22") {
		t.Fatalf("the secret came back from the API: %s", data)
	}
	var v azureSettingsView
	json.Unmarshal(data, &v)
	if !v.ClientSecretSet {
		t.Error("client_secret_set = false although one was saved")
	}
}

// Absent and empty are different answers: a form that does not send the secret
// means "leave it alone", and a form that sends "" means "clear it".
func TestAbsentSecretKeepsTheStoredOneAndEmptyClearsIt(t *testing.T) {
	r := newAzureRig(t)
	base := map[string]any{"mode": "client_secret", "tenant_id": "t", "client_id": "c",
		"subscription_id": "s", "resource_groups": []string{"rg-sandbox"},
		"inventory_every_min": 15, "cost_every_min": 60}
	with := map[string]any{}
	for k, v := range base {
		with[k] = v
	}
	with["client_secret"] = "kept"
	if resp, data := r.do(t, "PUT", "/api/v1/azure/settings", with); resp.StatusCode != 204 {
		t.Fatalf("seed = %d %s", resp.StatusCode, data)
	}
	// No client_secret key at all: the save must succeed on the stored one.
	if resp, data := r.do(t, "PUT", "/api/v1/azure/settings", base); resp.StatusCode != 204 {
		t.Fatalf("absent secret = %d %s", resp.StatusCode, data)
	}
	if got, _, _ := r.st.GetSetting("azure_client_secret"); got != "kept" {
		t.Fatalf("stored secret = %q, want it untouched", got)
	}
	// An explicit empty string means clear it, which client_secret mode refuses.
	cleared := map[string]any{}
	for k, v := range base {
		cleared[k] = v
	}
	cleared["client_secret"] = ""
	resp, data := r.do(t, "PUT", "/api/v1/azure/settings", cleared)
	if resp.StatusCode != 400 {
		t.Fatalf("empty secret = %d %s, want 400", resp.StatusCode, data)
	}
}

func TestInvalidSettingsAreRejectedWithTheirReasonAndNothingIsWritten(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		want string
		key  string
	}{
		{"no resource group", map[string]any{"mode": "managed_identity", "subscription_id": "s",
			"resource_groups": []string{}}, "resource group", "azure_mode"},
		{"no subscription", map[string]any{"mode": "managed_identity",
			"resource_groups": []string{"rg"}}, "subscription", "azure_resource_groups"},
		{"secret mode without a secret", map[string]any{"mode": "client_secret", "tenant_id": "t",
			"client_id": "c", "subscription_id": "s", "resource_groups": []string{"rg"}},
			"secret", "azure_mode"},
		{"unknown mode", map[string]any{"mode": "sudo", "subscription_id": "s",
			"resource_groups": []string{"rg"}}, "mode must be", "azure_mode"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newAzureRig(t) // a fresh rig per case: a shared one lets a
			// later save hide an earlier handler's stray write
			resp, data := r.do(t, "PUT", "/api/v1/azure/settings", c.body)
			if resp.StatusCode != 400 {
				t.Fatalf("PUT = %d %s, want 400", resp.StatusCode, data)
			}
			var e struct {
				Error string `json:"error"`
			}
			json.Unmarshal(data, &e)
			if !strings.Contains(e.Error, c.want) {
				t.Errorf("error = %q, want it to mention %q", e.Error, c.want)
			}
			if v, ok, _ := r.st.GetSetting(c.key); ok && v != "" {
				t.Errorf("%s was written to %q despite the rejection", c.key, v)
			}
			if n := r.azurer(t).reloads; n != 0 {
				t.Errorf("reloads = %d; a rejected save must not reconfigure the client", n)
			}
		})
	}
}

func TestSavingSettingsReloadsTheClient(t *testing.T) {
	r := newAzureRig(t)
	resp, data := r.do(t, "PUT", "/api/v1/azure/settings", map[string]any{
		"mode": "managed_identity", "subscription_id": "s",
		"resource_groups": []string{"rg-sandbox"}})
	if resp.StatusCode != 204 {
		t.Fatalf("PUT = %d %s", resp.StatusCode, data)
	}
	if n := r.azurer(t).reloads; n != 1 {
		t.Fatalf("reloads = %d, want 1", n)
	}
	// An interval the form left out keeps its default rather than becoming 0.
	var v azureSettingsView
	_, data = r.do(t, "GET", "/api/v1/azure/settings", nil)
	json.Unmarshal(data, &v)
	if v.InventoryEveryMin != 15 || v.CostEveryMin != 60 {
		t.Errorf("intervals = %d/%d, want the defaults", v.InventoryEveryMin, v.CostEveryMin)
	}
}

func TestTestAzureReportsAzuresOwnError(t *testing.T) {
	r := newAzureRig(t)
	r.azurer(t).err = errors.New("403 AuthorizationFailed: the client does not have authorization")
	resp, data := r.do(t, "POST", "/api/v1/azure/test", nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("POST test = %d, want 502", resp.StatusCode)
	}
	if !strings.Contains(string(data), "AuthorizationFailed") {
		t.Fatalf("body = %s, want Azure's own words", data)
	}
	if n := r.azurer(t).tested; n != 1 {
		t.Errorf("tested = %d, want 1", n)
	}
}

func TestTestAzureSucceeds(t *testing.T) {
	r := newAzureRig(t)
	if resp, data := r.do(t, "POST", "/api/v1/azure/test", nil); resp.StatusCode != 204 {
		t.Fatalf("POST test = %d %s", resp.StatusCode, data)
	}
}

func TestAzureEndpointsNeedASession(t *testing.T) {
	r := newRig(t) // no setupAndLogin
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/v1/azure"}, {"GET", "/api/v1/azure/settings"},
		{"PUT", "/api/v1/azure/settings"}, {"POST", "/api/v1/azure/test"},
	} {
		if resp, _ := r.do(t, c.method, c.path, map[string]any{}); resp.StatusCode != 401 {
			t.Errorf("%s %s = %d, want 401", c.method, c.path, resp.StatusCode)
		}
	}
	_ = time.Now
}

// The budget is one figure for the hub. Repeated beside each currency total it
// would read as a budget of 100 euros and a separate budget of 100 dollars —
// the same silently-wrong number the totals themselves refuse to produce.
func TestBudgetIsReportedOnceNotPerCurrency(t *testing.T) {
	r := newAzureRig(t)
	if resp, data := r.do(t, "PUT", "/api/v1/azure/settings", map[string]any{
		"mode": "managed_identity", "subscription_id": "s",
		"resource_groups": []string{"rg-sandbox"}, "budget_monthly": 100.0}); resp.StatusCode != 204 {
		t.Fatalf("PUT = %d %s", resp.StatusCode, data)
	}
	if err := r.st.UpsertAzureCosts([]store.AzureCost{
		{ResourceID: "a", Period: "2026-09", Amount: 12, Currency: "EUR", AsOf: r.now},
		{ResourceID: "b", Period: "2026-09", Amount: 3, Currency: "USD", AsOf: r.now},
		{ResourceID: "c", Period: "2026-09", Amount: 1, Currency: "CHF", AsOf: r.now},
	}); err != nil {
		t.Fatal(err)
	}
	_, data := r.do(t, "GET", "/api/v1/azure?period=2026-09", nil)
	var raw struct {
		Budget float64          `json:"budget"`
		Totals []map[string]any `json:"totals"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if raw.Budget != 100 {
		t.Fatalf("budget = %v, want 100 at the top level", raw.Budget)
	}
	if len(raw.Totals) != 3 {
		t.Fatalf("totals = %d, want three currencies", len(raw.Totals))
	}
	for _, tt := range raw.Totals {
		if _, present := tt["budget"]; present {
			t.Fatalf("a per-currency total carries a budget: %v", tt)
		}
	}
}

// A sync state that cannot be read is not a sync that never ran. The page reads
// this field to decide what an empty table means.
func TestUnreadableSyncStateIsAnErrorNotAnEmptyMap(t *testing.T) {
	r := newAzureRig(t)
	if err := r.st.ExecForTests("DROP TABLE azure_sync"); err != nil {
		t.Fatal(err)
	}
	resp, data := r.do(t, "GET", "/api/v1/azure", nil)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("GET = %d %s, want 500", resp.StatusCode, data)
	}
}
