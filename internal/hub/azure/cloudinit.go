package azure

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// CloudInit returns the base64 customData that installs the agent and points
// it at the hub.
//
// The token is a parameter and never a field of anything: nothing in this
// package holds it after this function returns. It reaches the machine in a
// file with mode 0600 rather than on a command line, because a command line is
// visible in `ps` and, worse, in /var/log/cloud-init-output.log — which is
// world-readable and stays there.
//
// Its blast radius is one host's metrics, and rotating it is one click
// (POST /api/v1/hosts/{id}/token). That is the whole mitigation, and it is
// stated rather than glossed: anyone who can log into the VM can read the
// token at /etc/smagent/env, which is the same trust boundary as the agent
// binary itself.
func CloudInit(hubURL, token string) (string, error) {
	if strings.TrimSpace(token) == "" {
		return "", errors.New("azure: refusing to build cloud-init without a host token; " +
			"the machine would boot, install the agent and never be accepted")
	}
	base := strings.TrimSuffix(strings.TrimSpace(hubURL), "/")
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("azure: the hub address %q is not a URL", hubURL)
	}
	ws := "wss://" + u.Host + u.Path
	insecure := ""
	if u.Scheme == "http" {
		// The agent refuses plain ws:// to a remote host unless told to, and
		// it is right to. Saying so out loud beats a machine that installs
		// cleanly and then loops on a connection error nobody reads.
		ws = "ws://" + u.Host + u.Path
		insecure = " --insecure"
	}

	// No --url: install.sh falls back to the GitHub release, which is where
	// the binary will be. Pointing it at an endpoint on the hub would be
	// inventing one — the hub serves /install.sh and nothing else. Publishing
	// that release is the open dependency of this whole lot, and it is in
	// TODOS rather than papered over here.
	doc := fmt.Sprintf(`#cloud-config
# Installs the ServersMonitor agent. The machine has no public IP: the agent
# dials out, so nothing here opens a port.
write_files:
  - path: /etc/smagent/token
    owner: 'root:root'
    permissions: '0600'
    content: %q
runcmd:
  - [ sh, -c, "curl -fsSL %s/install.sh | sh -s -- --hub %s --token-file /etc/smagent/token%s" ]
  - [ rm, -f, /etc/smagent/token ]
`, token, base, ws, insecure)

	return base64.StdEncoding.EncodeToString([]byte(doc)), nil
}
