package collect

import "testing"

func TestNoisyMount(t *testing.T) {
	noisy := []string{"/dev", "/System/Volumes/Preboot", "/Volumes/Recovery",
		"/private/var/folders/jq/X/abc", "/Library/Developer/CoreSimulator/Volumes/iOS_24A",
		"/snap/core/1234", "/var/lib/docker/overlay2/x", "/boot/efi",
		// Observed on a real Mac on 2026-09-18, and not caught: Xcode mounts a
		// device image inside the developer's own home, so an absolute prefix
		// starting at "/Library" never matches it.
		"/Users/vincentlauriat/Library/Developer/CoreDevice/DeviceFS",
		"/Users/someone/Library/Developer/CoreSimulator/Volumes/iOS_25A",
		"/Users/someone/Library/Developer/XCTestDevices/x"}
	for _, m := range noisy {
		if !noisyMount(m) {
			t.Errorf("%s should be filtered out", m)
		}
	}
	// "/System/Volumes/Data" is deliberately filtered: on macOS it is the same
	// APFS container as "/", which is kept.
	keep := []string{"/", "/home", "/data", "/mnt/backup", "/Volumes/Backup", "/var/lib/mysql",
		// The developer rule matches Apple's tooling directory, not the word
		// "Developer" anywhere in a path: a volume somebody named that is a
		// disk they chose to mount and want to see.
		"/Volumes/Developer", "/mnt/Developer/projects", "/srv/library/developer-docs"}
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
