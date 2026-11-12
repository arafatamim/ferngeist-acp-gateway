# Security

Ferngeist Gateway is designed to keep the public surface small and to make remote access explicit.

## Trust model

- The gateway runs on a machine you control.
- Public clients must pair before they can use protected endpoints.
- The admin API is local-only and is intended for the machine operator. It
  refuses requests with an `Origin` header or a `Host` that is not an IP
  literal or `localhost` (`403`), so web pages cannot reach it through
  cross-site requests or DNS rebinding.

## Public API security

The public API requires authentication for most useful actions.

### Paired device credentials

- Pairing issues a device credential.
- Credentials can expire.
- Credentials can be refreshed.
- Revoking a device invalidates its credential.

### Scopes

Some endpoints require additional scopes such as:

- `read`
- `control`

Pairing grants **every** paired device both `read` and `control`; there is no
read-only credential. `gateway.diagnostics.export` and
`gateway.runtime.restart_env` are granted only when
`FERNGEIST_GATEWAY_ALLOW_REMOTE_DIAGNOSTICS_EXPORT` /
`FERNGEIST_GATEWAY_ALLOW_REMOTE_RUNTIME_RESTART_ENV` are enabled.

## Proof of possession

In public mode, the gateway can require proof-of-possession.

When enabled:

- pairing binds the device credential to a proof key
- requests must include signed proof headers
- bearer-only credentials may be rejected unless explicitly allowed

## Remote access

If you expose the gateway outside your local network:

- use a tunnel or reverse proxy
- set the public URL correctly
- prefer HTTPS
- keep the admin API bound to localhost

Tailscale specifics:

- Tailscale Funnel exposes the gateway to the public internet with a stable
  URL. The gateway's own security model is unchanged: proof-of-possession
  pairing is enforced, device credentials are hashed, and the admin API
  remains bound to localhost.
- Tailscale Serve (or `FERNGEIST_GATEWAY_TAILSCALE_PRIVATE=1`) keeps the
  gateway reachable only from the tailnet — the more private option when the
  client app runs Tailscale too.
- Enabling any Tailscale mode turns the public-mode security defaults on from
  the first boot, even before the public URL is known: proof-of-possession
  required, legacy bearer-only credentials disabled.

See [docs/remote-access.md](docs/remote-access.md) for tunnel and proxy options.

## Diagnostics

Remote diagnostics export is disabled by default unless enabled in configuration.

This helps avoid exposing logs and runtime details unless you explicitly want that.

Shared HTTP request logs replace `path` and `acpSessionId` query values with
`redacted`, as they do for `access_token`, `token`, and `authorization`.
Matching is case-insensitive and covers duplicate keys and percent-encoded
selectors. If query parsing fails, the entire query is logged as the fixed
`redacted` marker, never as raw input. This does not add a raw-path audit event.
The redaction applies to HTTP request logs only: optional raw ACP frame logging
may still contain paths, ACP session IDs, and other sensitive content.

## Secrets and storage

- Device credentials are hashed before being stored.
- Pairing data and runtime state are stored in SQLite.

## Operational notes

- `daemon install` registers the extracted binary as a background service.
- In public mode, proof-of-possession is required by default.
- Legacy bearer-only credentials are disabled by default in public mode.

## Artifact retrieval

Pairing grants both `read` and `control`; a paired client can register a custom
agent with an arbitrary command and thereby execute code as the gateway OS user.
Artifact advertisement is a disclosure/provenance filter, not an OS sandbox or
an allowed-roots policy. Prompt-injected or malicious agents can advertise any
path the gateway user can read. Pair only trusted devices and agents.

The public `GET`/`HEAD /v1/runtimes/{runtimeId}/artifacts` route binds verified
DeviceID ownership before inspecting accepted ACP sessions or retained history.
Foreign, unknown and closed contexts return indistinguishable `404` responses.
Evidence includes structured local resource links/embedded resource URIs in
eligible agent messages or tool content, plus deliberately delimited exact
local paths/file URIs in agent-message text (including Markdown links).
Complete HTTP/HTTPS URL spans are excluded before local token extraction.
User/thought messages, tool text, titles, diffs and arbitrary JSON never grant.
Only adjacent text with the same bounded, nonempty message identity and ACP
session joins. Internal history markers preserve client/prompt/reply/load and
non-message boundaries, including requests answered by the gateway. Missing
identity never permits concatenation; unknown/evicted message edges fail closed.
An interruption or dropped stdout frame forbids joining but cannot establish a
message edge. Only a correlated prompt reply or adjacent new identified message
can complete an otherwise un-delimited token.
Ambiguous punctuation/whitespace needs a structured URI, not guessed stripping.
Retained replay history remains the sole evidence source; markers are evicted
with frames, and no file lookup or grant registry runs on the pump. Eviction,
close and restart deny new requests; accepted agent replay may advertise again.
These checks do not prove an agent chose a path safely: prompt injection can
still deliberately surface a host path.

Linux/macOS opening canonicalizes the parent at fetch time, rejects supplied
and canonical `/proc`, `/sys`, `/dev` trees, roots relative operations at the
opened parent, rejects final symlinks and nonregular files, and checks open-file
identity and size. A nonblocking basename open avoids a raced FIFO hang. These
checks protect one open, not the time since advertisement: grants refer to
current path contents, parent symlinks may have changed before fetch, and files
can mutate during transfer. Bind mounts and hostile local namespace races are
not fully excluded. No immutable snapshots or content validators are promised.
Windows remains unsupported and fails closed without file access.

Four global/two per-device slots bound the entire admission/open/transfer
lifetime, including HEAD; files over 512 MiB are refused. Credential, ownership
and retained evidence checks apply at admission only. An admitted response may
continue after revocation, expiry, evidence eviction or session close for up to
ten minutes; new requests are denied. Cancellation/disconnect/shutdown releases
the descriptor and capacity. All responses are no-store/nosniff, attachment
headers expose only a basename, and errors omit raw paths. Shared request-log
redaction covers the selectors; optional raw ACP frame logs remain outside it.
Existing workspace confinement and cross-device turns behavior are unchanged.

## Workspace read surface

The workspace endpoints (`/v1/runtimes/{id}/files`, `/files/list`, `/git/status`,
`/git/diff`, `/git/log`, `/git/refs`, `/git/commits/{sha}`) let a paired client
read files and git state inside the project the agent is working on.

The session-free `/v1/projects/git/{status,diff,log,refs}`,
`/v1/projects/git/commits/{sha}`, and `/v1/projects/files` routes accept an
absolute checkout-root `cwd` without an ACP session. **Any paired client with
`read` scope can select any checkout the gateway OS user can read.** The root
check rejects subdirectories and bare repositories and canonicalizes symlinks;
it is not an authorization allowlist. Relative file paths remain confined to
that canonical checkout. Project routes reject `acpSessionId`, and runtime
routes reject `cwd`, so selectors cannot silently override one another.

- **Auth:** all of them require the `read` scope (same gate as runtime logs).
- **Read-only:** no endpoint writes — no file writes, no `git add`/`commit`.
- **Path confinement:** runtime file, listing, and diff access is confined to the ACP
  **project directory** captured from the client's `session/new`
  (`params.cwd`, re-captured from `session/load` on resume). Absolute paths,
  `..` traversal, and symlink escapes are rejected with `400`; the root and the
  resolved target are both symlink-checked.
- **No git repo / no git binary:** `422` — the directory simply is not a git
  repository or `git` is not on PATH; nothing is exposed either way.
- **Untrusted directories:** a malicious or buggy agent project directory could
  contain hostile files or a deliberately slow/hung git repo. File reads are
  capped (1 MiB) and git commands run under a 15s timeout, so neither can stall
  a handler; content is returned verbatim and clients must treat it as untrusted
  (e.g. never render HTML from a workspace file).

## Related docs

- [docs/api.md](docs/api.md)
- [docs/pairing.md](docs/pairing.md)
- [docs/configuration.md](docs/configuration.md)
- [docs/remote-access.md](docs/remote-access.md)
