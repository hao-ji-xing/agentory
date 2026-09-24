package index

// SchemaDoc documents the tables and views for `agentory schema`, so that
// people and agents can write their own read-only SQL. Timestamps are Unix
// milliseconds (UTC); convert with
// datetime(ts/1000, 'unixepoch', 'localtime').
const SchemaDoc = `agentory index schema (SQLite; timestamps are Unix milliseconds, UTC;
local time: datetime(ts/1000, 'unixepoch', 'localtime'))

msgs — one searchable message
  full-text: id IN (SELECT rowid FROM msgs_fts WHERE msgs_fts MATCH '"term"')
  (trigram, terms of 3+ characters; tool_use/tool_result rows are not in
  msgs_fts — use text LIKE '%term%' for them)
  id            message id (as printed by search / show)
  file_id       transcript file (files.id); seq orders messages within it
  seq           position within the file
  source        history source, e.g. claude
  session_id    session (sub-agent messages belong to the parent session)
  agent_id      '' for the main agent, else the sub-agent id
  slug          sub-agent type, when recorded
  uuid          record id; parent_uuid links to the previous record
  parent_uuid   see uuid
  ts            timestamp
  role          user | assistant | system
  kind          prompt | reply | think | command | summary | tool_use | tool_result | meta | system
  tool          tool name (tool_use)
  cwd           working directory
  branch        git branch
  n_chars       original length; text of tool_use/tool_result may be truncated
  text          cleaned text; tool_use input is rendered as key=value lines
  model         model of assistant messages
  request_id    API request of assistant messages (requests.request_id)
  tool_use_id   links a tool_use to its tool_result
  is_error      1 when a tool_result reported an error
  prompt_source how a prompt was entered: typed | queued | system | …
  file_path     file a tool call operates on (Read/Edit/Write …)
  inv_kind      command | skill | subagent for invocation messages, else ''
  inv_name      invoked command/skill/sub-agent type, without a leading /
  inv_args      arguments that followed the name

invocations (view over msgs) — one row per command typed or skill/sub-agent started
  msg_id, file_id, seq, source, session_id, agent_id, ts, cwd, branch,
  actor (user | agent), kind, name, args, tool_use_id

requests — token usage per API request (deduplicated)
  request_id    API request id
  file_id, source, session_id, agent_id, ts, cwd, branch, model
  tok_in        uncached input tokens
  tok_out       output tokens (includes tok_think)
  cache_read    input tokens read from the prompt cache
  cache_w5m     input tokens written to the 5-minute cache
  cache_w1h     input tokens written to the 1-hour cache
  tok_think     thinking tokens, when reported

turns — completed agent turns
  id, file_id, source, session_id, agent_id, ts, cwd, branch
  duration_ms   wall time of the turn
  n_msgs        messages in the turn

sessions — one row per session
  id, source, cwd, branch
  project       project key (directory name under the history root)
  title         custom title, else generated title
  title_rank    2 = custom, 1 = generated
  first_prompt  first prompt (up to 300 characters)
  started_at    first message timestamp
  ended_at      last message timestamp
  n_msg         messages of the main agent
  cost_usd      session cost reported by the agent (API list prices)
  lines_added   lines added, as reported by the agent
  lines_removed lines removed, as reported by the agent
  duration_ms   session wall time, as reported by the agent

files — indexed transcript files
  id, source, path, size, mtime, byte_off (resume offset), n_msg,
  full (1 = long tool text mode), head_crc

meta — key/value settings (schema_version, full, last_sync)
  key, value
`
