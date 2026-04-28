# Configuration

Ferngeist Gateway is configured through environment variables and persisted state.

## Important settings

| Variable | Purpose | Default |
|---|---|---|
| `FERNGEIST_GATEWAY_LISTEN_ADDR` | Public API listen address | `127.0.0.1:5788` |
| `FERNGEIST_GATEWAY_ADMIN_ADDR` | Admin API listen address | `127.0.0.1:5789` |
| `FERNGEIST_GATEWAY_ENABLE_LAN` | Enable LAN discovery and access | `false` |
| `FERNGEIST_GATEWAY_PUBLIC_BASE_URL` | Public URL used for remote access | unset |
| `FERNGEIST_GATEWAY_STATE_DB` | SQLite state database path | platform default |
| `FERNGEIST_GATEWAY_LOG_DIR` | Log directory | platform default |
| `FERNGEIST_GATEWAY_MANAGED_BIN_DIR` | Managed binary install directory | platform default |
| `FERNGEIST_GATEWAY_REGISTRY_URL` | ACP registry URL | ACP default |
| `FERNGEIST_GATEWAY_CREDENTIAL_TTL_SECONDS` | Paired device token lifetime | `7 days` |
| `FERNGEIST_GATEWAY_REQUIRE_PROOF_OF_POSSESSION` | Require proof-of-possession pairing | `true` in public mode |
| `FERNGEIST_GATEWAY_ALLOW_LEGACY_BEARER_CREDENTIALS` | Allow legacy bearer-only credentials | `false` in public mode |

## Pairing controls

| Variable | Purpose |
|---|---|
| `FERNGEIST_GATEWAY_PAIRING_ARM_TTL_SECONDS` | How long a locally approved pairing stays valid |
| `FERNGEIST_GATEWAY_PAIRING_MAX_ATTEMPTS` | Max failed pairing attempts before lockout |
| `FERNGEIST_GATEWAY_PAIRING_LOCKOUT_SECONDS` | Lockout duration after too many failures |
| `FERNGEIST_GATEWAY_PAIRING_START_REFILL_SECONDS` | Rate limit refill for pairing start |
| `FERNGEIST_GATEWAY_PAIRING_COMPLETE_REFILL_SECONDS` | Rate limit refill for pairing complete |
| `FERNGEIST_GATEWAY_PAIRING_BURST_PER_IP` | Per-IP burst limit |
| `FERNGEIST_GATEWAY_PAIRING_BURST_GLOBAL` | Global burst limit |

## Notes

- `daemon install` registers the extracted binary as a background service.
- `PUBLIC_BASE_URL` should match the URL clients use to reach the gateway.
- In public mode, proof-of-possession is required unless legacy bearer credentials are explicitly enabled.
- Exact defaults can vary by platform and release build.