.PHONY: template-check fmt test test-integration sandbox-check sandbox-smoke clean

template-check:
	./scripts/sandbox/validate-template.sh

fmt:
	@echo "TODO(adopter): run the formatter for the runtime language(s)"

test:
	@echo "TODO(adopter): run unit and contract tests"

test-integration:
	./scripts/sandbox/check-host.sh
	./scripts/sandbox/smoke-test.sh

sandbox-check:
	./scripts/sandbox/check-host.sh

sandbox-smoke:
	./scripts/sandbox/smoke-test.sh

clean:
	./scripts/sandbox/cleanup.sh
