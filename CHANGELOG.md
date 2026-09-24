# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.1.1] - 2026-09-24

### Fixed

- Grouping by local time (`top --by day`, `hour`, `week`, …) was one hour off
  during daylight saving time on Windows, where Go's bundled zone data made
  the transition walk stop early. Transitions are now found by sampling the
  UTC offset daily and bisecting each change.

## [0.1.0] - 2026-09-24

### Added

- Codex source: parses `$CODEX_HOME/sessions/**/rollout-*.jsonl` and
  `archived_sessions/`, with thread titles from `session_index.jsonl`, for the
  transcript layouts of Codex 0.132 through 0.153. Prompts, replies, reasoning
  summaries, tool calls and results, interruptions, turns and token usage are
  mapped onto the shared kinds; duplicated UI events and injected context are
  left out. `codex exec` prompts get `prompt_source=sdk`, automation runs
  `prompt_source=automation`; `[$skill](…)` mentions and `SKILL.md` reads
  count as skill invocations.
- `model.FileSource`: an optional source interface for transcripts whose lines
  depend on earlier lines of the same file.
- Moved transcripts are recognized by file name and keep their rows instead of
  being indexed twice (`index` reports them as `moved`).
- `doctor` reports a missing agent directory as skipped rather than failed.
- `install.sh` (macOS, Linux) and `install.ps1` (Windows) install the latest
  GitHub release: checksum-verified binary in `~/.local/bin` plus the agent
  skill for Claude Code and Codex, unless `npx skills` manages the skill.
  Release archives now bundle the skill.
- The skill can be added on its own with `npx -y skills add
  hao-ji-xing/agentory -g -y`; it then installs the latest release of the CLI
  on first use and upgrades it when a newer release exists.
- CI builds a GoReleaser snapshot and runs the installer against it
  (`scripts/test-install.sh`), so a broken archive or installer fails the
  build before a release.

- Claude Code source: parses `~/.claude/projects/**/*.jsonl`, including
  sub-agent transcripts, session titles (custom titles win over generated
  ones) and all message kinds (`prompt`, `reply`, `think`, `command`,
  `summary`, `tool_use`, `tool_result`, `meta`, `system`).
- Noise removal: `<system-reminder>`, `<local-command-caveat>` and
  `<local-command-stdout>` blocks are stripped; slash commands become
  `kind=command`; interruptions and caveats become `kind=meta`.
- SQLite index with a contentless FTS5 trigram table kept in sync by
  triggers, and a `source` column on every table for future providers.
- Incremental sync before every query: only appended bytes are parsed; files
  that were rewritten, truncated or indexed with another truncation mode are
  rebuilt; half-written trailing lines are deferred.
- Search with AND semantics, quoted phrases, transparent `LIKE` fallback for
  terms shorter than three characters, filters (`-p`, `-s`, `-u`, `-k`,
  `--role`, `--tool`, `--branch`, `--source`), `-C` context, `--explain`,
  `--json`, and match highlighting.
- Commands: `search`, `show`, `sessions`, `projects`, `index`, `stats`,
  `doctor`, `watch`, `version`.
- Tool text truncation (2,000 characters, or 40,000 with `index --full`).
- `top --by <dim>[,<dim>]` aggregates by one or two dimensions — skill,
  slash command, sub-agent type, invocation name or actor, tool, file, tool
  outcome, tool input field, model, main vs sub-agent, project, branch,
  session, kind, role, source, day, week, month, hour or weekday — with
  `--measure count|tokens|turns|cost`.
- `usage <name>` shows how one command, skill or sub-agent type is used:
  who invoked it, argument groups, failures, interruptions and the prompt
  that followed.
- `sql` runs read-only queries (row limit, timeout); `schema` documents the
  tables.
- Token usage per API request (deduplicated across the lines a request is
  written on), agent turn durations, session cost, models, tool call
  outcomes, file paths and prompt sources are indexed.
- Prompts typed while the agent was busy (recorded only as queued-command
  attachments) are indexed as prompts.
- `search --full-text` prints whole messages and adds `text` to JSON hits.
- A Claude Code skill (`skills/agentory/SKILL.md`) that teaches the agent when
  and how to search history and compute usage statistics.

### Changed

- Tool input is rendered with short values first, so identifying fields such
  as `skill` or `subagent_type` survive truncation of long arguments.
- The full-text index is contentless and leaves out tool call arguments and
  output (about 80% of the text); searches that include those kinds scan
  them with LIKE. A full build of ~1.4 GB of transcripts drops from about
  60 s to about 20 s and the index from 720 MiB to 430 MiB.
- Existing indexes are rebuilt automatically (schema version 4).
- Grouping by local time uses precomputed zone offsets instead of SQLite's
  `localtime` modifier, which was about 30 times slower.
