# agentory

[English](README.md) | 简体中文

[![CI](https://github.com/haojixing/agentory/actions/workflows/ci.yml/badge.svg)](https://github.com/haojixing/agentory/actions/workflows/ci.yml)
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
go install github.com/haojixing/agentory@latest
```

或者从 [Releases 页面](https://github.com/haojixing/agentory/releases) 下载
Linux / macOS / Windows（amd64/arm64）预编译二进制，放进 `PATH` 后运行
`agentory doctor` 自检。

从源码构建：

```sh
git clone https://github.com/haojixing/agentory && cd agentory
make install        # 或：make build && ./agentory doctor
```

## 快速开始

```sh
agentory index                      # 首次建索引（约 1.4 GB 记录大约一分钟）
agentory "lock ordering"            # 搜索，等价于 agentory search ...
agentory show 4213 -C 3             # 查看命中消息及前后各 3 条
agentory sessions -p shop -s 7d     # 某项目最近 7 天的会话
```

即使不先跑 `agentory index`，第一次查询也会自动建索引。

### 让 agent 自己用

在 `CLAUDE.md`（或你的 agent 的同类指令文件）里加一段：

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
| `--color <何时>` | `auto`（默认；非 TTY 或设置了 `NO_COLOR` 时关闭）、`always`、`never` |

查询语法：空格分隔的多个词是 AND 关系；每个词做大小写不敏感的子串匹配；
用双引号包短语（`"lock ordering"`）。≥3 个字符的词走 trigram 索引，更短的词回退到 `LIKE`。

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
- `msgs_fts` 是外部内容表模式的 FTS5（`tokenize='trigram'`），由
  `AFTER INSERT/DELETE/UPDATE` 三个触发器维护一致性。
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
全量建索引耗时 52 秒，索引库 634 MiB；之后的增量同步只需几十毫秒。

**支持 Codex 吗？**
在计划中。存储结构和数据源接口都已就绪，解析器还没写。

## 参与贡献

见 [CONTRIBUTING.md](CONTRIBUTING.md)。测试数据必须是合成的——绝不要提交真实的会话记录。

## 许可证

[MIT](LICENSE) © haojixing
