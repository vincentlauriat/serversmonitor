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

// Shapes copied from `az disk show`, `az network public-ip show`, `az network
// nic show`, `az appservice plan show` on 2026-09-23, trimmed to the fields read.
const (
	diskUnattached = `{"id":"/subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/disks/d1","properties":{"diskState":"Unattached","diskSizeGB":32}}`
	diskAttached   = `{"id":"/subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/disks/d2","properties":{"diskState":"Attached","diskSizeGB":30}}`
	ipFree         = `{"id":"/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/ip1","properties":{"ipAddress":"20.1.2.3","publicIPAllocationMethod":"Static"}}`
	ipUsed         = `{"id":"/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/ip2","properties":{"ipConfiguration":{"id":"/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/n1/ipConfigurations/ipconfig1"}}}`
	nicFree        = `{"id":"/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/n0","properties":{"ipConfigurations":[]}}`
	nicUsed        = `{"id":"/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/n1","properties":{"virtualMachine":{"id":"/subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm1"}}}`
	planEmpty      = `{"id":"/subscriptions/s/resourceGroups/rg/providers/Microsoft.Web/serverfarms/p0","properties":{"numberOfSites":0,"status":"Ready"}}`
	planUsed       = `{"id":"/subscriptions/s/resourceGroups/rg/providers/Microsoft.Web/serverfarms/p1","properties":{"numberOfSites":2,"status":"Ready"}}`
)

func orphanFake(t *testing.T, bodies map[string]string, status map[string]int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/oauth2/") {
			io.WriteString(w, `{"token_type":"Bearer","expires_in":3599,"access_token":"tok"}`)
			return
		}
		for suffix, code := range status {
			if strings.HasSuffix(r.URL.Path, suffix) {
				w.WriteHeader(code)
				io.WriteString(w, `{"error":{"code":"AuthorizationFailed","message":"no"}}`)
				return
			}
		}
		for suffix, body := range bodies {
			if strings.HasSuffix(r.URL.Path, suffix) {
				io.WriteString(w, body)
				return
			}
		}
		w.WriteHeader(404)
		io.WriteString(w, `{"error":{"code":"ResourceNotFound","message":"gone"}}`)
	}))
}

func res(armID, typ string) Resource {
	return Resource{ID: NormalizeID(armID), ARMID: armID, Type: typ, Name: armID[strings.LastIndex(armID, "/")+1:]}
}

func TestEachDetectorReadsAzuresOwnShape(t *testing.T) {
	srv := orphanFake(t, map[string]string{"/d1": diskUnattached, "/d2": diskAttached, "/ip1": ipFree, "/ip2": ipUsed, "/n0": nicFree, "/n1": nicUsed, "/p0": planEmpty, "/p1": planUsed}, nil)
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	rs := []Resource{
		res("/subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/disks/d1", "Microsoft.Compute/disks"),
		res("/subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/disks/d2", "Microsoft.Compute/disks"),
		res("/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/ip1", "Microsoft.Network/publicIPAddresses"),
		res("/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/ip2", "Microsoft.Network/publicIPAddresses"),
		res("/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/n0", "Microsoft.Network/networkInterfaces"),
		res("/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/n1", "Microsoft.Network/networkInterfaces"),
		res("/subscriptions/s/resourceGroups/rg/providers/Microsoft.Web/serverfarms/p0", "Microsoft.Web/serverfarms"),
		res("/subscriptions/s/resourceGroups/rg/providers/Microsoft.Web/serverfarms/p1", "Microsoft.Web/serverfarms"),
		res("/subscriptions/s/resourceGroups/rg/providers/Microsoft.Web/sites/site1", "Microsoft.Web/sites"), // unknown to the detectors: no read at all
	}
	got, err := OrphanReasons(context.Background(), c, rs)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		NormalizeID(rs[0].ARMID): OrphanDiskUnattached,
		NormalizeID(rs[2].ARMID): OrphanIPUnassociated,
		NormalizeID(rs[4].ARMID): OrphanNICWithoutVM,
		NormalizeID(rs[6].ARMID): OrphanPlanWithoutSite,
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestA403IsUnverifiedAndA404IsSkipped(t *testing.T) {
	srv := orphanFake(t, map[string]string{"/d1": diskUnattached}, map[string]int{"/d3": 403})
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL})
	rs := []Resource{
		res("/subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/disks/d1", "Microsoft.Compute/disks"),
		res("/subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/disks/d3", "Microsoft.Compute/disks"), // 403
		res("/subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/disks/d4", "Microsoft.Compute/disks"), // 404
	}
	got, err := OrphanReasons(context.Background(), c, rs)
	if err != nil {
		t.Fatal(err)
	}
	if got[NormalizeID(rs[1].ARMID)] != OrphanUnverified {
		t.Fatalf("403 must be unverified: %v", got)
	}
	if _, ok := got[NormalizeID(rs[2].ARMID)]; ok {
		t.Fatalf("404 must be absent: %v", got)
	}
}

func TestAnyOtherErrorAbortsTheSweep(t *testing.T) {
	srv := orphanFake(t, nil, map[string]int{"/d1": 500})
	defer srv.Close()
	c := NewClient(staticSource("tok"), Options{Base: srv.URL, Sleep: func(context.Context, time.Duration) bool { return true }})
	_, err := OrphanReasons(context.Background(), c, []Resource{res("/subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/disks/d1", "Microsoft.Compute/disks")})
	if err == nil {
		t.Fatal("a 500 is not an answer about orphans")
	}
}
