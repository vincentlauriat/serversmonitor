# ServersMonitor — Lot 11: several people, Microsoft Entra ID, two roles

**Date**: 2026-09-28
**Status**: design, choices made by Vincent on 2026-09-28
**Scope**: this repository. The lot 1 spec deferred multi-user and Entra ID, with the session layer
designed so a second identity provider could arrive without touching `/api/v1`.

## 1. The choices

| Question | Answer |
|---|---|
| Who may sign in with Entra? | **Addresses an admin lists.** The users table is the allow list. |
| Roles? | **Admin and read-only.** |
| The local account? | **Kept as the way in** when Entra is misconfigured or down. |

## 2. Data (migration 10)

`users.role` (`admin` | `viewer`, default `admin`) and `users.provider` (`local` | `entra`, default
`local`): the account made at setup keeps every right. An Entra account is a row with an empty
password hash, added before its owner ever signs in. Removing a row removes its sessions in the same
transaction. The last local admin can be neither removed nor demoted.

## 3. Read-only, in one place

`server.auth` refuses any method other than GET and HEAD to a viewer, logout aside. Checking per
handler would leave the next write route open until someone remembered; checking in the wrapper
closes it by default.

## 4. The Microsoft sign-in

- **Configuration** in settings: tenant (a GUID), client id, client secret (write-only, like the
  SMTP password). The redirect URI is `<public URL>/api/v1/auth/entra/callback`, the public URL being
  the one set under Notifications.
- **Why a GUID**: Entra signs every tenant's tokens with the same keys, so the signature says nothing
  about the tenant, and `tid` can only be compared with an id, not a domain. `common`,
  `organizations` and `consumers` are refused with their own sentence.
- **Start**: state, nonce and PKCE verifier kept in memory for ten minutes; the state also in a
  `SameSite=Lax` cookie scoped to `/api/v1/auth/entra/` (a Strict cookie is not sent on the hop back
  from Microsoft).
- **Callback**: the state must match the cookie and exist in memory, and is removed before use, so a
  replay finds nothing. The code is exchanged with the verifier and the secret; the ID token is
  verified by hand (RS256 only, key from the tenant's JWKS, cached an hour and re-read for an unknown
  kid; issuer, audience, `tid`, expiry and not-before with five minutes of leeway, nonce).
- **Then**: the e-mail (`email`, else `preferred_username`, lowercased) must be an `entra` row. A
  local account's address coming through Microsoft is refused like an unknown one, with the same
  words, so the page does not tell which addresses exist.
- **Landing**: a tiny HTML page that navigates to `/`, not a 302, so the Strict session cookie is
  sent on the first load. Failures land on `/login?error=…`, shown as text.

## 5. A fix on the way

The password route replaced an empty hash by the dummy hash of a fixed word, for constant timing.
With password-less accounts arriving, that word would have opened them. The route now refuses any
account that is not `local`, after the same dummy verification.

## 6. Tests

Store: roles, duplicates, the last local admin, sessions gone with the user. Entra: a full sign-in
against a fake authority, a replayed state, Entra's own error text, and a refusal for each of expired,
wrong nonce, wrong audience, other tenant, forged issuer, no e-mail, another key, `alg: none`.
Server: a viewer reads and is refused eight kinds of write, keeps logout; an Entra account cannot use
the password route, `placeholder` included; the users API; the Entra settings; a full sign-in through
the hub's routes, then a stranger and the local admin's address refused; a callback without the state
cookie. Not testable here: a real Entra tenant.

## 7. Out of scope

Finer rights (per host, per resource). Entra groups or app roles. SCIM or any provisioning. A second
local account.
