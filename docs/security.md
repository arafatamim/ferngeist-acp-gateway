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

## File retrieval

`GET`/`HEAD /v1/files?path=<absolute-host-path>` reads any regular file the
gateway OS user can read, subject to the file checks below. It requires `read`
and `control` plus proof of possession. Pairing grants every device both, and a
`control` device can already make an agent (or a custom agent command) read any
such file, so this route adds no access a paired device lacks. It is closed to
any future credential without `control`. It keeps no per-file state:
nothing about agent output is recorded or consulted, so retrieval does not
expire with session history. Pair only trusted devices.

Linux/macOS opening canonicalizes the parent at fetch time, rejects supplied
and canonical `/proc`, `/sys`, `/dev` trees, roots relative operations at the
opened parent, rejects final symlinks and nonregular files, and checks open-file
identity and size. A nonblocking basename open avoids a raced FIFO hang. These
checks protect one open: a path refers to current contents, parent symlinks may
change between requests, and files can mutate during transfer. Bind mounts and hostile local namespace races are
not fully excluded. No immutable snapshots or content (strong) validators are promised.
Windows uses a separate native handle traversal. Pure path checks reject
UNC/device/NT/drive-relative/ADS/reserved/trailing-dot-space forms first.
The current DOS drive mapping must name a direct local HarddiskVolume device;
network, SUBST and ambiguous mappings are refused. Type checks use that pinned
volume, never a later lookup of the mutable drive alias. Native relative opens
forbid reparse processing and inspect every component before continuing;
symlinks, junctions, mount points and cloud reparse placeholders are unsupported.
Ancestors and the inspected file deny write/delete sharing while descendants
and the final read handle are opened. Handle-derived file identity/type/size
are rechecked; sharing conflicts and oplock breaks return 409. Native opens
request immediate completion rather than waiting for oplock acknowledgment;
an alternate-success handle is closed without reading. Afterwards the read handle allows
mutable contents, preserving admission-only semantics.

This follows the handle-relative and reparse-open contracts in
[Microsoft NtCreateFile](https://learn.microsoft.com/en-us/windows/win32/api/winternl/nf-winternl-ntcreatefile)
and the current-mapping semantics of
[QueryDosDevice](https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-querydosdevicew).
It does not defend against privileged local replacement of device objects or
hostile filesystem drivers. Native Windows tests are configured in CI but have
not run in this Linux implementation session; cross-compilation/lint is not
native proof. Other platforms remain unsupported and fail closed.

Single byte ranges use the same admission and file checks; even a one-byte
request is rejected if the full file exceeds 512 MiB. Ranged reads freeze their
offset/length to admitted size but can still fail short if the file shrinks.
Invalid or multiple byte ranges return 416 with no-store/nosniff; ranges in other
units are ignored. Successful responses carry a weak ETag hashed from file
identity, size, modification and change times, plus Last-Modified, and
`private, no-cache`, so a client may store a copy but must revalidate it.
`If-None-Match` can return 304; equal metadata cannot prove equal bytes, which
is why the tag is weak. Any If-Range header still forces a full 200, including
after same-second mutation, because a weak tag cannot satisfy it. Current-file ranges are not immutable
resumable versions; clients must reject short bodies and avoid splicing separate
responses together.

Four global/two per-device slots bound the entire admission/open/transfer
lifetime, including HEAD; files over 512 MiB are refused. Credential and scope
checks apply at admission only. An admitted response may continue after
revocation or expiry for up to ten minutes; new requests are denied. Cancellation/disconnect/shutdown releases
the descriptor and capacity. All responses are nosniff (errors also no-store), attachment
headers expose only a basename, and errors omit raw paths. Shared request-log
redaction covers the `path` selector; optional raw ACP frame logs remain outside it.

`POST /v1/files/stat` applies the same scopes, path validation and open checks
to up to 64 paths per request, opening and closing each in turn without
reading beyond the 8000-byte content-type sniff. It returns size and type only,
takes no transfer slot, and its per-path error codes omit host paths beyond
echoing the requested one in `uri`.
Existing workspace confinement and cross-device turns behavior are unchanged.

## Workspace read surface

The workspace endpoints (`/files/list`, `/git/status`, `/git/diff`, `/git/log`,
`/git/refs`, `/git/commits/{sha}`) let a paired client list files and read git
state inside the project the agent is working on. File contents come only from
`/v1/files` (above).

The session-free `/v1/projects/git/{status,diff,log,refs}`,
and `/v1/projects/git/commits/{sha}` routes accept an
absolute checkout-root `cwd` without an ACP session. **Any paired client with
`read` scope can select any checkout the gateway OS user can read.** The root
check rejects subdirectories and bare repositories and canonicalizes symlinks;
it is not an authorization allowlist. Relative diff paths remain confined to
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
