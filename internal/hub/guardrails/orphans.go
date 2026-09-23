package guardrails

import (
	"time"

	"github.com/vincentlauriat/serversmonitor/internal/hub/store"
)

// OrphanInput is what the hub knows after a sweep: every resource's current
// orphan reason (azure.OrphanReasons, plus hub_vm_silent added by the hub),
// names for messages, and the last journalled state per rule instance.
type OrphanInput struct {
	Now     time.Time
	Reasons map[string]string
	Names   map[string]string
	Last    map[store.GuardrailKey]store.GuardrailEvent
}

// Orphans returns the transitions: fired for a new reason (except
// unverified, since "not verified" is not "orphan"), resolved for a resource
// whose reason is gone or is now unverified — we no longer know.
func Orphans(in OrphanInput) []store.GuardrailEvent {
	var wants []want
	seen := map[string]bool{}
	for id, reason := range in.Reasons {
		seen[id] = true
		wants = append(wants, want{key: store.GuardrailKey{Subject: id, Rule: "orphan"}, on: reason != "" && reason != "unverified"})
	}
	for k, e := range in.Last {
		if k.Rule == "orphan" && e.Kind == "fired" && !seen[k.Subject] {
			wants = append(wants, want{key: k, on: false})
		}
	}
	return transitions(wants, in.Last, in.Now)
}

// HubVMSilent is the one reason the hub decides from its own data: a VM it
// created whose agent never reported, or stopped reporting long ago.
func HubVMSilent(hostStatus string, lastSeen *time.Time, now time.Time, silentDays int) bool {
	switch hostStatus {
	case "never_seen":
		return true
	case "offline":
		return lastSeen == nil || now.Sub(*lastSeen) > time.Duration(silentDays)*24*time.Hour
	}
	return false
}
