# Obscura Runtime Contract v1

## Purpose

This document fixes the provider boundary for Obscura's first multi-runtime
integration milestone. Obscura exposes one provider-neutral control and data
plane. Provider adapters translate that contract to a pinned upstream release;
they do not expose provider-specific credentials, session identifiers, or
network endpoints to callers.

The target providers are:

| Provider | Repository | Pinned release | Resolved commit | Adapter status |
| --- | --- | --- | --- | --- |
| AgentENV | `kvcache-ai/AgentENV` | `v0.2.3` | `6cccaa7842bd5be2051111d4f74d9e37aa721244` | lifecycle + execute/files contract tests pass; deployment admission pending |
| OpenSandbox | `opensandbox-group/OpenSandbox` | `release-1.1.1` | `531700ca18a7fa5fab6a2f25d6dc68a76eddab12` | contract review required |
| CubeSandbox | `TencentCloud/CubeSandbox` | `v0.7.3` | `4411c96ba8658a959de34693f54f4918db2a432f` | contract review required |

AgentENV `v0.2.3` is an annotated tag: tag object
`4890d5e32e34043f97e9b692cf349aca91b9be82` resolves to the commit shown above.
Pins for deployment and upgrade review use the resolved commit, while release
provenance records both values.

These are release pins, not claims that the current Obscura implementation is
compatible with them. A provider is admitted only after the contract tests in
this document pass against the pinned deployment.

The three releases expose different wire boundaries. Adapter configuration
therefore has a `controlEndpoint` and an optional `dataEndpoint`; lifecycle and
execution must not be assumed to share a host, authentication header, or
OpenAPI document. A data endpoint may also be resolved from the created
sandbox response. Endpoint and credential references are persisted; credential
values and short-lived session tokens are not.

Upstream references:

- [AgentENV v0.2.3](https://github.com/kvcache-ai/AgentENV/releases/tag/v0.2.3)
- [OpenSandbox 1.1.1](https://github.com/opensandbox-group/OpenSandbox/releases/tag/release-1.1.1)
- [CubeSandbox v0.7.3](https://github.com/TencentCloud/CubeSandbox/releases/tag/v0.7.3)

## Architecture

```text
Client
  -> Obscura Gateway (stable /v1 API)
      -> Kubernetes Sandbox desired state (lifecycle)
      -> Runtime data-plane calls (execute/files/pty/ports)
              -> generic HTTP runtime adapter
                  + provider profile/codec/hooks
                  -> provider control API
                  -> provider session/envd/exec API
```

The Controller owns asynchronous lifecycle reconciliation, durable bindings,
expiry, finalizers, uncertain-create handling, and confirmed deletion. It must
not carry long-lived execution streams. Gateway or a separately deployable
data-plane service owns execution streams and interactive sessions.

The provider session ID is an adapter-internal implementation detail. The
caller sees only an Obscura `sandboxID` and `executionID`; credentials, session
tokens, provider URLs, and provider handles are never returned in the public
API or written to ordinary Sandbox status.

Kubernetes CRD/operator-only projects are not runtime providers for this
milestone. A provider must expose a remotely reachable control API and a
complete data plane. A Kubernetes deployment may still be used to deploy the
provider itself.

## Obscura Control-Plane Contract

Every admitted provider must support these semantics, regardless of its wire
protocol:

| Operation | Required semantics |
| --- | --- |
| Create | accepts an immutable template/image reference, lifetime, resource and network policy; returns a stable provider handle |
| Discover | after a timeout or lost response, find the instance by the Obscura stable ID and immutable specification fingerprint |
| Observe | map provider state to `Creating`, `Ready`, `Running`, `Paused`, `Failed`, or `Deleted` |
| Delete | be retryable and provide positive confirmation that the resource is absent |
| Auth | use a configured Secret/reference; never put credential values in CR status or logs |
| Errors | distinguish invalid, auth, rate-limit, capacity, unavailable, conflict, unsupported, and ambiguous outcomes |
| Capabilities | report the operations available for this runtime deployment and, where relevant, this sandbox |

The stable identity is the Kubernetes Sandbox UID. The adapter must preserve
the UID and `spec_hash` as provider metadata or an equivalent idempotency key.
An uncertain POST is never replayed automatically. The adapter discovers first;
if it cannot prove absence or identity, it returns `ErrAmbiguous`.

No pinned provider is assumed to provide server-side idempotent POST for this
identity. Obscura must persist creation intent before the provider call, put
`obscura_uid` and `spec_hash` in provider-supported metadata/labels when
available, and use provider-specific list/filter operations to discover the
instance. An empty discovery result after an uncertain create is ambiguous,
not permission to issue another POST.

## Obscura Data-Plane Contract

The baseline data plane is required for a provider to be called a complete
sandbox runtime:

- command execution with environment variables and stdin;
- ordered, bounded stdout/stderr event streaming;
- deadline and cancellation, with no automatic replay;
- upload and download of workspace files;
- explicit terminal result or error event;
- provider-side authentication and sandbox isolation.

The following are optional capabilities and must be advertised rather than
silently assumed:

```text
pty, ports, pause_resume, snapshot, clone, volumes, checkpoint
```

An adapter may use HTTP, WebSocket, SSE, gRPC, or provider-specific session
protocols internally. It must translate them to the Obscura data-plane events.
The stream must end with `result`, `error`, `cancelled`, or `timeout`. EOF before
a terminal event is a transport error and must not cause command replay.

## Public Gateway Contract

The public API remains provider-neutral:

```text
POST   /v1/sandboxes
GET    /v1/sandboxes/{sandboxID}
DELETE /v1/sandboxes/{sandboxID}
POST   /v1/sandboxes/{sandboxID}/execute
PUT    /v1/sandboxes/{sandboxID}/files
GET    /v1/sandboxes/{sandboxID}/files
GET    /v1/sandboxes/{sandboxID}/pty       (when pty is advertised)
ANY    /v1/sandboxes/{sandboxID}/ports/... (when ports is advertised)
```

Provider session IDs are not public request fields. The adapter creates or
resolves them from the bound provider sandbox handle and closes them on
completion, cancellation, expiry, or client disconnect.

## Provider Review Matrix

This is the admission checklist, not an assertion that every cell is already
verified. `P` means the provider has an apparent component/API and needs an
integration test; only a recorded passing test may promote it to `Y`.

| Capability | AgentENV v0.2.3 | OpenSandbox 1.1.1 | CubeSandbox v0.7.3 |
| --- | --- | --- | --- |
| remote control API/OpenAPI | P | P | P |
| create/get/delete sandbox | P | P | P |
| stable metadata/idempotency discovery | P | P | P |
| command execution/session | P (`execd`/envd) | P (`execd`/session) | P (envd sidecar/API) |
| streaming stdout/stderr | P | P | P |
| cancel and deadline | P | P | P |
| file upload/download | P | P | P |
| PTY | P | P | P |
| exposed ports | P | P | P |
| snapshots | P | P | P |
| clone/fork | P | P | P |

Before enabling a provider in production, replace every `P` used by the
deployment with a recorded contract-test result and the exact deployed API
revision. Repository-level source inspection is not enough to establish that a
feature is enabled, authenticated, and semantically compatible in a deployment.

## Verified Release Facts

These facts are verified from the pinned source contracts and constrain adapter
design. They still require deployment-level tests for authentication, routing,
and enabled components.

### AgentENV v0.2.3

Pinned source references: [Connect transport](https://github.com/kvcache-ai/AgentENV/blob/6cccaa7842bd5be2051111d4f74d9e37aa721244/crates/aenv/src/grpc/mod.rs),
[file client](https://github.com/kvcache-ai/AgentENV/blob/6cccaa7842bd5be2051111d4f74d9e37aa721244/crates/aenv/src/client/files.rs),
and [process tests](https://github.com/kvcache-ai/AgentENV/blob/6cccaa7842bd5be2051111d4f74d9e37aa721244/thirdparty/envd/tests/process_test.rs).

- Lifecycle OpenAPI uses `X-API-Key` (with alternate bearer/team/admin schemes).
- `POST /v2/sandboxes` returns `201`, a sandbox object, and the
  `x-agentenv-sandbox-id` response header.
- `GET /v2/sandboxes` supports metadata and state filters, pagination, and
  `X-Next-Token`; `GET /sandboxes/{sandboxID}` and delete are lifecycle calls.
  The v2 create/list contract is suitable for discovery but does not guarantee
  idempotent POST.
- Pause, resume, connect, timeout, snapshots, and fork are separate lifecycle
  operations.
- Inside the sandbox, execution and PTY use envd's `process.Process` protobuf
  service. AgentENV's public Gateway does not proxy native gRPC/HTTP2; the
  pinned `aenv` client sends Connect-RPC over HTTP/1.1 to paths such as
  `/process.Process/Start`. Unary input/close calls use `application/proto`,
  while server streams use five-byte Connect envelopes and
  `application/connect+proto`.
- File download is `GET /files?path=...`; upload is `POST /files?path=...`
  with a multipart field named `file`. Both data-plane forms route with
  `x-agentenv-sandbox-id`, `x-agentenv-target-port: 49983`, and an
  `X-Access-Token` obtained from the current sandbox response. No `/proxy`
  prefix is used by the official `v0.2.3` client.
- The envd token is resolved immediately before an operation and remains
  adapter-internal. It is not returned in `Observation`, persisted in Sandbox
  status, or included in errors.
- stdin is sent only after a process Start event, with output drained
  concurrently. Unary input calls have a 15-second request limit; executions
  without an explicit deadline have a five-minute limit. HTTP cancellation and
  stream closure are contract-tested. Remote process termination on disconnect
  is not established: admission must verify it against envd and add an explicit
  signal/termination hook if the process survives. Closing a stream alone is
  not evidence that the shell stopped.

### OpenSandbox release-1.1.1

- Lifecycle is `specs/sandbox-lifecycle.yml`, served under `/v1`, and uses the
  `OPEN-SANDBOX-API-KEY` header.
- `POST /sandboxes` returns `202`; list supports metadata and pagination; GET
  and DELETE expose Running, Paused, Stopping, Terminated, and Failed-style
  states. Metadata is mutable, so Obscura-owned identity keys must be reserved
  or verified after discovery.
- Endpoint resolution is `/sandboxes/{sandboxId}/endpoints/{port}`.
- Execution and filesystem are a separate `specs/execd-api.yaml` service. It
  exposes SSE command streams, bash sessions, interruption, and file operations
  under `/command`, `/session`, and `/files`, with its own access-token auth.
- The lifecycle contract includes pause/resume and snapshots. PTY is not a
  guaranteed base capability of the pinned public contract and stays optional
  until a deployment test proves it.

### CubeSandbox v0.7.3

- `CubeAPI` publishes an E2B-compatible OpenAPI document, but its request and
  response models use a Cube-specific `RequestID`/`RetCode` envelope. It is not
  wire-compatible with AgentENV or OpenSandbox.
- Lifecycle routes include sandbox list/create/get/delete, pause/resume/connect,
  timeout, templates, volumes, and snapshots. Snapshot operations may return a
  long-running operation resource rather than a completed snapshot.
- Execution is served by CubeMaster's custom exec API. envd is injected only
  when the sandbox request opts in through Cube annotations and the selected
  instance type supports it; the adapter must configure and verify this plane.
- Exposed ports are resolved through Cube proxy metadata/host mappings, not a
  generic `/{sandbox}/ports` contract. Port proxying is optional until its
  deployment-specific route and authorization are tested.

## Adapter Rules

The implementation target is one generic HTTP runtime adapter plus small
provider profiles. The shared adapter owns URL validation, authentication
header injection, request deadlines and cancellation, redirect policy, error
classification, pagination safety, bounded file transfer, stream framing, and
terminal-event validation. A profile declares only the provider-specific
control/data path prefixes, authentication headers, state mapping, request and
response codecs, stream protocol (`ndjson`, `sse`, or a named custom codec),
file protocol, and capability flags. A profile may provide a narrow hook when a
response envelope or endpoint resolution cannot be expressed as configuration.

Each profile must have contract tests covering authentication, lost-response
discovery, specification conflicts, state mapping, deletion confirmation,
fragmented event streams, cancellation, deadlines, binary file round trips,
and all advertised optional capabilities. Tests must also verify that upstream
error bodies and secrets are not leaked. This keeps a new provider integration
close to profile data plus a few codecs instead of a copied lifecycle client.

The adapter must not pretend that an E2B-compatible lifecycle subset implies
execution, files, PTY, ports, or snapshots. Those are separate capabilities.

## Current Repository Gaps

The existing `control/internal/backend/e2b.go` is the first shared HTTP
transport slice, but its E2B profile is still a narrow prototype:

- it assumes a narrow E2B-style `/v2/sandboxes` discovery contract;
- profile-driven execution, files, PTY, and ports are implemented only for the
  legacy E2B-shaped test contract;
- snapshots and clone are absent from the shared backend API;
- capability flags are now carried in `Observation`, but only admitted profiles
  may publish them;
- AgentENV's Connect-RPC execute and multipart/raw file codecs have in-repo
  contract tests for framing, routing headers, stdin/close, event conversion,
  truncation, cancellation, limits, redaction, and binary transfer. It remains
  fail-closed in `Registry.Open` until the same suite is run against a real
  pinned `v0.2.3` deployment. OpenSandbox and CubeSandbox remain fail-closed
  until their provider codecs and contract tests are complete.

It must be replaced by the generic adapter behind provider profiles. Until a
profile's contract tests pass, the corresponding `SandboxRuntime` type must
remain unavailable rather than silently falling back to lifecycle-only
behavior. CubeSandbox is expected to need a small custom envelope/endpoint
hook; it must not fork the shared transport and safety logic.

The first implementation slice must introduce separate control/data endpoint
configuration and optional data-plane interfaces. It must not force the three
providers into the legacy E2B paths. AgentENV, OpenSandbox, and CubeSandbox
need separate provider profiles/codecs, even though transport, error
classification, and event conversion are shared by the generic adapter.

## Version and Upgrade Policy

Provider versions are part of the adapter contract. Deployment manifests and
documentation must pin the release tag and resolved commit. Upgrades require a
new contract-test run, a reviewed capability matrix, migration notes for changed
state/auth/session/file semantics, and an explicit adapter version change when
wire behavior changes. Floating branches and `latest` images are not valid
production runtime pins.
