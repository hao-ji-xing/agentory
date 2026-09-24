# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Claude Code source: parses `~/.claude/projects/**/*.jsonl`, including
  sub-agent transcripts, session titles (custom titles win over generated
  ones) and all message kinds (`prompt`, `reply`, `think`, `command`,
  `summary`, `tool_use`, `tool_result`, `meta`, `system`).
- Noise removal: `<system-reminder>`, `<local-command-caveat>` and
  `<local-command-stdout>` blocks are stripped; slash commands become
  `kind=command`; interruptions and caveats become `kind=meta`.
- SQLite index with an external-content FTS5 trigram table kept in sync by
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
- `top --by <dimension>` aggregates matching messages by skill, slash
  command, tool, tool input field (`input:<key>`), project, branch, session,
  kind, role, source or day.
- `search --full-text` prints whole messages and adds `text` to JSON hits.
- A Claude Code skill (`skills/agentory/SKILL.md`) that teaches the agent when
  and how to search history and compute usage statistics.

### Changed

- Tool input is rendered with short values first, so identifying fields such
  as `skill` or `subagent_type` survive truncation of long arguments. Existing
  indexes are rebuilt automatically (schema version 2).
