# SRCOS 会话交接简报

> **重开会话后先读这个。** 本文是交接简报，不是唯一状态来源。
>
> | 想知道什么 | 看哪 |
> |---|---|
> | 为什么这么设计（不变的） | [`../AGENTS.md`](../AGENTS.md) |
> | 我们决定了什么、做到哪了 | [`roadmap.md`](roadmap.md) |
> | 工具开发者要遵守的契约 | [`tool-spec.md`](tool-spec.md) |
| 流程管理员要遵守的契约 | [`flow-spec.md`](flow-spec.md) |
> | 这台机器的实测环境事实 | [`environments.md`](environments.md) |
> | **目标 / 现状 / 下一步 / 已踩的坑** | **本文** |
>
> 最后更新：2026-09-24（Phase 0/1 完成，Phase 2/3 基本完成，Phase 4 完成，**Phase 5 = 画布 + `srcos://` 资源协议与自带 viewer + 任务列表与 SSE 日志流**，**MCP 第二期（submit / cancel / run_flow）**，**任务队列消费者 + 任务以 systemd unit 运行（ADR-022 收尾：重启后仍有真实退出码）完成**；agent token + MCP（读 + 写）+ 动态路由 + 自动回收 + 管理端 + 流程 + 资源查看器 + 任务/日志 + 队列完成）

---

## 0. 30 秒版

**SRCOS 是 AI 平台的确定性执行后端。探索用 AI，执行用 SRCOS。**

现状：一个 Go 单二进制（`github.com/seqyuan/srcos`，24 个包，约 4 万行，约 640 个测试用例；52 个测试文件），
在**网关**（继承自 goprox 的多用户认证反向代理）之上长出了**工具平台**四层：
工具契约、实例化运行时、存储 provider、授权模型。认证面现在有两扇门：
浏览器的 session cookie，与程序（agent / MCP 客户端）的 agent token（`Authorization: Bearer`）。
全部在 node01 上端到端实测过。

**agent 面已经能用，而且真的能执行**：网关内置 `/mcp`（Streamable HTTP，9 个只读工具 + 3 个写工具），
官方 Python MCP SDK 客户端实测连得上、列得出、查得到、读得回、**提交得了**（`submit` scope，
可按工具收窄）。**提交即自动执行**：网关自己消费投递目录（ADR-022），队列是持久的。

服务实例现在**可以通过网关访问**（`/proxy/<user>/<tool>/`，动态路由表从实例记录重建）。

网关现在还会**自己收养与回收**：启动时 reconcile（活着的收养、死了的标 stopped）、每 10 秒按
`lifecycle` 回收（`idle_ttl` 计流量，不是计"启动多久"），未就绪实例在浏览器里是进度页而不是 404。

管理员现在有一个**控制台**（`/admin`）：实例总览（全部用户 + CPU/内存快照 + 日志）、跨用户强制停止、
工具/组/授权内联编辑 —— 授权改动立即生效，手工改 `grants.yaml` 也在 10 秒内生效。

流程现在**真的能跑了**：`srcos flow run --samples t.csv` 把「节点 × 样本」展开成普通任务，
按依赖顺序投递，节点产物落在平台推导的 `/flow/runs/<run>/...` 下（所以连线不需要任何模板语法），
`flow resume` 跳过已完成/已签名的节点。

流程还学会了**自己收敛**：并发上限（`--concurrency`，默认 4）、失败自动重试（`retry.max`，任务粒度、30s→20m 退避）、
`flow cancel`（跨进程）、按 grant 配额拦（并发也拦得住）。

管理员还能**画流程**：`/admin/flows/<id>/edit`（webui/ Vite+React，只有这一页加载它 —— ADR-012），
拓扑分层自动布局、两步点连线、**服务端校验**（画布不重复实现类型规则）、校验通过才写回 `flow.yaml`。

下一步：**Phase 5.5（dsh 路 B）** —— 把 `srcos://` 协议接成 dsh 插件（路 D 的地址语法已就位），
或 **Phase 6（SGE）** —— 需要一台真正的登录节点（roadmap §8 #12）。

---

## 1. 目标

### 一句话

> SRCOS 是 AI 平台的确定性执行后端。探索用 AI，执行用 SRCOS。

展开见 [`../AGENTS.md`](../AGENTS.md)「定位」章。三个支柱：

1. **执行路径 0 token** —— 一次性 token 投入（把探索出的方法固化成工具）+ 零边际 token 成本（反复执行）。
   本质是**把 AI 的探索成果资本化**。
2. **确定性 / 可复现 / 可审计** —— `work.sh` + `.sign` + 版本化工具 + `interface` 签名；
   谁、何时、哪个版本、什么参数全部留痕。
3. **不碰工具 UI** —— AI 让造 UI 的边际成本趋近于零，UI 因此不再是护城河；
   平台价值转移到**注册、授权、隔离、资源、编排、审计**。

### 差异化三角

| | 传统生信云平台 | 通用 AI Agent 平台 | **SRCOS** |
|---|---|---|---|
| 工具 UI | 平台托管（护城河） | 无，对话即 UI | 工具自己出；平台只提供**原语控件** |
| 交互 | 参数表单 | 自然语言 | 表单为主（确定性）+ agent 问答为辅（只读） |
| token 消耗 | 0 | 每步都消耗 | 执行路径 0；探索与问答路径消耗 |
| 确定性 / 可复现 | 中 | 差 | 强 |
| 可审计 | 中 | 弱 | 强 |

### 推论链（推导出后面所有设计）

```
UI 造起来便宜了 → UI 不再是护城河
  → 工具的核心资产 = work.sh（函数体）+ interface（类型签名），UI 是可替换的壳
  → 同一个工具可以有多个 UI：shiny / CLI / agent / 自动生成的表单
  → "注册什么"的答案不是 UI，而是 interface + entry
  → 一份 interface 派生三个前端（MCP schema / 画布连线 / 用户表单）
```

---

## 2. 现在到哪了

### 2.1 分层现状

```
┌─ 控制面 ────────────────────────────────────────────────────────────┐
│ ✅ 认证/授权：Grant（默认拒绝 · 组/用户/public/通配 · 聚合配额）      │
│ ✅ 程序认证：agent token（Bearer · 只存哈希 · scope · 过期 · 撤销）  │
│ ✅ 工具注册：扫描 --tools-dir / $SRCOS_TOOLS_DIR                    │
│ ⛔ 流程注册（Flow）：Phase 4                                        │
│ ✅ 实例编排：Backend/Handle + 端口池 + 动态路由表 + Reconcile/Reaper│
└─────────────────────────────────────────────────────────────────────┘
┌─ 数据面（继承自 goprox）────────────────────────────────────────────┐
│ ✅ 反向代理：<base> 注入 · WebSocket · 带宽限制 · SSO 头 · cookie 隔离│
│ ✅ 代理层接入动态路由表：/proxy/<user>/<tool>/ 可达 · 实例优先于卡片   │
│ ✅ 网关自查：启动 reconcile · 按 tick 回收 · 启动中/失败给页面而非 404 │
│ ✅ 管理端：/admin 控制台 + /api/admin/*（实例总览/强制停止/授权，仅管理员）│
│ ✅ 流程：契约/校验/展开/并发调度/重试/取消/续跑 + 画布（/admin/flows/<id>/edit）│
└─────────────────────────────────────────────────────────────────────┘
┌─ 运行层 ────────────────────────────────────────────────────────────┐
│ ✅ local（bwrap 沙箱 + systemd-run --user 限额，prlimit 兜底）       │
│ ✅ sge（qsub/qstat/qdel 架构 + rendezvous + ssh -L，未接真集群）     │
│ ⛔ apptainer sandbox（Phase 6）                                      │
└─────────────────────────────────────────────────────────────────────┘
┌─ 契约与界面 ────────────────────────────────────────────────────────┐
│ ✅ Tool：tool.yaml + 13 类注册期校验 + 机器可读 interface            │
│ ✅ Job：job.json + 目录即队列 + 对工具的校验 + 聚合配额              │
│ ✅ 存储：StorageProvider（/Volumes/data → /data，ro）               │
│ ✅ HTTP 面：/api/tools · /api/tools/<id> · /api/paths · /api/jobs   │
│ ✅ 生成式表单 + srcos-path-picker 原语控件                          │
│ ✅ agent token：Bearer 认证 · 只存哈希 · 只读 scope · 立即撤销       │
│ ✅ MCP Server：/mcp · 9 个只读工具 + 3 个写工具（需 submit scope）   │
│ ✅ agent token ＋ 自助页 /tokens ＋ /api/tokens（只认 session）      │
│ ✅ 资源协议：srcos:// ＋ /api/resources ＋ /view（自带 viewer）      │
│ ✅ 任务/日志：/tasks ＋ /tasks/<id> ＋ /api/jobs/<id>/logs（SSE）    │
│ ✅ 任务队列：网关消费投递目录（启动/唤醒/tick）· 认领 · 每 tick 结算 │
│ ✅ 画布：拖拽摆放（layout.yaml 旁挂）· expose 一键补齐            │
│ ✅ dsh 路 B：integrations/dsh-plugin（srcos 协议 + tab，未在 dsh 验证）│
└─────────────────────────────────────────────────────────────────────┘
```

### 2.2 已经端到端验证过的（node01 实测）

| 场景 | 结果 |
|---|---|
| **task** | `srcos job submit` → `job run` → 走 `ata -t 5` 主路径，5 样本并行产出正确，`systemd-run` 限额生效（CPUQuota=300% MemoryMax=3G） |
| **task 失败路径** | 未声明参数 / 必填缺失 / 资源越界 / 越界数值 / 工具非零退出，全部正确判别 |
| **service** | `svc start` → 端口池分配 20000 → bwrap 沙箱 → 探活通过 → 路由发布 → 直连 HTTP 200 |
| **service 停止** | `svc stop` → unit inactive、连接被拒、0 残留进程与 unit |
| **reconcile** | 模拟 SRCOS 重启 → `adopted 1`，重占端口 + 重发路由；记录说 running 但进程已死的标 stopped |
| **reaper** | `idleTTL` 到期回收；有开放 WebSocket 时不算空闲；SGE 上默认不做空闲回收 |
| **存储** | `/api/paths` 列出 `/Volumes/data` 内容并映射到 `/data`；沙箱内可读、**写入被拒（ro 生效）**、宿主路径 `/Volumes` 不可见 |
| **授权** | 无 `grants.yaml` → 所有工具对非管理员不可见；授权后 alice 可见可提交，carol 的目录/描述/提交/浏览全部 403「未授权」而非「未知」 |
| **配额** | 工具允许 4 核但配额给 2 核 → `cpu 4` 被配额拦（消息说明用了多少、上限多少）；实例满额后拦新实例；转终态后放行 |
| **路径浏览边界** | 未声明的 storage → 403；路径逃逸 → 400；未登录 → 401；`/assets/../../etc/passwd` → 404 |
| **agent token** | `token create` → `curl -H 'Authorization: Bearer srcos_...' /api/tools` → 200；`POST /api/jobs` → 403（只读 scope）；`token revoke` 后**不重启网关**再请求 → 401；把 `agent-tokens.yaml` 改坏 → 同一 token 立即 401（失败关闭），改回即恢复；`srcos del alice` 连带撤销该用户 token |
| **流程画布** | Playwright 驱动真实浏览器（headless Chromium）：打开 `/admin/flows/scrna/edit` → 渲染 2 节点 1 连线；点输出端口再点 int 输入端口 → 服务端拒绝：`a file output cannot fill a int input: only paths travel between nodes`；加节点 + 连线 → `校验通过（未保存）` → 保存 → `已保存到 flow.yaml`，文件里出现新节点与连线，且**画布自动补上了 `depends_on`**；刷新后 3 节点 2 连线（读的是文件）；节点几何无重叠、无越界（拓扑分层：count/qc-2 第 1 列、qc 第 2 列）；`srcos flow validate` 读同一份文件 → 3 nodes / 2 layers / 合法 |
| **资源查看器（`srcos://`）** | `/api/resources` 不带 `src` → 列出三个根（home / workspace·Demo / storage `share`）；带地址 → 元数据（`/home/alice/notes.md`、`mode: rw`、`viewer.kind: markdown`，**不含正文**）；`../../../../etc/passwd` → 403；未声明的 storage → 404；`raw`：`.md`/`.html`/`.csv` 全部 `text/plain`（永不 text/html）；`html` 端点 → `text/html` + `Content-Security-Policy: sandbox …`（**无 `allow-same-origin`**、无 `X-Frame-Options`）；`/view` 页面：Markdown 服务端渲染成 `<h1>Notes</h1>`/`<strong>world</strong>`、CSV 成 `rv-table`、文本带行号、目录可逐级导航、HTML 只出现在 `sandbox="allow-scripts …"` 的 iframe 里且**正文未内联进页面**；headless Chromium 实测：父页读该 iframe 的 `contentDocument` 为 `null`（不透明 origin），框内 `document.cookie` 抛异常 |
| **dsh 路 B（`integrations/dsh-plugin`）** | `make dsh-plugin`：**19 个用例**（地址前缀替换双向、协议归属、目录子地址、provider 的「未变化不重复发帧」「mtime 变了发新帧」「404 → `srcos/missing` 失败帧且流不断」「连不上 → `srcos/unreachable`」「外来地址不打扰网关」「abort 结束流」、配置注入/localStorage 回退、**打包契约**（bundle 塞进替身 `__ModuleLoader__` 后 `apply`/`inject`/`srcosDefinition` 的面，以及 `apply()` 用假 ctx 装配出 provider + tab + 面板体））；`sh test/live-run.sh`：起临时网关 + read token，真 HTTP 跑通 `roots` / 目录（**每个条目自带 `addr`**）/ 文本读 / 缺资源失败帧 / 401；`build.mjs` 产物按 dsh 的 `__ModuleLoader__.load({id, factory})` 信封注册（Node 里 fake `window` 加载并检查导出的 `apply`/`inject`/`srcosDefinition`/`SrcosPane`）|
| **画布：拖拽 + expose 补齐** | Playwright（headless Chromium）打开 `/admin/flows/scrna/edit` → 拖动节点 `translate(32,28)` → `translate(292,178)` → **刷新后位置不变**（读自 `layout.yaml`）→ 磁盘上 `layout.yaml` 有坐标而 `flow.yaml` **没有**（旁挂，契约不被污染）→ 服务端给出「1 个未接的必填输入」→ 一键补齐 → 草稿转为 `校验通过` → 保存写回 `flow.yaml`（`expose` 多出 `sample_id`） |
| **agent token 自助页** | Playwright：仪表盘有「令牌」入口 → `/tokens` 勾 `submit` + 工具 `ticker` → 生成 → 明文只显示一次且**不在 URL 里** → 刷新后列表（服务端渲染）显示 `有效` + `read,submit` + `ticker`，且页面里不再出现明文；用这枚 token 跑官方 Python MCP SDK → 12 个工具 → `srcos_submit_job` → 任务成功、日志可读；**撤销后**：行消失、该 token 立即被拒；**一枚活的 submit token** 打 `/api/tokens` 的 GET/POST/DELETE 全部 403「needs a browser session」，而同一个 token 打 `POST /api/jobs` 是 201 |
| **任务 = systemd 瞬时 unit（判定文件）** | 任务 `succeeded` + 日志里**既有 stdout 也有 stderr**（此前 unit 的日志根本没被捕获）+ `<log>.verdict` = `0 success`；失败任务 `failed` + `exitCode=3` + 「tool exited with code 3」；**运行中重启** → `tasks reconcile: adopted 1` → 进程死后 `settled … as succeeded (exit 0)`（真实判定，不是「no verdict」）；取消 → `stopped` 且 unit `inactive`（判定文件写的是 `TERM success`）；`job run --force` 重跑同一实例正常（`reset-failed` + 旧判定被删）；无 systemd 的降级模式仍能跑并写日志 |
| **任务队列（提交即自动执行）** | 网关起着：`POST /api/jobs`（带会话 cookie）→ 返回 `pending` + instanceId → **2.5s 后实例已 `succeeded`**（无人敲任何 CLI）；网关**停着**时用 `srcos job submit` 提交 → 启动网关 → `task queue: started 1 run(s) after startup` → 1s 内 `succeeded`；`--no-task-drainer` 时同一提交停 `pending` → `srcos job run` 手动跑成功；**运行中重启**：记录先被 `tasks reconcile: adopted 1`，进程死后下一 tick `settled`（state=stopped + 「no verdict」说明），**不重跑**；流程（`flow run`，2 样本）在队列运行时依然成功且每个 job 只出现一次（队列不偷流程节点）|
| **MCP 第二期（submit / cancel / run_flow）** | 官方 Python MCP SDK：read token → 9 个工具（无写工具），调 `srcos_submit_job` → `isError`「需要 submit scope」；submit token（`--tool ticker`）→ 12 个工具（3 个 write，`readOnlyHint=false`）→ `srcos_submit_job` 返回 `jobId`+`instanceId` → `srcos_task_status` 轮询到 `succeeded` → `srcos_task_logs` 有 tick 1..3 → `srcos_list_artifacts` 给出 `srcos://file/workspace/out?tool=ticker` → `srcos_read_file` 读到产物；白名单外工具（sleeper）被拒「may not submit」；`srcos_cancel_instance` → `stopped`；`srcos_run_flow` → 1 job 且跑到成功；网关日志有 `audit submit/cancel/run_flow` 五行（含工具版本与 job/instance/run）|
| **任务列表 + SSE 日志流** | `srcos job run` 一个每秒打印一行的任务 → `/tasks` 列表出现「运行中」；`/tasks/<id>` 页面**不重载**的情况下 `#log` 文本在 2.5s 内从 32 字长到 64 字（真流）；任务结束后 `state` 事件把徽章改成「成功」、指示器变「日志结束」、EventSource 自行 `close()`；产物的「预览」→ `/view?src=srcos%3A%2F%2Ffile%2Fworkspace%2Fout%3Ftool%3Dticker`（目录）→ 点 `result.txt` → 文本 viewer 显示 `all ticks done`；`/api/jobs/<id>/logs` 默认纯文本尾部，`?follow=1` 返回 `text/event-stream` + `X-Accel-Buffering: no` 并以 `event: state` / `event: end` 收尾；未知 id、`<id>/other`、另一用户的实例全部 404（`-race` 全绿）|
| **流程的并发/重试/取消/配额** | `--concurrency 2` + 6 样本 × `sleep 3` → **9.4s**（顺序 18s），时间线显示恰好 2 个重叠；`retry: {max: 2}` + 故意失败一次的任务 → 日志 `retry … in 30s (attempt 1/2)` → 最终 succeeded，记录里 `units: {s01-x: 2}`；`flow cancel`（另一进程）→ 运行 `cancelled`、实例 `stopped`、`sleep 120` 被杀、调度器自行收敛、节点标 `skipped`；`--max-instances 1` + 并发 2 → 第二个任务被拒：`instance quota reached: you have 1 of 1 allowed for this tool; stop one first` |
| **流程跑起来** | `srcos flow run --samples samples.csv scrna` → 2 节点 × 2 样本 = **4 个普通任务**（`job list` 里带 flow/run/node/sample 标签），按依赖顺序执行；下游 `qc` **真的读到了**上游 `count` 的产物（`clean.txt` = `counts.txt` 的内容，路径 `/flow/runs/<run>/nodes/count/s01-s001/outs`）；`nodes/*/.sign` 自动写好；`flow status` 列出运行；`flow resume` 两节点全 `skip (already done, signed)`；`flow run --dry-run` 打印每个 job 的参数与产物路径。失败路径：下游工具 `exit 3` → 该节点 `failed`（只提交了 1 个样本就停）、`when: always` 的 report 节点仍跑、普通下游 `strict` 标 `skipped`、退出码 1 |
| **流程契约与校验** | `srcos flow list` 显示 3 节点 / 3 层 / 样本列 `fastq_dir,sample_id`；`flow validate` 对合法流程给出拓扑序（`count → qc → report(when=always)`）；对坏流程**一次报出 8 个问题**（含环 `a → c → b → a`、未知输出、非法 `from`、4 个未满足的必填输入）；`count@9.9.9` 版本不匹配与 `kind: service` 当节点各自被点名；退出码 1 |
| **管理端（控制台 + API）** | `/admin` 渲染出全部用户的实例（含 `usage`：python 服务 RSS 71.6 MiB / CPU 0.013s，样本来自 systemd cgroup）、端点、限额与沙箱、授权矩阵（`组:bio`）与「强制停止」；非管理员 `/admin` → 302 回仪表盘、`/api/admin/tools` → 403；管理员 PUT 授权 → `grants.yaml` 落盘且**同一进程内 alice 立刻从 `/api/tools` 消失**（无需重启）；`POST /api/admin/instances/<id>/stop` → 记录 `stopped` 且 unit 变 inactive；仪表盘只对管理员显示「管理」入口 |
| **冷启动与自动回收** | `svc start` 一个 8 秒才就绪的服务 → 浏览器访问拿到 **503 + Retry-After: 3 + 「服务启动中」进度页**（自动刷新、含日志末尾）→ 就绪后再访问 **200**；`idle_ttl: 20s` 的实例被每 5 秒访问、持续 40 秒**不被回收**（活跃心跳写到 `data/service-activity.yaml`），停止访问 20 秒后网关日志出现 `reaped alice-idle1-svc (idle past idleTTL)`、unit 变 inactive、记录 stopped、页面变 **502「服务未在运行」+ 重启命令**；重启网关时日志 `reconcile: adopted 1 [alice-slow-svc]`、服务仍 200；`idle_ttl: 10m` 的邻居实例不受影响 |
| **服务实例经网关访问** | CLI `svc start` 起一个 service 工具 → `curl -b cj /proxy/alice/websvc/` → 200（含 `<base>` 注入）；子路径 `/index.html` → 200；裸路径 `/index.html` 带路由 cookie → 200（SPA 回投）；bob 访问 alice 的实例 → 404；裸短链接 `/websvc/lab` → 302 到 `/proxy/alice/websvc/lab`；**重启网关**后仍 200（启动时从记录重建路由表）；`svc stop`（另一进程）→ 第一次 502 + 日志「dropped ... after a failed dial」→ 第二次 404；记录被手改成 `10.0.0.5:80` / `169.254.169.254:80` → 不成为路由 |
| **MCP（agent 面）** | 官方 Python MCP SDK 客户端连 `/mcp` → `initialize`（`srcos dev`、协议 2025-06-18）/ `list_tools`（9 个）/ `call_tool`：列工具、列 storage（范围=工具声明）、浏览 `/data/ref`、读 `/data/ref/genes.tsv`、查实例状态与日志、列产物全部正确；`read_file /etc/passwd` → `isError`「not under any mount」；无 token → 401 + `WWW-Authenticate`；GET → 405；通知 → 202 空体；每次调用一行审计日志；撤销 token 后立即 401 |
| **降级模式的停止/回收** | 把 `systemd-run` 从 PATH 里拿掉（等价于无 user systemd 的登录节点）启动 service → 记录里有 `pid` + `pid_start`；**另起一个进程** `svc stop` → 进程消失、端口释放、记录 `stopped` 且无 error；`svc reap` 同样停掉；`svc reconcile` 对活着的降级实例 `adopted 1`、杀掉进程后 `orphaned 1`；systemd 可用时行为不变（`limiter=systemd-run` → `is-active` 变 inactive、unit 无残留） |

### 2.3 尚未实现（明确边界，不要误以为有）

- ⛔ **流程级审计** —— 现在只有实例记录 + `flowrun.yaml`（谁在何时跑了哪个流程、用了哪个版本、
  什么参数：`flowrun.yaml` 有参数与 job id，但还没有专门的审计流）
- ⛔ **画布的拖拽摆放与 `expose` 自动推导** —— 布局现在是拓扑推导（不写进契约），
  `expose` 的来源要在检查器里显式选；拖拽摆放需要给布局找个不污染 `flow.yaml` 的落点
- ⛔ **Phase 5 全部完成** —— 下一步是在**真实 dsh 里验证路 B**（插件已写、逻辑已测）、
  Phase 5.5 的**路 A**（dsh 当 service 工具），或 Phase 6（SGE，需要真登录节点）
- ⛔ **dsh 路 B 的 in-dsh 加载** —— 插件包与构建都在（`integrations/dsh-plugin/`），
  地址映射 / provider / 真实 REST 都用 Node 验过；但**没有往任何 profile 装过**，
  所以「dsh 真的加载并渲染它」这件事仍未验证（§3.3）
- ⛔ **降级模式（无 user systemd）下的判定** —— 那种情况下任务是个普通子进程，进程与等待者一起
  消失时没有判定文件可读，`ReconcileTasks` 只能写 `stopped` + 说明（ADR-022 的兜底分支）
- ⛔ **申请/审批**（谁能用哪个工具的申请流）—— 现在只有管理员直接 `grant`
- ⛔ **网关侧起停服务**（`svc start` 仍只在 CLI；管理端能停、不能起）—— admin API 的下一步
- ⛔ **storage 声明的管理端编辑**（`storages.yaml` 目前只有 CLI/手写）
- ⛔ **审计日志** —— 目前每个被接受的 agent 请求会写一行网关日志（含用户/token/方法/路径），
  但没有独立的审计流、没有结构化落盘、也没有“改配置”的审计
- ⛔ **`apptainer` sandbox** —— 声明了会明确报错
- ⛔ **SGE 未接真集群** —— 架构与测试都在（用 fake runner），但从未在真登录节点上跑过
- ⛔ **前端打包（`webui/`）** —— ADR-012 的 Vite 包还没建；目前前端是 Go 内嵌模板 + 一个原语控件
- ⛔ **dsh 集成** —— ADR-016 的三层策略已定，一步未做

---

## 3. 下一步

### 3.1 建议的下一步（按顺序，理由在右）

| # | 做什么 | 为什么现在做 |
|---|---|---|
| ~~1~~ | ~~**agent token 认证面**~~：✅ **已完成** | 见 §2.2 与 roadmap ADR-019「实现契约」 |
| ~~2~~ | ~~**MCP Server（read-only）**~~：✅ **已完成**（`internal/mcp` + `internal/inspect`，9 个只读工具，`interface → JSON Schema` 派生） | 招牌功能已可演示；只读面 + `submit` 预留 |
| ~~3~~ | ~~**代理层接入动态路由表**~~：✅ **已完成**（网关从实例记录重建路由表；实例优先于卡片；裸路径/Referer/路由 cookie 三处解析统一） | 平台缺的最后一块已经补上：`/proxy/<user>/<tool>/` 可达 |
| ~~4~~ | ~~**启动中进度页 + `svc reap` 定时调度**~~：✅ **已完成**（网关启动 reconcile、按 tick 回收、`idle_ttl` 按流量判定、未就绪给页面） | 冷启动体验与自动回收都到位了 |
| ~~5~~ | ~~**管理端页面**~~：✅ **已完成**（`/admin` + `/api/admin/*`：实例总览含 CPU/内存快照、跨用户强制停止、工具/组/授权编辑；顺带完成授权热加载） | 非 CLI 用户能运维了；管理 API 也把「实例运维」从 CLI 搬进了网关 |
| ~~6~~ | ~~**Flow 调度器**~~：✅ **主体已完成**（样本展开 + 顺序调度 + `.sign` 续跑 + `flow run\|resume\|status`） | 契约、校验、展开、调度、续跑都通了；剩余收尾见下 |
| ~~7~~ | ~~**Phase 4 收尾**~~：✅ **已完成**（并发、重试、取消、配额） | Phase 4 收工 |
| ~~8~~ | ~~**MCP 第二期**~~：✅ **已完成**（`submit` / `cancel` / `run_flow` + `submit` scope 与 `submit_tools` 白名单；`internal/execute` 是写入面唯一实现，REST 与 MCP 共用） | agent 真的能"执行用 SRCOS"了 |
| ~~9~~ | ~~**管理端画布**~~：✅ **已完成**（`webui/` Vite+React，只在该页加载；服务端校验；校验通过才写回） | 顺带补了一条契约规则：连线即依赖 |
| ~~10~~ | ~~**Phase 5 其余前端件**~~：✅ **全部完成**（`srcos://` 协议 + viewer、任务列表 + SSE 日志流、agent token 自助页、画布拖拽与 expose 推导） | Phase 5 收工 |
| ~~11~~ | ~~**任务队列消费者**（ADR-022）~~：✅ **已完成**（提交即自动执行；启动冲刷 + 唤醒 + tick；认领互斥；队列跳过流程节点） | 「目录即队列」终于名副其实；剩下的是「重启后退出码」那条 backend 限制 |

### 3.2 需要用户提供信息才能做的

| 项 | 阻塞什么 |
|---|---|
| **真正的 SGE 登录节点主机名** | `backend: sge` 的全部实现细节（`qsub`/`qstat` 路径、共享盘挂载点、`ssh` 免密可行性）。要在那台机器上跑 `scripts/probe-env.sh` |
| **`storages.yaml` 里除 `/Volumes/data` 外还要哪些根** | 目前只有 `data` → `/Volumes/data`（ro）。需要写区时得声明单独的项目目录 |
| **Phase 1 首个真实用例选哪个**（dsh 3080 / shiny 3838 / RStudio 8787） | node01 上三个都在跑，可以直接用现状验证 spec，比继续造示例更贴近需求 |

### 3.3 尚未被验证的假设

- **`runc 1.2.4` + `rootlesskit` 能否做无 root 容器化** —— 两者都已安装且 `/etc/apparmor.d/runc` 的 userns
  profile 已存在，可能零配置可用。这是 ADR-014 的进阶方案，值得实测
- **`prlimit` 路径下的 RSS 看门狗** —— RLIMIT 无法表达"每单元进程数"，超限只能靠外部轮询
- **dsh 插件的 in-dsh 加载** —— `integrations/dsh-plugin/` 的逻辑与真实 REST 都已验过，但
  没有装进任何 profile；dsh 是 developer preview，`src/client.js` 里用到的每个 API 都标了
  它读到的是哪个源文件（`packages/client/...`），第一次装进 dsh 时以那份源码为准

---

## 4. 已经踩过的坑（不要再踩）

这一节是本文最有价值的部分。每条都是实测出来的，不是推测。

### 4.1 沙箱 / bwrap

| 坑 | 结论 |
|---|---|
| **Ubuntu 24.04 的 `apparmor_restrict_unprivileged_userns=1`** 会拦掉未带 setuid 的 bwrap，报 `setting up uid map: Permission denied` | 用**按二进制单独授权**的 AppArmor profile 修，**不要**全局关 sysctl。node01 已修好，重跑 `scripts/probe-env.sh` 可确认 |
| **`bwrap --setenv` 的签名是 `--setenv VAR VALUE`（两个参数）**，不是 `VAR=VALUE` | 写成后者的症状是 `bwrap: setenv failed` |
| **userns 把所有未映射 gid 折叠成 65534**，而沙箱进程本身就在 65534 组里 | 宿主的 `group` 权限位在沙箱内**等于公开可读**。所以 **`MountSpec` 的粒度 = 数据可见范围，必须 bind 最小必要路径，绝不 bind 父目录** |
| **bwrap 无法在已只读绑定的 `/usr` `/bin` `/lib*` 内创建挂载点** | 报 `Can't create file at ...: Read-only file system`。工具自带二进制统一放 `/opt/srcos/bin`（沙箱 PATH 第一项），注册期直接拒绝违规配置 |
| **`--clearenv` 后必须显式给 `PATH`** | 否则工具连 `bash` 都找不到 |
| **`unshare --user` 失败不代表 bwrap 不能工作** | AppArmor 授权是按二进制给的 |
| **`ResolveExisting` 会把「文件不存在」报成「symlink escape」**（2026-09-23，写 viewer 时发现） | 它靠「最深的存在祖先」做 symlink 校验：目标本身不存在时，那个祖先必然在 mount 之外 → 报成逃逸（例：用户的虚拟 home 还没创建时，`srcos://file/home` 得到误导性的 `symlink escapes mount /home/alice`）。修法：**先 `os.Lstat` 判存在**（不存在即 `ErrNotFound`），再交给 `ResolveExisting`。教训：错误消息会变成错误的诊断方向 |
| **「目录即队列」的另一半是消费者**（2026-09-24，ADR-022） | 投递目录一直是队列，但**唯一消费者是人手敲 `srcos job run`** —— agent 与表单提交等于石沉大海。补上守护进程时暴露出三个必须一起定的规则：① **`pending` 不能算配额占用**（否则一条提交过不了它自己排队位置蕴含的那道检查：队列里排 10 个 ≠ 占 10 份资源）；② **认领必须是原子的**（O_EXCL 文件），否则网关队列与另一个进程的 `job run` 会把同一任务跑两遍；③ **流程节点不能被队列当普通项偷走**（`flowrun` 自己按依赖顺序调度并认领；队列还额外按 `Tags["run"]` 跳过，那个判断写在提交里、无窗口期）。教训：**一个「队列」如果没有消费者，它只是目录** |
| **重启后再结束的任务：退出码无从得知**（2026-09-24，ADR-022） | 网关重启不会杀掉任务（systemd 拥有进程），但记录退出码的是那个死掉的进程。更早的漏洞是 `Reconcile` **只处理 service**（`if inst.Kind != service { continue }`），于是任务记录会永远停在 `running` —— 既是谎话，又永久占着用户的配额。修法分两层：`runtime.ReconcileTasks` 在每个 tick 上结算失去等待者的任务；判定本身由 **systemd 写文件**（见下一条） |
| **「让 systemd 记判定」的三个坑**（2026-09-24，ADR-022 收尾） | ① **不能依赖 `systemctl show`**：瞬时 unit 退出后会被快速回收（实测 1.4s 后 `LoadState=not-found`），所以判定必须落成文件 —— 用 `ExecStopPost=-/bin/sh -c "echo $EXIT_STATUS $SERVICE_RESULT > <log>.verdict"`（前置 `-` 让写判定失败不影响运行结果）。② **unit 的 stdout 不继承我们的 fd**：`cmd.Stdout = file` 对 `systemd-run --unit`（非 scope）什么都抓不到，服务日志此前只有 systemd-run 自己那行；正解是 `-p StandardOutput=append:<log>`。③ **重跑同一实例会撞旧 unit 与旧判定**：先 `reset-failed` 再删旧 `.verdict`。顺带修掉 task/service 两条路径每次启动泄漏一个日志 fd |
| **取消会被等待者盖成 `failed`**（2026-09-24，写 MCP cancel 时发现） | `Cancel` → `StopService` 在 A 处写 `stopped`，而 `RunTask` 的等待者在 B 处被进程退出唤醒后可写 `failed (signal: terminated)` —— 两个 goroutine 各自 `SaveInstance`，谁后写谁赢，于是**一次故意的取消可能被记成崩溃**。修法：`RunTask` 在 `Wait` 返回后**回读记录**，若已是 `stopped` 就直接返回它（故意停止是权威）。确定性回归测试见 `execute` 的 `TestWaiterDoesNotOverwriteADeliberateStop`（先写 stopped、再放行进程）—— 去掉守卫它必失败（实测：`state=succeeded`）。教训：**记录是两个进程之间的决策点，只能回读，不能假设** |
| **提交返回的实例 id 一时查不到**（2026-09-24，MCP e2e 第一跑发现） | `submit` 先返回 `instanceId`、再由后台 goroutine 启动；而记录是启动时才写的 —— 于是 agent「刚提交就查」得到误导性的 `not found`。修法：`startTask` **先写 pending 记录再起 goroutine**。教训：一个立刻返回的 id 必须立刻可寻址 |
| **`Address.Child` 的语义陷阱：目录导航路径翻倍**（2026-09-23，浏览器 e2e 发现） | 资源条目的 `Rel` 是**相对 scope 根**的，而 `Address.Child(rel)` 是「拼接到当前路径」的。在 `/view?src=…/home/sub` 里用 `Child(entry.Rel)` 得到 `home/sub/sub/x.txt` —— 单测只断言「页面里出现了文件名」所以全绿，Playwright 点一下才发现 404。修法：条目的地址直接 `child.Path = entry.Rel`，**删掉 `Child`**（一个只在一个地方用、语义又容易搞错的便捷方法比没有更危险），并把「子链接必须是 scope 相对的」写成断言。教训：**UI 是检验契约的探针**（与流程画布那次同源）|

### 4.2 进程与资源

| 坑 | 结论 |
|---|---|
| **RLIMIT_NPROC 是按 real UID 全系统计数的，不是按单元** | node01 上该计数已达 1169，钳到 512 会让所有 `clone()` 返回 EAGAIN —— 连 bwrap 建 namespace 都失败，报成误导性的 `Creating new namespace failed: Resource temporarily unavailable`。每单元进程数上限本质是 cgroup 能力（systemd `TasksMax` 语义正确），**prlimit 路径下已移除 `--nproc`** |
| **`systemctl list-units <pattern>` 不按裸名匹配**（需要 glob） | "先问存在吗"的预检会静默返回 false 并跳过 stop —— 服务于是活过了自己的停止命令。正解：**直接 `stop` 并解释它自己的错误**（`not loaded`/`not found` 视为成功） |
| **降级模式下替换外层 `cmd.Env` 会弄丢 `DBUS_SESSION_BUS_ADDRESS`** | `systemd-run --user` 起不来，报 `Failed to connect to bus`。正解：用**内层 `env -i`** 清环境，让外层 limiter 继承宿主环境 |
| **`pkill -f '<pattern>'` 会匹配到执行它的 shell 自身** | 如果 pattern 出现在 shell 的命令行里（通常都会），pkill 会杀掉自己。用不自匹配的模式（如 `srcos[-]dev`）或改用 `lsof -ti:<port> \| xargs kill` |
| **`pkill -f 'srcos[-]dev serve'` 也自救不了** 如果同一条命令行里另有 `srcos-dev serve` 字面量（如 `nohup /tmp/srcos-dev serve ...`） | 括号只能避开「pattern 自己」，避不开同行其他字面量。症状：整条命令静默无输出（shell 被杀）。安全做法：**kill 与 start 分两次命令**，kill 用 `lsof -ti:<port> \| xargs kill` |
| **降级模式下 `svc stop` 停不掉自己启动的服务**（2026-09-22 已修） | 症状：`go test ./internal/runtime/` 每次泄漏 5 个 `python3 -m http.server`，连跑 4 次占满测试端口池（24100–24120）→ 服务测试全红（`no free port: port pool exhausted`）；生产上等于无 user systemd 的登录节点里**服务停不掉**（`_ = h.Stop()` 从不发生，实例却被标 stopped）。根因：`StartService` 成功后丢掉了活跃 Handle，`StopService` 只能按名停（`systemctl --user stop <ref>`），降级模式没有 unit 可停。修法：把 SRCOS 直接启动的子进程 `pid` + `/proc/<pid>/stat` 的 `starttime`（内核时钟节拍，标识「这个 pid 的这一次实例」）写进实例记录，`Local.StopUnit/UnitAlive` 在没有 user systemd 时用它停止/判断存活 —— **且发信号前必须校验 starttime 一致**（pid 会被 OS 复用，裸 pid 不能杀）。副作用收益：`Reconcile` 在降级模式下也能区分活着/已死（修前一律判成 orphaned，于是把正在服务的实例标成 stopped） |

| **`sandbox: none`（降级）+ `type: path` 参数不相容** | 参数值是**沙箱路径**（如 `/data/ref`），而降级模式没有 mount namespace，宿主上没有这个路径 —— 工具直接报 `ls: cannot access '/data/ref'`。工作区/home 在降级时会换成宿主路径（`PathView`），但 storage 路径没有这条映射。**要么用 `sandbox: bwrap` 跑带 storage 的工具，要么先修 `PathView.Env` 把 storage 参数也翻成宿主路径**（2026-09-22 用 MCP 端到端验证时发现，未修） |
| **MCP 端点上 session cookie 不是凭据**（别把浏览器那套搬过来） | `/mcp` 只认 `Authorization: Bearer <agent token>`：程序没有浏览器，混用会让「谁在调用」变得不可审计；工具自建的 UI 想用只读数据，就签发自己的 token（`srcos_read_file` 那套范围是现成的只读后端） |

| **并发第一次上线就暴露了两个真 bug**（2026-09-22，流程并发） | ① `sandbox.BwrapProbe` 的缓存是裸 `done bool`：第二个并发调用者看到 `done=true` 但结果还没写，于是拿到 **空 path + 空 why** → `BuildInner` 造出 `errors.New("")` → 实例记录成 `failed`、error 为空（"state failed"），完全查不出原因。修法：`sync.Once` + 失败时绝不允许空消息（`why` 为空也要给一句）。**教训：空消息的错误是最坏的失败模式** ② 任务当时用 `systemd-run --user --scope` 启动，**scope 的名字是 systemd 生成的**，`systemctl --user stop <我们记的 ref>` 永远 "not loaded"（还被当成成功！），而降级模式下 `processHandle` 又不报 pid → 任务根本停不掉（`flow cancel` 会假装停成功）。修法：`processHandle` 也实现 `PidReporter`，任务记录 pid + starttime，停止统一走「按引用 → 回退到记录里的 pid（校验 starttime）」。**2026-09-24 后任务改用自命名的瞬时 unit（`--unit=<我们起的名字>`），「按名字停」才真正成立**，pid 只留给降级模式 |

| **画布暴露的语义漏洞：连线不等于依赖**（2026-09-22） | 在画布上给一个没有 `depends_on` 的节点连线时，它仍然留在第 1 层 —— 调度器可以先跑它去读一个**还不存在**的上游产物路径。修法：契约加一条规则（`flow-spec` §2.4 规则 5）**连线即依赖**：下游必须直接或间接 `depends_on` 上游，注册期校验；画布在画线时**自动补上** `depends_on`。教训：**UI 是检验契约的探针** —— 手写 YAML 的人不会忘，鼠标连线的人会 |

### 4.3 契约与幂等

| 坑 | 结论 |
|---|---|
| **`.sign` 放工具级 → 不同参数的任务互相误判为"已完成"** | **幂等的单位是「一次任务」不是「一个工具」**，必须用 `$SRCOS_TASK_ID` 分派产物目录与标记 |
| **`xargs -I{} bash -c '{}'` 会二次处理反斜杠** | 把生成好的命令再破坏一层（`hi S001` 变成 `hinS001n`）。正解：`xargs -n 1 bash -c 'script' _` + `$1` + 环境变量 |
| **工具级 `interface.inputs[].default` 不会自动生效** | 必须经 `job.EffectiveParams` 合并后再生成 `SRCOS_PARAM_*`。空字符串视为"用默认值"不覆盖 |
| **用全角 `）` 闭合 `$(` 会让 bash 解析失败** | 全角括号不是括号，`$(...)` 永远不闭合，吞掉整行。写 bash 时注意输入法 |

### 4.4 CLI 与 API

| 坑 | 结论 |
|---|---|
| **Go 的 `flag` 包遇到第一个非 flag 参数就停止解析** | `srcos grant group bio --user alice` 会静默忽略 `--user`。`parseFlagsLoose` 必须逐个位置参数重新 `Parse` |
| **`tool.Interface/Input/Output` 缺 json tag 会让 API 返回 Go 字段名**（`Inputs`/`Name`/`Type`） | 它是公开契约（三处派生共用），必须有 json tag |
| **`fmt.Sscanf` 解析内存字符串会把非法后缀读成合法数字** | 等于配额静默不存在。用 `strconv` 严格解析 |
| **`systemctl --user list-units <name>` 见上** | 重复列在此处因为它是"看起来对了但没做"的典型 |
| **`/api/...` 必须在 mux 里精确注册** | 现有设计用精确匹配以避免遮蔽被代理后端的 `/api/...` 树。新增保留路径要同步 `README.md` 的「保留路径」表 |

### 4.5 文档与流程

| 坑 | 结论 |
|---|---|
| **Python heredoc 里嵌双引号写中文** 很容易触发 `SyntaxError` | 用三引号或写成 JSON 文件再读 |
| **`retain` 工具可能这一轮不可用** | 退路是把记忆直接 append 到 `~/.pi/agent/memory/<sha1(cwd)>/entries.jsonl`（格式见文件内已有条目） |

---

## 5. 关键设计决策速查（一句话版）

完整版见 [`roadmap.md`](roadmap.md) §5（21 条 ADR）。

| ADR | 一句话 |
|---|---|
| 002 | 后端只有 `local` 与 `sge`；`sandbox` 与 `image` 是 backend 的正交属性，不是后端 |
| 003 | `service` 与 `task` 是**同一个 `RunUnit`**，差异只有 `kind` / `restartPolicy` / `ingress` |
| 004 | 契约是 `job.json` + `work.sh`；**目录即队列**，任何语言任何位置都能提交 |
| 005 | 任务粒度 = 一个 `work.sh`；样本级并行归工具（`ata`/`annotask`），资源声明是**聚合需求** |
| 006 | `work.sh` **必须同步阻塞**；`doneWhen` 探针是逃生口；不做 `qstat` 子作业跟踪 |
| 007 | `backend` 由工具声明；`internal.executor: qsubsge` + `backend: sge` 是**非法组合**（SGE 禁嵌套 qsub） |
| 008/018 | `interface` 必须**机器可读**，一份签名派生三个前端（MCP schema / 画布 / 表单） |
| 011/016 | dsh 集成只借设计不借代码；三层策略（默认自带 viewer / 路 A iframe / 路 B 插件），**先 B 后 A**；禁止把 dsh 变成硬依赖 |
| 014 | 不用需要 root 的工具（架构偏好）；`bwrap` 优先，`systemd-run --user` 限资源，`prlimit` 兜底 |
| 015 | HPC 上**实例是 Job 不是 Pod**；资源交给 SGE；闲置回收默认关闭；共享 FS 当控制通道 + `ssh -L` 当数据通道 |
| 017 | 定位：AI 平台的确定性执行后端；**必须**从 `interface` 自动生成 fallback 表单，否则"不管 UI"自相矛盾 |
| 019 | MCP Server 内置网关，第一期 read-only（✅ **已完成**，见 roadmap「MCP 实现契约」）；agent token（✅）；**不做** MCP Client |
| 020 | 存储与路径 provider 化；**闭环：path 可选范围 == 已挂载 storage == `requires_storages`** |
| 021 | **OS 用户 ≠ SRCOS 注册用户**；虚拟 home/workspace 由 SRCOS 构造；隔离靠 mount namespace 而非 UID |
| 016 | dsh 集成三层（路 D 同构地址 / 路 B 协议插件 ✅ / 路 A iframe 未做）；**路 B 注记：provider 注册在客户端侧**，bundle 不需要 dsh 工具链，凭据是 agent token |
| 023 | **画布布局是旁挂文件**（`layout.yaml`，不进契约、不跑校验）；`expose` 由服务端按与校验互补的规则推导，画布只展示答案 |
| 022 | **任务队列的消费者在网关内**；一次提交自动执行一次（认领互斥、`pending` 不算占用、流程节点不走队列）；任务以 systemd **瞬时 unit** 运行，判定由 `ExecStopPost` 写入 `<日志>.verdict`，所以重启后仍有真实退出码 |
| — | **Grant 授权：默认拒绝；只有「允许」没有 deny**（见下文） |

### 授权模型的两条原则（未进 ADR 编号，但与 ADR 同级重要）

1. **默认拒绝** —— 未被任何 grant 提到的工具对非管理员不可见。发布一个工具不应等于发给所有人。
   逃生口是显式的 `default_allow: true` 或工具级 `public: true`。
2. **只有「允许」，没有 `deny`** —— 访问权是所有 grant 的并集，所以"为什么 alice 能用"永远能指到
   某一行某人写下的记录，而不是"因为没有 deny 规则"。这是可审计的前提。

---

## 6. 开发循环与验证

```bash
make build      # 构建到 ./srcos（版本取自最近的 git tag）
make test       # go test ./...
make vet        # go vet ./...
make fmt        # gofmt -w .
make webui      # Phase 5 起；webui/ 未建时只提示，不阻断

# 提交前必须
make vet && make test        # 涉及前端时另加 make webui

# 环境探测（换主机时重跑，结果追加到 docs/environments.md）
bash scripts/probe-env.sh
```

### 一条能跑通全链路的验证脚本

```bash
go build -o /tmp/srcos-dev .
rm -rf /tmp/e2e && mkdir -p /tmp/e2e/config
cp config/grants.example.yaml /tmp/e2e/config/grants.yaml   # 或自己写

# 1. 校验工具契约（含 sandbox 是否真的可用）
/tmp/srcos-dev tool validate --tools-dir srcos-tools
/tmp/srcos-dev tool list     --tools-dir srcos-tools

# 2. 提交 + 执行 task
D="-d /tmp/e2e/config --tools-dir $PWD/srcos-tools"
/tmp/srcos-dev job submit $D -n "demo" --tool hello-fanout \
  --param samples=A1,A2,A3 --output /workspace/out
/tmp/srcos-dev job run  $D --tool hello-fanout
/tmp/srcos-dev job list -d /tmp/e2e/config
/tmp/srcos-dev job logs -d /tmp/e2e/config <instance-id>

# 3. 起一个 service 并确认真的停了
/tmp/srcos-dev svc start $D --tool <service-tool>
curl -s -o /dev/null -w '%{http_code}\n' "http://<endpoint>/"
/tmp/srcos-dev svc stop  $D --tool <service-tool>
systemctl --user is-active srcos-<user>-<tool>-svc    # 应为 inactive

# 4. 授权
/tmp/srcos-dev grant list -d /tmp/e2e/config
/tmp/srcos-dev grant set <tool> -d /tmp/e2e/config --user alice --max-cpu 8

# 5. 网关 + HTTP 面
/tmp/srcos-dev user alice -d /tmp/e2e/config
/tmp/srcos-dev serve -d /tmp/e2e/config --tools-dir $PWD/srcos-tools --host 127.0.0.1 --port 31111
curl -s -c cj -X POST http://127.0.0.1:31111/login -H 'Origin: http://127.0.0.1:31111' \
  --data 'username=alice&password=...'
curl -s -b cj http://127.0.0.1:31111/api/tools
curl -s -b cj 'http://127.0.0.1:31111/api/paths?tool=<tool>&input=<path-input>'

# 6. agent token（程序凭据）——与浏览器 session 完全独立
TOK=$(/tmp/srcos-dev token create -d /tmp/e2e/config --user alice --label annovibe \
      | grep -o 'srcos_[a-z2-7]*\.[A-Za-z0-9_-]*' | head -1)
curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $TOK" \
  http://127.0.0.1:31111/api/tools        # 200
curl -s -o /dev/null -w '%{http_code}\n' -X POST -H "Authorization: Bearer $TOK" \
  -H 'Content-Type: application/json' -d '{"tool":"hello-fanout"}' \
  http://127.0.0.1:31111/api/jobs         # 403：第一期只读（需预留的 submit scope）
ID=$(/tmp/srcos-dev token list -d /tmp/e2e/config | awk 'NR==2{print $1}')
/tmp/srcos-dev token revoke -d /tmp/e2e/config "$ID"
curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $TOK" \
  http://127.0.0.1:31111/api/tools        # 401：撤销立即生效，网关未重启
```

### 验证 MCP（agent 面，一步到位）

```bash
# 造一个带 storage 的工具（sandbox 用 bwrap：降级模式与 type: path 不相容，见 §4.2），
# 签发 token，起网关（--tools-dir 指向含该工具的目录），然后任选一种客户端：
TOKEN=$(./srcos token create -d /tmp/e2e/config --user alice --label mcp \
        | grep -o 'srcos_[a-z2-7]*\.[A-Za-z0-9_-]*' | head -1)

# ① curl（最小）
curl -s -H "Authorization: Bearer $TOKEN" -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' \
  http://127.0.0.1:31111/mcp | python3 -m json.tool | head -20

# ② 官方 Python SDK（真实客户端，验证 initialize 握手与会话）
SRCOS_TOKEN=$TOKEN python3 - <<'PY'
import asyncio, os
from mcp import ClientSession
from mcp.client.streamable_http import streamablehttp_client
async def main():
    h = {"Authorization": "Bearer " + os.environ["SRCOS_TOKEN"]}
    async with streamablehttp_client("http://127.0.0.1:31111/mcp", headers=h) as (r, w, _):
        async with ClientSession(r, w) as s:
            print((await s.initialize()).serverInfo)
            print([t.name for t in (await s.list_tools()).tools])
asyncio.run(main())
PY

# 无 token → 401 + WWW-Authenticate；GET /mcp → 405；通知 → 202 空体；
# 审计行形如：[srcos] mcp alice via token <id> (label) [read] tools/call srcos_list_paths
```

### 在 node01（有 systemd）上验证降级路径

降级模式的代码只在 `useSystemd()` 为假时才会走到。这台机器有 `systemd-run`，所以想验证
「无 user systemd 的登录节点」那条路，就用一个只含所需可执行文件的 PATH 跑 CLI（缺了
`systemd-run` 探测就会失败 → 降级）：

```bash
mkdir -p /tmp/fakebin
for b in bash sh python3 env sleep kill cat printf; do ln -sf "$(command -v $b)" /tmp/fakebin/$b; done
env -i PATH=/tmp/fakebin HOME=/tmp SRCOS_USER=alice SRCOS_TOOLS_DIR=<tools> \
  ./srcos svc start -d /tmp/e2e/config --tools-dir <tools> --tool <service-tool>
# 记录里应出现 pid + pid_start；再另起一个同样 PATH 的进程 svc stop → 进程应真的消失
```

### 清理测试残留（容易漏）

```bash
systemctl --user stop 'srcos-*' && systemctl --user reset-failed 'srcos-*'
lsof -ti:20000-30000 2>/dev/null | xargs -r kill    # 端口池范围
lsof -ti:31111,31112,31113 2>/dev/null | xargs -r kill
pkill -f 'http[.]server 241'                        # 历史遗留：降级模式的 stop 修好后已不再泄漏，留着当保险
ss -tln | awk '$4 ~ /:241[0-9][0-9]$/'              # 应为空（不为空说明有旧进程残留）
```

---

## 7. 代码地图

```
main.go                     CLI 入口与一级子命令分发（serve/user/sso/tool/job/svc/grant/token）
cmd_job.go                  tool / job / svc 子命令 + 通用 flag 解析（job run 也认领任务）
cmd_grant.go                grant 子命令
cmd_token.go                token 子命令（create / list / revoke；`--expires 90d|never|<date>`）
cmd_flow.go                 flow 子命令（list / validate；`--flows-dir`）

internal/resource/           资源协议（纯解析，无 I/O）：地址语法（与 dsh-resource:// 同构）+ viewer 注册表
  ├ address.go               srcos://<provider>/<scope>/<path>[?tool=]（无用户名：用户相对解析）
  └ viewer.go                patterns 认领 + 优先级 + glob 匹配（Match）
internal/config/            用户/状态/路径/存储与授权文件位置；registry 扫描 users/*.yaml
internal/auth/              密码（bcrypt）、TOTP、会话 cookie、SSO、ClientIP/SameOrigin
internal/inspect/           只读答案的唯一实现（REST API / HTML 页面 / MCP 三前端共用）
  ├ inspect.go              工具可见性、storage 闭包、路径浏览（Jail）
  ├ instances.go            实例 / 日志 / 产物（按用户收敛）
  ├ admin.go                管理视图：全部实例 + 资源快照、工具授权状态
  ├ read.go                 srcos_read_file 的读范围与文本、大小约束
  ├ resource.go             srcos:// 的唯一解析入口（复用 sandbox.Spec/Jail；scope 用户相对）
  └ logstream.go            FollowLogs：日志尾部回放 + 跟随 + 终态收尾（半行不当作一行、15s 心跳）
internal/mcp/               MCP Server（Streamable HTTP，无状态 /mcp）
  ├ mcp.go                  JSON-RPC 2.0 + 协议版本协商 + 认证 + 错误分层
  └ tools.go                9 个只读工具 + 3 个写工具（只对有 submit scope 的 token 列出）+ 参数校验
internal/agenttoken/        agent token（程序凭据）：只存 SHA-256、每次校验重读文件、失败关闭
  ├ agenttoken.go           Token/Store/Identity/Scope（read|submit）+ submit_tools + 注册期校验
  └ usage.go                使用时间（data/agent-token-usage.yaml，网关写、按 token 降频）
internal/execute/           写入面唯一实现：Submit / Cancel / RunFlow（校验 + 配额 + 投递）
  ├ execute.go              submit 授权 = scope × submit_tools × Grant；取消幂等；流程逐节点校验白名单
  └ queue.go                任务队列消费者：启动冲刷 / 提交唤醒 / tick；认领、(用户,工具) 串行、全局上限
                            （任务在 runtime 侧跑在 systemd 瞬时 unit 里，判定见 local.go 的 verdict*）
internal/proxy/             httputil.ReverseProxy 的 Rewrite/ModifyResponse 全部逻辑
  └ conns.go                ActiveConns：长连接（WebSocket）计数，供回收判断"在用"
internal/server/            网关 mux、登录/TOTP 页面、工具页面、/assets、代理路由
  └ routes.go               动态路由 + 启动 reconcile / 周期回收 / 每 tick 结算任务 / 未就绪状态页
                            （TaskLoop 跑任务队列；ScanLoop 里的 reconcileTasks 收尾失去等待者的任务）
internal/api/               管理 API（services）+ 工具/存储/任务 API（tools.go, jobs.go）
  ├ admin.go                /api/admin/*（仅管理员）：实例总览/强制停止/日志/授权/组/管理员
  ├ flows.go                /api/admin/flows*：画布的读/写/校验（校验复用 internal/flow）
  ├ resources.go            /api/resources｜raw｜html（srcos:// 的 HTTP 拼写；raw 永不 text/html）
  ├ tokens.go               /api/tokens（自助凭据：只认 session、只给自己签、白名单只收窄）
  ├ logs.go                 /api/jobs/<id>/logs（默认纯文本尾部；?follow=1 为 SSE）
  └ execute.go              POST /api/jobs（run）｜/api/jobs/<id>/cancel｜/api/flows/<id>/run
internal/web/               内嵌模板（Go html 字符串）+ toolpages.go（生成式表单）
  ├ admin.go                /admin 控制台（服务端渲染 + 少量 JS 动作）
  ├ service_status.go       「启动中」进度页 / 失败说明页
  ├ resourcepage.go         /view 的 viewer 页面（文本/Markdown/表格/图片/PDF/HTML/目录 + 根选择页）+ NoticePage
  ├ markdown.go             服务端 Markdown（子集：先转义再自行输出标签，无需额外清洗器）
  ├ tasks.go                /tasks 与 /tasks/<id>（状态/产物/日志面板 + EventSource 客户端）
  ├ tokens.go               /tokens（列表服务端渲染；生成走 /api/tokens，明文只画进 DOM）
  └ templates/srcos-path-picker.js   原语控件（Web Component）

internal/tool/              tool.yaml 类型 + 13 类注册期校验 + input 值校验
  └ schema.go               Interface → JSON Schema（ADR-018 的第一处派生）
internal/job/               job.json 类型 + 对工具的校验 + 目录扫描 + Slug/NewID/Submit + Claim（认领）
internal/grant/             授权策略（默认拒绝 · 组/用户/public/通配 · 配额）+ yaml 往返
internal/storage/           StorageProvider（列表/浏览/Jail 复用/可达性检查）
internal/sandbox/           MountSpec + Jail（symlink 逃逸防护）+ bwrap argv 物化
internal/portpool/          loopback 端口池（真 bind 探测、Reserve 供 reconcile）
internal/runtime/           编排层
  ├ instance.go             Instance 记录（task/service 共用）+ Paths + PathView
  ├ backend.go              Backend / Handle 接口 + Limiter(spawn) + Runner(build)
  ├ local.go                local backend（task=scope，service=unit）+ BuildInner
  ├ proc_unix.go            processAlive + pid starttime（降级模式停止进程的身份校验）
  ├ run.go                  RunTask / StartService / StopService / Reap / Reconcile
  │                         （Reaper.LastActive：由调用方提供「最近一次流量」）
  └ sge/                    SGE backend：qsub 翻译 / qstat -xml 解析 / rendezvous / ssh -L
internal/route/             动态路由表（编排层与代理层唯一的耦合点；ParseTarget 只收环回端点）
integrations/dsh-plugin/    dsh 路 B（`@seqyuan/srcos-dsh`）：srcos 协议 provider + 侧边栏 tab
                            src/{address,config,api,provider}.js 是 Node 可测的纯逻辑；
                            src/client.js 是 dsh 胶水（每个 API 标注了上游源文件）；
                            build.mjs 拼 dsh 的 __ModuleLoader__ 信封（零 dsh 工具链）
webui/                      管理端画布（Vite + React + TS，产物嵌入 internal/web/dist/）
                            拖动用指针捕获（node 上 pointerdown/move/up），坐标去抖 400ms 写 layout；补齐 expose 一键应用
internal/flow/              流程契约：Flow 类型 + DAG（拓扑序/环检测）+ 15 类注册期校验
  └ canvas.go               画布旁挂数据：layout.yaml（ADR-023）+ MissingExpose（与校验互补）
  ├ plan.go                 样本表解析 + 展开成 (节点×样本) 的 job（参数四种来源）
  ├ layout.go               run 目录布局（/flow 内建挂载；路径由 run id 推导，无模板）
  └ record.go               运行记录（flowrun.yaml）+ .sign 逃生口
internal/flowrun/           流程执行器：并发窗口、AND 依赖、when: always、重试+退避、取消、配额、续跑
internal/activity/          write-behind 时间戳日志（token 使用时间 / 服务活跃时间共用）
internal/runtime/usage.go   资源快照（systemd cgroup / /proc）—— UnitSampler 后端接口
internal/rate/              令牌桶限速（登录 + 带宽）

srcos-tools/hello-fanout/   示例工具：tool.yaml + work.sh + workspace-template
config/*.example.yaml       三份契约示例（state / storages / grants）
docs/                       roadmap（ADR+进度）· tool-spec（工具契约）· environments（环境事实）
                            handoff（本文）· tunnel · dsh-demo · reverse-proxy-tls
scripts/probe-env.sh        无 root 环境探测
```

---

## 8. 环境事实（node01）

完整记录见 [`environments.md`](environments.md)。**换主机时重跑 `scripts/probe-env.sh` 并追加。**

- `Ubuntu 24.04` · `kernel 6.8.0-139` · `user seqyuan(uid=1000)` · **bash 工具就跑在这台机器上**
- ✅ **bwrap 可用**（已用 AppArmor 按二进制授权修好）
- ✅ **`systemd-run --user` 完全可用**（`--scope` 与 `--unit` 均可；`CPUQuota`/`MemoryMax`/`TasksMax` 均接受，`Linger=yes`）
- ✅ `runc 1.2.4` / `rootlesskit` / `slirp4netns` 已装（未验证能否无 root 容器化）
- ❌ **无 `apptainer`/`singularity`**（注意 `/usr/games/singularity` 是 pygame 同名游戏）
- ❌ **node01 不是 SGE 登录节点**（无 `qsub`/`qstat`）
- 存储：`/`(ext4 394G, 含 `/home`) · `/work`(ext4 7.2T) · **`/Volumes/data`(ext4 15.5T, 最佳 storage 落点)** · `/Volumes/process`(NVMe)
- ⚠️ **数据盘全是 ext4（非 XFS）** → 不能用 XFS project quota 做配额
- **已在跑**：`dsh`(3080) · `shiny-server`(3838) · `RStudio Server`(8787) · 8080
- 免密 sudo + `docker` 组均可用 → 「不用 root」是**架构偏好**

---

## 9. 待决策

见 [`roadmap.md`](roadmap.md) §8（15 项）。其中与"下一步"直接相关的三条：

| # | 问题 | 倾向 |
|---|---|---|
| 12 | 真正的 SGE 登录节点在哪？ | 阻塞 `backend: sge` 的细节；需要在那台机器上跑 `probe-env.sh` |
| 13 | `runc` + `rootlesskit` 能否无 root 容器化？ | 值得实测（ADR-014 的进阶方案） |
| 14 | Phase 1 首个真实用例用哪个（dsh / shiny / RStudio）？ | 三者都在 node01 上运行，建议直接用现状验证 |

---

## 10. 重开会话时的第一句话建议

```
读 AGENTS.md、docs/roadmap.md、docs/handoff.md，然后从 handoff §3.1 的第 10 项
（Phase 5 剩余前端件：agent token 自助页 / 画布拖拽与 expose 自动推导）
或 §2.3 的持久任务执行器开始。
```

如果要继续做**已规划的**工作，说「继续」+ 指向 `handoff §3.1` 的编号即可。
如果要**换方向**，先说清要改哪一条不变式（`AGENTS.md`）或哪一条 ADR，
因为按仓库约定，架构回退必须先改文档再改代码。

> 上一轮（管理端流程画布）的收尾：`make vet && go test ./... -race` 全绿（前端另跑 `make webui`）；
> 端到端验证见 §2.2 的「流程画布」行（Playwright 驱动真实浏览器）。
