---
name: agentory
description: >
  Search and analyze the user's past AI coding agent conversations (Claude Code
  transcripts under ~/.claude/projects) with the `agentory` CLI: full-text search
  across every session, read a hit with surrounding context, list sessions and
  projects, and count usage (which skills, slash commands, tools or sub-agents
  were used, per project or per day). Use it when the user asks whether something
  was discussed before, what was concluded or decided last time, what they were
  working on in a project or time range, how a past bug was fixed, or for usage
  statistics such as "which skills did I use last week" — in any language
  (e.g. 以前聊过吗 / 上次结论是什么 / 过去一周 skill 使用统计). Also use it before
  re-deriving a decision that may already exist in an earlier session.
  Do not use it for the current conversation's own context (it is already in
  front of you) or for searching source code (use grep/rg).
---

# agentory — conversation history search

`agentory` keeps a local SQLite FTS5 index of agent transcripts. Every command
first syncs incrementally (only newly appended bytes are read, typically tens of
milliseconds), so results include the session that is running right now.

Always pass `--json` and parse the output; human output is for people.

## Pick the right command

| Question | Command |
|---|---|
| Did we discuss X? What was decided about X? | `agentory "X" --json -n 10` |
| Read one hit in full with its surroundings | `agentory show <id> -C 3 --json` |
| Replay a whole session | `agentory show <session-id-or-prefix> --json` |
| What happened in project P recently? | `agentory sessions -p P -s 7d --json` |
| Which projects are active? | `agentory projects --json` |
| How often was a skill / command / tool used? | `agentory top --by skill -s 7d --json` |
| Is the index healthy? | `agentory doctor` |

## Search

```sh
agentory "lock ordering" deadlock --json -n 10     # terms are ANDed, quotes = phrase
agentory 发票 -p erp -s 30d --json                  # project substring + time window
agentory "migration" -k prompt,reply --json        # restrict kinds
agentory "go test" --tool Bash --json              # tool calls of one tool
agentory "panic:" --all --json                     # include tool output / meta / system
```

- Matching is case-insensitive substring. Terms of 3+ characters use the FTS5
  trigram index; 1–2 character terms (e.g. two-character Chinese words) fall
  back to a slower `LIKE` scan — add a longer term or a filter when possible.
  `--explain` shows which path each term took.
- Default kinds are `prompt,reply,think,command,summary`. Tool calls
  (`tool_use`), tool output (`tool_result`), injected text (`meta`) and system
  events are only searched with `--all` or `-k`.
- Sub-agent (sidechain) messages are excluded unless `--include-subagent`.
- Results are newest first. `-n` caps the number of hits (default 20).
- Time syntax for `-s/--since` and `-u/--until`: `30m`, `12h`, `7d`, `2w`,
  `3mo`, `today`, `yesterday`, `2026-09-01`, `2026-09-01 14:00` (local time; a
  bare date in `--until` includes that day).

Each hit has `id`, `time`, `project`, `branch`, `kind`, `session_id`, `title`
and a ~300-character `snippet`. Add `--full-text` when you need the complete
stored text of every hit (for example to extract a field); otherwise follow up
on promising hits with `agentory show <id> -C 3 --json`, which returns full
messages plus neighbours from the same transcript.

Stored tool text is truncated to 2,000 characters (40,000 after
`agentory index --full`); `n_chars` tells the original length.

## Usage statistics with `top`

`agentory top --by <dimension> [query] [filters] --json` counts matching
messages per group. Dimensions:

| `--by` | Groups | Counts |
|---|---|---|
| `skill` | skill name | invocations of the Skill tool |
| `command` | slash command (`/deploy`) | commands the user typed |
| `tool` | tool name | tool calls |
| `input:<key>` | value of one tool input field | tool calls, e.g. `input:subagent_type` |
| `project`, `branch`, `session`, `kind`, `role`, `source` | as named | messages (default kinds) |
| `day` | local calendar day | messages, oldest day first |

The JSON has `total` (all matching messages), `groups` (distinct keys) and
`buckets` (`key`, `count`, `last`, and `label` = title for sessions), limited
by `-n` (default 20) — raise `-n` when you need the long tail.

Recipes:

```sh
# "Which skills did I use in the past week?" — report both views:
agentory top --by skill   -s 7d --include-subagent --json -n 100   # skills the agent invoked
agentory top --by command -s 7d --json -n 100                      # slash commands the user typed
# Typed slash commands include built-ins such as /clear, /model, /compact —
# call them out separately rather than listing them as skills.

agentory top --by skill -p erp -s 30d --json            # per project
agentory top --by day --tool Skill -s 14d --json        # activity per day
agentory top --by input:subagent_type -s 7d --json      # which sub-agents were launched
agentory top --by project -s 7d --json                  # where the work happened
agentory top --by session "deadlock" --json             # which sessions discussed a topic
```

## Answering well

- Quote conclusions with their date, project and message id so the user can
  open them (`agentory show <id> -C 3`).
- Prefer the most recent relevant hit, but note when later sessions changed an
  earlier decision.
- If nothing matches, try synonyms, the other language, fewer terms, a wider
  `-s` window, `--all`, or `--include-subagent` before concluding it was never
  discussed.
- Transcripts can contain secrets that were pasted into sessions. Do not copy
  long raw excerpts into files, commits or messages to others; summarize.

## Troubleshooting

- `agentory: command not found` → install with
  `go install github.com/hao-ji-xing/agentory@latest` and make sure `$(go env GOPATH)/bin`
  is on `PATH`, or download a release binary.
- First run builds the whole index (about a minute for ~1.4 GB of transcripts).
- Run `agentory doctor` for root directory, FTS consistency and freshness
  checks. `AGENTORY_DB` overrides the index path, `CLAUDE_CONFIG_DIR` the
  Claude Code directory.
