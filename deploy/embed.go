// Package deploy embeds the agent install script so the hub can serve it at /install.sh.
package deploy

import _ "embed"

//go:embed install.sh
var InstallScript []byte
