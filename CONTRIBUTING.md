# Contributing to agentory

Thanks for your interest! Bug reports, fixes and new sources are welcome.

## Development

Requirements: Go (see `go.mod` for the version). No C toolchain is needed;
everything builds with `CGO_ENABLED=0`.

```sh
make test     # go test ./...
make lint     # gofmt -l + go vet
make build    # ./agentory
make cross    # linux/darwin/windows × amd64/arm64 into dist/
```

Point the binary at a scratch index while hacking so you don't disturb your
real one:

```sh
AGENTORY_DB=/tmp/agentory-dev.db ./agentory index
```

## Layout

```
main.go                      entry point
internal/model/              provider-agnostic types and the Source interface
internal/source/registry.go  list of built-in sources
internal/source/claudecode/  Claude Code transcript parser
internal/index/              SQLite schema, incremental sync
internal/query/              search, context, listings, snippets
internal/cli/                commands, flags, rendering
testdata/                    synthetic transcripts used by tests
```

## Adding a source

1. Create `internal/source/<name>/` implementing `model.Source`.
   `ParseLine` must be stateless: one raw line in, messages and/or session
   metadata out.
2. Map every record onto the shared `model.Kind` set and strip injected noise.
3. Register the constructor in `internal/source/registry.go`.
4. Add table-driven parser tests and at least one end-to-end CLI test.

## Tests must catch regressions

A test that would still pass after the implementation is broken is not
worth having. When you add behavior, check that the new test fails if you
revert or break the change (e.g. drop a cleaning rule, remove a trigger,
skip the resume check).

## Privacy: synthetic fixtures only

Never commit real conversation transcripts, not even fragments. Everything
under `testdata/` must be invented. Use fake home directories such as
`/home/alice`; a test guards against `/Users/` paths and obvious secrets.

## Commits and pull requests

- Use [Conventional Commits](https://www.conventionalcommits.org/)
  (`feat:`, `fix:`, `docs:`, `test:`, `refactor:`, `ci:`, `chore:`).
- Keep `gofmt -l .` empty and `go vet ./...` clean; CI checks both.
- Update `CHANGELOG.md` under **Unreleased** for user-visible changes.
