# Contributing to stellwerk

Contributions are welcome: bug reports, recipes for apps, new source kinds and target types.

## License of contributions

stellwerk is Apache-2.0. By submitting a contribution you agree it is licensed under the same terms, which also follows from Section 5 of the Apache License. There is no CLA.

Sign off every commit (`git commit -s`). The `Signed-off-by` trailer asserts the [Developer Certificate of Origin](https://developercertificate.org/): you have the right to submit the change under the project license.

## Setup

You need Go (the version in `go.mod`) and a C compiler for SQLite (CGO).

```sh
make tools         # installs the pinned linters, scanners and fleetlint
prek install       # git hooks from .pre-commit-config.yaml
make               # check-fast: format check, lint on changed code, short tests
make check         # the definition of green, the same target CI runs
```

`make check` runs the formatter check, golangci-lint, `go vet` with and without HA, the tests with the race detector (plus the `noha` build and the kill and restart test), a coverage floor, govulncheck, the license allowlist, a tidy module graph, gitleaks, the schema and example config checks, and fleetlint. The Matrix end-to-end test needs a Synapse, see `test/e2e/e2e_test.go`.

## Working rules

- The core (`internal/core`) stays pure: no I/O, no adapters. New technology enters as an adapter behind a port. `internal/archtest` fails the build otherwise.
- A new adapter is done when it passes its port's contract suite (`portstest`, `enginetest`). [docs/extending.md](docs/extending.md) shows how.
- Config changes: update `stellwerk.example.yaml`, run `go run -tags goolm ./cmd/stellwerk -schema > docs/stellwerk.schema.json`.
- Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `docs:`, ...). The hook checks it.
- User-facing changes get a line in CHANGELOG.md under the next version.

## Prose

Docs are declarative and on point: what it does, how to use it, why it is built that way. No marketing words.
