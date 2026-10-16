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

- [ ] Add failing migration tests for the new launch/reset metadata while preserving v1 tombstones and quarantine records.
- [ ] Add `Resetting` to the internal state machine without exposing a new protobuf enum.
- [ ] Persist the immutable launch profile, environment revision, episode generation, and reset intent before side effects.
- [ ] Add atomic storage methods to begin reset, complete reset, and quarantine a failed reset.
- [ ] Test reopen behavior for interrupted reset and future-schema rejection.
- [ ] Run storage and lifecycle tests, then the complete Rust test suite.
- [ ] Commit only Runtime storage/state files.

### Task 4: Clean-reset orchestration

**Files:**
- Modify: `runtime/crates/runtime/src/orchestrator.rs`
- Modify: `runtime/crates/runtime/src/vmm.rs`
- Modify: `runtime/crates/runtime/src/provider.rs`

- [ ] Add a failing test proving writable disk content is replaced from the immutable base during clean reset.
- [ ] Add tests for stop failure, network cleanup failure, relaunch failure, readiness failure, and final Ready persistence failure.
- [ ] Add tests proving each failure leaves the sandbox non-ready and quarantined.
- [ ] Refine the VMM boundary into an isolation-driver lifecycle without changing Firecracker behavior.
- [ ] Implement clean reset as persisted intent, stop/reap, network and disk cleanup, fresh launch, readiness probe, generation increment, and Ready commit.
- [ ] Reject checkpoint/template modes internally until their compatibility checks exist.
- [ ] Run orchestrator tests, complete Rust tests, and `git diff --check`.
- [ ] Commit the Runtime-only change.

### Task 5: Bounded trajectory spool

**Files:**
- Create: `runtime/crates/runtime/src/trajectory.rs`
- Modify: `runtime/crates/runtime/src/lib.rs`
- Modify: `runtime/crates/runtime/src/service.rs`

- [ ] Test ordered append, duplicate/out-of-order rejection, recovery after reopen, maximum record size, maximum spool size, and explicit backpressure.
- [ ] Implement length-delimited append-only records with checksum and crash-tail recovery.
- [ ] Record lifecycle/reset transitions and execution terminal events before forwarding.
- [ ] Redact environment values and never serialize credentials.
- [ ] Run focused trajectory tests and complete Rust tests.
- [ ] Stage only native-runtime hunks if `service.rs` still contains unrelated work; otherwise leave service integration uncommitted and report the dependency.

### Task 6: Scorer isolation boundary

**Files:**
- Create: `runtime/crates/runtime/src/scoring.rs`
- Modify: `runtime/crates/runtime/src/lib.rs`

- [ ] Test immutable artifact manifests, digest validation, scorer/environment provenance, terminal trajectory sequence binding, and sandbox-authored score rejection.
- [ ] Implement scorer handoff/result value types and validation without starting a scorer inside the sandbox.
- [ ] Add trajectory records for handoff and accepted result metadata.
- [ ] Run focused and complete Rust tests and commit the task.

### Task 7: Frozen-protocol service integration

**Files:**
- Modify: `runtime/crates/runtime/src/service.rs`
- Modify: `runtime/crates/runtime/src/bin/node.rs`
- Modify: `runtime/README.md`

- [ ] Rebase integration on the committed protocol baseline; do not edit protobuf or controller files.
- [ ] Add service tests for clean reset, unsupported reset modes, reset/execute races, and health capabilities.
- [ ] Translate internal `Resetting` to an existing non-ready external state.
- [ ] Validate environment pins before any launch side effect and return deterministic gRPC status codes.
- [ ] Expose only capabilities that the node enforces.
- [ ] Run `make test-rust`, protocol compatibility checks that do not regenerate files, and `git diff --check`.
- [ ] Confirm the feature's committed diff contains no controller-runtime protocol changes.

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

- [ ] `make test-rust`
- [ ] `make test` when integration touches shared build paths
- [ ] `git diff --check`
- [ ] Review committed diff for secrets, unbounded buffers, unsafe cleanup, and protocol changes
- [ ] Record Linux/containerd/KVM tests that were not run locally
- [ ] Request spec-compliance and code-quality review before opening the PR
