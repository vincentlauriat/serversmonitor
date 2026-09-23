package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/vincentlauriat/serversmonitor/internal/hub/azure"
	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

func TestGuardrailsViewComputesFromTheStore(t *testing.T) {
	r := newAzureRig(t)
	now := r.now
	if err := r.st.SetSetting("azure_budget_monthly", "100"); err != nil {
		t.Fatal(err)
	}
	if err := r.st.ReplaceAzureInventory([]string{"rg"}, []store.AzureResource{
		{ID: "/s/d1", ARMID: "/S/d1", Name: "d1", Type: "Microsoft.Compute/disks",
			ResourceGroup: "rg", Location: "westeurope", Tags: map[string]string{}},
		{ID: "/s/ip1", ARMID: "/S/ip1", Name: "ip1", Type: "Microsoft.Network/publicIPAddresses",
			ResourceGroup: "rg", Location: "westeurope", Tags: map[string]string{}},
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := r.st.SetAzureOrphans(map[string]string{"/s/d1": "disk_unattached", "/s/ip1": "ip_unassociated"}, now); err != nil {
		t.Fatal(err)
	}
	if err := r.st.UpsertAzureCosts([]store.AzureCost{
		{ResourceID: "/s/d1", Period: now.Format("2006-01"), Amount: 40, Currency: "EUR", AsOf: now},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.st.InsertGuardrailEvent(store.GuardrailEvent{Subject: "budget", Rule: "budget_threshold",
		Detail: "80", Kind: "fired", Value: 85, At: now}); err != nil {
		t.Fatal(err)
	}

	resp, data := r.do(t, "GET", "/api/v1/azure/guardrails", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET guardrails = %d %s", resp.StatusCode, data)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if v["budget"].(float64) != 100 || v["spent"].(float64) != 40 {
		t.Fatalf("totals: %v", v)
	}
	if cs, ok := v["currencies"].([]any); !ok || len(cs) != 1 || cs[0] != "EUR" {
		t.Fatalf("currencies: %v", v["currencies"])
	}
	orphans, ok := v["orphans"].([]any)
	if !ok || len(orphans) != 2 {
		t.Fatalf("orphans: %v", v["orphans"])
	}
	for _, o := range orphans {
		m := o.(map[string]any)
		if m["name"] == "ip1" && m["deletable"] != false {
			t.Fatal("a public IP is not deletable")
		}
		if m["name"] == "d1" && (m["deletable"] != true || m["cost"].(float64) != 40) {
			t.Fatalf("disk: %v", m)
		}
	}
	ths, ok := v["thresholds"].([]any)
	if !ok || len(ths) == 0 {
		t.Fatalf("thresholds: %v", v["thresholds"])
	}
	th := ths[0].(map[string]any)
	if th["pct"].(float64) != 80 || th["firing"] != true {
		t.Fatalf("threshold 80 must show firing from the journal: %v", th)
	}
	// The rig's clock (2026-09-17) is well past the four-day dead zone
	// Projection refuses to guess in, and a cost row was read as of the same
	// day: a projection must come back, not null.
	if v["projection"] == nil {
		t.Fatal("projection = null, want a figure past day 4")
	}
}

// A resource whose typed read Azure refused is not a resource the hub can
// vouch for. Rendering it as an ordinary, deletable orphan would offer a
// Delete button for something never actually confirmed to be safe to remove —
// the invariant that runs through the whole lot.
func TestUnverifiedOrphanIsNeverDeletable(t *testing.T) {
	r := newAzureRig(t)
	now := r.now
	// App Service Plans are otherwise deletable (KindOf returns true), so this
	// proves the unverified override, not just KindOf's own table.
	if err := r.st.ReplaceAzureInventory([]string{"rg"}, []store.AzureResource{
		{ID: "/s/plan1", ARMID: "/S/plan1", Name: "plan1", Type: "Microsoft.Web/serverfarms",
			ResourceGroup: "rg", Location: "westeurope", Tags: map[string]string{}},
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := r.st.SetAzureOrphans(map[string]string{"/s/plan1": azure.OrphanUnverified}, now); err != nil {
		t.Fatal(err)
	}
	resp, data := r.do(t, "GET", "/api/v1/azure/guardrails", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET guardrails = %d %s", resp.StatusCode, data)
	}
	var v struct {
		Orphans []struct {
			Reason    string `json:"reason"`
			Deletable bool   `json:"deletable"`
		} `json:"orphans"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Orphans) != 1 || v.Orphans[0].Reason != "unverified" || v.Orphans[0].Deletable {
		t.Fatalf("unverified orphan must never be deletable: %+v", v.Orphans)
	}
}

func TestGuardrailSettingsRoundTripAndRefusal(t *testing.T) {
	r := newAzureRig(t)
	resp, data := r.do(t, "PUT", "/api/v1/azure/guardrails/settings", map[string]any{
		"thresholds": []int{50, 90}, "resource_share_pct": 25, "hub_vm_silent_days": 2, "timezone": "Europe/Paris"})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("put = %d %s", resp.StatusCode, data)
	}
	resp, data = r.do(t, "GET", "/api/v1/azure/guardrails/settings", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get = %d %s", resp.StatusCode, data)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if v["resource_share_pct"].(float64) != 25 || v["timezone"] != "Europe/Paris" {
		t.Fatalf("got %v", v)
	}
	resp, data = r.do(t, "PUT", "/api/v1/azure/guardrails/settings", map[string]any{
		"thresholds": []int{50}, "resource_share_pct": 25, "hub_vm_silent_days": 2, "timezone": "Mars/Olympus"})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(data), "Mars/Olympus") {
		t.Fatalf("refusal = %d %s", resp.StatusCode, data)
	}
}

func TestSchedulesRoutes(t *testing.T) {
	r := newAzureRig(t)
	now := r.now
	if err := r.st.ReplaceAzureInventory([]string{"rg"}, []store.AzureResource{
		{ID: "/s/site1", ARMID: "/S/site1", Name: "site1", Type: "Microsoft.Web/sites",
			ResourceGroup: "rg", Location: "westeurope", Tags: map[string]string{}},
	}, now); err != nil {
		t.Fatal(err)
	}
	body := map[string]any{"resource_id": "/s/site1", "off_windows": []map[string]any{
		{"days": []int{1, 2, 3, 4, 5}, "from": "20:00", "to": "07:00"}}, "enabled": true}
	resp, data := r.do(t, "PUT", "/api/v1/azure/schedules", body)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("put = %d %s", resp.StatusCode, data)
	}

	// A type lot 4 cannot stop is refused with the type in the message.
	if err := r.st.ReplaceAzureInventory([]string{"rg"}, []store.AzureResource{
		{ID: "/s/site1", ARMID: "/S/site1", Name: "site1", Type: "Microsoft.Web/sites",
			ResourceGroup: "rg", Location: "westeurope", Tags: map[string]string{}},
		{ID: "/s/vnet", ARMID: "/S/vnet", Name: "vnet", Type: "Microsoft.Network/virtualNetworks",
			ResourceGroup: "rg", Location: "westeurope", Tags: map[string]string{}},
	}, now); err != nil {
		t.Fatal(err)
	}
	resp, data = r.do(t, "PUT", "/api/v1/azure/schedules",
		map[string]any{"resource_id": "/s/vnet", "off_windows": []any{}, "enabled": true})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(data), "virtualNetworks") {
		t.Fatalf("vnet: %d %s", resp.StatusCode, data)
	}

	resp, data = r.do(t, "GET", "/api/v1/azure/schedules", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get = %d %s", resp.StatusCode, data)
	}
	var list map[string][]map[string]any
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatal(err)
	}
	if len(list["schedules"]) != 1 || list["schedules"][0]["name"] != "site1" {
		t.Fatalf("list = %v", list)
	}

	resp, data = r.do(t, "POST", "/api/v1/azure/schedules/delete", map[string]any{"resource_id": "/s/site1"})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d %s", resp.StatusCode, data)
	}
	resp, data = r.do(t, "GET", "/api/v1/azure/schedules", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get after delete = %d %s", resp.StatusCode, data)
	}
	if err := json.Unmarshal(data, &list); err != nil {
		t.Fatal(err)
	}
	if len(list["schedules"]) != 0 {
		t.Fatalf("after delete = %v, want none left", list)
	}
}

// A schedule written with a differently-cased id than the one already stored
// must still be found and replaced, not doubled — this is what the
// normalize-on-write fix actually buys.
func TestScheduleWriteNormalizesTheResourceID(t *testing.T) {
	r := newAzureRig(t)
	now := r.now
	if err := r.st.ReplaceAzureInventory([]string{"rg"}, []store.AzureResource{
		{ID: "/s/site1", ARMID: "/S/Site1", Name: "site1", Type: "Microsoft.Web/sites",
			ResourceGroup: "rg", Location: "westeurope", Tags: map[string]string{}},
	}, now); err != nil {
		t.Fatal(err)
	}
	resp, data := r.do(t, "PUT", "/api/v1/azure/schedules",
		map[string]any{"resource_id": "/S/Site1", "off_windows": []any{}, "enabled": true})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("put = %d %s", resp.StatusCode, data)
	}
	scs, err := r.st.ListAzureSchedules()
	if err != nil {
		t.Fatal(err)
	}
	if len(scs) != 1 || scs[0].ResourceID != "/s/site1" {
		t.Fatalf("schedules = %+v, want one row keyed on the normalized id", scs)
	}
}

// The delete route's id must normalize the same way, or a differently-cased
// resource_id silently deletes nothing (DELETE ... WHERE resource_id = ?
// affects zero rows, no error) instead of the row the person is looking at.
func TestScheduleDeleteNormalizesTheResourceID(t *testing.T) {
	r := newAzureRig(t)
	now := r.now
	if err := r.st.UpsertAzureSchedule(store.AzureSchedule{ResourceID: "/s/site1", OffWindows: "[]", Enabled: true}, now); err != nil {
		t.Fatal(err)
	}
	resp, data := r.do(t, "POST", "/api/v1/azure/schedules/delete", map[string]any{"resource_id": "/S/Site1"})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d %s", resp.StatusCode, data)
	}
	scs, err := r.st.ListAzureSchedules()
	if err != nil {
		t.Fatal(err)
	}
	if len(scs) != 0 {
		t.Fatalf("schedules = %+v, want the differently-cased delete to still remove it", scs)
	}
}

func TestOrphanDeleteRouteCarriesTheRefusal(t *testing.T) {
	r := newAzureRig(t)
	r.azurer(t).orphanDeleteErr = azure.Refuse("the hub's role cannot delete this kind of resource: ip1 (type)")
	resp, data := r.do(t, "POST", "/api/v1/azure/orphans/delete",
		map[string]any{"resource_id": "/s/ip1", "confirm_name": "nope"})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(data), "type") {
		t.Fatalf("got %d %s", resp.StatusCode, data)
	}
	az := r.azurer(t)
	if az.orphanDeleteID != "/s/ip1" || az.orphanDeleteName != "nope" {
		t.Fatalf("hub got id %q name %q", az.orphanDeleteID, az.orphanDeleteName)
	}
}

func TestOrphanDeleteSucceeds(t *testing.T) {
	r := newAzureRig(t)
	resp, data := r.do(t, "POST", "/api/v1/azure/orphans/delete",
		map[string]any{"resource_id": "/s/d1", "confirm_name": "d1"})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete = %d %s", resp.StatusCode, data)
	}
}

func TestOrphanDeleteErrorsMapToStatuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"unknown resource", store.ErrNoSuchResource, http.StatusNotFound},
		{"azure off", azure.ErrNotConfigured, http.StatusBadRequest},
		{"refusal", azure.Refuse("the name typed does not match"), http.StatusBadRequest},
		// Neither ours nor a refusal: the ARM call itself failed, which is
		// Azure's problem to explain, not the caller's mistake to fix.
		{"azure failure", errors.New("500 InternalServerError"), http.StatusBadGateway},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newAzureRig(t)
			r.azurer(t).orphanDeleteErr = tc.err
			resp, data := r.do(t, "POST", "/api/v1/azure/orphans/delete",
				map[string]any{"resource_id": "/x", "confirm_name": "y"})
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d (%s)", resp.StatusCode, tc.want, data)
			}
		})
	}
}

// An empty guardrails state is an empty state, not an error and not a page
// that throws on `.map` over a null field — the same convention the rest of
// the Azure API follows (see TestNoProvisionsRendersAsAnEmptyArray).
func TestEmptyGuardrailsIsEmptyListsNotNulls(t *testing.T) {
	r := newAzureRig(t)
	resp, data := r.do(t, "GET", "/api/v1/azure/guardrails", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET guardrails = %d %s", resp.StatusCode, data)
	}
	for _, want := range []string{`"currencies":[]`, `"shares":[]`, `"orphans":[]`, `"events":[]`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("body = %s, want it to contain %s", data, want)
		}
	}
	// projection is the one field that is legitimately null here — there is
	// no cost data at all to project from.
	if !strings.Contains(string(data), `"projection":null`) {
		t.Errorf("body = %s, want a null projection with no cost data", data)
	}
}

func TestGuardrailsEndpointsNeedASession(t *testing.T) {
	r := newRig(t) // no setupAndLogin
	for _, c := range []struct{ method, path string }{
		{"GET", "/api/v1/azure/guardrails"},
		{"GET", "/api/v1/azure/guardrails/settings"}, {"PUT", "/api/v1/azure/guardrails/settings"},
		{"GET", "/api/v1/azure/schedules"}, {"PUT", "/api/v1/azure/schedules"},
		{"POST", "/api/v1/azure/schedules/delete"}, {"POST", "/api/v1/azure/orphans/delete"},
	} {
		if resp, _ := r.do(t, c.method, c.path, map[string]any{}); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401", c.method, c.path, resp.StatusCode)
		}
	}
}
