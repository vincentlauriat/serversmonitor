package azure

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// vnetWith is a VNet read carrying one subnet with the given properties, the
// way ARM inlines subnets in a virtualNetworks GET.
func vnetWith(subnetID, props string) []byte {
	return []byte(`{"location":"westeurope","properties":{"subnets":[` +
		`{"id":"/subscriptions/x/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/v/subnets/other","properties":{"natGateway":{"id":"nat"}}},` +
		`{"id":"` + subnetID + `","properties":` + props + `}]}}`)
}

func TestSubnetOutbound(t *testing.T) {
	for _, tc := range []struct {
		name, props, want string
		reaches           bool
	}{
		{"NAT Gateway", `{"defaultOutboundAccess":false,"natGateway":{"id":"/x/natGateways/nat"}}`, OutboundNAT, true},
		{"default outbound on", `{"defaultOutboundAccess":true}`, OutboundDefault, true},
		{"a subnet from before 2025-09-30", `{"addressPrefix":"10.0.0.0/24"}`, OutboundLegacy, true},
		{"route table, maybe a firewall", `{"defaultOutboundAccess":false,"routeTable":{"id":"/x/routeTables/to-fw"}}`, OutboundRouteTable, false},
		{"nothing at all", `{"defaultOutboundAccess":false}`, OutboundNone, false},
	} {
		got := SubnetOutbound(vnetWith(goodSubnet, tc.props), goodSubnet)
		if got.State != tc.want || got.Reaches() != tc.reaches || got.Detail == "" {
			t.Errorf("%s: got %+v, want %s (reaches %v)", tc.name, got, tc.want, tc.reaches)
		}
	}
	// ARM answers with its own casing; the setting holds what was pasted.
	if got := SubnetOutbound(vnetWith(strings.ToUpper(goodSubnet), `{"defaultOutboundAccess":false}`), goodSubnet); got.State != OutboundNone {
		t.Errorf("the subnet id must match case-insensitively, got %+v", got)
	}
	if got := SubnetOutbound([]byte(`{"location":"westeurope"}`), goodSubnet); got.State != OutboundUnknown || got.Reaches() {
		t.Errorf("a subnet missing from the read is unknown, got %+v", got)
	}
	if got := SubnetOutbound([]byte(`not json`), goodSubnet); got.State != OutboundUnknown {
		t.Errorf("an unreadable body is unknown, got %+v", got)
	}
}

// A subnet with no outbound access is reported, and the VM is still created:
// the choice made for lot 8 is to warn, never to refuse.
func TestCreateVMReportsOutboundAndCarriesOn(t *testing.T) {
	var puts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/oauth2/"):
			io.WriteString(w, `{"token_type":"Bearer","expires_in":3599,"access_token":"tok"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/virtualNetworks/vnet-sandbox"):
			w.Write(vnetWith(goodSubnet, `{"defaultOutboundAccess":false}`))
		default:
			if r.Method == http.MethodPut {
				puts++
			}
			io.WriteString(w, `{"id":"`+r.URL.Path+`","properties":{"provisioningState":"Succeeded"}}`)
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClient(staticSource("tok"), Options{Base: srv.URL,
		Sleep: func(ctx context.Context, _ time.Duration) bool { return ctx.Err() == nil }})

	var seen []Outbound
	req := createReq()
	req.Outbound = func(o Outbound) { seen = append(seen, o) }
	if err := CreateVM(context.Background(), c, readyConfig(), req, collect(new([]Created))); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0].State != OutboundNone {
		t.Fatalf("outbound reported = %+v, want one none", seen)
	}
	if puts != 2 {
		t.Fatalf("the NIC and the VM must still be created, got %d PUTs", puts)
	}
}
