package azure

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const costBody = `{"properties":{
 "columns":[{"name":"Cost","type":"Number"},{"name":"ResourceId","type":"String"},{"name":"Currency","type":"String"}],
 "rows":[
  [0.000838612982998454,"/subscriptions/sub/resourcegroups/rg/providers/microsoft.web/serverfarms/asp-urban","EUR"],
  [0.0,"/subscriptions/sub/resourcegroups/rg/providers/microsoft.web/sites/visualrami","EUR"],
  [1.5,"/subscriptions/sub/resourcegroups/rg/providers/microsoft.operationalinsights/workspaces/gone","EUR"]
 ]}}`

func TestCostsReadColumnsByName(t *testing.T) {
	// Reading by position would silently swap the amount and the id the day
	// Azure reorders its columns, which it already does not do in request order.
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		gotBody = string(raw)
		io.WriteString(w, costBody)
	}))
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	cs, err := Costs(context.Background(), c, "sub", []string{"rg"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 3 {
		t.Fatalf("got %d cost rows, want 3", len(cs))
	}
	if cs[0].Amount != 0.000838612982998454 || cs[0].Currency != "EUR" {
		t.Fatalf("row 0 = %+v", cs[0])
	}
	if !strings.HasSuffix(cs[0].ResourceID, "/asp-urban") {
		t.Fatalf("resource id = %q", cs[0].ResourceID)
	}
	for _, want := range []string{`"ActualCost"`, `"MonthToDate"`, `"ResourceId"`, `"Sum"`} {
		if !strings.Contains(gotBody, want) {
			t.Errorf("request body is missing %s: %s", want, gotBody)
		}
	}
}

func TestCostsSurviveAReorderedResponse(t *testing.T) {
	reordered := `{"properties":{
	 "columns":[{"name":"Currency"},{"name":"ResourceId"},{"name":"Cost"}],
	 "rows":[["USD","/subscriptions/sub/resourcegroups/rg/providers/x/y/z",42.5]]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, reordered)
	}))
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	cs, err := Costs(context.Background(), c, "sub", []string{"rg"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].Amount != 42.5 || cs[0].Currency != "USD" {
		t.Fatalf("cost = %+v", cs)
	}
}

func TestCostsNormaliseTheResourceID(t *testing.T) {
	mixed := `{"properties":{"columns":[{"name":"Cost"},{"name":"ResourceId"},{"name":"Currency"}],
	 "rows":[[1.0,"/subscriptions/SUB/resourceGroups/RG/providers/Microsoft.Web/sites/VisualRami","EUR"]]}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, mixed)
	}))
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	cs, err := Costs(context.Background(), c, "sub", []string{"rg"})
	if err != nil {
		t.Fatal(err)
	}
	if cs[0].ResourceID != strings.ToLower(cs[0].ResourceID) {
		t.Fatalf("cost ids must be normalised too: %q", cs[0].ResourceID)
	}
}

func TestCostsRejectAResponseMissingAColumn(t *testing.T) {
	// Guessing a missing column would invent numbers, which is the one thing a
	// cost feature must never do.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"properties":{"columns":[{"name":"Cost"},{"name":"Currency"}],"rows":[[1.0,"EUR"]]}}`)
	}))
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	if _, err := Costs(context.Background(), c, "sub", []string{"rg"}); err == nil {
		t.Fatal("a response with no ResourceId column must be an error")
	}
}

func TestCostsQueryEveryGroup(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		io.WriteString(w, `{"properties":{"columns":[{"name":"Cost"},{"name":"ResourceId"},{"name":"Currency"}],"rows":[]}}`)
	}))
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	if _, err := Costs(context.Background(), c, "sub", []string{"rg-one", "rg-two"}); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 || !strings.Contains(paths[0], "rg-one") || !strings.Contains(paths[1], "rg-two") {
		t.Fatalf("paths = %v", paths)
	}
}
