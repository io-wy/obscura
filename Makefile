.PHONY: test test-go test-rust protocol-check proto-generate guest-test manifests test-envtest build-control test-git-workflow git-help git-hooks

export PATH := $(CURDIR)/.tools/bin:$(PATH)

test: test-go test-rust protocol-check guest-test test-git-workflow

test-go: proto-generate
	cd control && go test ./...

test-rust:
	cargo test --manifest-path runtime/Cargo.toml

protocol-check:
	node scripts/check-protocol.mjs

proto-generate:
	./scripts/generate-go-proto.sh

guest-test: proto-guest
	cd guest/runner && npm run typecheck && npm run build
	cd guest/runner && node --experimental-strip-types --test test/**/*.test.ts

manifests:
	cd control && go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.20.1 object crd paths=./api/... output:crd:artifacts:config=../deploy/crds

test-envtest: proto-generate manifests
	./scripts/test-controller-envtest.sh

build-control: proto-generate
	cd control && go build ./cmd/controller ./cmd/gateway

test-git-workflow:
	python3 -m unittest discover -s scripts/tests -v

git-help:
	python3 scripts/git-workflow.py --help

git-hooks:
	python3 scripts/git-workflow.py install-hooks

.PHONY: proto-guest dev-bootstrap dev-doctor dev-up dev-smoke test-controlplane test-mac test-race test-manifests kind-up kind-smoke kind-status kind-down

proto-guest:
	./scripts/generate-guest-proto.sh

dev-bootstrap:
	./scripts/dev-bootstrap.sh

dev-doctor:
	./scripts/dev-doctor.sh

dev-up: proto-generate
	mkdir -p .tools/bin
	cd control && go build -o ../.tools/bin/obscura-devlab ./cmd/devlab
	./scripts/envtest-assets.sh ../.tools/bin/obscura-devlab --crds=../deploy/crds

dev-smoke:
	cd control && go run ./cmd/devlab --mode=smoke --url="$${OBSCURA_GATEWAY_URL:-http://127.0.0.1:9000}"

test-controlplane: proto-generate
	./scripts/envtest-assets.sh go test -tags=envtest ./internal/controller ./internal/devlab -count=1 -timeout=4m

test-mac: test test-controlplane

test-race: proto-generate
	cd control && go test -race ./internal/backend ./internal/controller ./internal/kubegateway ./internal/devruntime

test-manifests:
	cd control && go test ./api/v1alpha1 -count=1
	kubectl kustomize deploy >/dev/null
	kubectl kustomize deploy/overlays/native >/dev/null
	kubectl kustomize deploy/overlays/kind >/dev/null

kind-up:
	./scripts/kind-local.sh up

kind-smoke:
	./scripts/kind-local.sh smoke

kind-status:
	./scripts/kind-local.sh status

kind-down:
	./scripts/kind-local.sh down
