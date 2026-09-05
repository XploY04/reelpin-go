# make check is what CI runs. If a problem gets past it, the fix is a new check
# here, not a command someone is expected to remember.

GO ?= go

.PHONY: help
help:
	@echo "check              everything CI runs: fmt, vet, unit, race, docs, boundaries"
	@echo "fmt                rewrite files with gofmt"
	@echo "test               unit tests"
	@echo "test-race          unit tests under the race detector"
	@echo "test-integration   tagged tests; needs TEST_DATABASE_URL"
	@echo "run                run the API"
	@echo "up / down          the local docker compose stack"

.PHONY: check
check: fmt-check vet test test-race docs boundaries
	@echo "\nall checks passed"

.PHONY: fmt
fmt:
	$(GO) fmt ./...

.PHONY: fmt-check
fmt-check:
	@unformatted=$$(gofmt -l cmd internal); \
	if [ -n "$$unformatted" ]; then \
		echo "these files are not gofmt clean:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi
	@echo "gofmt clean"

.PHONY: vet
vet:
	$(GO) vet ./...

.PHONY: test
test:
	$(GO) test ./...

.PHONY: test-race
test-race:
	$(GO) test -race ./...

# Skips itself without TEST_DATABASE_URL rather than failing, so `make check`
# stays runnable on a laptop with nothing running.
.PHONY: test-integration
test-integration:
	$(GO) test -tags=integration ./...

.PHONY: docs
docs:
	@python3 scripts/check_docs.py

.PHONY: boundaries
boundaries:
	@python3 scripts/check_boundaries.py

.PHONY: run
run:
	$(GO) run ./cmd/api

.PHONY: up
up:
	docker compose up -d

.PHONY: down
down:
	docker compose down
