# USAGE.md — 上手与排障手册

本文是**可照抄执行的操作教程**：每一步都有完整命令与配置。架构说明、能力矩阵、模型元数据、发布流程请看 [README.md](README.md)（中文版 [README.zh-CN.md](README.zh-CN.md)）。

> **项目血缘**：`coolxll/lingma-ipc-proxy`（协议验证原型）→ [`Lutiancheng1/lingma-proxy`](https://github.com/Lutiancheng1/lingma-proxy)（完整本地产品）→ 本仓库。许可与署名见 [README.md 的 License 章节](README.md#license)。

---

## 目录

1. [30 秒速览](#1-30-秒速览)
2. [系统要求](#2-系统要求)
3. [安装](#3-安装)
4. [5 分钟接通第一个客户端](#4-5-分钟接通第一个客户端)
5. [客户端配置大全](#5-客户端配置大全)
6. [端点速查表](#6-端点速查表)
7. [模型与站点](#7-模型与站点)
8. [配置参考](#8-配置参考)
9. [并发](#9-并发)
10. [工具调用](#10-工具调用)
11. [推理 / 思考透传](#11-推理--思考透传)
12. [图片输入](#12-图片输入)
13. [排障手册](#13-排障手册)
14. [本地构建](#14-本地构建)
15. [FAQ](#15-faq)
16. [上线自检清单](#16-上线自检清单)

---

## 1. 30 秒速览

Lingma Proxy 把通义灵码 / Qoder 的私有协议转换成 **OpenAI 与 Anthropic 兼容 API**，让 Claude Code、Codex CLI、CodeBuddy、Cline、Continue 等第三方客户端直接用上账号里的模型（含 Qwen、Kimi、MiniMax 等）。

```
你的客户端 ──OpenAI/Anthropic 协议──▶ lingma-proxy :8095 ──私有协议──▶ 通义灵码 / Qoder 后端
```

**先读这一条**：模型 ID 因账号/企业而异，**永远以你自己 `GET /v1/models` 的返回为准**，不要照抄本仓文档里的任何模型名。

---

## 2. 系统要求

| 项 | 要求 | 确认方式 |
| --- | --- | --- |
| 通义灵码 / Qoder | 已安装并**登录**，面板里能正常对话 | 打开 IDE 的 Lingma 面板发一句话 |
| 操作系统 | Windows / macOS / Linux | — |
| Go（仅源码构建） | 1.22.0+ | `go version` |
| 内存 | 建议 ≥ 4 GB（CLI 后端会起独立运行时） | — |

> ⚠️ **必须先在 IDE 面板验证能对话**。代理复用 IDE 已有的登录态；面板都不通的话，后面所有问题都无意义。

---

## 3. 安装

### 方式 A：桌面版（推荐，无需 Go）

1. 前往 [Releases](https://github.com/Lutiancheng1/lingma-proxy/releases) 下载对应平台的压缩包。
2. 解压后目录里有一个 `LingmaProxy.exe`（macOS 为 `.app`）和一个 `lingma-proxy.json`（端口/站点/预热配置）。
3. 双击启动。默认监听 `127.0.0.1:8095`，Web 控制台在 `8096`。
4. 启动日志里会打印带令牌的完整控制台 URL，形如 `http://127.0.0.1:8096/#token=<token>`。
5. 打开控制台点「探测模型」，确认拿到模型列表。

**三个站点变体**（压缩包内 exe 同名，靠 `lingma-proxy.json` 区分）：

| 变体 | 默认端口 | 服务站点 |
| --- | --- | --- |
| `cn` | 8095 | Qoder CN |
| `intl` | 9095 | Qoder 国际版 |
| `both` | 10095 | 两个站点，模型名带 `cn/` / `intl/` 前缀 |

### 方式 B：源码构建 CLI

```bash
git clone https://github.com/Lutiancheng1/lingma-proxy.git
cd lingma-proxy
go build -o ./dist/lingma-proxy ./cmd/lingma-ipc-proxy
./dist/lingma-proxy --host 127.0.0.1 --port 8095 --session-mode auto
```

Windows：

```powershell
.\scripts\build.ps1
.\dist\lingma-proxy.exe --host 127.0.0.1 --port 8095 --session-mode auto
```

### 方式 C：Docker（Linux）

```bash
docker run --rm -p 8095:8095 ghcr.io/lutiancheng1/lingma-proxy:<tag>
```

> 镜像由上游账号发布在 GHCR（`ghcr.io/lutiancheng1/lingma-proxy`）。Docker 镜像**不内置**桌面端、Node、浏览器或本机登录缓存，因此容器内需要自行挂载登录态或改用 remote 模式。

---

## 4. 5 分钟接通第一个客户端

### 4.1 确认代理活着

```bash
curl -s http://127.0.0.1:8095/health          # 期望 200
curl -s http://127.0.0.1:8095/v1/models       # 期望有模型列表
```

`/v1/models` **冷启动会真实探测一次后端**，CLI 后端实测可到 20 秒以上；热起来是毫秒级。**冷启动慢不是故障**，不要去调超时。

### 4.2 打通一次最小对话

```bash
curl -s http://127.0.0.1:8095/v1/chat/completions \
  -H 'content-type: application/json' \
  -H 'authorization: Bearer any' \
  -d '{"model":"<你 /v1/models 里的 ID>","max_tokens":32,
       "messages":[{"role":"user","content":"只回复两个字：正常"}]}'
```

返回 200 且 `choices[0].message.content` 有内容即打通。

### 4.3 填进客户端

见下一章。**两个最容易错的点**：

- Claude Code 的 `ANTHROPIC_BASE_URL` **不带** `/v1`；
- Codex CLI 的 `base_url` **要带** `/v1`。

---

## 5. 客户端配置大全

> API key 一律填任意非空字符串（如 `any`）——本地代理不校验它。填 `any` 即可。

### 5.1 Claude Code ✅

```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:8095"
export ANTHROPIC_API_KEY="any"
```

进入后切模型：

```text
/model kmodel
```

**`ANTHROPIC_BASE_URL` 不要带 `/v1`** —— Claude Code 会自己拼 Anthropic 路径。

已实测：纯文本对话、工具调用、粘贴图片、同会话内图片+工具。

### 5.2 Codex CLI ✅

Codex 走 **OpenAI Responses 协议**，`wire_api` 必须是 `responses`：

```toml
# ~/.codex/config.toml
model = "kmodel"
model_provider = "lingma_proxy"
approval_policy = "never"
sandbox_mode = "danger-full-access"

[model_providers.lingma_proxy]
name = "Lingma Proxy"
base_url = "http://127.0.0.1:8095/v1"
env_key = "OPENAI_API_KEY"
wire_api = "responses"
```

```bash
export OPENAI_API_KEY="any"
codex exec --skip-git-repo-check --dangerously-bypass-approvals-and-sandbox --json '只回复 OK'
```

**`base_url` 要带 `/v1`**，与 Claude Code 相反。

> 若要让 Codex 对 `kmodel` 这类自定义 ID 也发 `reasoning`，需另配 model catalog，格式见 [README 的 Codex CLI 章节](README.md#codex-cli)。

### 5.3 CodeBuddy ✅

OpenAI 兼容自定义模型，导入形状：

```json
{
  "name": "Lingma Proxy",
  "provider": "openai-compatible",
  "baseURL": "http://127.0.0.1:8095/v1",
  "apiKey": "any",
  "model": "kmodel"
}
```

已实测：标准对话、token 用量统计。

### 5.4 Hermes Agent ✅

```bash
# ~/.hermes/.env
OPENAI_API_KEY=any
```

配合 `api_mode` 选择 `anthropic_messages` 可获得 `thinking_delta` 推理流。

### 5.5 MiniMax Code

在 MiniMax Code 的 provider 配置里加一个自定义 provider，协议选 **Anthropic Messages**，Base URL 填代理地址，API key 填 `any`，模型填 `/v1/models` 返回的 ID。裸模型名（不带 `cn/` 前缀）会走默认站点。

### 5.6 ZCode

ZCode 的 provider 真实注册表在 `~/.zcode/v2/provider_config.json`（可用 `ZCODE_PERSONAL_PROVIDER_CONFIG_FILE` 指定）。加一个指向本代理的 Anthropic provider 即可。**注意：ZCode 的 headless `--prompt` 会被运行中的宿主接管，只有 GUI 才会真正走本地代理。**

### 5.7 任意 OpenAI 兼容客户端

Base URL = `http://127.0.0.1:8095/v1`，API key = `any`。
适用：Cline、Continue、OpenCode、LobeChat、Chatbox 等一切支持 `openai-compatible` 的客户端。

### 5.8 任意 Anthropic 兼容客户端

Base URL = `http://127.0.0.1:8095`，API key = `any`（或 `x-api-key` 头）。

---

## 6. 端点速查表

| 用途 | 端点 |
| --- | --- |
| 存活 | `GET /health`、`GET /` |
| 模型列表 | `GET /v1/models` |
| 能力发现 | `GET /capabilities`、`GET /v1/capabilities` |
| OpenAI Chat Completions | `POST /v1/chat/completions` |
| OpenAI Responses（Codex 必需） | `POST /v1/responses` |
| Anthropic Messages | `POST /v1/messages` |
| Anthropic token 计数 | `POST /v1/messages/count_tokens` |
| 请求记录 | `GET /debug/requests` |
| 访问日志 | `GET /debug/access-logs` |
| 应用日志 | `GET /debug/app-logs`（仅桌面版提供） |

**兼容别名**：`/anthropic/v1/messages`、`/api/v1/chat/completions`、`/api/v1/responses`、`/api/requests`、`/api/access-logs`、`/api/logs`、`/api/app-logs`、`/api/v1/models`、`/api/tags`、`/props`、`/version`、`/debug/logs`。

三个对话端点均支持 `stream: true` 与 `false`。

---

## 7. 模型与站点

### 7.1 模型 ID 是你自己的

`GET /v1/models` 返回的 ID 取决于你的账号与所在企业租户，**因人而异**。个人版、商业版、校园版、不同企业租户会暴露不同的模型、别名、配额与后端域名。

**永远信自己的 `/v1/models`，不要信任何截图或文档里的示例 ID。**

### 7.2 双站点命名空间

`both` 变体同时服务两个站点时，模型名带前缀：

| 前缀 | 站点 | 后端 |
| --- | --- | --- |
| `cn/` | Qoder CN | `openapi.qoder.com.cn` |
| `intl/` | Qoder 国际版 | `openapi.qoder.sh` |
| 无前缀 | 默认站点（CN） | 同 `cn/` |

例：`cn/Qwen3.8-Flash`、`intl/Auto`。

### 7.3 `Auto` 的含义

`Auto` 是后端自己的自动选模档。不带前缀时它跟随默认站点。

---

## 8. 配置参考

### 8.1 优先级

**命令行参数 > 环境变量 > 配置文件 > 默认值**

### 8.2 命令行参数

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `--host` | `127.0.0.1` | 监听地址。**不要改成 `0.0.0.0`**：本代理无鉴权，改了就等于把推理口暴露给整个局域网 |
| `--port` | `8095` | 监听端口 |
| `--transport` | `auto` | `auto` / `pipe` / `websocket` |
| `--backend` | `remote` | `remote`（默认，推荐）/ `ipc` / `qodercli` |
| `--session-mode` | `auto` | `auto` 每次用独立会话（适配编辑器并发）；`sticky` 复用单会话 |
| `--model` | 空 | 指定模型 |
| `--cwd` | 空 | 告诉后端当前工作目录，影响相对路径工具 |
| `--current-file-path` | 空 | 当前打开文件，影响上下文 |
| `--timeout` | `0` | `0` = 不设代理侧截止时间 |
| `--remote-fallback` | 关 | remote 失败时回退到 IPC |
| `--remote-fallback-models` | 内置 | 回退时使用的模型列表 |
| `--qodercli-sites` | 全部 | `cn` / `global` |
| `--session-mode` | `auto` | 见上 |
| `--config` | 自动 | 配置文件路径 |
| `--export-remote-auth` | 空 | 导出登录凭据文件 |
| `--export-server-bundle` | 空 | 导出 docker-compose 包 |
| `--remote-auth-pick` | `auto` | 选择登录缓存（`auto` = 剩余有效期最长） |
| `--qodercli-turn-ceiling` | 见帮助 | 单回合上限 |

完整列表：`lingma-proxy --help`

### 8.3 环境变量

**服务面**

| 变量 | 作用 |
| --- | --- |
| `LINGMA_PROXY_HOST` / `LINGMA_PROXY_PORT` | 监听地址与端口 |
| `LINGMA_PROXY_BACKEND` | `remote` / `ipc` / `qodercli` |
| `LINGMA_PROXY_TRANSPORT` | 传输方式 |
| `LINGMA_PROXY_SESSION_MODE` | `auto` / `sticky` |
| `LINGMA_PROXY_MODEL` / `LINGMA_PROXY_CWD` / `LINGMA_PROXY_CURRENT_FILE_PATH` | 模型与工作目录 |
| `LINGMA_PROXY_MODE` / `LINGMA_PROXY_SHELL_TYPE` | 模式与 shell 类型 |
| `LINGMA_PROXY_MAX_CONCURRENT` | 并发上限，默认 `4`，范围 `1`–`16` |
| `LINGMA_PROXY_CONFIG` | 配置文件路径 |
| `LINGMA_CONSOLE_HOST` | Web 控制台绑定地址，默认 `127.0.0.1` |
| `LINGMA_DESKTOP_DEBUG` | 桌面版调试开关 |

**后端**

| 变量 | 作用 |
| --- | --- |
| `LINGMA_REMOTE_BASE_URL` | remote 后端地址（留空则自动在 IDE 配置文件里找） |
| `LINGMA_REMOTE_PROXY_URL` | remote 请求的上游 HTTP 代理 |
| `LINGMA_REMOTE_VERSION` | 伪装版本号 |
| `LINGMA_REMOTE_AUTH_FILE` | 登录凭据文件 |
| `LINGMA_REMOTE_FALLBACK_MODELS` | 回退模型列表 |
| `LINGMA_QODERCLI_BIN` / `_HOST` / `_RUNTIME` | 覆盖 CLI 探测结果 |
| `LINGMA_QODERCLI_SITES` | 站点选择（`cn` / `global`） |
| `LINGMA_QODERCLI_PROFILE` | CLI profile |
| `LINGMA_QODER_CLIENT_ID` / `LINGMA_QODER_OPENAPI_BASE_URL` | 覆盖 OAuth 客户端与网关（**按站点分别设置**，见下） |
| `LINGMA_IPC_PIPE` / `LINGMA_IPC_SOCKET` | IPC 传输端点 |
| `LINGMA_SHARED_CLIENT_INFO` | 共享客户端信息 |
| `LINGMA_CACHE_DIR` | 缓存目录 |
| `LINGMA_VERBOSE_CREDENTIAL_ERRORS` | 打印完整凭据候选路径 |

> ⚠️ `LINGMA_QODER_CLIENT_ID` 与 `LINGMA_QODER_OPENAPI_BASE_URL` 目前对**两个站点同时生效**。只 pin 一个站点的 token 会让另一个站点的请求也报"可用"，然后在网关炸。跨站点部署请分开设置。

### 8.4 配置文件

`lingma-proxy.json`（与 exe 同目录）或 `~/.config/lingma-ipc-proxy/`，字段与命令行参数同名。

---

## 9. 并发

```bash
LINGMA_PROXY_MAX_CONCURRENT=8 lingma-proxy --port 8095
```

- 默认 **4** 路，范围 `1`–`16`
- **非法或越界值静默回落为 4**（不报错），所以写错不会提示你，只会看起来"没生效"
- **多路并发务必配 `session_mode=auto`**，否则并行请求挤同一条 sticky 会话互相污染
- 超出上限的请求排队等槽位，客户端看到的是延迟而不是立即 429

---

## 10. 工具调用

Lingma 后端没有公开的原生工具调用协议，本代理是**模拟**出来的，五步链路：

1. 把 OpenAI / Anthropic 的工具定义归一化
2. 把工具契约注入 Lingma 的 prompt
3. 从模型回复里解析 action block
4. 把解析出的动作转回 `tool_calls` / `tool_use`
5. 把 `tool_result` 回灌给 Lingma 继续

**因此模型是否配合直接决定工具调用是否成功**。实测 `Qwen3-Coder` 最稳，部分模型会拒绝或改用自然语言描述。

已做的加固：

- 按客户端真实工具名生成路由表
- 为 `read_file` / `search_files` / `terminal` / `web_search` 各给专门示例
- 模型声称无法访问文件/终端/网络时自动重试
- 常见别名归一：`Bash`→`terminal`、`Read`→`read_file`、`Grep`→`search_files`、`Edit`→`patch`
- 解析器同时认得**围栏 JSON** 与 **XML**（`tool_call` / `function=` / `invoke name=` / `parameter=`）两套方言

**排障**：`tool_calls` 一直是空数组时，先确认请求体里 `tools` 非空。`tools` 为空时代理**不会**凭空补工具——这是有意的，否则会把不该有的工具调用造出来。

---

## 11. 推理 / 思考透传

- **remote 模式**：接受 `thinking` / `reasoning` 入参，但上游 remote SSE 只流普通 `delta.content`，**不会**产出独立 reasoning 块。文本里可能夹带"推理过程："字样，但那不是结构化字段。
- **IPC 模式**：转发 IDE 原生的独立思考流。客户端请求 reasoning 且上游确实发出 thought chunk 时，可映射为 Responses 的 `reasoning` item 或 Anthropic 的 `thinking` 块。

详见 [README 的推理兼容性矩阵](README.md#reasoning--thinking-support)。协议层成立 ≠ 客户端一定显示推理面板（Codex 还会看系统上下文、请求形状与上游路由）。

---

## 12. 图片输入

OpenAI 的 `image_url` 与 Anthropic 的 image block 都支持，会自动归一化。来源可以是 data URL、HTTP URL、`file://` URL 或绝对本地路径。

**本地路径只接受这些扩展名**，其余一律拒绝：

```
.png   .jpg   .jpeg   .gif   .webp   .bmp
```

同时要求是**常规文件**（拒绝目录与设备节点，因此 `/dev/zero` 不能把无限读打进内存）且不超过 **20 MiB**。

这不是过度设计：没有这层闸，任何客户端——或者被 prompt 注入的 agent——发一个 `image_url: "/etc/passwd"` 就能把本地文件 base64 之后送进模型上下文。

超大图片自动按 JPEG 缩放。请求日志里大段 base64 会折叠成图片标记，不撑爆日志视图。

---

## 13. 排障手册

| 症状 | 先查 | 处理 |
| --- | --- | --- |
| 客户端连不上 | `curl http://127.0.0.1:8095/health` | 不通就是代理没起来或端口被占 |
| **端口被别的进程占走** | `Get-NetTCPConnection -LocalPort 8095 -State Listen`（Windows）/`lsof -i :8095`（macOS/Linux） | 桌面版与无头 CLI **同名不同进程**，手工跑过 `lingma-proxy-cli.exe` 会抢走端口；`kill` 掉或停掉对应的计划任务 |
| Claude Code 404 | `ANTHROPIC_BASE_URL` 是否多写了 `/v1` | 去掉 `/v1` |
| Codex 报错 | `wire_api` 是不是 `responses`、`base_url` 是否带 `/v1` | 两处都要对 |
| `/v1/models` 很慢 | 是否刚启动 | 冷启动 20 秒量级属正常；**如果站点不可达时每次都恰好 ~8 秒**，那是探测超时，不是冷启动 |
| `tool_calls` 恒空 | 请求体 `tools` 是否非空；换 `Qwen3-Coder` | 空 `tools` 不会被补工具，这是设计 |
| 报"上游连接错误" | 代理是否刚重启；IDE 面板此刻能对话吗 | 先恢复 IDE 侧对话 |
| 401 / 403 | 是否在访问控制台或 `/debug/*` | 控制台无 token 必须 401，这是对的；`/debug/*` 跨源 403 |
| 局域网机器连不上 | `--host` 是否是 `127.0.0.1` | 监听回环只收本机。跨机需显式改，但**请先自行加反向代理与鉴权** |
| 控制台打不开 | 日志里的 `#token=` | 令牌也在设置页与 app-state 的 `admin_token` |

**看真实请求**：

```bash
curl -s http://127.0.0.1:8095/debug/requests     # 最近请求记录（base64 自动折叠成图片标记）
curl -s http://127.0.0.1:8095/debug/access-logs  # 访问日志
curl -s http://127.0.0.1:8095/debug/app-logs     # 应用日志（仅桌面版）
```

---

## 14. 本地构建

```bash
gofmt -w .
go build -o ./dist/lingma-proxy ./cmd/lingma-ipc-proxy
```

提交前跑门禁：

```bash
go vet ./...
go test ./...
```

`gofmt -l` 在 Windows 上会整份文件报红——这是检出为 CRLF 而 gofmt 要 LF，与代码无关。仓库根的 `.gitattributes` 用 `*.go text eol=lf` 按文件固定了这事。

**桌面版**必须走脚本，不要手工"退出/复制/打开"零散步骤：

```bash
./scripts/rebuild-local-app.sh                     # 打包 → 制停旧进程 → 覆盖 /Applications → 重新打开
ENABLE_DEVTOOLS=0 ./scripts/rebuild-local-app.sh   # 关掉右键 Inspect Element
```

Windows：

```powershell
# 先按 production tag 出包（漏掉这个 tag 会链到 dev 模式外壳，进程活着但不绑端口、日志全无）
$env:CGO_ENABLED = '0'
go build -tags production -trimpath -ldflags '-s -w -H windowsgui -X main.devtoolsBuild=true' `
  -o desktop/build/bin/LingmaProxy.exe ./desktop
# 再走部署脚本（内部按哈希回读校验，并重打包三个站点 zip）
.\scripts\repackage-desktop.ps1 -Exe desktop\build\bin\LingmaProxy.exe -TargetDir "G:\qoder fan\both"
```

> **手工出包必须带 `-tags production`**，否则产物指纹与发布件对不上。

---

## 15. FAQ

**Q：一定要装 IDE 吗？**
CLI 模式需要本机存在 Qoder/Lingma 的登录缓存。桌面版会做一次预热把模型目录落盘，重启后 `/v1/models` 毫秒级返回。

**Q：为什么 `/v1/models` 每次都慢？**
如果是**每次都恰好 8 秒**，那是某个站点探测超时了（remote 模式），不是冷启动。检查该站点是否可达。若站点暂时不可达，代理会用落盘的旧目录兜底；本版本对失败探测加了 30 秒负缓存，一个窗口内只付一次探测税。

**Q：能跨机器用吗？**
能，但 `--host` 必须显式改掉回环地址，且**你必须自己加鉴权**——本代理默认不校验任何凭据，裸暴露等于把账号里的模型开放给整个网络。控制台另有独立令牌，默认绑回环。

**Q：为什么我的客户端连不上但 curl 能连？**
99% 是 base URL 规则：Claude Code 不带 `/v1`，Codex CLI 要带 `/v1`，通用 OpenAI 客户端带 `/v1`，Anthropic 兼容客户端不带。

**Q：支持并发吗？**
支持，默认 4 路（`LINGMA_PROXY_MAX_CONCURRENT`，范围 1–16）。多路务必配 `session_mode=auto`。

**Q：工具调用一直失败？**
先确认 `tools` 字段非空；再换 `Qwen3-Coder` 这类工具调用纪律好的模型。解析侧两套方言都已覆盖，若仍失败多半是模型侧不配合，不是代理缺陷。

**Q：数据会发到哪？**
请求发给 Lingma/Qoder 自己的后端。代理本身不落盘请求内容（桌面版的状态文件只存最近 300 条记录用于本地排障，且截断到 8KB/段）。

---

## 16. 上线自检清单

- [ ] Lingma / Qoder 面板本身能正常对话
- [ ] `GET /health` 返回 200
- [ ] `GET /v1/models` 返回**你自己的**模型列表
- [ ] 纯文本回合 200
- [ ] 带 `tools` 的回合返回 `stop_reason=tool_use` 或非空 `tool_calls`
- [ ] 流式回合 index 连续、`done` / `completed` 出现且只出现一次
- [ ] 控制台无 token 返回 401
- [ ] `/debug/*` 跨源返回 403、同源 200
- [ ] 图片 + 工具同回合可用
- [ ] 多路并发不出现 429 风暴

---

## 许可与署名

本仓库的 `LICENSE` 覆盖本仓原创贡献（MIT）。上游 [`Lutiancheng1/lingma-proxy`](https://github.com/Lutiancheng1/lingma-proxy) 与更早的 [`coolxll/lingma-ipc-proxy`](https://github.com/coolxll/lingma-ipc-proxy) 未提供明确的根开源许可，因此本仓**不主张重许可第三方材料**。详见 [README 的 License 与 Acknowledgements 章节](README.md#license)。
