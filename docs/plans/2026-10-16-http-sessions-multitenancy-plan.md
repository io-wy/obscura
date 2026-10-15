# HTTP Sessions and Multitenancy Implementation Plan

> **For agentic workers:** follow the superpowers workflow. Every production change starts
> with a focused failing test; review specification compliance and code quality before final verification.

**Goal:** Add the scoped HTTP Runtime session capabilities and a shared, quota-enforcing multi-tenant Gateway.

**Architecture:** A cluster-scoped `SandboxTenant` maps credentials to an isolated namespace and stores
optimistically updated quota reservations. Optional backend capability interfaces carry execution, files,
PTY, and HTTP proxy traffic without expanding the lifecycle contract. The Kubernetes controller remains
the owner of asynchronous lifecycle cleanup and releases quota only after confirmed backend deletion.

**Tech Stack:** Go 1.26, controller-runtime/client-go, Kubernetes CRDs, HTTP/NDJSON,
`github.com/coder/websocket`, envtest, httptest.

## Fixed API and Limit Decisions

- Gateway authentication remains `X-API-Key`; all non-health errors use
  `{"error":{"code":"...","message":"..."}}`.
- `GET|PUT /v1/sandboxes/{sandboxID}/files?path=/absolute/path` reads or replaces one file.
- `GET /v1/sandboxes/{sandboxID}/pty` upgrades to WebSocket. The first client frame is a JSON
  `start` message; subsequent JSON frames are `input`, `resize`, `output`, `exit`, or `error`.
- `/v1/sandboxes/{sandboxID}/ports/{port}/{path...}` proxies ordinary HTTP methods. WebSocket
  port upgrades are outside the first phase.
- HTTP Runtime endpoints are `POST /sandboxes/{handle}/commands`,
  `GET|PUT /sandboxes/{handle}/files`, `GET /sandboxes/{handle}/pty`, and
  `/sandboxes/{handle}/ports/{port}/{path...}`.
- Request JSON is limited to 1 MiB, file bodies to 32 MiB, an NDJSON event to 256 KiB,
  cumulative execution output to 16 MiB, a WebSocket message to 256 KiB, and proxy bodies
  to 32 MiB. Defaults are 5 minutes for execution and 30 minutes for PTY, always clamped to
  Sandbox expiry.
- The proxy forwards `Content-Type`, `Accept`, `Accept-Encoding`, `Content-Encoding`, `Range`,
  and conditional/cache headers. It strips credentials, cookies, hop-by-hop headers, and forwarding headers.

### Task 1: Define and generate the tenant API

**Files:** modify `control/api/v1alpha1/types.go`, `control/api/v1alpha1/schema_test.go`;
generate `control/api/v1alpha1/zz_generated.deepcopy.go` and
`deploy/crds/sandbox.obscura.dev_sandboxtenants.yaml`.

1. Add failing schema/deep-copy tests for a cluster-scoped `SandboxTenant` with status subresource.
2. Define credential Secret references, namespace, quota limits, resource allocations, session leases,
   observed credential hashes, and Conditions with validation bounds and list-map keys.
3. Register the types, generate deepcopy/CRD output, and make schema tests pass.

### Task 2: Implement atomic reservations

**Files:** create `control/internal/quota/quota.go` and `control/internal/quota/quota_test.go`.

1. Write failing tests for single-instance limits, aggregate limits, idempotent reservation,
   changed idempotent requests, session limits, release, expiry, and concurrent CAS conflicts.
2. Implement APIReader reads plus optimistic-lock status patches with bounded conflict retries.
3. Keep allocations through all Sandbox phases; expose explicit release methods used only by cleanup.

### Task 3: Reconcile tenant credentials and orphaned reservations

**Files:** create `control/internal/controller/tenant.go` and tests; modify controller startup and RBAC.

1. Write failing reconciler tests for Secret hashing, rotation, revocation, duplicate tenant namespace
   rejection, expired session lease cleanup, and stale allocation cleanup only when no Sandbox exists.
2. Reconcile referenced Secrets into SHA-256 credential hashes in Tenant status; never persist key values.
3. Add Secret watches, cluster-wide Sandbox/Tenant watches, APIReader absence checks, Conditions, and RBAC.

### Task 4: Add tenant authentication and isolation to Gateway

**Files:** refactor `control/internal/kubegateway/gateway.go`; add focused gateway auth/quota tests;
modify `control/cmd/gateway/main.go` and devlab setup.

1. Write failing tests for key-to-tenant resolution, two-key rotation, revocation, duplicate hash failure,
   cross-namespace ID guessing, deterministic per-tenant idempotency, and structured errors.
2. Replace fixed `Namespace`/`APIKey` with a Tenant resolver backed by Tenant status hashes.
3. Reserve quota before Sandbox creation, release only a reservation whose CR creation definitively failed,
   and preserve uncertain/pending reservations for reconciliation.
4. Acquire and release execution session leases around the existing execute flow.

### Task 5: Release allocations after confirmed Runtime cleanup

**Files:** modify `control/internal/controller/sandbox.go` and its tests.

1. Add failing tests proving Creating and Terminating objects remain charged, failed deletes do not release,
   confirmed absence releases once, and finalizer removal waits for quota release.
2. Resolve the owning Tenant by namespace, release its allocation with conflict retry, then remove finalizer.
3. Fail closed when tenant ownership is missing or ambiguous so cleanup cannot silently corrupt accounting.

### Task 6: Split backend session capabilities

**Files:** modify `control/internal/backend/backend.go`, `native.go`, tests, and Gateway assertions.

1. Add compile-time and behavior tests for optional `Executor`, `FileSystem`, `PTY`, and `PortProxy`.
2. Remove `Exec` from the lifecycle `Backend`; make native and HTTP clients implement only supported interfaces.
3. Define bounded request/response and PTY message types without Kubernetes dependencies.

### Task 7: Implement HTTP command streaming

**Files:** modify `control/internal/backend/e2b.go` and `registry_test.go`.

1. Write mock HTTP tests for exact command request JSON, auth header, fragmented NDJSON, terminal events,
   premature EOF, invalid identity/sequence, cancellation, timeout, per-event and aggregate output limits,
   and backend error mapping.
2. Implement the command endpoint with streaming decode, immediate event delivery, terminal-event validation,
   and no automatic replay.
3. Update Gateway tests for pre-stream JSON errors and post-stream terminal transport errors.

### Task 8: Implement file read/write

**Files:** modify HTTP backend and Gateway; add contract and route tests.

1. Test absolute cleaned paths, URL encoding, binary round trips, content length/type, cancellation,
   over-limit requests/responses, and mapped upstream errors.
2. Implement backend file methods and Gateway GET/PUT routes with streaming limits.

### Task 9: Implement PTY WebSocket bridging

**Files:** add the websocket dependency; modify HTTP backend and Gateway; add WebSocket integration tests.

1. Test start/input/resize/output/exit messages, bidirectional cancellation, normal and error closes,
   frame limits, Sandbox expiry, and concurrent session quota across Gateway instances.
2. Implement an abstract PTY session and HTTP Runtime WebSocket client.
3. Bridge the client and upstream sockets with one cancellation owner and guaranteed lease release.

### Task 10: Implement bounded HTTP port proxying

**Files:** modify HTTP backend and Gateway; add reverse-proxy contract tests.

1. Test port validation, escaped relative paths, query preservation, allowed/stripped headers,
   streamed status/body, body limits, disconnect cancellation, and redirect refusal.
2. Implement the constrained proxy route without accepting arbitrary hosts or forwarding credentials.

### Task 11: Update deployment and operator documentation

**Files:** modify `deploy/base`, examples, `control/README.md`, `control/internal/backend/README.md`,
`docs/kubernetes.md`, and `docs/testing.md`.

1. Replace namespace Roles with the minimum ClusterRoles needed by the shared controller and Gateway.
2. Add a Tenant, auth Secret, tenant namespace, namespaced Runtime, and quota example.
3. Document the CRD + Operator boundary, credential rotation/revocation, exact HTTP Runtime contract,
   limits, error schema, quota accounting, and explicitly deferred functionality.

### Task 12: Full verification and review

1. Run targeted package tests after every task and `gofmt` changed Go files.
2. Run `make manifests`, `make test-go`, `make test-race`, `make test-controlplane`, and
   `make test-manifests`; repair all failures.
3. Run `git diff --check`, inspect generated changes, audit credential/error leakage and cancellation paths,
   and review the final diff against the approved design.
4. Re-run all affected tests fresh and record any environment-only gaps without claiming real Runtime coverage.
