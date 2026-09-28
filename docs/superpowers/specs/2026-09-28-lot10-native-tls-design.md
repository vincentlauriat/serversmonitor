# ServersMonitor — Lot 10: native TLS

**Date**: 2026-09-28
**Status**: design, scope chosen by Vincent on 2026-09-28 (certificate files, ACME, and a :80 redirect)
**Scope**: this repository. Until now the hub assumed a reverse proxy for HTTPS.

## 1. What is missing

Agents on other machines need `wss://`, and a login over plain HTTP sends the password in clear.
The only answer was a reverse proxy: one more service to run for a hub whose point is to be two
binaries.

## 2. Configuration

| Variable | Effect |
|---|---|
| `SM_TLS_CERT`, `SM_TLS_KEY` | mode `files`: both or neither |
| `SM_TLS_DOMAINS` | mode `acme`: a comma list, lowercased; exclusive with the files |
| `SM_TLS_EMAIL`, `SM_ACME_DIRECTORY` | ACME contact, and a directory other than Let's Encrypt production |
| `SM_LISTEN` | `:8090` without TLS (unchanged), `:443` with |
| `SM_HTTP_LISTEN` | with TLS, `:80` by default, `off` for none; refused without TLS |

With TLS on, cookies are Secure whatever `SM_SECURE_COOKIES` says: the hub knows it serves HTTPS.

## 3. Files

The certificate is loaded at startup, so a wrong path fails there. It is re-read when either file's
modification time changes, checked at most every ten seconds so a busy hub does not stat on every
handshake. **A renewal that cannot be read keeps the certificate that works**: a half-written file
must not take HTTPS down.

## 4. ACME

`golang.org/x/crypto/acme/autocert`, already one module away (`x/crypto` was a dependency; `x/net`
comes with it). Host policy: the listed domains only, so a scan with a random SNI never asks Let's
Encrypt for anything. Cache: `<data dir>/acme`, beside the database, so a restarted container does
not hit the rate limits. TLS-ALPN-01 is served on the HTTPS port itself; HTTP-01 on the plain
listener when it exists.

## 5. The plain listener

Redirects to the same host and path over HTTPS, keeping the port only when HTTPS is not on 443.
`GET` and `HEAD` get a 301; anything else a 308, so a client that posts keeps its method and body
rather than being turned into a GET. In ACME mode it answers `/.well-known/acme-challenge/` first.

## 6. Tests

Configuration modes and misconfigurations; the redirect's ports, query strings and codes; the
reloader picking up a renewal after ten seconds and keeping a working certificate over a broken
one; a real TLS handshake on a certificate from disk; the ACME wiring (acme-tls/1 offered, challenge
path not redirected, an unlisted name refused). Not testable here: a real Let's Encrypt issuance,
which needs a public name.

## 7. Out of scope

Mutual TLS for agents. HSTS. Serving several certificates for several names in files mode.
