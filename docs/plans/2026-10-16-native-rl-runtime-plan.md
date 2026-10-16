# Native Agentic RL Runtime Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers methodology with test-first implementation and review after each task.

**Goal:** Implement the native runtime semantic core for deterministic agentic RL episodes without changing the established controller-runtime protocol.

**Architecture:** Keep Kubernetes and the Go controller as the desired-state plane. Add a Rust node-local semantic layer that verifies immutable environment revisions, persists episode generations, performs fail-closed clean resets through an isolation driver, and records bounded trajectory/scoring provenance. Add containerd, gVisor, and production Firecracker conformance only after the semantic core passes fault-injection tests.

**Tech Stack:** Rust 2021, tonic/prost, SQLite/rusqlite, SHA-256, existing VMM/network abstractions, OCI/containerd in the Linux integration phase.

## Constraints

- Do not modify `api/proto`, `control/api`, controller reconciliation, or backend request mapping.
- Treat the current protocol fields and reset modes as fixed inputs.
- Do not commit the existing uncommitted AgentENV/controller/protocol work with native-runtime commits.
- Keep network, filesystem, process, credential, scorer, and trajectory state isolated by episode.
- A failed reset must leave a durable non-ready or quarantined identity.
- Write a failing test before each behavior change.

### Task 1: Environment revision validation

**Files:**
- Create: `runtime/crates/runtime/src/environment.rs`
- Modify: `runtime/crates/runtime/src/lib.rs`

- [x] Define plain Rust inputs for runtime version, image digest, dependency lock, dataset version, scorer version, template, resources, network, and workspace policy.
- [x] Test that the same complete input produces the same revision digest.
- [x] Test that every pinned field and enforced policy changes the digest.
- [x] Test rejection of malformed SHA-256 image digests and runtime-version mismatches.
- [x] Implement canonical encoding, validation, and revision evidence.
- [x] Run `cargo test --manifest-path runtime/Cargo.toml environment`.
- [x] Commit only the new module, `lib.rs`, and this checklist update.

### Task 2: Episode lease and internal fencing

**Files:**
- Create: `runtime/crates/runtime/src/episode.rs`
- Modify: `runtime/crates/runtime/src/lib.rs`

- [x] Define `EpisodeLease` with sandbox ID, episode ID, generation, and environment revision.
- [x] Test monotonic generation on successful reset.
- [x] Test rejection of execution completion from an older internal generation.
- [x] Test that reset cannot finish while an execution guard remains active unless the driver reports it reaped.
- [x] Implement the smallest state machine that satisfies the tests.
- [x] Document that the frozen external request has no epoch field; fencing protects node-internal work and active streams, not independently delayed new callers.
- [x] Run the focused Rust tests and commit the task.

### Task 3: Persist reset intent and episode generation

**Files:**
- Modify: `runtime/crates/runtime/src/storage.rs`
- Modify: `runtime/crates/runtime/src/sandbox.rs`

- [x] Add migration tests for new launch/reset metadata while preserving v1 tombstones and quarantine records.
- [x] Add `Resetting` to the internal state machine without exposing a new protobuf enum.
- [x] Persist the immutable launch profile, measured environment revision, episode generation, and reset intent before side effects.
- [x] Add atomic storage methods to begin reset, complete reset, and fail a reset; reopened uncertain identities are quarantined.
- [x] Test reopen behavior for interrupted reset and future-schema rejection.
- [x] Run storage and lifecycle tests, then the complete Rust test suite.
- [x] Include Runtime storage/state in the native implementation commit, after the shared protocol baseline.

### Task 4: Clean-reset orchestration

**Files:**
- Modify: `runtime/crates/runtime/src/orchestrator.rs`
- Modify: `runtime/crates/runtime/src/vmm.rs`
- Modify: `runtime/crates/runtime/src/provider.rs`

- [x] Test that writable disk content is replaced from the immutable base during clean reset.
- [x] Test stop, network cleanup, conntrack, relaunch, readiness, and final Ready persistence failures.
- [x] Test each failure leaves a non-ready identity with retained cleanup ownership; restart quarantines it.
- [x] Retain the tested existing Vmm lifecycle; defer the generic isolation-driver refactor to Task 8.
- [x] Implement persisted intent, stop/reap, TAP/nft/conntrack and disk cleanup, verified kernel/rootfs copies, fresh launch, readiness, generation increment, and Ready commit.
- [x] Reject checkpoint/template modes until compatibility checks exist.
- [x] Run orchestrator tests, complete Rust tests, and `git diff --check`.
- [x] Include the Runtime-only change in the native implementation commit.

### Task 5: Bounded trajectory spool

**Files:**
- Create: `runtime/crates/runtime/src/trajectory.rs`
- Modify: `runtime/crates/runtime/src/lib.rs`
- Modify: `runtime/crates/runtime/src/service.rs`

- [x] Test ordered append, duplicate/out-of-order rejection, recovery after reopen, record/spool bounds, and explicit backpressure.
- [x] Implement append-only records with independent header/body checksums, crash-tail recovery, writer lock and directory sync.
- [x] Record lifecycle/reset transitions and execution events/terminal metadata before forwarding; Delete can clean up despite a full spool.
- [x] Omit raw command/env/I/O/error; store payload lengths and hashes. Caller identifiers must be secret-free. Content replay is deferred.
- [x] Run focused trajectory tests and complete Rust tests.
- [x] Split `service.rs`: protocol imports/stub/fingerprint in the shared baseline; native behavior in the implementation commit.

### Task 6: Scorer isolation boundary

**Files:**
- Create: `runtime/crates/runtime/src/scoring.rs`
- Modify: `runtime/crates/runtime/src/lib.rs`

- [x] Test immutable manifests, digest validation, scorer/environment provenance, terminal identity binding, and forged scorer capability rejection.
- [x] Implement host-only scorer value types/capability validation; actual worker authentication, CAS verification and isolation remain deployment obligations.
- [x] Add recorded host adapters for handoff and accepted result metadata digests; no scoring RPC is added.
- [x] Run focused and complete Rust tests.
- [x] Include scoring in the native implementation commit.

### Task 7: Frozen-protocol service integration

**Files:**
- Modify: `runtime/crates/runtime/src/service.rs`
- Modify: `runtime/crates/runtime/src/bin/node.rs`
- Modify: `runtime/README.md`

- [x] Separate integration from the shared protocol baseline; the native implementation changes no protobuf or controller files.
- [x] Test clean reset/replay, unsupported modes, reset/execute races, guest READY sequence compatibility, expired admission, duplicate execution, backpressure and health.
- [x] Translate internal `Resetting` to an existing non-ready external state.
- [x] Verify supported image/runtime pins before launch; reject unsupported policy/version/lifetime requests.
- [x] Advertise only idempotent-create-v1/reset-clean-v1; Linux conformance is a separate deployment gate.
- [x] Run `make test-rust`, protocol checks without regeneration, and `git diff --check`.
- [x] Confirm the native implementation diff contains no controller-runtime protocol changes.

### Task 8: Container throughput driver

**Files:**
- Create: `runtime/crates/runtime/src/containerd.rs`
- Modify: `runtime/crates/runtime/src/bin/node.rs`
- Modify: `runtime/crates/runtime/Cargo.toml`
- Modify: `runtime/README.md`
- Create: `runtime/tests/containerd_integration.rs`

- [ ] Select a maintained containerd gRPC client compatible with the pinned Rust toolchain and record the exact version.
- [ ] Write Linux-only tests for digest-only image admission, cgroups v2 limits, namespace isolation, disposable OverlayFS layers, process reaping, and clean reset.
- [ ] Implement direct containerd integration without invoking `docker` or `ctr`.
- [ ] Make the driver explicit node configuration; retain Firecracker as a separate option.
- [ ] Add a runsc runtime selector without claiming gVisor admission until its integration suite passes.
- [ ] Run unit tests on macOS and containerd integration tests on Linux.

### Task 9: Benchmarks and provider comparison

**Files:**
- Create: `runtime/benches/episode_lifecycle.rs`
- Create: `docs/testing/native-runtime-benchmark.md`
- Modify: `runtime/README.md`

- [ ] Measure cold start, warm acquire, clean reset, and episode throughput at controlled concurrency.
- [ ] Record CPU, memory, disk, pool hit rate, quarantine rate, and estimated cost per 1,000 episodes.
- [ ] Run the same workload through the admitted pinned AgentENV deployment.
- [ ] Report p50/p95/p99 and test-host details; do not compare unverified or differently sized environments.
- [ ] Use results to decide when gVisor or Firecracker becomes the default for a workload class.

## Final Verification

- [x] `make test-rust`
- [ ] `make test` when integration touches shared build paths
- [x] `git diff --check`
- [x] Review implementation diff for secrets, unbounded buffers, unsafe cleanup, and protocol changes
- [x] Record Linux/containerd/KVM tests that were not run locally
- [ ] Request spec-compliance and code-quality review before opening the PR

## Current delivery boundary

Tasks 3–6 implement the semantic slice above; Task 7 is implemented and tested
against the shared Obscura controller/runtime protocol. Commits separate that
protocol baseline from the native implementation and from the independent
AgentENV/HTTP provider adapters. The native runtime does not depend on AgentENV
code, SDKs, services or deployment. Tasks 8–9 remain open: no containerd/runsc
driver, warm-pool performance results, AgentENV comparison, latency/resource
metrics or real Linux/KVM conformance is claimed. See the design's implemented
slice section for the reduced metadata-only trajectory and scorer boundaries.

### Local validation evidence

- `make test-rust protocol-check`: 88 Rust tests passed; both Proto files and two
  protocol fixtures validated without regeneration.
- `cargo clippy --manifest-path runtime/Cargo.toml --all-targets -- -D warnings`:
  passed. Added the missing `WarmVmPool::is_empty` companion to satisfy the existing lint.
- `cd control && go test ./...`: passed against existing generated sources.
- `git diff --check`: passed; independent spec/correctness review completed and
  its reset, deadline, revision, guest-sequence and spool-corruption findings fixed.
- Protocol, CRD types and Controller checksums match the pre-implementation
  working-tree baseline. Their pre-existing edits are in the shared baseline,
  outside the native implementation commit.
- No Linux/KVM/conntrack/containerd integration or performance measurements ran
  on this macOS host. Full `make test` was not invoked because its generation
  prerequisites would write into the concurrent protocol/guest work.
- Commit preparation exports the shared baseline into a separate directory and
  runs Go, Rust and protocol checks without any new AgentENV adapter files. The
  baseline adds generated CRD/deepcopy fields so Kubernetes retains new policies.
  Native integration and AgentENV adapters are then staged as separate commits.
- The exported shared-baseline-plus-native tree also passes `make test-go
  test-rust protocol-check` (88 Rust tests) and strict all-target Clippy, with no
  AgentENV source, envd Proto or provider profile present.
