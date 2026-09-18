package azure

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A real mixed-case ARM id. Building the fixture lowercase would make the
// normalisation test pass vacuously.
const armSiteID = "/subscriptions/SUB/resourceGroups/rg-dev-vincent-sandbox/providers/Microsoft.Web/sites/visualrami"

func TestNormalizeIDLowercasesEverything(t *testing.T) {
	got := NormalizeID(armSiteID)
	if got != strings.ToLower(armSiteID) {
		t.Fatalf("normalize = %q", got)
	}
	if strings.Contains(got, "resourceGroups") || strings.Contains(got, "Microsoft.Web") {
		t.Fatalf("mixed case survived: %q", got)
	}
	if NormalizeID("  "+armSiteID+"  ") != strings.ToLower(armSiteID) {
		t.Fatal("surrounding blanks must be trimmed")
	}
}

func inventoryServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/resources"):
			// The catalogue pass: properties is null, exactly as ARM returns it.
			fmt.Fprintf(w, `{"value":[
			 {"id":%q,"name":"visualrami","type":"Microsoft.Web/sites","location":"westeurope","kind":"app,linux","tags":{"env":"dev"},"properties":null},
			 {"id":"/subscriptions/SUB/resourceGroups/rg-dev-vincent-sandbox/providers/Microsoft.Web/serverFarms/asp-urban","name":"asp-urban","type":"Microsoft.Web/serverfarms","location":"westeurope","sku":{"name":"B1"},"properties":null},
			 {"id":"/subscriptions/SUB/resourceGroups/rg-dev-vincent-sandbox/providers/Microsoft.KeyVault/vaults/kv-x","name":"kv-x","type":"Microsoft.KeyVault/vaults","location":"westeurope","properties":null}
			]}`, armSiteID)
		case strings.Contains(r.URL.Path, "/Microsoft.Web/sites"):
			fmt.Fprintf(w, `{"value":[
			 {"id":%q,"name":"visualrami","type":"Microsoft.Web/sites","kind":"app,linux",
			  "properties":{"state":"Running","defaultHostName":"visualrami.azurewebsites.net","httpsOnly":true}}
			]}`, armSiteID)
		case strings.Contains(r.URL.Path, "/Microsoft.Web/serverfarms"):
			io.WriteString(w, `{"value":[
			 {"id":"/subscriptions/SUB/resourceGroups/rg-dev-vincent-sandbox/providers/Microsoft.Web/serverFarms/asp-urban","name":"asp-urban",
			  "sku":{"name":"B1"},"properties":{"status":"Ready"}}
			]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
}

func TestInventoryEnrichesWebResources(t *testing.T) {
	srv := inventoryServer(t)
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	rs, err := Inventory(context.Background(), c, "SUB", []string{"rg-dev-vincent-sandbox"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 3 {
		t.Fatalf("got %d resources, want 3", len(rs))
	}
	byName := map[string]Resource{}
	for _, r := range rs {
		byName[r.Name] = r
	}

	site := byName["visualrami"]
	if site.ID != strings.ToLower(armSiteID) {
		t.Fatalf("id not normalised: %q", site.ID)
	}
	if site.State == nil || *site.State != "Running" {
		t.Fatalf("state = %v, the enrichment pass must fill it", site.State)
	}
	if site.Host != "visualrami.azurewebsites.net" {
		t.Fatalf("host = %q", site.Host)
	}
	if site.Kind != "app,linux" || site.Tags["env"] != "dev" {
		t.Fatalf("site = %+v", site)
	}
	if site.ResourceGroup != "rg-dev-vincent-sandbox" {
		t.Fatalf("resource group = %q, it is parsed from the id", site.ResourceGroup)
	}

	if plan := byName["asp-urban"]; plan.SKU != "B1" || plan.State == nil || *plan.State != "Ready" {
		t.Fatalf("plan = %+v", plan)
	}
}

func TestInventoryKeepsTypesItCannotEnrich(t *testing.T) {
	// An inventory that silently omits what it does not understand is worse
	// than one that admits the gap.
	srv := inventoryServer(t)
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	rs, _ := Inventory(context.Background(), c, "SUB", []string{"rg-dev-vincent-sandbox"})
	for _, r := range rs {
		if r.Name == "kv-x" {
			if r.State != nil {
				t.Fatalf("an unenriched type has no state, got %q", *r.State)
			}
			return
		}
	}
	t.Fatal("the key vault must still be listed")
}

func TestInventoryFailsWhenAnyCallFails(t *testing.T) {
	// A sync is successful only when every call in it succeeded. A half-read
	// inventory would mark the missing resources deleted.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/resources") {
			fmt.Fprintf(w, `{"value":[{"id":%q,"name":"visualrami","type":"Microsoft.Web/sites","location":"we","properties":null}]}`, armSiteID)
			return
		}
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":{"code":"AuthorizationFailed","message":"no authorization to read sites"}}`)
	}))
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	if _, err := Inventory(context.Background(), c, "SUB", []string{"RG"}); err == nil {
		t.Fatal("a failed enrichment pass must fail the whole inventory")
	}
}

func TestInventoryCoversEveryConfiguredGroup(t *testing.T) {
	seen := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, part := range strings.Split(r.URL.Path, "/") {
			if strings.HasPrefix(part, "rg-") {
				seen[part] = true
			}
		}
		io.WriteString(w, `{"value":[]}`)
	}))
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	if _, err := Inventory(context.Background(), c, "SUB", []string{"rg-one", "rg-two"}); err != nil {
		t.Fatal(err)
	}
	if !seen["rg-one"] || !seen["rg-two"] {
		t.Fatalf("both groups must be visited, saw %v", seen)
	}
}

func TestInventoryRefusesAnEmptyGroupList(t *testing.T) {
	// Subscription-wide scope needs a subscription-scope role assignment that
	// this lot does not ask anyone for, and would answer 403.
	c := NewClient(staticSource("tok"), Options{Base: "http://unused.invalid"})
	_, err := Inventory(context.Background(), c, "SUB", nil)
	if err == nil {
		t.Fatal("an empty group list must be refused, not turned into a subscription sweep")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "resource group") {
		t.Fatalf("the reason must name resource groups: %v", err)
	}
}
