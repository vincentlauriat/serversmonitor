package deploy

import (
	"strings"
	"testing"
)

// The first VM the hub ever created (2026-09-22) booted, ran cloud-init,
// downloaded the agent and then never connected: the unit named a
// supplementary group, `docker`, that does not exist on a machine without
// Docker, and systemd refuses to start a service whose groups it cannot
// resolve (status=216/GROUP), forever, every five seconds. The service runs
// as root, which reads the Docker socket without any group, so the line
// bought nothing and cost the whole lot.
func TestUnitNamesNoGroupThatMayNotExist(t *testing.T) {
	s := string(InstallScript)
	if !strings.Contains(s, "[Service]") {
		t.Fatal("the install script no longer writes a systemd unit; update this test")
	}
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "SupplementaryGroups=") {
			t.Fatalf("the unit must not depend on a group the target may lack: %q", line)
		}
	}
	if !strings.Contains(s, "User=root") {
		t.Fatal("the agent runs as root to read the Docker socket; if that changes, revisit the groups")
	}
}
