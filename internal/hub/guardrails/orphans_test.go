package guardrails

import (
	"testing"

	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

func TestOrphansFireOnceAndResolveWhenTheReasonGoes(t *testing.T) {
	now := day(20)
	in := OrphanInput{Now: now, Reasons: map[string]string{"/s/d1": "disk_unattached", "/s/ip1": "unverified"}, Names: map[string]string{"/s/d1": "d1"}, Last: map[store.GuardrailKey]store.GuardrailEvent{}}
	got := keys(Orphans(in))
	if got["/s/d1|orphan|"] != "fired" || len(got) != 1 {
		t.Fatalf("unverified must not fire: %v", got)
	}
	in.Last[store.GuardrailKey{Subject: "/s/d1", Rule: "orphan", Detail: ""}] = store.GuardrailEvent{Kind: "fired"}
	if got := keys(Orphans(in)); len(got) != 0 {
		t.Fatalf("re-fired: %v", got)
	}
	// Reason gone → resolved. Reason now unverified → resolved too (we no longer know).
	in.Reasons = map[string]string{"/s/d1": "unverified"}
	if got := keys(Orphans(in)); got["/s/d1|orphan|"] != "resolved" {
		t.Fatalf("got %v", got)
	}
}

func TestHubVMSilent(t *testing.T) {
	now := day(20)
	old := day(10)
	recent := day(19)
	if !HubVMSilent("never_seen", nil, now, 3) {
		t.Fatal("never seen is silent")
	}
	if !HubVMSilent("offline", &old, now, 3) || HubVMSilent("offline", &recent, now, 3) {
		t.Fatal("offline: silent only past the age")
	}
	if HubVMSilent("online", &old, now, 3) {
		t.Fatal("online is never silent")
	}
}
