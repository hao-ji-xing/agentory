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
internal/source/codex/       Codex transcript parser
internal/index/              SQLite schema, incremental sync
internal/query/              search, context, listings, snippets
internal/cli/                commands, flags, rendering
skills/agentory/             Claude Code skill that teaches agents to use the CLI
testdata/                    synthetic transcripts used by tests
```

## Adding a source

1. Create `internal/source/<name>/` implementing `model.Source`.
   `ParseLine` must be stateless: one raw line in, messages and/or session
   metadata out. If a line cannot be understood without earlier lines (Codex
   writes the session id only once), also implement `model.FileSource`; its
   parser must recover that context from the head of the file when parsing
   resumes mid-file.
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

## Releasing

Push a tag such as `v0.2.0`. The release workflow runs the tests, builds a
snapshot and checks it with `scripts/test-install.sh` (serve the archives
locally, run `install.sh` against them, verify the binary and the skill), and
then publishes the archives and `checksums.txt` with GoReleaser. To run the
same check locally:

```sh
goreleaser release --snapshot --clean && sh scripts/test-install.sh
```
