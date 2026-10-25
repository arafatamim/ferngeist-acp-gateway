# Development

## Build

```powershell
go build ./...
```

## Run

```powershell
go run .\cmd\ferngeist daemon run
```

## Test

```powershell
go test ./...
```

## Optional local agent smoke test

```powershell
$env:FERNGEIST_RUN_REAL_AGENT_TESTS="1"
go test ./internal/runtime -run TestOptionalInstalledOpenCodeACPSmoke -v
```

## Multi-session E2E check

The multi-session-per-agent flow (two processes of the same agent, independent
resilient sessions, stop-all) is covered by unit/integration tests
(`TestStartNewLaunchesSeparateRuntime`, `TestCreateAllowsSameAgentParallelSessions`,
`TestAgentsListsMultipleRuntimesAndStartNew`). To verify against a **real
binary** end to end:

```bash
# 1. Build gateway + mock agent into a sandbox
go build -ldflags "-X main.buildVersion=dev" -o /tmp/fg/ferngeist-gateway ./cmd/ferngeist
go build -o /tmp/fg/sandbox/bin/mock-stdio-agent ./cmd/mock-stdio-agent

# 2. Run the daemon with the sandbox as its working directory (catalog detects
#    bin/mock-stdio-agent relative to cwd)
cd /tmp/fg/sandbox
FERNGEIST_GATEWAY_LISTEN_ADDR=127.0.0.1:5790 \
FERNGEIST_GATEWAY_ADMIN_ADDR=127.0.0.1:5791 \
FERNGEIST_GATEWAY_UPDATE_CHECK_ENABLED=0 \
  /tmp/fg/ferngeist-gateway daemon run

# 3. Drive it: POST /admin/v1/pairings/start (port 5791) -> GET the code ->
#    POST /v1/pair/complete -> POST /v1/agents/mock-acp/connect twice (each call
#    launches its own process) -> ACP over
#    GET /v1/acp/{runtimeId}?sessionId=&attachToken= -> POST stop -> confirm
#    GET /v1/agents shows both runtimes stopped but still tracked.
```

Success looks like: two distinct runtime IDs, two independent gateway sessions
serving concurrent ACP conversations, notifications forwarded on both pumps,
resume + `session/load` working after a disconnect, and stop-all terminating
both processes.

## Project listing measurements

Run the same opt-in measurement before and after a listing change:

```bash
FERNGEIST_MEASURE_PROJECTS=1 go test -count=1 ./internal/projects \
  -run '^TestProjectListingMeasurements$' -v > /tmp/projects-before.log
# Apply the listing change, then repeat with /tmp/projects-after.log.
```

The fixture builds only the mock agent and creates two temporary Git repositories.
Build and repository setup are outside the timed interval. Each of five repetitions
measures a fresh lister (cold), the same initialized process with its response cache
cleared (warm), an immediate cached call, and eight concurrent simulated devices
sharing a fresh lister. Clearing the response cache directly avoids waiting for its
10-second TTL; it does not discard the warm process. No real agents or daemon
services are used.

Each JSON record reports microseconds to the first encoded agent line and to all
calls completing, total encoded agent-line bytes, retained warm-process count,
sampled peak RSS of the Go test process, sampled peak RSS summed across its
descendants, and descendant RSS after the listing has gone idle. Concurrent time
to first result is the earliest device's result; completion covers all eight
devices and bytes are summed across them. Compare medians and ranges across the
five repetitions, rather than treating a single millisecond-scale sample as a
performance guarantee.

RSS sampling requires Linux procfs; unavailable values are `-1` on other platforms.
The 1ms sampler can miss short-lived children and adds measurement overhead. Child
peaks include temporary Git subprocesses; idle child RSS isolates the retained mock
agent. Summed RSS counts shared pages in every process and does not equal reclaimable
physical memory. The test process is a proxy for gateway memory, not a production
daemon measurement. Cold versus warm latency estimates the cost of recreating an
idle agent; no artificial host memory pressure is generated.

Listing responses remain fresh for 10 seconds. A lister-owned, one-second sweep
reclaims expired payloads while retaining known repository roots for snapshot
reconciliation. It also samples Linux host and cgroup-v2 memory
[PSI stall counters](https://docs.kernel.org/accounting/psi.html). An increase since
the previous sample reclaims one least recently used idle listing process; the
next sample determines whether to reclaim another. Borrowed listing connections
and chat/session processes are outside that idle pool. There is no process-count
cap. Where PSI is unavailable, including other operating systems, the existing
three-minute warm-process expiry still applies. The polling interval limits
sampling and discard work; it is not calibrated against real-agent workloads.

The package measurement excludes authentication, HTTP transport, the final
`checkouts` line, and device running-turn decoration. Measure the full endpoint
separately with a real loopback HTTP listener and eight paired test credentials:

```bash
FERNGEIST_MEASURE_PROJECTS=1 go test -count=1 ./internal/api \
  -run '^TestProjectListingHTTPMeasurements$' -v > /tmp/projects-http-after.log
```

This opt-in test builds the mock agent in a temporary directory, creates two Git
repositories, and runs three repetitions of single-device cold, immediate cached,
aged-cache, and eight-device concurrent cold requests. Each repetition waits
11 seconds outside the measured calls so the production response cache expires
while a retained process can remain warm. Actual host memory pressure can reclaim
it during that interval, so aged-cache does not guarantee warm-process reuse; the
package measurement isolates reuse by clearing the cache immediately. Linux idle
snapshots record surviving mock-child counts and RSS before the aged-cache request
and after each workload. The emitted records contain per-device time to the
first complete agent line, time through the final checkouts line and response EOF,
and the full NDJSON byte count. Each body must contain two chats and two checkouts.
Setup, pairing, fixture compilation and cache aging are excluded. The test runs no
real agents, services or host provisioning and does not simulate memory pressure.

The endpoint test measures latency and bytes; the package test supplies separate
sampled process/child RSS and retained-process counts. Neither substitutes for
production daemon measurements, real agent startup costs, mobile network latency,
proof-of-possession authentication, large chat inventories, or slow checkouts. Keep
before/after endpoint results separately: an endpoint run performed only after a
change supplies no endpoint baseline. No sandbox daemon setup is needed for either
opt-in test.

Measured on 2026-10-25, Linux amd64, Go `go1.27.1-X:nodwarf5`, with one mock
agent, two chats, and two Git repos. The package baseline preceded the queue,
cache-maintenance and pressure changes; the after run includes them. Values below
are median first-result / completion milliseconds across five repetitions. These
small samples show workload costs, not statistically established speed changes.

| Package workload | Before (ms) | After (ms) |
| --- | --- | --- |
| Cold | 3.171 / 3.173 | 3.044 / 3.046 |
| Warm | 1.838 / 1.839 | 1.800 / 1.801 |
| Cached | 1.613 / 1.616 | 1.670 / 1.672 |
| Eight-device cold batch | 3.558 / 4.440 | 3.805 / 4.226 |

Each agent line was 645 bytes/device and every package workload retained one warm
process. Maximum sampled test-process RSS across all workloads was
25,088,000 / 23,773,184 bytes before/after; summed child peaks were
27,123,712 / 74,764,288 bytes. Transient concurrent Git commands and sampling
timing make child peaks variable; these numbers do not establish memory savings.
Idle mock RSS was approximately 10–12 MB. RSS includes shared pages and the test
process includes fixture overhead, as described above.

The full HTTP run was **after-only**, three repetitions (24 per-device samples
for concurrent cold). Each successful response was 897 bytes, including checkouts.
Its median first-result / completion milliseconds were:

| HTTP workload | After (ms) |
| --- | --- |
| Cold | 4.238 / 8.666 |
| Cached | 1.871 / 7.242 |
| Aged-cache | 4.006 / 9.109 |
| Eight-device cold, per device | 4.086 / 12.326 |

All three idle snapshots before aged-cache had zero surviving mock children and
zero child RSS, compared with one child using about 12.1 MB after cold and after
aged-cache relisting. Connections closed about one second after becoming idle,
consistent with actual host PSI-driven eviction; no memory pressure was induced.
Consequently this endpoint run measures recreation after idle reclamation, while
the package warm scenario measures reuse. Run the two commands above to reproduce
the measurements; no HTTP baseline was captured.

## macOS launchd integration test

The launchd service manager (`internal/service/manager_darwin.go` +
`manager_darwin_common.go`) is verified against real launchd on GitHub Actions
`macos-latest` runners (which have a GUI login session) by
`TestLaunchdLifecycle`. Locally it requires a Mac with an active login
session:

```bash
FERNGEIST_RUN_REAL_AGENT_TESTS=1 go test ./internal/service -run TestLaunchdLifecycle -v
```

The test installs, restarts, stops, starts, reinstalls, and uninstalls the
LaunchAgent, cleaning up after itself. It is opt-in (env-gated) so ordinary
`go test ./...` runs stay hermetic.

## Self-update in dev builds

`ferngeist-gateway update` is gated on the build's `updateChannel` ldflag:
`""`/`"self"` installs may self-update, package-manager installs refuse. Dev
builds (`go run`) have `updateChannel` empty, so the command runs — but it
requires an installed daemon service and a tagged release, so it will fail with
a helpful error on a dev box (`daemon service is not installed`). To simulate a
package-manager install while developing:

```bash
go run -ldflags "-X main.buildVersion=0.0.0-dev -X main.updateChannel=brew" ./cmd/ferngeist update
```

## Notes

- The gateway is a local backend service for ACP-compatible agents.
- It exposes a unified WebSocket API.
- It is used as the backend for the Ferngeist Android app.
