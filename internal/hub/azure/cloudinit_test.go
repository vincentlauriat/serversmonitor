package azure

import (
	"encoding/base64"
	"strings"
	"testing"
)

const sentinelToken = "TOKEN-SENTINEL-8f2b1c"

func decodeCloudInit(t *testing.T, hubURL, token string) string {
	t.Helper()
	enc, err := CloudInit(hubURL, token)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("customData must be base64: %v", err)
	}
	return string(raw)
}

func TestCloudInitInstallsTheAgentAndPointsItAtTheHub(t *testing.T) {
	doc := decodeCloudInit(t, "https://monitor.example.net", sentinelToken)
	if !strings.HasPrefix(doc, "#cloud-config\n") {
		t.Fatalf("cloud-init ignores anything that does not start with #cloud-config:\n%s", doc)
	}
	if !strings.Contains(doc, "https://monitor.example.net/install.sh") {
		t.Fatalf("the installer must come from the hub itself:\n%s", doc)
	}
	// The agent dials out; https is wss, not https.
	if !strings.Contains(doc, "wss://monitor.example.net") {
		t.Fatalf("the agent must be told a websocket address:\n%s", doc)
	}
	if !strings.Contains(doc, sentinelToken) {
		t.Fatal("the token has to reach the machine somehow")
	}
}

func TestTheTokenNeverReachesTheProcessTable(t *testing.T) {
	// A token on a command line is visible in `ps` and in
	// /var/log/cloud-init-output.log, which is world-readable. It goes into a
	// file with mode 0600 instead, and the installer reads it from there.
	doc := decodeCloudInit(t, "https://monitor.example.net", sentinelToken)
	for _, line := range strings.Split(doc, "\n") {
		if strings.Contains(line, sentinelToken) && strings.Contains(line, "--token") {
			t.Fatalf("the token is on a command line: %q", line)
		}
	}
	if !strings.Contains(doc, "--token-file") {
		t.Fatalf("the installer must be pointed at the file:\n%s", doc)
	}
	if !strings.Contains(doc, "permissions: '0600'") {
		t.Fatalf("the token file must be 0600:\n%s", doc)
	}
}

func TestPlainHTTPBecomesWS(t *testing.T) {
	doc := decodeCloudInit(t, "http://10.1.2.3:8091", sentinelToken)
	if !strings.Contains(doc, "ws://10.1.2.3:8091") || strings.Contains(doc, "wss://10.1.2.3") {
		t.Fatalf("http must become ws, not wss:\n%s", doc)
	}
	// A hub on plain http is also one the agent must not refuse to trust.
	if !strings.Contains(doc, "--insecure") {
		t.Fatalf("a plain-http hub needs --insecure, or the agent will refuse it:\n%s", doc)
	}
}

func TestAnHTTPSHubDoesNotGetInsecure(t *testing.T) {
	doc := decodeCloudInit(t, "https://monitor.example.net", sentinelToken)
	if strings.Contains(doc, "--insecure") {
		t.Fatalf("--insecure must not be handed out for free:\n%s", doc)
	}
}

func TestATrailingSlashChangesNothing(t *testing.T) {
	a := decodeCloudInit(t, "https://monitor.example.net", sentinelToken)
	b := decodeCloudInit(t, "https://monitor.example.net/", sentinelToken)
	if a != b {
		t.Fatalf("a trailing slash produced a different document:\n%s\n---\n%s", a, b)
	}
}

func TestAnEmptyTokenIsRefused(t *testing.T) {
	// A VM that boots with no token dials in forever and is never accepted.
	// Failing here costs a second; failing there costs a machine.
	if _, err := CloudInit("https://monitor.example.net", ""); err == nil {
		t.Fatal("want an error")
	}
}
