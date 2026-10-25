# Task UI 交接协议（Tool UI Spec）

> 面向**给工具写页面的人**。这里定义页面与 SRCOS 之间的全部交接面：
> 提交、状态、日志、产物、取消、路径选择。
>
> 页面归工具，协议归平台。SRCOS 不解释页面状态、不托管表单、不渲染工具的领域结果；
> 它只提供执行事实。仓库不变式见 [`../AGENTS.md`](../AGENTS.md)，执行契约见
> [`tool-spec.md`](tool-spec.md)。

---

## 0. 一句话

> 「点运行」= 页面调用 **`POST /api/jobs`** 提交一个普通 task；之后页面轮询
> **`GET /api/jobs/<id>`**、看 **`/logs`**、列 **`/artifacts`**、需要时 **`/cancel`**。
> **没有第三种 RunUnit**：UI 是 `kind: service`（或任何静态页面），执行是 `kind: task`。

一个共享包可以同时注册 UI 与 task：

```
srcos-tools/<pkg>/
├── ui/tool.yaml      # kind: service，root: ..
├── task/tool.yaml    # kind: task，   root: ..
├── task/work.sh
└── shared/...        # 两边共享；SRCOS_TOOL_DIR 指向包根
```

完整可运行示例：[`../srcos-tools/scrna-demo/`](../srcos-tools/scrna-demo/README.md)。

---

## 1. 身份

页面有两类部署形态，凭据不同；**不要混用**。

### 1.1 浏览器里的同源页面（本示例的形态）

页面由网关代理到 `/proxy/<user>/<ui-tool>/...`，与 `/api/*` **同源**。

- 凭据：浏览器现有的 **session cookie**，`fetch` 加 `credentials: "same-origin"` 即可。
- 写请求（`POST`）必须满足 CSRF 校验：浏览器会带同源 `Origin`，平台接受；
  跨站页面会被 `403 cross-origin request rejected`。
- 页面只读自己的实例：接口按登录用户过滤，别人的实例一律 `404`。

### 1.2 服务端页面（Shiny / Streamlit / 自建后端）

页面进程在沙箱里，没有浏览器 session，必须用**实例凭据**：

```yaml
# ui/tool.yaml —— 只有 kind: service 可以声明 agent
agent:
  mcp: [read, submit]          # 要提交就必须有 submit
  tools: [scrna-demo-task]     # 可选：把 submit 收窄到目标任务（必须是 owner 权限的子集）
```

- SRCOS 在实例启动时签发 token，写入 `$SRCOS_AGENT_TOKEN_FILE`（只读 bind；宿主路径在
  `data/instance-credentials/<instance-id>/agent-token`），实例停止时撤销。
- 调平台用 `$SRCOS_API`（如 `http://127.0.0.1:30152/api`）；请求头
  `Authorization: Bearer <token 文件内容>`。
- `$SRCOS_API` 在「本机没有已知网关」时不注入——页面应能区分「没网关」和「调用失败」。
- 实例 token 的权限永远是所属用户权限的**子集**：Grant、配额、`tools` 白名单仍然生效。

---

## 2. 提交

### 2.1 请求

```
POST /api/jobs
Content-Type: application/json
Origin: <网关自身 origin>          # 浏览器自动带；Bearer 调用可省略
Authorization: Bearer <token>       # 仅服务端页面

{
  "tool": "scrna-demo-task",
  "name": "scrna S001",
  "params": { "sample_id": "S001", "copies": 3, "prefix": "scrna" },
  "outputs": ["/workspace/out"],
  "tags": { "sample": "S001" },
  "idempotencyKey": "0d1f…"
}
```

| 字段 | 必需 | 说明 |
|---|---|---|
| `tool` | ✅ | **task 工具**的 id。service 不能用 `/api/jobs` 提交（是 `400`），服务要 `/api/tools/<id>/start` |
| `params` | — | 键必须匹配该工具 `interface.inputs[].name`；未声明的键、缺失的必填、越界数值都 `400` |
| `outputs` | — | **沙箱绝对路径**（如 `/workspace/out`），必须落在可写挂载上；不支持模板 |
| `resources` | — | 只能降不能升（工具上限 × Grant 配额） |
| `tags` | — | 键值对，用于列表筛选；不要放凭据 |
| `idempotencyKey` | — | 见 §2.3 |
| `name` | — | 展示名，≤200 字 |

**workspace 是 per `(用户, 工具)` 的**：页面自己的 `/workspace` 不是任务工具的 `/workspace`。
页面只传沙箱路径，跨工具共享数据要走 `requires_storages`（管理端声明）。

### 2.2 响应

```json
{ "jobId": "job-…", "dir": "/…/data/ws/alice/scrna-demo-task/jobs/job-…",
  "tool": "scrna-demo-task", "instanceId": "alice-scrna-demo-task-job-…",
  "state": "pending" }
```

`201 Created`。**用 `instanceId` 做后续所有调用**（状态/日志/产物/取消都接受完整实例 id，
也接受无歧义的后缀）。提交返回时记录已写好，立刻可查。

### 2.3 幂等键

- 幂等键按 **`(用户, 工具)`** 命名空间；同一个键只对应一个 job 目录。
- 重试同一个请求（网络抖动、用户双击）返回**第一次**的结果与当前状态，不会产生第二个 job。
- **不同的一次点击要生成不同的键**。推荐每次点击 `crypto.randomUUID()`；如果提交失败需要
  重试，复用**这一次点击**的键（本示例的「重试提交」按钮就是这么做的）。
- 不要用一个固定键表示「同一个工具」：那会把不同参数的任务误判为同一次提交。

---

## 3. 状态

```
GET /api/jobs/<instanceId>
```

`200` + 扁平 JSON（`inspect.InstanceView` 的字段 + `links`）：

```json
{
  "id": "alice-scrna-demo-task-job-…",
  "tool": "scrna-demo-task",
  "kind": "task",
  "name": "scrna S001",
  "state": "running",
  "exitCode": 0,
  "error": "",
  "backend": "local",
  "sandbox": "bwrap",
  "limiter": "systemd-run",
  "duration": "",
  "startedAt": "2026-10-25T10:00:00Z",
  "endedAt": "2026-10-25T10:00:42Z",
  "outputs": ["/workspace/out"],
  "tags": {"sample": "S001"},
  "links": {
    "logs": "/api/jobs/<id>/logs",
    "artifacts": "/api/jobs/<id>/artifacts",
    "cancel": "/api/jobs/<id>/cancel"
  }
}
```

**状态以这个端点为准。** 不要从日志内容、`state` SSE 事件或文件存在与否推断终态。

### 3.1 task 状态机

```
pending ──▶ running ──▶ succeeded   (退出码 0)
                    ├─▶ failed      (退出码非 0 / 运行时失败 / 探针超时)
                    └─▶ stopped     (被取消)
```

- `pending`：已提交，队列尚未认领（网关不在时也可能一直等）。
- `running`：进程已启动。
- 终态集合：`succeeded | failed | stopped`。轮询到这里就停。
- `submitted`：**只有**声明了 `completion.doneWhen` 的 task 才会出现（退出 0 代表「已提交」，
  探针还没成功）；普通 task 不要依赖它。
- service 另有 `starting` / `idle` / `stopping`；task UI 不关心。

失败原因在 `error`，进程退出码在 `exitCode`。工具非零退出是 **`state: failed`（HTTP 仍是 200）**，
不是 HTTP 错误。

---

## 4. 日志

```
GET /api/jobs/<id>/logs            # 纯文本尾部（默认 200 行，?tail=N）
GET /api/jobs/<id>/logs?follow=1   # Server-Sent Events
```

SSE 事件：

| event | data | 含义 |
|---|---|---|
| `line` | 一行日志 | 追加到页面 |
| `note` | 说明 | 例如日志读取失败的原因 |
| `state` | 终态 | **提示**进程已结束；随后仍有 `end` |
| `end` | 空 | 流结束，页面应 `close()` |

保活是 SSE 注释 `: ping`（没有 event）。`X-Accel-Buffering: no` 已设，反向代理不会攒包。
一次提交只有一次执行，日志文件随实例保留，刷新页面可以重新 `tail`。

> v0 没有进度事件。需要进度就写进日志（stdout），页面从 `line` 里自己解析。

---

## 5. 产物

```
GET /api/jobs/<id>/artifacts
```

```json
{
  "artifacts": [
    { "path": "/workspace/out", "exists": true, "isDir": true, "entries": 1,
      "mtime": "2026-10-25T10:00:41Z",
      "addr": "srcos://file/workspace/out?tool=scrna-demo-task" }
  ]
}
```

- 只列**提交时 `outputs` 声明的**路径，不做目录遍历。
- 解析走实例自己的挂载表；`exists:false` 时会带 `note`（`missing` / `unresolvable` /
  `escapes its mount (symlink)` 等）。**不要**把 `note` 当成可展示的正文。
- 有 `addr` 时，把它交给平台的 viewer：

  ```
  /view?src=<urlencode(addr)>
  ```

  没有 `addr` 的产物不在任何可浏览 scope 里（工具包、流程运行目录），只能由工具自己呈现。
- 目录产物只给 `entries` 计数；展开由 viewer 负责。

---

## 6. 取消

```
POST /api/jobs/<id>/cancel
Content-Type: application/json
{}
```

- 需要 `submit` scope（浏览器 session 天然具备；Bearer 要显式带上）。
- **幂等**：已经结束的实例再取消是 `200`，不是错误。
- 只停自己的实例；别人的实例是 `404`。
- 响应：`{"instanceId": "...", "state": "stopped"}`。
- 取消是权威的：等待者不会把 `stopped` 覆盖成 `failed`。

---

## 7. 路径选择（`type: path` 输入）

页面**不要**自己拼宿主路径。用平台的原语控件或接口：

```
GET /api/paths?tool=<task-tool>&input=<path-input>&path=<sandbox-path>&select=<directory|file>
```

- 范围由 **task 工具**的 `interface.inputs[]`（`from` → `requires_storages`）推导，
  **不是 UI service 的 storages**；`storage=` 只能在该 input 声明的根之间切换。
- 返回的是**沙箱路径**，直接作为 `params.<name>` 提交即可；平台在写入路径会再次复核
  （storage 闭包、文件/目录类型、`files` 是否存在）。
- 前端控件：`<script src="/assets/srcos-path-picker.js"></script>` +
  `<srcos-path-picker tool="…" input="…" name="…">`。
- 需要「新建输出目录」时不要用 `path`（它要求路径已存在）；用普通 `string` 或
  `type: directory` 的原始沙箱路径。
- 完整示例：[`../srcos-tools/scrna-demo/path/`](../srcos-tools/scrna-demo/path/tool.yaml)
  （`requires_storages: [data]`，UI 里按工具切换显示 picker）。**别忘了给这个 task 工具授权**；
  `/api/paths` 与 `POST /api/jobs` 都走 Grant，少给一个就是 403/404。

---

## 8. 认证与错误分类

未登录 / 无有效 Bearer → `401`。跨站写 → `403`（CSRF）。
业务错误统一是 `{"error": "..."}`；**HTTP 状态是分类，`error` 是细节**。

| 场景 | HTTP | 典型 `error` |
|---|---|---|
| JSON 坏 / 缺参数 / 参数不匹配 interface / 对 service 提交 | `400` | `invalid request: …` |
| 未授权、无 submit scope、工具不在 allowlist、配额超限 | `403` | `not permitted: …` |
| 未知实例、别人的实例、未授权的工具 | `404` | `not found: …` |
| 本机没配 tools / flows | `503` | `not configured on this host` |
| 平台自身故障 | `500` | 具体原因 |

`404` 同时用于「不存在」和「你看不到」，这是有意为之：不让页面通过状态码探测别人的实例。

**执行失败不走 HTTP 错误**：工具非零退出、超时、被取消都体现在 `state` / `exitCode` / `error`。
页面的错误处理应分两层：请求层（HTTP）和运行层（实例状态）。

---

## 9. task-like UI 的形态（允许与不允许）

允许：

- UI 是一个 `kind: service` 工具（Shiny / Streamlit / 静态 SPA / 任意前端），
  自行收集参数、自行呈现结果。
- 用户点「运行」→ 页面 `POST /api/jobs` → 轮询 / SSE / artifacts / cancel。
- 页面上一次运行的状态只存在页面里；刷新丢失是正常行为（平台不持久化页面状态）。
- 同一工具包里的 UI 与 task 用 `root: ..` 共享 `scripts/`、模板和静态资源。

不允许 / 不需要：

- ❌ 新增第三种 RunUnit，或让平台理解「页面处于提交前还是提交后」。
- ❌ 平台托管工具业务 UI、表单引擎、拖拽参数面板。
- ❌ 从 `interface` 推断页面布局；`interface` 是**执行签名**，只用于校验/连线/兜底表单/CLI。
- ❌ 页面直接写别的工具的 `$SRCOS_JOB_DIR`（workspace 不共享）；提交只走 `POST /api/jobs`。
- ❌ 从日志内容推断成功/失败；状态只看 `GET /api/jobs/<id>`。

---

## 10. 最小检查清单

- [ ] 任务工具是 `kind: task`，UI 是 `kind: service`（或静态托管），**所有**被引用的 task 工具都有 Grant。
- [ ] `type: path` 的 `from` storage 已在部署的 `storages.yaml` 声明，且 task 工具是 `sandbox: bwrap`。
- [ ] 提交用 `instanceId` 追踪；每次点击新的 `idempotencyKey`，重试复用同一个。
- [ ] 轮询 `GET /api/jobs/<id>` 到 `succeeded|failed|stopped` 即停。
- [ ] 日志用 `?follow=1`，收到 `end` 关闭 `EventSource`。
- [ ] 产物链接只使用返回的 `addr`，通过 `/view?src=` 打开。
- [ ] 取消按钮幂等；终态后禁用。
- [ ] 路径参数用 `/api/paths`（按 task 工具），不拼宿主路径。
- [ ] 页面不缓存旧的 `app.js`（示例服务发 `Cache-Control: no-store`）。
