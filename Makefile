# Postlite requires cgo (go-sqlite3) and the `vtable` build tag — without the tag
# the package does not compile (it uses sqlite3.VTab et al). On hosts without a
# system C compiler (e.g. NixOS), run these inside `nix-shell` (see shell.nix),
# which provides gcc + sqlite + psql, or wrap a target, e.g.:
#   nix-shell --run 'make test'
GO      ?= go
TAGS    ?= vtable
DATA_DIR ?= ./data
export CGO_ENABLED = 1

.PHONY: test build run vet fmt tidy

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
