# Security

Ferngeist Gateway is designed to keep the public surface small and to make remote access explicit.

## Trust model

- The gateway runs on a machine you control.
- Public clients must pair before they can use protected endpoints.
- The admin API is local-only and is intended for the machine operator.

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

## Secrets and storage

- Device credentials are hashed before being stored.
- Pairing data and runtime state are stored in SQLite.
- Runtime bearer tokens are short-lived.

## Operational notes

- `daemon install` registers the extracted binary as a background service.
- In public mode, proof-of-possession is required by default.
- Legacy bearer-only credentials are disabled by default in public mode.

## Workspace read surface

The workspace endpoints (`/v1/runtimes/{id}/files`, `/git/status`, `/git/diff`)
let a paired client read files and git state inside the project the agent is
working on.

- **Auth:** all three require the `read` scope (same gate as runtime logs).
- **Read-only:** no endpoint writes — no file writes, no `git add`/`commit`.
- **Path confinement:** file and diff access is confined to the ACP **project
  directory** captured from the client's `session/new` (`params.cwd`, re-captured
  from `session/load` on resume). Absolute paths, `..` traversal, and symlink
  escapes are rejected with `400`; the root and the resolved target are both
  symlink-checked.
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
