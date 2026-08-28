SHELL = /bin/bash
PROJECT_ROOT = $(dir $(abspath $(lastword $(MAKEFILE_LIST))))
PUBLISH_REPOSITORY ?= ghcr.io/bousborne/rreading-glasses
PUBLISH_PLATFORMS ?= linux/amd64,linux/arm64
PUBLISH_BUILDER ?=
GIT_SHA ?= $(shell git rev-parse --short=7 HEAD)
HARDCOVER_TAG ?= hardcover-sha-$(GIT_SHA)
GOODREADS_TAG ?= goodreads-sha-$(GIT_SHA)
BUILDER_FLAG = $(if $(strip $(PUBLISH_BUILDER)),--builder $(PUBLISH_BUILDER),)

.PHONY: all
all: build lint test

.PHONY: generate
generate: go.mod $(wildcard *.go) $(wildcard */*.go)
	go generate ./...

.PHONY: build-hc
build-hc: generate go.mod $(wildcard *.go) $(wildcard */*.go)
	go build -o $(PROJECT_ROOT)/bin/rghc ./cmd/rghc/...

.PHONY: build-gr
build-gr: generate go.mod $(wildcard *.go) $(wildcard */*.go)
	go build -o $(PROJECT_ROOT)/bin/rggr ./cmd/rggr/...

.PHONY: serve-gr
serve-gr:
	go run ./cmd/rggr/main.go serve --verbose --upstream=www.goodreads.com --port 8080

.PHONY: serve-hc
serve-gc:
	go run ./cmd/rghc/main.go serve --verbose --port 8080 --hardcover-auth "Bearer $(HARDCOVER_API_KEY)"

.PHONY: build
build: build-hc build-gr

.PHONY: lint
lint:
	golangci-lint run --fix --timeout 10m

.PHONY: test
test:
	go test -v -count=1 -race -coverpkg=./... -covermode=atomic -coverprofile=coverage.txt ./...

.PHONY: release-hc
release-hc: release-check
	docker buildx build -f Dockerfile \
		$(BUILDER_FLAG) \
		--platform $(PUBLISH_PLATFORMS) \
		--tag $(PUBLISH_REPOSITORY):$(HARDCOVER_TAG) \
		--build-arg RGPATH=./cmd/rghc \
		--push \
		.
	docker buildx imagetools inspect $(PUBLISH_REPOSITORY):$(HARDCOVER_TAG)
	@echo "Published $(PUBLISH_REPOSITORY):$(HARDCOVER_TAG)"

.PHONY: release-gr
release-gr: release-check
	docker buildx build -f Dockerfile \
		$(BUILDER_FLAG) \
		--platform $(PUBLISH_PLATFORMS) \
		--tag $(PUBLISH_REPOSITORY):$(GOODREADS_TAG) \
		--build-arg RGPATH=./cmd/rggr \
		--push \
		.
	docker buildx imagetools inspect $(PUBLISH_REPOSITORY):$(GOODREADS_TAG)
	@echo "Published $(PUBLISH_REPOSITORY):$(GOODREADS_TAG)"

.PHONY: release-check
release-check:
	@git diff --check
	@test -z "$$(git status --porcelain)" || { \
		echo "Refusing to publish from a dirty worktree; commit the verified changes first." >&2; \
		exit 1; \
	}
	@docker buildx inspect $(PUBLISH_BUILDER) >/dev/null

.PHONY: release
release: release-hc release-gr
