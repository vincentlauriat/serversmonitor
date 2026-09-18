# Getting started

## 1. Run the hub

```
docker run -d --name smhub -p 8090:8090 -v smhub-data:/data serversmonitor/hub
```

Open <http://localhost:8090> and create the admin account. That account is the only one.

For agents on other machines, put the hub behind HTTPS (Caddy, Traefik, nginx) and set
`SM_SECURE_COOKIES=true`. Agents then connect with `wss://`. The agent refuses a plain `ws://`
to anything but the local machine unless you pass `--insecure`, because the token travels in a
header.

## 2. Add a host

Settings → Hosts → **Add host**. Copy the command shown once, run it on the machine:

```
curl -fsSL https://hub.example/install.sh | sudo sh -s -- --hub wss://hub.example --token <token>
```

Linux gets a systemd unit (`smagent.service`), macOS a LaunchDaemon. Docker container metrics
appear on their own when the agent can read `/var/run/docker.sock`; if it cannot, the section
reads *no container data* rather than showing an empty list.

## 3. Alerts

Four rules exist out of the box: CPU above 90 % for 10 minutes, memory above 90 % for 10 minutes,
disk above 90 % for 30 minutes, temperature above 80 °C for 5 minutes. A fifth is built in and
cannot be deleted: a host that misses three intervals is offline.

Muting a host silences it completely, thresholds included. Use it for a laptop that is switched
off every evening.

Rules are evaluated once a minute, so a rule you just created can take up to a minute to fire.
An offline transition does not wait for that tick.

## Environment

| Variable | Default | Meaning |
|---|---|---|
| `SM_LISTEN` | `:8090` | hub listen address |
| `SM_DATA_DIR` | `./data`, `/data` in Docker | where the SQLite file lives |
| `SM_SECURE_COOKIES` | `false` | set to `true` behind HTTPS |
| `SM_LOG_LEVEL` | `info` | debug, info, warn, error |
| `SM_HUB` | — | agent: hub URL |
| `SM_TOKEN` | — | agent: the host token |
| `SM_INSECURE` | — | agent: `1` allows plain `ws://` to a remote host |
| `SM_DOCKER_SOCKET` | `/var/run/docker.sock` | agent: empty disables container metrics |

## Building from source

```
make web      # builds the SvelteKit front and copies it where Go embeds it
make build    # bin/smhub and bin/smagent
make test     # go test ./...
make release  # cross-compiled binaries in release/
```

Node is needed only to build the front. The hub binary carries it, so there is nothing to serve
separately and nothing to install at runtime.
