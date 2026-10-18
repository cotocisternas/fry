.DEFAULT_GOAL := help
export SCENARIO IMAGE
.PHONY: demo smoke
demo:
	./scripts/toolchain.sh exec python3 -m scripts.dev.demo_admin

smoke:
	@if [ -n "$$IMAGE" ]; then \
	  ./scripts/smoke.sh "$${SCENARIO:-help}" "$$IMAGE"; \
	else \
	  ./scripts/smoke.sh "$${SCENARIO:-help}"; \
	fi

.PHONY: setup setup-tools doctor test-deps-build test-deps-check test-deps-lint test-deps-test native-tools-check test-script-processes test-scripts lint-policy-check
setup: setup-tools
	./scripts/test-deps.sh build

setup-tools:
	./scripts/setup.sh

doctor:
	./scripts/doctor.sh

test-deps-build:
	./scripts/test-deps.sh build

test-deps-check:
	./scripts/test-deps.sh check

test-deps-lint:
	./scripts/quality.sh test-deps-lint

test-deps-test:
	./scripts/test-deps.sh test

native-tools-check:
	./tooling/verify-native.sh

test-script-processes:
	./scripts/quality.sh test-script-processes

test-scripts:
	./scripts/quality.sh test-scripts

lint-policy-check:
	./scripts/quality.sh lint-policy-check

.PHONY: baseline-check
baseline-check:
	./scripts/tests/baseline.sh

.PHONY: proto-bootstrap proto-tools-check proto-generate
proto-bootstrap:
	./scripts/generate/proto.sh bootstrap

proto-tools-check:
	./tooling/rpc/verify.sh

proto-generate:
	./scripts/generate/proto.sh generate proto/admin gen/admin
	./scripts/generate/proto.sh generate proto/member gen/member

.PHONY: help fmt fmt-check lint lint-config build vet generate generate-check sqlc sqlc-check sqlc-vet test test-integration test-race test-forks coverage coverage-report test-image toolchain-versions test-browser security proto-check proto-drift-check check release-check

help fmt fmt-check lint lint-config build vet generate generate-check sqlc sqlc-check sqlc-vet test-browser security proto-check proto-drift-check check release-check:
	./scripts/quality.sh $@

test test-integration test-race test-forks coverage coverage-report:
	./scripts/quality.sh $@ $(TEST_FLAGS)

toolchain-versions:
	./scripts/toolchain.sh versions

test-image:
	./scripts/test-deps.sh build
