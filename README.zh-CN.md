# agentory

[English](README.md) | 简体中文

[![CI](https://github.com/hao-ji-xing/agentory/actions/workflows/ci.yml/badge.svg)](https://github.com/hao-ji-xing/agentory/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**跨 AI coding agent 的对话历史全文检索 CLI。**

`agentory` 把 coding agent 留在本地磁盘上的会话记录（目前支持
[Claude Code](https://docs.claude.com/en/docs/claude-code)，数据源层可插拔）
建成本地 SQLite FTS5 索引，让你——或者 agent 自己——在毫秒级回答
「我以前是不是聊过 X？当时的结论是什么？」。

- **首先为 agent 设计**：输出紧凑，所有命令支持 `--json`，带稳定的消息 id
  （`agentory show 4213 -C 3`），agent 可以顺着追问。
- **单个静态二进制**：纯 Go（`CGO_ENABLED=0` + `modernc.org/sqlite`），
  不需要 Python、Node，也没有常驻进程。
- **永远是最新的**：每次查询前先做增量同步，只读上次之后追加的字节，
  所以你**正在进行的这个会话**也能搜到。
- **中文友好**：trigram FTS5 索引；`发票` 这类 1–2 字的词自动、透明地
  降级为 `LIKE` 匹配。
- **去噪**：剥离 system-reminder、命令 caveat、本地命令输出等注入内容；
  工具调用和返回会被索引，但默认不参与搜索。
- **只在本地**：索引库就在你的 home 目录里，不向网络发送任何数据。

## 演示

下面的输出来自 [`testdata/`](testdata) 里的合成假数据：

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

标记含义：`u>` 用户输入，`u/` slash 命令，`a>` 回复，`a~` 思考，
`a$ Tool` 工具调用，`t<` 工具返回，`Σ` 压缩摘要，`m:` 注入/中断，
`s:` 系统事件，`[sub:…]` 子 agent。

## 安装

Go 1.26+：

```sh
go install github.com/hao-ji-xing/agentory@latest
```

或者从 [Releases 页面](https://github.com/hao-ji-xing/agentory/releases) 下载
Linux / macOS / Windows（amd64/arm64）预编译二进制，放进 `PATH` 后运行
`agentory doctor` 自检。

从源码构建：

```sh
git clone https://github.com/hao-ji-xing/agentory && cd agentory
make install        # 或：make build && ./agentory doctor
```

## 快速开始

```sh
agentory index                      # 首次建索引（约 1.4 GB 记录约 20 秒）
agentory "lock ordering"            # 搜索，等价于 agentory search ...
agentory show 4213 -C 3             # 查看命中消息及前后各 3 条
agentory sessions -p shop -s 7d     # 某项目最近 7 天的会话
```

即使不先跑 `agentory index`，第一次查询也会自动建索引。

### 让 agent 自己用

仓库自带一个 Claude Code skill：[`skills/agentory/SKILL.md`](skills/agentory/SKILL.md)。
装一次之后，你问「以前聊过 X 吗」「上周我用了哪些 skill」这类问题时，Claude 会自己调用 `agentory`：

```sh
mkdir -p ~/.claude/skills
ln -s "$PWD/skills/agentory" ~/.claude/skills/agentory   # 在仓库目录下执行
```

其他 agent：在它的指令文件里加一段类似的说明：

```markdown
## Conversation history
Before re-deriving a past decision, search earlier sessions:
`agentory "<keywords>" --json -n 10` → then `agentory show <id> -C 3 --json`.
Filters: `-p <project>`, `-s 30d`, `-k prompt,reply`, `--all` for tool output.
```

## 命令

```
agentory <query> [flags]              等价于 search（最高频路径）
agentory search <query> [flags]
agentory show <msg-id|session-id> [-C N]
agentory top --by <维度>[,<维度>] [--measure count|tokens|turns|cost]
agentory usage <命令|skill> [flags]       某个命令或 skill 是怎么被使用的
agentory sql "<SELECT …>" [--json]        只读 SQL；表结构见 `agentory schema`
agentory sessions [flags]
agentory projects
agentory index [--full] [--rebuild] [--prune] [-v]
agentory stats
agentory doctor                       环境与索引健康检查
agentory watch                        基于 fsnotify 常驻更新索引
```

### 搜索参数

| 参数 | 含义 |
|---|---|
| `-p, --project <子串>` | 项目名或工作目录子串 |
| `-s, --since <时间>` | `30m`、`12h`、`7d`、`2w`、`3mo`、`today`、`yesterday`、`2026-09-01`、`2026-09-01 14:00` |
| `-u, --until <时间>` | 同上；只写日期时包含当天 |
| `-k, --kind <列表>` | 默认 `prompt,reply,think,command,summary` |
| `--role <角色>` | `user` / `assistant` / `system` |
| `--tool <名字>` | 只看某个工具的调用（如 `--tool Bash`） |
| `--branch <名字>` | git 分支 |
| `--source <列表>` | 数据源（目前只有 `claude`） |
| `-n, --limit <n>` | 最多返回条数（默认 20），按时间倒序 |
| `-C, --context <n>` | 每条命中前后各 n 条（不跨会话、不跨文件） |
| `--all` | 同时搜索 `tool_use`、`tool_result`、`meta`、`system` |
| `--include-subagent` | 包含子 agent（sidechain）消息 |
| `--json` | 机器可读输出 |
| `--no-sync` | 跳过查询前的增量同步 |
| `--explain` | 显示每个词走的是 FTS5 `MATCH` 还是 `LIKE` 回退 |
| `--full-text` | 输出完整消息；`--json` 下在 snippet 之外多一个 `text` 字段 |
| `--color <何时>` | `auto`（默认；非 TTY 或设置了 `NO_COLOR` 时关闭）、`always`、`never` |

查询语法：空格分隔的多个词是 AND 关系；每个词做大小写不敏感的子串匹配；
用双引号包短语（`"lock ordering"`）。≥3 个字符的词走 trigram 索引，更短的词回退到 `LIKE`。

### 多维探索：`top`、`usage`、`sql`

`agentory top --by <维度>[,<维度>]` 按一个或两个维度聚合，查询词和过滤参数与 `search` 相同（以下输出来自 `testdata/` 里的合成数据）：

```console
$ agentory top --by name,actor
     1  Explore / agent  last 2026-08-20 01:01  with args 1/1
     1  model / user     last 2026-08-20 01:00  with args 1/1
2 messages in 2 groups by name,actor

$ agentory top --by model --measure tokens
               requests    output     input  cache rd  cache wr     hit
claude-opus-5        2       300         8     24.0K      1600   93.7%
(all)                2       300         8     24.0K      1600   93.7%
2 requests in 1 group by model

$ agentory top --by session --measure cost
                                             usd  sessions  lines
5f0c1d2e-0000-4000-8000-000000000001       $0.01         1  +0/-0  Fix invoice discount
(all)                                      $0.01         1  +0/-0
1 session in 1 group by session
```

| `--by` | 分组依据 |
|---|---|
| `skill`、`command`、`subagent_type` | agent 调用的 skill、你手敲的斜杠命令、启动的子 agent |
| `name`、`actor` | 以上任意一种；`user`（你）或 `agent` |
| `tool`、`file`、`error`、`input:<key>` | 工具调用、涉及的文件、结果（`ok`/`error`/`no result`）、某个入参 |
| `model`、`agent` | 模型；主 agent 与子 agent |
| `project`、`branch`、`session`、`kind`、`role`、`source` | 同名字段 |
| `day`、`week`、`month`、`hour`、`weekday` | 本地时间 |

`--measure` 决定统计什么：`count`（消息数，默认）、`tokens`（按请求去重的 API 用量：输入、输出、
缓存读写与命中率）、`turns`（agent 回合数与耗时）、`cost`（各会话自报的成本，按 API 标价折算，
不是订阅的实际扣费）。

`agentory usage <名字>` 展示某个斜杠命令、skill 或子 agent 类型的使用方式——包括你敲的和 agent
调的：次数与项目分布、名字后面跟的参数（按出现次数分组）、失败、被打断，以及你接下来说了什么：

```console
$ agentory usage model
model — 1 use (1 typed by you, 0 by the agent), 1 with arguments, 0 errors, 0 interrupted
first 2026-08-20 01:00  last 2026-08-20 01:00
projects: shop×1

arguments (1 distinct):
      1  08-20 01:00  opus

recent:
  08-20 01:00  #3  user   shop  opus
      → next: Why does the invoice total double count the discount? 发票金额为什么重复扣了折扣？
```

其余问题交给 `agentory sql "SELECT …"`；`agentory schema` 列出全部表结构（`msgs`、`invocations`
视图、`requests`、`turns`、`sessions`）。查询只读，带行数上限和超时。

### 消息分类（kind）

| kind | 含义 | 默认参与搜索 |
|---|---|---|
| `prompt` | 你真实敲的输入 | 是 |
| `reply` | assistant 正文 | 是 |
| `think` | assistant 思考 | 是 |
| `command` | slash 命令（`/model opus`） | 是 |
| `summary` | context 压缩摘要 | 是 |
| `tool_use` | 工具调用入参，渲染成 `key=value` 多行 | `--all` |
| `tool_result` | 工具返回（默认前 2000 字符；`index --full` 为 40000） | `--all` |
| `meta` | 注入内容、中断 | `--all` |
| `system` | 系统事件 | `--all` |

## 配置

| 环境变量 | 默认值 | 作用 |
|---|---|---|
| `AGENTORY_DB` | `$XDG_DATA_HOME/agentory/index.db` 或 `~/.local/share/agentory/index.db` | 索引库位置 |
| `CLAUDE_CONFIG_DIR` | `~/.claude` | Claude Code 存放 `projects/` 的目录 |
| `NO_COLOR` | 未设置 | 关闭颜色 |

## 工作原理

- 会话记录是 append-only 的 JSONL。索引为每个文件记录「最后一个完整行」的字节偏移；
  同步时 `stat` 所有文件，只解析偏移之后的新字节。末尾写了一半的行留到下次再读。
- 续读前校验偏移前一个字节是换行符、且文件头 4 KiB 没变；文件被原地重写、变小、
  mtime 倒退或截断模式变化时，该文件单独全量重建。
- `msgs_fts` 是 contentless 模式的 FTS5（`tokenize='trigram'`），由
  `AFTER INSERT/DELETE/UPDATE` 三个触发器维护一致性。工具调用的入参和输出约占全部文本的 80%，
  默认搜索也不查它们，所以不进全文索引：全量建库因此快了约 3 倍、索引小了 40%；
  `--all` 搜索时对这部分走 `LIKE` 扫描（几百毫秒）。
- 原始记录被删除后索引里仍保留（agent 会自己清理旧会话）；需要时用
  `agentory index --prune` 清掉。
- 数据源实现一个很小的接口（`internal/model.Source`）；接入新 agent 只需新增一个包，
  再在 `internal/source/registry.go` 注册一行。所有表都已带 `source` 列。

## FAQ

**为什么不直接用 `grep` / `rg`？**
速度不是问题——`rg` 扫 1.4 GB 不到两秒。问题在于 JSONL 一行就是一条带转义的巨型 JSON：
命中后没法按会话、时间、角色、工具聚合，注入的提醒和工具输出会淹没真正的对话，
也没法「给我看这条命中前后三条」。`agentory` 只解析一次、清洗干净，
把结构化字段和全文索引放在一起。

**为什么不用 `tgrep` 这类 trigram 服务？**
它们的优势在几十万个文件的量级。常见的对话历史只有一千个左右文件，
单个 SQLite 文件更简单，也不需要守护进程。

**会把我的对话发到别处吗？**
不会。`agentory` 没有任何网络代码。索引是 home 目录下的一个 SQLite 文件
（见 `AGENTORY_DB`），所在目录以 `0700` 权限创建，也绝不会写进代码仓库。
随时可以删除，可完整重建。注意：索引包含与原始记录同样敏感的内容（包括你贴进会话里的东西），
请像对待原始记录一样对待它。

**为什么 2 个字的搜索慢一些？**
trigram 索引无法匹配少于 3 个字符的词，这些词只能用 `LIKE` 扫描。
加一个更长的词或过滤条件（`-p`、`-s`）可以缩小扫描范围；`--explain` 会显示每个词走的路径。

**索引有多大？**
在一份真实历史（1025 个 Claude Code 会话文件，共 1.4 GB JSONL）上，Apple Silicon 笔记本
全量建索引约 20 秒，索引库约 430 MiB；之后的增量同步只需几十毫秒。

**支持 Codex 吗？**
在计划中。存储结构和数据源接口都已就绪，解析器还没写。

## 参与贡献

见 [CONTRIBUTING.md](CONTRIBUTING.md)。测试数据必须是合成的——绝不要提交真实的会话记录。

## 许可证

[MIT](LICENSE) © haojixing
