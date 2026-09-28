package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/azure"
)

func TestOutboundEndpoint(t *testing.T) {
	r := newAzureRig(t)
	f := r.azure.(*fakeAzurer)
	f.outbound = azure.Outbound{State: azure.OutboundNone, Detail: "no way out"}
	res, body := r.do(t, "GET", "/api/v1/azure/vms/outbound", nil)
	if res.StatusCode != 200 || !strings.Contains(string(body), `"state":"none"`) || !strings.Contains(string(body), "no way out") {
		t.Fatalf("outbound = %d %s", res.StatusCode, body)
	}
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"Azure off", azure.ErrNotConfigured, http.StatusBadRequest},
		{"no subnet", azure.Refuse("no subnet is configured for new VMs"), http.StatusBadRequest},
		{"Azure refused the read", errors.New("403 AuthorizationFailed"), http.StatusBadGateway},
	} {
		f.outboundErr = tc.err
		if res, body := r.do(t, "GET", "/api/v1/azure/vms/outbound", nil); res.StatusCode != tc.want {
			t.Errorf("%s: %d %s, want %d", tc.name, res.StatusCode, body, tc.want)
		}
	}
}

// The card needs both halves of "why did this agent never call in": what the
// subnet looked like, and whether the host has been seen since.
func TestProvisionsCarryOutboundAndHostStatus(t *testing.T) {
	r := newAzureRig(t)
	now := time.Now().UTC()
	host, _, err := r.st.CreateHost("vm-quiet", now)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := r.st.StartAzureProvision("vm-quiet", &host.ID, now)
	r.st.SetAzureProvisionOutbound(id, "none", "no way out")
	r.st.FinishAzureProvision(id, "succeeded", "", now)
	gone := int64(9999)
	r.st.StartAzureProvision("vm-gone", &gone, now)

	_, body := r.do(t, "GET", "/api/v1/azure/vms", nil)
	var out struct {
		Provisions []struct {
			Name           string `json:"name"`
			Outbound       string `json:"outbound"`
			OutboundDetail string `json:"outbound_detail"`
			HostStatus     string `json:"host_status"`
		} `json:"provisions"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for i, p := range out.Provisions {
		got[p.Name] = i
	}
	q := out.Provisions[got["vm-quiet"]]
	if q.Outbound != "none" || q.OutboundDetail != "no way out" || q.HostStatus != "never_seen" {
		t.Fatalf("vm-quiet = %+v", q)
	}
	if g := out.Provisions[got["vm-gone"]]; g.HostStatus != "" || g.Outbound != "" {
		t.Fatalf("a deleted host reads as no status, got %+v", g)
	}
}
