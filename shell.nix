# Reproducible dev shell for postlite.
#
# Postlite links against SQLite via cgo, so a C compiler is required, and it must
# be built/tested with the `vtable` build tag. This shell provides Go, gcc, the
# sqlite CLI (handy for inspecting databases) and the postgresql client (psql) for
# manual end-to-end checks.
#
#   nix-shell            # enter the dev shell
#   make test            # CGO_ENABLED=1 go test -tags vtable ./...
{ pkgs ? import <nixpkgs> { } }:

pkgs.mkShell {
  packages = [
    pkgs.go
    pkgs.gcc
    pkgs.sqlite
    pkgs.postgresql
  ];

  shellHook = ''
    export CGO_ENABLED=1
    echo "postlite dev shell — build/test with: make test  (uses -tags vtable)"
  '';
}
