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

## Related docs

- [docs/api.md](docs/api.md)
- [docs/pairing.md](docs/pairing.md)
- [docs/configuration.md](docs/configuration.md)
- [docs/remote-access.md](docs/remote-access.md)
