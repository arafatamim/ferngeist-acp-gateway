# Pairing

Ferngeist Gateway supports two pairing flows:

- **Public API pairing** for paired clients
- **Admin API pairing** for local setup and management

## Public pairing

Use the public API when a client app needs to pair with a gateway remotely.

Flow:

1. Start a pairing challenge.
2. Enter the pairing code in the client app.
3. Complete pairing with the challenge ID, code, and device name.
4. Receive a device credential.

Public pairing does not expose the pairing code.

## Admin pairing

Use the admin API when you are pairing from the machine that runs the daemon.

Flow:

1. Start pairing from the local admin surface.
2. Read the pairing code or deep-link payload.
3. Complete pairing from the client app.
4. Confirm the device is listed as paired.

Admin pairing is intended for local operator use.

## Device credentials

Paired devices receive a credential that is used to authenticate API requests.

Notes:

- Credentials can expire.
- Credentials can be refreshed.
- Revoking a device invalidates its credential.

## Proof of possession

Some deployments require proof-of-possession pairing.

In that mode:

- the pairing flow binds the credential to a proof key
- API requests must include signed proof headers
- legacy bearer-only credentials may be disabled

## Common operations

- start pairing
- check pairing status
- complete pairing
- refresh a device token
- revoke a paired device

## Related docs

- [docs/api.md](docs/api.md)
- [docs/usage.md](docs/usage.md)
- [docs/configuration.md](docs/configuration.md)
