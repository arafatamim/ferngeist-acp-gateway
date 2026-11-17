# Pairing

Ferngeist Gateway supports two pairing flows:

- **Public API pairing** for a client enrolling a new device
- **Admin API pairing** for local setup and management

## Public pairing

Use the public API when a client app needs to pair with a gateway remotely.

Flow:

1. Arm pairing locally with `ferngeist-gateway pair` or the admin API.
2. Start a pairing challenge while the pairing window is armed.
3. Enter the pairing code in the client app.
4. Complete pairing with the challenge ID, code, and device name.
5. Receive a device credential.

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
- Credentials can be recovered after expiry for a grace period: an expired
  credential that proves possession of its device key (proof-of-possession)
  can refresh within a sliding window (`FERNGEIST_GATEWAY_CREDENTIAL_GRACE_SECONDS`,
  default 90 days). Each refresh resets the window. Once the grace period
  lapses, the device must re-pair.
- Each device pairs independently with its own credential and proof key;
  refreshing one device never affects another device's credential.
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

- [docs/api.md](api.md)
- [docs/usage.md](usage.md)
- [docs/configuration.md](configuration.md)
