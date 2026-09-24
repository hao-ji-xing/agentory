---
name: agentory
description: >
  Search and analyze the user's past AI coding agent conversations (Claude Code
  transcripts under ~/.claude/projects) with the `agentory` CLI: full-text search
  across every session, read a hit with surrounding context, list sessions and
  projects, aggregate usage (skills, slash commands, tools, sub-agents, files,
  errors, per project, model or day), show how one command or skill is used
  (its arguments, failures, interruptions and the user's follow-up), and report
  tokens, prompt-cache hit rate, time and cost. Use it when the user asks
  whether something was discussed before, what was concluded or decided last
  time, what they worked on in a project or time range, how a past bug was
  fixed, how their way of using the agent changed, or for usage statistics
  such as "which skills did I use last week and how" — in any language
  (e.g. 以前聊过吗 / 上次结论是什么 / 过去一周 skill 使用统计 / 我给某个命令都跟了什么 /
  token 和缓存命中). Also use it before re-deriving a decision that may already
  exist in an earlier session. Do not use it for the current conversation's
  own context (it is already in front of you) or for searching source code
  (use grep/rg).
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
| How is one command or skill used (arguments, failures, what came next)? | `agentory usage <name> -s 30d --json` |
| Tokens, cache hit rate, cost, time spent | `agentory top --by <dim> --measure tokens\|turns\|cost --json` |
| Anything else | `agentory schema`, then `agentory sql "SELECT …" --json` |
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
  events are only searched with `--all` or `-k`; tool text is scanned rather
  than full-text indexed, so such searches take a few hundred milliseconds.
- Sub-agent (sidechain) messages are excluded unless `--include-subagent`.
- Prompts typed while the agent was busy (queued) are indexed like any other.
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

## Aggregations with `top`

`agentory top --by <dim>[,<dim>] [query] [filters] [--measure m] --json`
groups the index by one or two dimensions.

| `--by` | Groups | Measures |
|---|---|---|
| `skill` | skills the agent invoked (Skill tool) | count |
| `command` | slash commands the user typed (`/deploy`) | count |
| `subagent_type` | sub-agents the agent started | count |
| `name`, `actor` | any invocation; `user` (typed) vs `agent` (invoked) | count |
| `tool`, `file` | tool calls; files they touched | count |
| `error` | tool call outcome: `ok`, `error`, `no result` | count |
| `input:<key>` | one tool input field | count |
| `kind`, `role` | message kind / role | count |
| `model` | model | count, tokens |
| `agent` | `main` vs `subagent` | count, tokens, turns |
| `project`, `branch`, `session`, `source` | as named | all |
| `day`, `week`, `month`, `hour`, `weekday` | local time (oldest first) | all |

Measures (`--measure`, default `count`):

- `count` — messages matching the query and filters; invocation dimensions
  also report `with_args` (uses that carried arguments).
- `tokens` — API requests (deduplicated): `input` (uncached), `output`,
  `cache_read`, `cache_write_5m/1h`, `thinking`, `cache_hit_rate` (%).
- `turns` — completed agent turns: count, total and average duration.
- `cost` — sessions' self-reported cost at API list prices (`usd`), lines
  added/removed; grouped by the session's start. It is not the
  subscription bill — say so when you report it.

A query, `-k`, `--role` and `--tool` apply only to `count`. JSON has `total`,
`groups`, `all` (sums over every group) and `buckets` (`key`, `keys` for two
dimensions, `count`, `last`, `label` = session title, and the measure's
object). `-n` limits the buckets (default 20); raise it for the long tail.
`--key <value>` keeps one value of the first dimension.

## How a command or skill is used: `usage`

`agentory usage <name> [-s 30d] [-p P] --json` covers both what the user typed
(`/name args`) and what the agent invoked (Skill tool, sub-agent type):
`by_actor`, `with_args`, `errors`, `interrupted` (the user interrupted
before the next prompt), `projects`, `arg_groups` (distinct arguments with
counts, most frequent first; `-n` sets how many) and `recent` uses (`-r`)
with `next` — the prompt the user wrote afterwards, which shows whether they
corrected or continued (`next_source: queued` means it was typed while the
invocation was still running). A leading `/` is optional; unknown names
return `suggestions`. Names are matched exactly: aliases such as `ic:x` and
`ic-x` are separate — check `top --by name` and combine them yourself.

## Recipes

```sh
# "Which skills did I use last week, and how?"
agentory top --by name,actor -s 7d --include-subagent --json -n 100
agentory usage code-review -s 7d --json                  # then drill into the interesting ones
# Typed commands include built-ins such as /clear, /model, /compact —
# call them out separately rather than listing them as skills.

agentory top --by day -s 30d --json                      # activity over time
agentory top --by week --measure cost -s 90d --json      # cost trend
agentory top --by model --measure tokens -s 30d --json   # tokens and cache hit rate per model
agentory top --by project --measure turns -s 7d --json   # where the time went
agentory top --by tool,error -s 30d --json               # failing tools
agentory top --by file -p erp -s 14d --json              # most-touched files
agentory top --by session "deadlock" --json              # sessions that discussed a topic
```

## Your own SQL

`agentory schema` prints every table and column (messages, the
`invocations` view, `requests` with token usage, `turns`, `sessions` with
cost). `agentory sql "<SELECT …>" --json` runs one read-only statement
(writes are rejected; 1000 rows and 30 s by default, `-n`/`--timeout` to
change). Timestamps are Unix milliseconds; use
`datetime(ts/1000, 'unixepoch', 'localtime')`. Prefer `top`/`usage` when they
answer the question.

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
