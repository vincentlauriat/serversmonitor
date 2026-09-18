package alerts

import (
	"testing"
	"time"
)

func TestMachineFiresAfterDuration(t *testing.T) {
	m := NewMachine()
	k := Key{RuleID: 1, HostID: 1}
	now := time.Unix(0, 0)
	d := 10 * time.Minute
	if tr := m.Observe(k, true, now, d); tr != None || m.State(k) != Pending {
		t.Fatalf("first breach: tr=%v state=%v", tr, m.State(k))
	}
	if tr := m.Observe(k, true, now.Add(5*time.Minute), d); tr != None {
		t.Fatal("still pending at 5 min")
	}
	if tr := m.Observe(k, true, now.Add(10*time.Minute), d); tr != Fired || m.State(k) != Firing {
		t.Fatalf("at 10 min: tr=%v", tr)
	}
	if tr := m.Observe(k, true, now.Add(20*time.Minute), d); tr != None {
		t.Fatal("firing must not re-fire")
	}
	if tr := m.Observe(k, false, now.Add(21*time.Minute), d); tr != Resolved || m.State(k) != Ok {
		t.Fatalf("recovery: tr=%v", tr)
	}
}

func TestMachinePendingResetsWithoutEvent(t *testing.T) {
	m := NewMachine()
	k := Key{1, 1}
	now := time.Unix(0, 0)
	m.Observe(k, true, now, time.Minute)
	if tr := m.Observe(k, false, now.Add(30*time.Second), time.Minute); tr != None || m.State(k) != Ok {
		t.Fatal("pending -> ok must be silent")
	}
	m.Observe(k, true, now.Add(40*time.Second), time.Minute)
	if tr := m.Observe(k, true, now.Add(90*time.Second), time.Minute); tr != None {
		t.Fatal("clock must restart after a reset")
	}
	if tr := m.Observe(k, true, now.Add(100*time.Second), time.Minute); tr != Fired {
		t.Fatalf("must fire one minute after the restart, got %v", tr)
	}
}

func TestMachineZeroDurationFiresImmediately(t *testing.T) {
	m := NewMachine()
	if tr := m.Observe(Key{1, 1}, true, time.Unix(0, 0), 0); tr != Fired {
		t.Fatalf("tr = %v", tr)
	}
}

func TestMachineRestore(t *testing.T) {
	m := NewMachine()
	k := Key{1, 1}
	m.Restore([]Key{k})
	if m.State(k) != Firing {
		t.Fatal("restore must set Firing")
	}
	if tr := m.Observe(k, true, time.Unix(0, 0), time.Minute); tr != None {
		t.Fatal("a restored firing alert must not fire again")
	}
	if tr := m.Observe(k, false, time.Unix(0, 0), time.Minute); tr != Resolved {
		t.Fatal("restored firing state must resolve")
	}
}
