# agentory

English | [简体中文](README.zh-CN.md)

[![CI](https://github.com/hao-ji-xing/agentory/actions/workflows/ci.yml/badge.svg)](https://github.com/hao-ji-xing/agentory/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**Full-text search over your AI coding agent conversation history.**

`agentory` indexes the session transcripts that coding agents leave on your
disk (currently [Claude Code](https://docs.claude.com/en/docs/claude-code);
the source layer is pluggable) into a local SQLite FTS5 database, so that you
— or the agent itself — can answer *"did we discuss X before, and what was the
conclusion?"* in milliseconds.

- **Built for agents first.** Compact output, `--json` everywhere, stable
  message ids (`agentory show 4213 -C 3`) that an agent can follow up on.
- **Single static binary.** Pure Go (`CGO_ENABLED=0`, `modernc.org/sqlite`),
  no Python, no Node, no daemon.
- **Always fresh.** Every query first does an incremental sync that only reads
  the bytes appended since last time, so the session you are in right now is
  searchable.
- **Chinese-friendly.** Trigram FTS5 index, with a transparent `LIKE` fallback
  for 1–2 character terms such as `发票`.
- **Noise-free.** System reminders, command caveats, local command output and
  other injected text are stripped; tool traffic is indexed but hidden unless
  you ask for it.
- **Local only.** The index lives in your home directory. Nothing is ever sent
  over the network.

## Demo

The output below is produced from the synthetic fixture in [`testdata/`](testdata):

```console
$ agentory index
2 files: 2 new, 0 appended, 0 rebuilt, 0 unchanged
15 messages indexed in 2ms
index: ~/.local/share/agentory/index.db (201.2 KiB)

$ agentory 发票 折扣
#4  2026-08-20 01:01  shop/feat-invoice  u>
  Why does the invoice total double count the discount? 发票金额为什么重复扣了折扣？
  → agentory show 4 -C 3

$ agentory "invoice total" --json | jq '.hits[0] | {id, project, branch, kind, snippet}'
{
  "id": 9,
  "project": "shop",
  "branch": "feat-invoice",
  "kind": "reply",
  "snippet": "applyDiscount runs twice: once per line and again on the invoice total. 结论：折扣只应在行级别应用一次。"
}

$ agentory sessions
2026-08-20 02:00  5f0c1d2e  shop/feat-invoice               13 msgs  Fix invoice discount

$ agentory 折扣 --explain | head -1
plan: mode=like  terms: "折扣"→LIKE (shorter than 3 chars)
```

Markers: `u>` prompt, `u/` slash command, `a>` reply, `a~` thinking,
`a$ Tool` tool call, `t<` tool result, `Σ` compaction summary, `m:` meta,
`s:` system, `[sub:…]` sub-agent.

## Install

With Go 1.26+:

```sh
go install github.com/hao-ji-xing/agentory@latest
```

Or download a prebuilt binary for Linux, macOS or Windows (amd64/arm64) from
the [releases page](https://github.com/hao-ji-xing/agentory/releases), put it on
your `PATH`, and run `agentory doctor`.

From source:

```sh
git clone https://github.com/hao-ji-xing/agentory && cd agentory
make install        # or: make build && ./agentory doctor
```

## Quick start

```sh
agentory index                      # first build (about a minute for ~1.4 GB of transcripts)
agentory "lock ordering"            # search; same as `agentory search ...`
agentory show 4213 -C 3             # read a hit with 3 messages of context
agentory sessions -p shop -s 7d     # recent sessions of a project
```

The first query builds the index automatically if you skip `agentory index`.

### Let your agent use it

This repository ships a Claude Code skill in
[`skills/agentory/SKILL.md`](skills/agentory/SKILL.md). Install it once and
Claude will reach for `agentory` on its own when you ask things like "did we
discuss X before?" or "which skills did I use last week?":

```sh
mkdir -p ~/.claude/skills
ln -s "$PWD/skills/agentory" ~/.claude/skills/agentory   # from a clone
```

Other agents: add something like this to their instructions file:

```markdown
## Conversation history
Before re-deriving a past decision, search earlier sessions:
`agentory "<keywords>" --json -n 10` → then `agentory show <id> -C 3 --json`.
Filters: `-p <project>`, `-s 30d`, `-k prompt,reply`, `--all` for tool output.
```

## Commands

```
agentory <query> [flags]              same as search (the most common path)
agentory search <query> [flags]
agentory show <msg-id|session-id> [-C N]
agentory top --by <dimension> [query] [flags]
agentory sessions [flags]
agentory projects
agentory index [--full] [--rebuild] [--prune] [-v]
agentory stats
agentory doctor                       environment and index health check
agentory watch                        keep the index updated (fsnotify)
```

### Search flags

| Flag | Meaning |
|---|---|
| `-p, --project <substr>` | project name or working-directory substring |
| `-s, --since <when>` | `30m`, `12h`, `7d`, `2w`, `3mo`, `today`, `yesterday`, `2026-09-01`, `2026-09-01 14:00` |
| `-u, --until <when>` | same syntax; a bare date includes that whole day |
| `-k, --kind <list>` | default `prompt,reply,think,command,summary` |
| `--role <role>` | `user`, `assistant` or `system` |
| `--tool <name>` | tool calls of one tool (e.g. `--tool Bash`) |
| `--branch <name>` | git branch |
| `--source <list>` | history source (currently `claude`) |
| `-n, --limit <n>` | max results (default 20), newest first |
| `-C, --context <n>` | n messages before/after each hit (same transcript only) |
| `--all` | also search `tool_use`, `tool_result`, `meta`, `system` |
| `--include-subagent` | include sub-agent (sidechain) messages |
| `--json` | machine-readable output |
| `--no-sync` | skip the incremental sync before the query |
| `--explain` | show whether each term used FTS5 `MATCH` or the `LIKE` fallback |
| `--full-text` | print whole messages; in `--json` add a `text` field next to the snippet |
| `--color <when>` | `auto` (default; off when not a TTY or `NO_COLOR` is set), `always`, `never` |

Query syntax: whitespace-separated terms are ANDed; each term is a
case-insensitive substring; wrap a phrase in double quotes (`"lock ordering"`).
Terms of three or more characters use the trigram index; shorter ones fall
back to `LIKE`.

### Usage statistics: `top`

`agentory top --by <dimension>` counts matching messages per group, with the
same query terms and filters as `search`:

```console
$ agentory top --by skill --since 7d
    25  ic-commit          last 2026-09-23 17:51
    14  ic-web-debug       last 2026-09-23 17:27
    10  ic-review          last 2026-09-23 17:42
…
134 messages in 35 groups by skill, showing 20 (use -n for more)
```

| `--by` | groups by |
|---|---|
| `skill` | skill name of Skill tool calls |
| `command` | slash commands you typed |
| `tool` | tool name |
| `input:<key>` | one tool input field, e.g. `input:subagent_type` |
| `project`, `branch`, `session`, `kind`, `role`, `source` | as named |
| `day` | local calendar day |

### Message kinds

| kind | what | searched by default |
|---|---|---|
| `prompt` | what you actually typed | yes |
| `reply` | assistant text | yes |
| `think` | assistant thinking | yes |
| `command` | slash command (`/model opus`) | yes |
| `summary` | context-compaction summary | yes |
| `tool_use` | tool call arguments, rendered as `key=value` lines | `--all` |
| `tool_result` | tool output (first 2,000 chars; 40,000 with `index --full`) | `--all` |
| `meta` | injected context, interruptions | `--all` |
| `system` | system events | `--all` |

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `AGENTORY_DB` | `$XDG_DATA_HOME/agentory/index.db` or `~/.local/share/agentory/index.db` | index location |
| `CLAUDE_CONFIG_DIR` | `~/.claude` | where Claude Code keeps `projects/` |
| `NO_COLOR` | unset | disable colors |

## How it works

- Transcripts are append-only JSONL. For every file the index stores the byte
  offset of the last complete line. A sync `stat`s all files and only parses
  bytes past that offset. A trailing half-written line is left for the next
  sync.
- Before resuming, it checks that the byte before the offset is a newline and
  that the first 4 KiB are unchanged; if a file was rewritten, shrank, went
  back in time, or the truncation mode changed, that file is re-indexed from
  scratch.
- `msgs_fts` is an external-content FTS5 table (`tokenize='trigram'`) kept in
  sync by `AFTER INSERT/DELETE/UPDATE` triggers.
- Deleted transcripts stay searchable (agents clean up old sessions on their
  own); use `agentory index --prune` to forget them.
- Sources implement a small interface (`internal/model.Source`); adding another
  agent is one package plus one line in `internal/source/registry.go`. Every
  table has a `source` column already.

## FAQ

**Why not just `grep` / `rg`?**
Raw speed is not the problem — `rg` scans 1.4 GB in under two seconds. The
problem is that one JSONL line is one huge escaped JSON message: matches can't
be grouped by session, time, role or tool, injected reminders and tool output
drown the real conversation, and there is no way to say "show me the three
messages around this hit". `agentory` parses the transcripts once, cleans them,
and keeps structure next to a full-text index.

**Why not a trigram server like `tgrep`?**
Those shine with hundreds of thousands of files. A typical history is ~1,000
files; a single SQLite file is simpler and needs no daemon.

**Does it send my conversations anywhere?**
No. `agentory` has no network code. The index is a SQLite file under your home
directory (see `AGENTORY_DB`) in a directory created with mode `0700`, and it
is never written inside a repository. Delete it at any time; it is fully rebuildable.
Note that the index contains the same sensitive material as your transcripts
(including anything you pasted into a session), so treat it like them.

**Why do 2-character searches feel slower?**
Trigram indexes can't match terms shorter than three characters, so those
terms are scanned with `LIKE`. Add a longer term or a filter (`-p`, `-s`) to
narrow the scan; `--explain` shows which path each term takes.

**How big is the index?**
On a real history of 1,025 Claude Code transcripts (1.4 GB of JSONL) a full
build took 50 s and produced a 636 MiB index on an Apple Silicon laptop; later
syncs take tens of milliseconds.

**Codex support?**
Planned. The storage and source interface are ready; the parser is not
written yet.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Test fixtures must be synthetic —
never commit real transcripts.

## License

[MIT](LICENSE) © haojixing
