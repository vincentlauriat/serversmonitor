#!/bin/sh
# ServersMonitor agent installer. Usage:
#   curl -fsSL https://<hub>/install.sh | sudo sh -s -- --hub wss://<hub> --token <token> [--url <binary base url>] [--insecure]
#
# --token-file <path> reads the token from a file instead. Prefer it when
# something else writes the token: a token on a command line is visible in
# `ps` and lands in cloud-init's world-readable output log.
set -eu

HUB=""; TOKEN=""; INSECURE=""
BASE_URL="${SM_RELEASE_URL:-https://github.com/vincentlauriat/serversmonitor/releases/latest/download}"
while [ $# -gt 0 ]; do
  case "$1" in
    --hub) HUB="$2"; shift 2 ;;
    --token) TOKEN="$2"; shift 2 ;;
    --token-file) TOKEN=$(cat "$2"); shift 2 ;;
    --url) BASE_URL="$2"; shift 2 ;;
    --insecure) INSECURE="1"; shift ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done
[ -n "$HUB" ] && [ -n "$TOKEN" ] || { echo "usage: install.sh --hub <url> (--token <token> | --token-file <path>)" >&2; exit 2; }

OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "unsupported architecture: $ARCH" >&2; exit 1 ;;
esac
case "$OS" in linux|darwin) ;; *) echo "unsupported OS: $OS" >&2; exit 1 ;; esac

BIN=/usr/local/bin/smagent
echo "downloading smagent-$OS-$ARCH"
curl -fsSL "$BASE_URL/smagent-$OS-$ARCH" -o "$BIN.tmp"
chmod 755 "$BIN.tmp"
mv "$BIN.tmp" "$BIN"

if [ "$OS" = linux ]; then
  mkdir -p /etc/smagent
  printf 'SM_HUB=%s\nSM_TOKEN=%s\nSM_INSECURE=%s\n' "$HUB" "$TOKEN" "$INSECURE" > /etc/smagent/env
  chmod 600 /etc/smagent/env
  cat > /etc/systemd/system/smagent.service <<UNIT
[Unit]
Description=ServersMonitor agent
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=/etc/smagent/env
ExecStart=$BIN
Restart=always
RestartSec=5
User=root

[Install]
WantedBy=multi-user.target
UNIT
  systemctl daemon-reload
  systemctl enable --now smagent
  echo "smagent installed and started (systemctl status smagent)"
else
  PLIST=/Library/LaunchDaemons/dev.serversmonitor.agent.plist
  cat > "$PLIST" <<PL
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>dev.serversmonitor.agent</string>
  <key>ProgramArguments</key><array><string>$BIN</string></array>
  <key>EnvironmentVariables</key><dict>
    <key>SM_HUB</key><string>$HUB</string>
    <key>SM_TOKEN</key><string>$TOKEN</string>
    <key>SM_INSECURE</key><string>$INSECURE</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
</dict></plist>
PL
  chmod 644 "$PLIST"
  launchctl bootout system "$PLIST" 2>/dev/null || true
  launchctl bootstrap system "$PLIST"
  echo "smagent installed as a LaunchDaemon"
fi
