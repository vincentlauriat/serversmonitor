package collect

import "testing"

func TestNoisyMount(t *testing.T) {
	noisy := []string{"/dev", "/System/Volumes/Preboot", "/Volumes/Recovery",
		"/private/var/folders/jq/X/abc", "/Library/Developer/CoreSimulator/Volumes/iOS_24A",
		"/snap/core/1234", "/var/lib/docker/overlay2/x", "/boot/efi"}
	for _, m := range noisy {
		if !noisyMount(m) {
			t.Errorf("%s should be filtered out", m)
		}
	}
	// "/System/Volumes/Data" is deliberately filtered: on macOS it is the same
	// APFS container as "/", which is kept.
	keep := []string{"/", "/home", "/data", "/mnt/backup", "/Volumes/Backup", "/var/lib/mysql"}
	for _, m := range keep {
		if noisyMount(m) {
			t.Errorf("%s must be kept", m)
		}
	}
}

func TestVirtualIface(t *testing.T) {
	for _, n := range []string{"lo", "lo0", "docker0", "veth1234", "br-abc", "utun0", "awdl0"} {
		if !virtualIface(n) {
			t.Errorf("%s should be skipped", n)
		}
	}
	for _, n := range []string{"eth0", "en0", "wlan0", "enp3s0"} {
		if virtualIface(n) {
			t.Errorf("%s must be counted", n)
		}
	}
}
