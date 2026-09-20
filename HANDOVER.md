# 交接说明：qodercli 后端（v1.6.12 + 8 个提交）


面向接手这个仓库的人。所有结论都是 2026-09-19 在本机实测得到的，不是读代码猜的。

> 本文写的是 **Qoder CN 单站点**时的后端。之后 `4a8e14d` 起后端同时服务 CN 与国际版两个站点
> （站点路由、`intl/` 前缀、国际版必需的 `--config-dir`、第二份桌面实例的配置），
> 那部分增量见 [`docs/qoder-international-handoff.md`](docs/qoder-international-handoff.md)。
> 其中的逐模型档位表 `scripts/tier_matrix.py` 测的是 CN 目录，国际版档位走透传。

---

## 1. 为什么要改：旧路子已经彻底不通

新版 Qoder CN 把推理请求的签名收进了官方客户端内部：

- `gateway.qoder.com.cn` 只接受 CLI 里 WASM `prepareRequest` 产出的签名（`?Encode=1` 混淆 + 硬件绑定密钥），
  Go 侧直接重放一律 `503 / 403 Signature invalid`。
- 旧的 Remote API 登录缓存（`~/.lingma` 那套 cosy 凭据）在新版里已经不写了，
  所以 `backend: remote` 拿不到凭据，`/v1/models` 直接 500。

**结论：任何"在 Go 里重放网关请求"的尝试都是死路。** 现在的做法是代理 spawn 桌面版自带的
CLI / worker runtime 当上游，签名交给客户端自己完成。

## 2. 当前代码状态

| 项 | 值 |
| --- | --- |
| HEAD | `4a8e14d`（`git describe` = `v1.6.12-8-g4a8e14d`） |
| 远端 | `origin/main` 已同步，无未推送提交 |
| **注意** | **这 8 个提交没有打 tag**，所以 GitHub Releases 上那个 v1.6.12 安装包**不含本次修复** |

```
4a8e14d feat: serve the international Qoder site from the CLI backend
cf4fbdb fix: map an Anthropic disabled thinking block to no reasoning
a78c42e fix: send client system text to the CLI's system slot
655d00b fix: let a Qoder CN CLI result-frame error outrank stderr noise
ddb8b5e feat: infer the top Qoder CN thinking tier from a large budget
3759437 feat: honour a client-chosen Qoder CN reasoning tier
1bf25b2 feat: let clients pin a Qoder CN CLI reasoning tier
4b3e229 feat: add qodercli backend so the proxy survives the Qoder CN signing change
```

已验证可独立构建：在 `HEAD` 建一个干净 worktree，`go build ./cmd/lingma-ipc-proxy` 通过，
`go test ./internal/service ./internal/qodercli ./internal/toolemulation ./internal/httpapi` 全绿。
（`internal/remote` 的 `TestSaveCredentialFileRoundTrip` 在 Windows 上必失败——断言文件模式 0600，
NTFS 报 666。这是既有问题，与本次改动无关。）

## 3. 运行前提（"要不要重新授权"）

**不需要 API key，不需要网页登录，桌面版甚至可以不开着。**

代理只做两件事：

1. 读本机登录态文件：`%APPDATA%\com.qodercn.app.stable\Local State`（DPAPI 解出 AES key）
   → 用该 key 解 `auth.v1.dat` → 得到 device token `dt-…` + refreshToken。
2. 用 device token 调 `POST https://openapi.qoder.com.cn/api/v1/me/jobToken`
   （body 必须带 `{"clientId":"732aef47-9cf2-46a2-95fe-4cebb5d0d1fa"}`，CLI 自己那个 id 会 400）
   → 换到 24 小时有效的 job token，以环境变量 `QODERCN_JOB_TOKEN` 喂给子进程。

所以换机器/换环境时要满足：

- 装了 Qoder CN 桌面版**并且登录成功过**（国际版也行，`com.qoder.app.stable` 在候选目录里）。
- 必须是**同一个 Windows 账户**——DPAPI 按用户绑定，别的账户解不开。
- 程序能被自动发现（`internal/qodercli/detect.go` 会扫 `C:\<AppName>`、
  `%LOCALAPPDATA%\Programs\<AppName>`、`/Applications/…` 等）。找不到时用
  `LINGMA_QODERCLI_PROFILE` 指定 userData 目录。
- 在桌面版里退出登录、或 refreshToken 过期 → 代理报「Qoder CN 登录态不可用」，
  在桌面版里重新登录即可（桌面版自己的登录流程可能弹一次浏览器，那是它的一次性 OAuth，与代理无关）。

## 4. 构建与运行

```bash
# CLI
go build -o lingma-ipc-proxy ./cmd/lingma-ipc-proxy
./lingma-ipc-proxy                      # 默认监听 127.0.0.1:8095

# 常用 flag
-backend qodercli          # 强制走 CLI 后端（默认会在旧凭据缺失时自动回落）
-qodercli-sites cn,global  # 让 CLI 后端同时服务 CN / 国际站
-port 8095  -timeout 300
```

### Windows 桌面版（`desktop/`）手工构建

```bash
cd desktop/frontend && npm run build
cd desktop        && go build -tags production -o LingmaProxy.exe .
```

三个必踩的坑，先记下来：

1. `wails build` v2.12.0 在 Go 1.27 下 panic（`package "archive/zip" without types`，
   `GODEBUG=gotypesalias=0` 也救不回来）→ 只能按上面两步手工来。
2. 漏 `-tags production` 会得到一个启动即弹「Wails applications will not build without the
   correct build tags」的空壳。
3. **`go build -o` 传绝对 MSYS 路径（`/c/Users/...`）会"退出 0 但没写文件"**，留下上一次的旧产物
   骗过你。必须 `cd desktop` 后用相对文件名，装之前核对 mtime 和大小（生产构建约 20.7 MB）。
   同一个坑的另一个变体：`git worktree add /c/...` 会落到 `<当前盘>:\c\...`。

### 出安装包

改 `VERSION` 和 `internal/version/version.go`（两者必须一致，`scripts/check-version-sync.sh` 会卡），
然后打 tag 推上去——`.github/workflows/release.yml` 由 `push tags v*` 触发。

## 5. 推理档位（这块最容易误会）

**CLI 词表是 6 档，不是 4 档**（反混淆 `qoder-worker-runtime.obf.mjs` 得到）：

```
REASONING_EFFORT_LEVELS = none | low | medium | high | xhigh | max
thinking 预算            =    0 | 1024 |   8192 | 24576 | 49152 | 65536
别名 off / disabled → none
```

Qoder CN 桌面 UI 的标签：`none=关闭思考  minimal=最小  low=低  medium=中  high=高  xhigh=极高  max=最大`。

**但网关按模型下发 `efforts` 支持表**，不在表里的档被 CLI 静默丢弃（不报错），
丢弃后回落到网关给的 `default_effort`。实测（2026-09-19）：

| 模型 | 真正生效的档位 |
| --- | --- |
| Qwen3.8-Flash、Qwen3.8-Max | `none / low / medium / xhigh`（**没有 high、没有 max**，就是"只有 4 档"） |
| GLM-5.3、Kimi-K3、Kimi-K2.8-Preview、DeepSeek-Flash | `none / low / high / max` |
| GLM-5.2、GLM-5.3-Flash、DeepSeek-V4-Pro | `none / high / max` |
| Auto、Qwen3.7-Max、Qwen3.7-Plus、Qwen3.7-Flash、MiniMax-M2.7 | 具名档基本不生效（只能开关） |

两条必须记住的语义：

- **不传档位 ≠ 关闭思考**。不给档时会用网关默认档，实测 Qwen3.8-Flash 默认档的思考量比 `xhigh` 还大。
  要关思考必须显式传 `none`。
- 代理侧已用 `internal/qodercli` 的 `reasoningLadders` + `clampReasoningEffort` 兜住：
  请求的档不在该模型梯子里时**向下取最近档**（`high → medium`、`max → xhigh`），
  实测在已安装构建上生效。

`reasoningLadders` 是**上游支持表的实测快照**，上游改了就会失真。重测：

```bash
python scripts/tier_matrix.py Qwen3.8-Flash        # 单模型 6 档 = 6 次真实请求
```

⚠️ 别一上来就 14 模型 × 6 档全扫（84 次真实对话），会撞账号的短时限流：
CLI 返回错误码 110「daily usage limit」，代理侧表现成 HTTP 500，看着像代码坏了。

## 6. 四家客户端的接入位置

**这些配置在各自机器的 home 目录，不在仓库里**，换环境要重配。本机已配好并实测：

| 客户端 | 配置文件 | 关键字段 | Qwen3.8-Flash 的档位列表 | 改完要不要重启 |
| --- | --- | --- | --- | --- |
| ZCode | `~/.zcode/v2/provider_config.json` | `modelConfigRules.providerModelRules[].config.optionSpecs.reasoningLevel.values`；`baseUrl` **不带** `/v1` | `disabled, low, medium, xhigh` | 要（GUI 读环境变量指向的这份文件） |
| MiniMax Code | `~/.minimax/config.yaml` | `custom_provider.lingma-proxy`，`npm: '@ai-sdk/anthropic'`，baseURL **带** `/v1`；模型条目 `reasoning: true` + `thinking.effortOptions` | `off, low, medium, xhigh` | **要整体重启**（WM_CLOSE 只关窗口，进程留托盘、缓存不刷新，得 `taskkill /F /T`） |
| Codex（经 opencodex） | `~/.opencodex/config.json` | `providers.lingma-proxy.modelReasoningEfforts`（逐模型）+ `noReasoningModels` | `low, medium, xhigh` | 要 `ocx restart`（运行中代理用启动时缓存的 config） |
| DSH Desktop | `~/.dsh/settings.yaml` | `llm-pi-ai.providers.lingma-proxy`，`api: anthropic-messages`，baseURL **不带** `/v1`，`apiKeyEnv` 指向 `~/.dsh/.credentials.yaml` 的 ref，`compat.forceAdaptiveThinking: true` | `off→none, low, medium, xhigh` | 不用（按请求热加载） |

两个反复踩到的写法差异：

- **baseURL 带不带 `/v1` 取决于客户端用哪个 SDK**：MiniMax Code 用 `@ai-sdk/anthropic` 要带；
  ZCode / DSH 直接把 baseURL 交给 Anthropic SDK（SDK 自己拼 `/v1/messages`），带了会变成 `/v1/v1/messages`。
- **DSH 的 `reasoningEfforts.off` 不能写 `null`**：`null` 的语义是"支持这个选项，但什么都不发"，
  于是落到网关默认档（比 xhigh 更费思考）。必须写成 `off: none`，pi-ai 才会发
  `thinking:{type:'disabled'}`，代理再映射成 `--reasoning-effort none`。

## 7. 已知限制

- **token 用量恒为 0**。Qoder CN CLI 返回的 `usage` 全零（实测上游如此，不是代理丢的），
  所以走这个代理的客户端拿不到真实上下文占用，只能自己估算。
- **图片输入不支持**（qodercli 后端会直接报错）。
- **每个请求 spawn 一次 CLI 子进程**，首字延迟实测 10~30 秒，别按常驻服务的性能预期它。
- **必须始终传 `--tools ""`**，否则 CLI 会启用它自己的原生工具链，在 `--max-turns 1` 下失败。
- CLI 的 stream-json 输出**没有** `stream_event` 增量，SSE 只能由最终文本分块合成。
- 桌面应用冷启动后第一个请求偶发 `openApiJsonRequest` 失败（jobToken 交换竞态），重试即好。
  这类失败现在被识别为 `remote.ErrTransientUpstream` 并以 **HTTP 503 / `overloaded_error`** 返回
  （以前是裸 500，ZCode 会显示成 `reason=unknown retryable=false`，把两秒钟的网络抖动报成死路）。
- CLI 退出码非 0 但已经产出完整回答是常态（Windows 上的 teardown 竞态），
  代码里"有可用结果或真实 result-frame 错误就优先于 stderr 噪音"这条逻辑不能删。

## 8. 怎么确认它是好的

```bash
curl -s http://127.0.0.1:8095/health
curl -s http://127.0.0.1:8095/v1/models | head -c 200     # 应返回账号下的模型列表（本机 14 个）

# 单发一次对话
curl -s -X POST http://127.0.0.1:8095/v1/messages \
  -H 'content-type: application/json' -H 'anthropic-version: 2023-06-01' \
  -d '{"model":"Qwen3.8-Flash","max_tokens":64,"messages":[{"role":"user","content":"只回复：收到"}]}'
```

要看代理**实际**给 CLI 传了什么参数（这是排查档位问题的正解）：

```powershell
# 后台先发一发请求，然后采样
Get-CimInstance Win32_Process -Filter "Name='Qoder CN.exe'" |
  ForEach-Object { $_.CommandLine } | Select-String '--model|--reasoning-effort|--append-system-prompt'
```

但注意：argv 只能证明代理传了什么，**证明不了 CLI 采纳了什么**。
要确认落档，看 CLI 自己的 session transcript：
`~/.qoder-cn/projects/<项目slug>/<session-id>.jsonl` 里 `type=runtime-config` 那条的
`reasoningEffort`，以及 assistant 消息里有没有 thinking 块（0 长度才是真的关掉了）。
`scripts/tier_matrix.py` 就是按这个方法写的。

---

## 9. 三个可独立使用的 exe（同一份代码，零分支）

**不要为站点分叉代码或历史。** `4a8e14d` 之后一个二进制就能同时服务 CN 与国际版；
"服务哪个站点"只是配置。三份 exe 靠的是 `desktop/instance.go` 已有的机制：
**exe 旁边放一份 `lingma-proxy.json`** 会被最优先读取（`configSearchPaths()` 的第一项），
其中的 `instance_name` / `instance_id` 让多份实例并排运行（否则 Wails 的单实例锁会把第二次启动
折回第一个窗口，且两份会互相覆盖同一份共享设置）。

### 三者的完整区别

| | `LingmaProxy-cn` | `LingmaProxy-intl` | `LingmaProxy-both` |
| --- | --- | --- | --- |
| 端口 | **8095** | **9095** | **10095** |
| `qodercli_sites` | `["cn"]` | `["global"]` | `["cn","global"]` |
| `instance_id` | `lingma-proxy-cn` | `lingma-proxy-intl` | `lingma-proxy-both` |
| 窗口标题 | Lingma Proxy (CN only) | Lingma Proxy (Intl only) | Lingma Proxy (CN + Intl) |
| `/v1/models` 数量 | 14 | 17 | 31 |
| 模型 id 形态 | 裸名：`Qwen3.8-Flash` | 裸名：`Ultimate` | CN 裸名 + 国际版带前缀：`intl/Ultimate` |
| 登录态文件 | `com.qodercn.app.stable` | `com.qoder.app.stable` | 两份都要，缺哪个站点该站点就报「登录态不可用」 |
| 适用 | 只跑 CN，给现有客户端当默认服务 | 只跑国际版 | 两个站点同时要用 |

三份 `backend` 都是 `"qodercli"`，`host` 都是 `127.0.0.1`。

**⚠️ both 版必须用 `intl/` 前缀调国际版模型。** 两个站点有同名模型（`Auto`、`Qwen3.8-Flash`、
`GLM-5.3`、`Kimi-K3` 等），both 版把国际版那一侧的 id 统一加上 `intl/` 前缀以避免撞名：
CN 侧是 `Qwen3.8-Flash`，国际侧是 `intl/Qwen3.8-Flash`。单站点的 cn / intl 版**不带前缀**。
从 both 换到 intl 或反过来，客户端里填的模型 id 要跟着改。

实测（2026-09-20 本机）：intl 版起在 9095 返回 17 个、both 版起在 10095 返回 31 个
（14 裸名 + 17 个 `intl/` 前缀）、改端口前的 cn 版起在 8096 返回 14 个且不含国际版独有模型。
cn 版现在按用户要求占 8095。

### 构建

```bash
cd desktop/frontend && npm run build          # 已有 dist 可跳过
cd desktop          && go build -tags production -o LingmaProxy.exe .
```

然后同一个 `LingmaProxy.exe` 复制进三个目录，各放一份对应配置的 `lingma-proxy.json`：

```json
{"host":"127.0.0.1","port":8095,"backend":"qodercli",
 "qodercli_sites":["cn"],"instance_name":"Lingma Proxy (CN only)","instance_id":"lingma-proxy-cn"}
```

## 10. 使用教学（拿到 zip 之后怎么做）

1. 解压到任意**固定**目录（exe 会在自己旁边读写 `lingma-proxy.json`，别只放桌面临时文件夹）。
2. 双击 `LingmaProxy.exe`。窗口标题就是配置里的 `instance_name`，可据此确认起的是哪一份。
3. 验证起来了：`curl http://127.0.0.1:<端口>/health` 返回 200；
   `curl http://127.0.0.1:<端口>/v1/models` 出模型列表。
4. 前提是本机对应站点的 Qoder 桌面版**登录过**（见第 3 节；不需要开着那个 app，也不需要网页登录）。
5. 给客户端接入：把 baseURL 的端口改成这一份的端口。
   - Anthropic 风格（ZCode / DSH 这类直连）：CN 版填 `http://127.0.0.1:8095`（**不带 `/v1`**），
     MiniMax Code 这类走 `@ai-sdk/anthropic` 的要带 `/v1`。
   - OpenAI 风格经 opencodex：改 `~/.opencodex/config.json` 里 `providers.lingma-proxy.baseUrl`
     后必须 `ocx restart`（运行中的代理用启动时缓存的配置）。
6. 端口占用：`both` 现在是 10095，不再和 8095 抢；`cn` 占 8095，所以起 `cn` 之前要先停掉
   原来跑在 8095 的那份，否则它会报端口占用或起在别的端口上。
7. 三份可以同时运行（`instance_id` 不同），只要端口互不冲突。
8. 停止：关闭窗口即可；后台残留用 `taskkill /F /IM LingmaProxy.exe`。

## 11. 发布位置

GitHub Release（tag 故意不带 `v` 前缀，避开 `release.yml` 的 `on: push: tags ["v*"]`）：

- 页面 https://github.com/tcflying/lingma-proxy/releases/tag/desktop-3site
- `.../releases/download/desktop-3site/LingmaProxy-cn-win-x64.zip`
- `.../releases/download/desktop-3site/LingmaProxy-intl-win-x64.zip`
- `.../releases/download/desktop-3site/LingmaProxy-both-win-x64.zip`

`gh` 的两个坑：建 tag 需要 `workflow` scope → 改用 `git tag` + `git push` 推 tag，再
`gh release create --verify-tag`；而且**必须显式 `-R tcflying/lingma-proxy`**，
否则 gh 会解析到 upstream `Lutiancheng1/lingma-proxy` 并报 tag 不存在。
覆盖已有资产用 `gh release upload <tag> <files> --clobber`。
