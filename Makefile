# Postlite requires cgo (go-sqlite3) and the `vtable` build tag — without the tag
# the package does not compile (it uses sqlite3.VTab et al). On hosts without a
# system C compiler (e.g. NixOS), run these inside `nix-shell` (see shell.nix),
# which provides gcc + sqlite + psql, or wrap a target, e.g.:
#   nix-shell --run 'make test'
GO      ?= go
TAGS    ?= vtable
DATA_DIR ?= ./data
export CGO_ENABLED = 1

# Docker image settings. IMAGE is the Docker Hub repository to build/push to and
# DOCKER_USER is the account used for `docker login`.
DOCKER      ?= docker
IMAGE       ?= malayh/postlite
DOCKER_USER ?= malayh

.PHONY: test build run vet fmt tidy docker-login build-image push-image

test:
	$(GO) test -tags $(TAGS) ./...

build:
	$(GO) build -tags $(TAGS) -o bin/postlite ./cmd/postlite

run: build
	./bin/postlite -data-dir $(DATA_DIR)

vet:
	$(GO) vet -tags $(TAGS) ./...

fmt:
	$(GO) fmt ./...

tidy:
	$(GO) mod tidy

# --- Docker image ---------------------------------------------------------

# Authenticate with Docker Hub as DOCKER_USER (prompts for the password/token).
docker-login:
	$(DOCKER) login -u $(DOCKER_USER)

# Build the image tagged :latest (and :$(VERSION) too when VERSION is set).
build-image:
	$(DOCKER) build -t $(IMAGE):latest $(if $(VERSION),-t $(IMAGE):$(VERSION),) .

# Build and push a release. Pass the version non-interactively with
#   make push-image VERSION=1.2.3
# or run `make push-image` and you'll be prompted for it. Pushes both the
# version tag and :latest to $(IMAGE).
push-image:
	@VERSION="$(VERSION)"; \
	if [ -z "$$VERSION" ]; then printf "Release version (e.g. 1.2.3): "; read VERSION; fi; \
	if [ -z "$$VERSION" ]; then echo "error: a version is required" >&2; exit 1; fi; \
	echo "Building and pushing $(IMAGE):$$VERSION (and :latest)"; \
	$(DOCKER) build -t $(IMAGE):$$VERSION -t $(IMAGE):latest . && \
	$(DOCKER) push $(IMAGE):$$VERSION && \
	$(DOCKER) push $(IMAGE):latest
