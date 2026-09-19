# Qoder 国际版接入交接文档

对应提交：`4a8e14d feat: serve the international Qoder site from the CLI backend`（分支 `main`）
桌面版预构建包：https://github.com/tcflying/lingma-proxy/releases/tag/intl-desktop-9095
本文只描述代码与配置，不含任何账号凭据。

范围划分：qodercli 后端的整体交接（为什么换、构建、档位表语义、四家客户端接入、复测手法）看根目录
[`HANDOVER.md`](../HANDOVER.md)；本文只覆盖**双站点（CN + 国际版）**带来的改动与运行方式。

## 1. 现在能用什么

qodercli 后端从「只认 Qoder CN」变成**同时服务两个 Qoder 部署**：

| 站点 | Site 值 | 网关 | CLI 环境变量 | 桌面端 profile |
| --- | --- | --- | --- | --- |
| Qoder CN | `cn` | `openapi.qoder.com.cn` | `QODERCN_JOB_TOKEN` | `com.qodercn.app.stable` |
| Qoder 国际版 | `global` | `openapi.qoder.sh` | `QODER_JOB_TOKEN` | `com.qoder.app.stable` |

两站点的 OAuth client id 是同一个（见 `internal/qodercli/token.go` 的 `defaultClientID`），
worker runtime 也是同一份，差异只有域名、环境变量前缀、安装目录名和 profile 目录名。

模型目录（`--list-models` 实测）：CN 14 个，国际版 17 个。两边**有同名模型**（`Qwen3.8-Flash`、
`Qwen3.8-Max`、`Auto` 等），因此双站点模式下国际版一律加 `intl/` 前缀。国际版独有：
`Ultimate`、`Performance`、`Efficient`、`Sonus`、`Cantus`、`MiniMax-M3`；国际版**没有**
`MiniMax-M2.7`、`GLM-5.2`、`Qwen3.7-Flash`。

## 2. 代码改动地图

| 文件 | 作用 |
| --- | --- |
| `internal/qodercli/sites.go`（新） | `Site`（`cn`/`global`）与每个站点的域名、env 前缀、standalone 二进制名、agent-sdk 包名、安装目录名、profile 候选、独立 config-dir；`EnabledSites()` / `UsableSites()` / `SetEnabledSites()` |
| `internal/qodercli/detect.go` | 按站点解析安装位置：`DetectSite(site)`、`AvailableSite(site)`；`Location` 增加 `Site` 与 `ConfigDir` |
| `internal/qodercli/profile.go` | Electron profile 目录候选按站点分表；`LINGMA_QODERCLI_PROFILE` 只在目录名与请求站点一致时生效 |
| `internal/qodercli/token.go` | `NewTokenSource(profileDir, site)`，网关按站点取默认值 |
| `internal/qodercli/client.go` | 报错文案按站点；`--config-dir` 注入；`clampReasoningEffort` 变成 Client 方法且只裁 CN 表 |
| `internal/service/service.go` | 客户端按站点持有；`/v1/models` 合并目录；`splitCLISite()` 站点前缀路由；`resolveCLISite()` 回退规则 |
| `desktop/instance.go`（新） | `instance_name` / `instance_id` / 配置来源文件，让桌面应用能跑第二份 |
| `desktop/app.go`、`desktop/main.go` | 窗口标题与单实例锁取自 `instance_id`；设置写回来源文件并保留未知键；`app-state-<id>.json`；后端标签显示实际站点 |
| `cmd/lingma-ipc-proxy/main.go`、`config.example.json`、`README.md` | `--qodercli-sites` 旗标与 `qodercli_sites` 配置键、文档 |

## 3. 站点路由规则（读代码前必须知道）

1. 请求里的模型 id 会先过 `splitCLISite()`：在**任意**以 `/` 分隔的段里出现
   `intl` / `global` / `国际` / `国际版`（大小写不敏感）即判为国际版，剩余部分才是真实模型名。
   所以 `lingma-proxy/intl/Qwen3.8-Flash-xhigh` 也能正确路由（很多客户端会带自己的 provider 命名空间）。
2. 没点名站点时默认 `cn`；只有当 CN 在该机不可用而别的站点可用时才整体切过去。
   **点名了站点但该机没有 → 直接报错，不静默换站点。**
3. `intl/` 前缀只在**同时服务两个站点**时出现在 `/v1/models` 里；单站点实例给裸模型名。
   响应里的 `model` 回显与请求写法保持一致（点名了才带前缀）。
4. 思考档位：`reasoningLadders` 那张逐模型表是对 **CN 目录**实测出来的，国际版走透传，
   由它自己的签名模型目录决定档位是否生效（实测 `--reasoning-effort xhigh` 能被国际版接受）。

## 4. 国际版凭据链与那个必须记的坑

链路和 CN 完全同构：`com.qoder.app.stable` 下 `Local State`（DPAPI 解出 AES key）→ 解 `auth.v1.dat`
拿到 device token → `POST <站点网关>/api/v1/me/jobToken` 换 job token → 以 JSON 原样塞进
`QODER_JOB_TOKEN` → spawn 桌面端自带的 worker runtime 完成签名与推理。Go 侧无法绕开 CLI 自己签名。

**坑：国际版 CLI 必须用独立的 `--config-dir`。** 共享默认目录 `~/.qoder` 时，CLI 会读到桌面端自己残留的
登录态，然后对每个请求回
`Failed to fetch model list: auth.getUserInfo failed: token is not active`——
而同一个 job token 直接打 `/api/v1/userinfo` 是 200，症状完全指不到真因。
代码里由 `Site.ownConfigDir()` 处理：国际版固定用
`%APPDATA%\lingma-proxy\qoder-cli-global`（Windows），CN 不传这个参数，argv 与改动前逐字节相同。

## 5. 配置项

`lingma-proxy.json`（headless 与桌面通用，exe 同目录优先）：

```json
{
  "backend": "qodercli",
  "port": 9095,
  "model": "Auto",
  "qodercli_sites": ["global"],
  "instance_name": "Lingma Proxy · 国际版",
  "instance_id": "lingma-proxy-desktop-intl"
}
```

- `qodercli_sites`：钉住服务的站点，也可用环境变量 `LINGMA_QODERCLI_SITES=cn,global`；不配 = 两站都做。
  只能收窄，不能启用本机没装/没登录的站点。
- `instance_name` / `instance_id`：仅桌面版使用。`instance_id` 决定 Wails 单实例锁，
  同时把仪表盘状态文件隔离成 `app-state-<instance_id>.json`。

## 6. 怎么起一个独立实例

headless：

```bash
LINGMA_QODERCLI_SITES=global ./lingma-proxy -port 9095   # 不配站点就是双站点合并
```

桌面版（Windows 手工生产构建，注意 `-o` 要用相对路径，绝对 MSYS 路径会「构建成功但没写文件」）：

```bash
cd desktop && npm --prefix frontend run build && go build -tags production -o LingmaProxyIntl.exe .
```

把 exe 放进**新目录** + 上面那份 `lingma-proxy.json`，双击即可（不需要环境变量，CLI 自己走系统代理）。

**sidecar 必须是合法 JSON**：`"cwd": "C:\Users\you"` 这种单反斜杠写法会让解析失败，
而加载逻辑是「失败就换下一个候选文件」→ 静默回退到共享的 `~/.config/lingma-proxy/config.json`
→ 又拿到旧实例的端口和 `instance_id` → 第二个窗口**秒退且无日志**（exit 0，无 WER 记录）。
现在 `readInstanceProfile` 会把被忽略的文件打进日志。Windows 路径一律写 `C:\\Users\\you`。

## 7. 已验证事实与复测方法

| 项 | 结果 |
| --- | --- |
| 国际版 job token | `POST openapi.qoder.sh/api/v1/me/jobToken` → 200，24h |
| 国际版模型目录 | 17 个，`--list-models` 正常 |
| 国际版推理 | `Qwen3.8-Flash` / `Kimi-K3` 均正常出词；Anthropic `/v1/messages` + `reasoning_effort=xhigh` 正常 |
| 双站点合并 | 单进程 `/v1/models` = 31 条（14 + 17 `intl/`） |
| 单站点独立实例 | `/v1/models` = 17 条、无前缀 |
| CN 回归 | 未受影响，argv 无 `--config-dir` |

复测档位/模型不用重跑全表：代理发的每个请求都会落成一次 CLI 运行，
看 `<config-dir>/logs/runs/<时间戳-p<pid>>/manifest.json` 的 argv，以及同目录 `qodercli.log` 里的
`reasoning_effort=<档>` 与 `"is_reasoning":true/false`（CLI stdout 里看不出来）。
**别做 14×6 那种批量探针**，会打光当天 Chat 额度，表现为所有请求 HTTP 500（CLI 错误码 110），次日恢复。

## 8. 已知限制

- 国际版账号是 Free（`/api/v2/user/plan` 返回 `is_paid_plan:false`、quota 0），实测仍能出词（`billable:false`），
  但额度与稳定性不保证。
- `api2.qoder.sh` / `center.qoder.sh` 在中国大陆网络下必须走代理；国际版 CLI 连不上时报的是
  connect timeout 而不是业务错误，排查时别误判成代码回归。
- 图片输入在这条链路仍不支持（国际版同样）。
- 登录态解密目前只实现了 Windows（`os_crypt_other.go`），macOS/Linux 上两站点都不可用。
- 国际版逐模型档位表未实测，走透传。

## 9. 回滚

- 只想回旧行为：`LINGMA_QODERCLI_SITES=cn`（或删掉独立实例目录），CN 侧 argv 与改动前逐字节相同。
- 独立桌面实例是单独目录 + 单独 `instance_id` + 单独状态文件，删除目录即可，不影响旧实例。
