# Native Agentic RL Runtime Design

## Goal

Build Obscura's native runtime as an episode-oriented execution substrate for
internal agentic RL and evaluation. The runtime must optimize for deterministic
reset, reproducible environments, scorer isolation, ordered trajectory capture,
and measurable throughput and cost.

AgentENV and CubeSandbox remain external providers and contract-test references.
The native runtime does not copy their public APIs. It implements the existing
Obscura controller-runtime contract and owns the node-local execution semantics.

## Fixed Boundary

The controller-runtime protocol, Kubernetes API, backend request mapping, and
provider-neutral Gateway API are inputs to this feature. This feature must not
change files under `api/proto`, `control/api`, or the controller/backend contract
to make an implementation easier. A genuine protocol contradiction must be
documented and reviewed separately before any protocol change.

The existing contract already supplies stable sandbox identity, immutable spec
hashes, version pins, workspace modes, credential policy, reset modes, ordered
execution events, and capability discovery. The native runtime must enforce
these fields rather than merely accept and hash them.

## Architecture

```text
Obscura Controller / Gateway
            |
            | frozen agentruntime.v1 contract
            v
Native node runtime (Rust)
  +-- lifecycle state machine and durable identity
  +-- episode lease and reset coordinator
  +-- environment revision verifier
  +-- ordered trajectory recorder
  +-- scorer handoff boundary
  +-- isolation driver
        +-- containerd/runc       first throughput backend
        +-- gVisor/runsc          untrusted-code backend
        +-- Firecracker           stronger isolation backend
```

Kubernetes remains the desired-state and tenant control plane. The node runtime
manages low-latency sandbox allocation below the Pod abstraction so an episode
does not require a new Kubernetes Pod. Each isolation implementation is hidden
behind the same driver interface and advertises only capabilities it enforces.

## Core Model

### Environment revision

An environment revision is immutable and content addressed. Its identity binds:

- OCI image digest and architecture;
- dependency lock digest;
- dataset version;
- scorer version;
- runtime implementation version;
- resource and network policy;
- workspace/reset policy.

Creation fails closed when a requested digest cannot be resolved or verified.
Tags and mutable filesystem paths are not admissible production revisions.

### Sandbox and episode

A sandbox is the durable controller-visible identity. An episode is one lease of
that sandbox between successful reset boundaries. Each lease has a monotonically
increasing epoch. Execute, score, and trajectory events are associated with the
sandbox ID, episode ID, lease epoch, execution ID, and environment revision.

Internal work and active streams from an old epoch are fenced after reset.
The frozen external Execute request has no epoch: a new request delayed until
after reset cannot be attributed to the old caller. Upstream callers must order
reset and new executions; this feature does not extend the protocol.

### Reset

The first admitted mode is clean reset:

1. move the sandbox to `Resetting` internally and reject new execution;
2. cancel and reap all episode processes;
3. detach the writable workspace and network namespace;
4. delete the disposable writable layer and episode-local secrets;
5. create a fresh writable layer from the verified immutable lower layer;
6. create fresh process/network namespaces and increment the lease epoch;
7. run a reset probe and publish `Ready` only after verification succeeds.

A failed or interrupted reset quarantines the sandbox. It must never return to
the warm pool. Checkpoint and template restore remain unsupported until their
disk, memory, runtime-version, and network-state compatibility can be proven.

Persistent workspaces are separate mounts and are not evidence of a clean
environment. Their identity and ownership must be explicit in the launch spec.

## Isolation Drivers

The driver boundary owns prepare, start, execute attachment, reset, stop, and
health inspection. It returns opaque handles and capability evidence; it does
not own controller identity or retry policy.

The initial throughput driver uses containerd with OCI images, cgroups v2,
namespaces, a read-only content-addressed lower filesystem, and one disposable
OverlayFS upper layer per episode. The runtime talks to containerd directly;
shelling out to `docker` or `ctr` is not an admitted production path.

gVisor is the next isolation tier and should reuse the OCI/containerd lifecycle
with a `runsc` runtime selection. Firecracker remains a separate driver for
adversarial or shared-infrastructure workloads. It is not the default until its
real Linux/KVM path, snapshot compatibility, reset behavior, and cost are
measured against the container driver.

## Scoring Isolation

The scorer is trusted evaluation infrastructure and never runs inside the
candidate sandbox. The sandbox produces an immutable artifact manifest. A
separate scorer worker receives read-only artifacts, the pinned scorer revision,
and a capability-scoped dataset reference. It has a distinct filesystem,
network policy, and credential domain.

Score output records scorer revision, environment revision, artifact digests,
and the trajectory terminal sequence. A sandbox cannot write or overwrite its
own score. Scoring failure does not make a dirty sandbox reusable.

The first runtime slice defines and tests this boundary but does not build a
distributed scorer service. Controller scheduling for scorers is a later,
separate feature.

## Trajectory Capture

Trajectory is a domain event log, not a replacement for infrastructure traces.
Every event has a monotonic sequence within an execution and carries sandbox,
episode, epoch, execution, and revision identity. Required event classes are:

- lifecycle and reset transitions;
- stdin, stdout, stderr, result, cancellation, and timeout;
- tool/process invocation metadata;
- resource samples and terminal usage totals;
- policy decisions and denied network attempts;
- artifact manifests and scorer handoff/results.

The node appends bounded records before forwarding them. Payload blobs can be
stored separately by digest. OTLP remains appropriate for service traces and
metrics; it is not the canonical episode trajectory. The first implementation
uses a local append-only spool with explicit backpressure and recovery. Remote
object storage and analytical indexing are later deployment concerns.

## Credentials And Network

Long-lived credentials are not injected into environment variables or persisted
in sandbox metadata. A broker resolves a policy reference into short-lived,
least-privilege credentials and exposes them through a controlled guest channel.
Credential material is scoped to one sandbox episode and revoked at reset.

Network policy defaults to no egress. Host allowlists require enforcement through
a controlled DNS/egress proxy or equivalent mechanism; DNS-only filtering is not
sufficient. Audit-only policy is explicitly labeled and cannot be reported as
enforced. The existing nftables implementation remains the base for native
network isolation; eBPF is not required for the first milestone.

## Pooling And Scheduling

Pools are keyed by the complete environment revision and isolation tier. A pool
entry is eligible only after an integrity probe. Admission reserves resources
before side effects and never overcommits beyond configured CPU, memory, disk,
and concurrent-reset limits.

Pool policy uses measured demand and exposes cold-start latency, warm-acquire
latency, reset latency, hit rate, quarantine count, and idle resource cost.
Predictive or distributed scheduling is out of scope until these measurements
exist.

## Failure Semantics

- Create and reset are durable state machines, not blind retried procedures.
- Lost responses are observed by stable identity; destructive work is not
  replayed from an empty response.
- Driver failure after side effects quarantines the sandbox and its resources.
- Runtime restart never claims an old execution lease without reconciliation.
- A full trajectory spool returns backpressure or fails the episode according
  to policy; it never silently drops required events.
- Raw command, environment values, process I/O, and raw provider errors are not
  recorded by the metadata spool. Callers must keep secrets out of identity
  fields; hashing payloads does not provide anonymization.

## Delivery Phases

### Phase 1: semantic core

- isolation-driver contract and fake driver tests;
- environment revision validation and evidence;
- episode leases and clean-reset state machine;
- bounded local trajectory spool;
- scorer artifact/result boundary;
- metrics for latency, reuse, reset, quarantine, and resource totals.

### Phase 2: container throughput backend

- direct containerd integration;
- OCI digest verification and OverlayFS reset;
- cgroups v2 and namespace enforcement;
- warm-pool admission and Linux integration tests;
- benchmark against the admitted AgentENV deployment.

### Phase 3: isolation tiers

- gVisor runtime selection and compatibility suite;
- Firecracker driver conformance and snapshot/reset verification;
- policy-based driver selection using measured risk and cost.

## Acceptance Criteria

- The controller-runtime protocol diff is empty for this feature.
- Two consecutive episodes cannot observe each other's processes, writable
  filesystem, environment variables, network namespace state, or credentials.
- Stale lease traffic is rejected after reset.
- Environment revisions reject mutable or mismatched artifacts.
- Scoring runs outside the candidate sandbox and score provenance is complete.
- Required trajectory events are ordered, bounded, recoverable, and attributable.
- Fault injection during each reset step either produces a verified clean Ready
  sandbox or a quarantined sandbox, never an uncertain pooled sandbox.
- Benchmarks report p50/p95/p99 cold start, warm acquire, reset, episodes per node,
  pool hit rate, quarantine rate, and estimated cost per 1,000 episodes.

## Implemented slice and explicit deferrals (2026-10-16)

The current slice implements durable reset/generation state, immutable measured
launch profiles, Firecracker orchestration, generation-specific guest sockets,
active-stream fencing, a bounded metadata spool, and host-side scoring value and
capability boundaries. It uses the existing `Vmm` trait; a generic container
isolation-driver interface is deferred until the containerd implementation.

The spool deliberately stores typed metadata and payload digests, not replayable
task contents. This narrows the trajectory goal for the first slice: raw-content
capture, remote content-addressed storage, resource samples and policy event
producers remain open. Lifecycle deletion uses SQLite as the final authority
when a full spool prevents recording its optional audit event. Storage operations
are synchronous on blocking workers; execution deadlines cannot interrupt stuck
host I/O. An interrupted admission may leave a Started record without a terminal;
it must not be treated as successful completion or scoring evidence.

Scoring adapters record handoff/result digests; actual independent worker
deployment, authentication, CAS verification, durable full score storage and
cross-restart deduplication remain external obligations. Terminal evidence is
specifically a HostExecution Terminal record. Only operation counters exist;
latency, resource, reuse and quarantine metrics are deferred.

The complete EnvironmentRevision canonicalizer is a library boundary. Service
admission verifies supported runtime/image pins and rejects unsupported
dependency/dataset/scorer verification, persistence, credential brokerage and
maximum-lifetime requests. Legacy template aliases remain accepted and are
measured; they do not constitute production manifest admission.

All conformance evidence so far is local Rust/fake-driver/UDS testing on macOS.
Linux/KVM/conntrack isolation, containerd/runsc admission, warm-pool reuse and
real benchmarks are unverified; the full acceptance criteria above remain
deployment gates, not claims that this first slice has met them.
