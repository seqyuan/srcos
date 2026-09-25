# SRCOS Roadmap

> **本文档是 SRCOS 开发状态、优先级和明确延期范围的唯一来源。**
> 设计与实施历史散落在对话和草稿中的部分，以本文档为准。
> 文档只记录「为什么这么设计」和「做到哪了」，不替代 README（面向使用者）。

---

## 1. 定位与设计思想

> ### SRCOS 是 AI 平台的确定性执行后端。探索用 AI，执行用 SRCOS。

这条是本项目**全部设计取舍的最终判据**，展开论述见 [`../AGENTS.md`](../AGENTS.md)「定位」章。

**一句话**：SRCOS 把「一台服务器、一个 Web 入口、每人管理自己的转发」升级为
「**一个入口，上架工具，授权使用，人人有隔离工作区，工具被实例化管理**」——
一个面向实验室/HPC 的小型云工具平台。

成本结构上，SRCOS 的赌注是：**一次性 token 投入（把探索出的方法固化成工具）+ 零边际 token 成本（反复执行）**
—— 本质是**把 AI 的探索成果资本化**。因此 SRCOS 与 AI 平台是互补而非竞争关系，
应该主动充当 AI 平台的执行层（MCP 就是对接口，见 ADR-019）。

### 1.1 沿用自原 goprox 的四条不变原则

| 原则 | 含义 |
|---|---|
| **单二进制、零依赖分发** | 拷贝一个文件就能跑，配置落在二进制旁边的 `config/`，可整体备份迁移 |
| **文件系统即数据库** | 声明式 YAML 配置 + 目录扫描，不引入外部数据库 |
| **后端只监听回环** | 网关是唯一入口，这是全部安全模型的基石 |
| **代理层与编排层解耦** | 编排层不碰 HTTP，代理层不碰容器，两者只通过「动态路由表」耦合 |

### 1.2 新增的原则

| 原则 | 含义 |
|---|---|
| **不碰工具 UI** | 工具界面由开发者自建；SRCOS 只提供投递、状态契约与**原语控件**（路径选择器、文件预览） |
| **一份 `interface`，三个前端** | 机器可读的类型签名同时派生 MCP schema / 画布连线 / 用户表单（ADR-018） |
| **能力即 provider** | 凡是有多种来源或实现的能力都抽象成 provider，平台只依赖接口（ADR-020） |
| **不用需要 root 的工具** | 排除 docker daemon / udocker / proot；优先 apptainer、bubblewrap（ADR-014） |
| **一个运行原语** | 常驻服务与一次性任务是同一个 `RunUnit`，差异只有三个字段（ADR-003） |
| **不做 OS 级 UID 隔离** | 所有实例以同一 OS 用户运行；隔离靠 mount namespace + `Jail`（ADR-021） |

### 1.3 参考过的项目

| 项目 | 借了什么 |
|---|---|
| `../ennote`（`ennoworker/internal/workspace/`） | 虚拟挂载三层抽象：挂载表 + `Jail` 路径解析 + 物化器；信任必须存于被信任对象之外 |
| `../annopi` | `task.yml` 模块规范、`pipeline.yml` 的 `tasks`+`dependencies`、`deps` 三层优先级、`annopi install xxx@v1.2.3` 的模块复用 |
| `../annotask`、`../ata` | 样本级并行执行器（工具内部用，SRCOS 不执行）、断点续跑 `.sign` 标记、OOM 自适应重试 |
| `../goqsub` | SGE 提交参数（`-pe smp`、`-l h_vmem`、`-q`、`-p`）的取值参考 |
| `deepseek-harness` | 「资源地址协议 + viewer 注册表 + 认领优先级」的设计、provider 契约形态、bundle/patch 的组合模型；代码无法直接复用（ADR-011），集成策略见 ADR-016 |

---

## 2. 现状

> **重开会话先读 [`handoff.md`](handoff.md)** —— 它是会话交接简报（目标 / 已验证事实 / 下一步 /
> 已踩的坑 / 代码地图 / 环境事实）。本节只记"做到哪了"，不重复那份简报。

### 2.1 已完成

**网关层**（继承自 goprox，功能完整）

- 多用户共享端口、路径前缀隔离 `/proxy/<用户>/<服务路径>/`
- bcrypt 密码 + 可选 TOTP + 登录限速；轻量 SSO（身份头 + HMAC 签名）
- 原生 TLS / 可信反代 / Cloudflare 隧道；HTML `<base>` 注入 + `crypto.randomUUID` polyfill
- WebSocket/SSE 透传、per-service 带宽限制、`backend_path` 三种映射、`default_service` 根路径托管
- 管理 API 的 CSRF 校验、`SafeDialContext` 防 SSRF/自环

**平台层**（本次新增，见 §6 的 Phase 进度）

- 工具契约：`tool.yaml` + 13 类注册期校验 + **机器可读 `interface`**（ADR-008/018）
- 任务契约：`job.json` + **目录即队列** + 对工具的校验（参数子集、资源只能降、`doneWhen`）
- 运行时：`Backend`/`Handle` 抽象；`local`（bwrap 沙箱；**task 与 service 都是 systemd 瞬时 unit**，
  `prlimit` 兜底）与 `sge`（qsub 翻译 / `qstat -xml` 解析 / rendezvous / `ssh -L`）两个 backend
- 生命周期：`RunTask` / `StartService` / `StopService` / `Reconcile` / `Reaper` / **`ReconcileTasks`**
  （每 tick 结算失去等待者的任务；判定由 systemd 的 `ExecStopPost` 落下，见 ADR-022）
- **任务队列消费者**（ADR-022）：提交即自动执行（启动冲刷 + 提交唤醒 + tick；认领互斥）
- **写入面**（ADR-019 第二期）：`internal/execute` 是 `submit` / `cancel` / `run_flow` 的唯一实现，
  REST 与 MCP 共用；授权 = scope × `submit_tools` × Grant 双维度 + 配额
- 存储：`StorageProvider`（ADR-020 闭环：path 范围 == 已挂载 storage == `requires_storages`）
- 路由与端口：动态路由表（非回环拒绝、归属栅栏）+ loopback 端口池（真 bind 探测）
- 授权：`Grant`（**默认拒绝**、只有「允许」没有 deny、组/用户/public/通配、**聚合配额**）
- HTTP 面：`/api/tools`、`/api/tools/<id>`、`/api/paths`、`/api/jobs`（+ `/cancel`、`<id>/logs`）、
  `/api/flows/<id>/run`、`/api/resources`（+ `raw` / `html`）、`/api/tokens`、`/tools`、`/view`、
  `/tasks`、`/tokens`、`/assets/*`
- 界面：**生成式表单**（从 `interface` 派生）+ **`srcos-path-picker` 原语控件**
- **资源协议与 viewer**（ADR-011/016）：`srcos://<provider>/<scope>/<path>`（与
  `dsh-resource://` 同构）+ viewer 注册表 + `/view`（文本/Markdown/表格/图片/PDF/sandbox HTML/目录）
- **任务与日志**：`/tasks` + `/tasks/<id>`（产物直链 `/view`）+ `/api/jobs/<id>/logs`（SSE 实时流）
- **agent token 自助页**：`/tokens`（只给自己签、白名单只能收窄、**只认浏览器 session**）
- CLI：`tool` / `job` / `svc` / `grant` / `token` / `flow`（list/validate/run/resume/status）/
  `serve` / `user` / `passwd` / `del` / `2fa-reset` / `sso`
- **MCP Server**（`/mcp`，agent token 认证）：9 个只读工具 + 3 个写工具（`submit` / `cancel` /
  `run_flow`）+ `interface → JSON Schema` 派生
- **流程画布**（Phase 5）：`/admin/flows/<id>/edit`（Vite+React 只在这一页加载）+ **拖拽摆放**
  （旁挂 `layout.yaml`）+ **`expose` 一键补齐**（ADR-023）
- **审计流**（ADR-024）：`internal/audit`（结构化 JSONL、只追加、按天轮转、参数脱敏）+
  `srcos audit tail|list` + 管理端 `/admin/audit`（+ `/api/admin/audit`）；埋点覆盖写入面
  （submit/cancel/run_flow）、拒绝事件（CSRF/未认证/只读试写）、配置变更（grant/group/admins/token，API 与 CLI 两条门）、生命周期（done/settled/reaped/stopped/orphaned/adopted）+ `srcos audit prune`（保留策略，显式）+ hash chain（`srcos audit verify`，每文件）+ **外发**（`state.yaml` 的 `audit.forward_url`，信任锚）
- **工具访问申请/审批**（B3）：`grant.requestable` 显式开关 + `internal/accessrequest`
  （`data/requests/*.yaml`）+ `/api/requests`（用户）+ `/api/admin/requests`（approve/deny）
  + `/requests` 页 + `/admin` 待审区；批准 = `AddUserToGrant` + 落盘，审计 `request.*`
- **托管 agent 凭据**（ADR-025）：工具 `agent: {mcp, tools}` 声明 → 实例启动签发 `data/agent-tokens.yaml`
  → `$HOME/.srcos/agent-token`（0600）→ 停止撤销 + 启动清扫；认证走 `agenttoken.Chain`（REST 与 MCP 一致）

**工程基线**

- 重命名为 srcos（`go.mod` = `github.com/seqyuan/srcos`）；删 `site/` 文档站；建立 git 仓库
- 建立 `AGENTS.md`（不变式与定位）+ **25 条 ADR** + `docs/tool-spec.md`（工具契约冻结）+
  `docs/flow-spec.md`（流程契约冻结）
- `scripts/probe-env.sh`（无 root 环境探测）+ `docs/environments.md`（node01 实测记录）
- **25 个包 / 143 个 Go 文件 / 约 4.6 万行 / 65 个测试文件 / 514 个测试函数**，
  `go vet` + `go test ./... -race` 全绿；`webui/`（889 行 TS/TSX）独立构建与测试

### 2.2 尚未实现（**不要误以为有**）

- ~~流程编排（`Flow` / `FlowRun`）~~ —— ✅ 完成（Phase 4，含并发/重试/取消/续跑与画布）
- ~~MCP Server（`/mcp`，read-only）~~ —— ✅ 完成（Phase 3.5）
- ~~**代理层接入动态路由表**~~ —— ✅ 完成（2026-09-22）：网关从实例记录重建路由表，
  `/proxy/<user>/<tool>/` 可达、实例优先于卡片、裸路径（SPA）同样回投到实例
- ~~授权变更热加载（改 `grants.yaml` 需重启）~~ —— ✅ 完成（2026-09-22：10 秒内自动生效）
- ~~审计流（散落的日志行，没有结构化落盘与配置变更审计）~~ —— ✅ **第一期完成**（2026-09-26，ADR-024，scope A）
- ~~审计流第二期~~ —— ✅ **完成**（2026-09-26）：管理端查询页（`/admin/audit` + `/api/admin/audit`）+ 生命周期跃迁审计 + 保留策略（`srcos audit prune`）+ hash chain（`srcos audit verify`）；仅真实签名未做
- ~~工具访问申请/审批流~~ —— ✅ 完成（2026-09-26，B3；设计见 `docs/plans/2026-09-26-tool-access-request-design.md`）
- ~~托管 agent 的实例身份（A1）~~ —— ✅ 完成（2026-09-26，ADR-025：工具声明 `agent:` → 启动签发运行态令牌 → home 0600 文件 → 随实例撤销）
- ~~agent 提交幂等键（A2）~~ —— ✅ 完成（2026-09-26，ADR-019 第二期契约）
- `storages` 的 rw 配额
- `apptainer` sandbox
- SGE 只在 fake runner 上测过，**从未在真登录节点运行**

### 2.3 下一步

按 [`handoff.md`](handoff.md) §3.1 的顺序：

| # | 做什么 | 需要什么 |
|---|---|---|
| 12 | ~~**审计流**~~ —— ✅ **第一期完成**（2026-09-26，ADR-024，范围 A）；第二期（防篡改/管理页/生命周期/保留）见 handoff §3.1 #12 | — |
| 13 | **Phase 6 SGE**：真登录节点上 `probe-env.sh`，再接真集群 | 真登录节点主机名（§8 #12） |
| 14 | 下一步候选：agent 端到端 runner 身份（A1）、agent 提交幂等键（A2）；`storages.yaml` 热加载评估后不做 | — |

---

## 3. 架构

### 3.1 分层

```
┌─ 控制面（新增）──────────────────────────────────────────────┐
│ 认证/授权（Grant：用户/组 × 工具）                            │
│ 工具注册（Tool：镜像/命令/资源/interface）                     │
│ 流程注册（Flow：nodes + bindings + expose）                   │
│ 实例编排 Controller（创建/回收/自愈）                          │
│ 动态路由表（tool/instance → 127.0.0.1:port）                   │
└──────────────────────────────────────────────────────────────┘
                        ↓ 只通过动态路由表耦合
┌─ 数据面（现有，几乎不改）─────────────────────────────────────┐
│ 反向代理 httputil.ReverseProxy                                │
│ <base> 注入 · WebSocket · 带宽限制 · SSO 头 · Location 改写    │
└──────────────────────────────────────────────────────────────┘
                        ↓
┌─ 运行层 Runtime（新增，两个 backend）─────────────────────────┐
│ backend=local（登录节点/单机）  │  backend=sge（qsub 集群）    │
│ sandbox: none | bwrap | apptainer（属性，不是后端）            │
└──────────────────────────────────────────────────────────────┘
                        ↓
┌─ 实例与存储──────────────────────────────────────────────────┐
│ 实例：127.0.0.1:<port>（service）/ 无端口（task）              │
│ ws/<user>/<tool>/  ·  img/*.sif（必须在共享盘）                │
└──────────────────────────────────────────────────────────────┘
```

### 3.2 现有核心端口转发原理（理解全文的前提）

单进程 Go 反向代理，**路由靠 URL 路径前缀而不是端口**：

| 环节 | 实现 |
|---|---|
| 入口 | `net/http` + `httputil.ReverseProxy`，共享一个 `Transport`（连接池复用） |
| 路由 | `/proxy/<用户>/<服务路径>/<剩余>` → 前缀匹配 → 后端 `host:port` |
| `Rewrite` 钩子 | 改写 `URL.Path`（拼 `backend_path`）、`Host`、`Origin`（同源改写以过后端信任栅栏）、`X-Forwarded-*`；剥网关 cookie；注入 SSO 身份头 + HMAC |
| `ModifyResponse` 钩子 | ① `Location` 改回 `/proxy/...` 前缀；② HTML 注入 `<base href>` + polyfill（**"后端零改动挂到子路径"的关键**）；③ 按 `bwlimit` 套上下行限速 |
| WebSocket | 101 直接透传原始 `ReadWriteCloser`，不包装 body（否则升级被破坏） |

### 3.3 工具实例化（规划）

- 实例只监听 `127.0.0.1:<随机端口>` → **完全落在现有安全模型里**，代理层白送 `<base>` 注入/WS/限速/SSO
- 现有路由来自静态 `config/users/<user>.yaml`；新增一层 `instance registry`（内存 map + 持久化），代理前先查
- 未就绪的实例返回「启动中」进度页（而非现在的 502），就绪后跳转
- 端口池 `127.0.0.1:20000-30000`；**启动时 reconcile 是必须的**（清理孤儿实例与端口）

---

## 4. 核心概念

### 4.1 Tool（小工具 / 模块）

注册单元，对齐 annopi 的 `task.yml`，但 `interface` 升级为**机器可读**：

```yaml
# srcos-tools/cellranger/tool.yaml
schemaVersion: 1
id: cellranger
version: 1.2.3
kind: task                  # task | service
backend: sge                # local | sge（工具声明，管理端可覆盖）
sandbox: apptainer          # none | bwrap | apptainer
image: /share/srcos/img/cellranger-7.2.0.sif
env: []                     # sandbox=none 时的环境准备，如 ["module load cellranger/7.2.0"]
entry: work.sh              # SRCOS 投递的入口

interface:                  # ← 机器可读（不是 annopi 的"仅文档"）
  inputs:
    - { name: fastq_dir, type: directory, required: true,
        files: [R1.fastq.gz, R2.fastq.gz] }
    - { name: sample_id, type: string, required: true }
    - { name: ref, type: path, from: cluster-share, select: directory,
        default: /data/share/ref/GRCh38 }
  outputs:
    - { name: outs, type: directory,
        provides: [filtered_feature_bc_matrix.h5, metrics_summary.csv] }

requires_storages: [cluster-share]   # 声明需要哪些 storage，决定 type: path 参数的可选范围
resources: { cpu: 8, memory: "32Gi", walltime: "4:00:00", queue: sci.q }
internal: { executor: local, parallelism: 5 }   # 供校验，SRCOS 不执行
```

`interface` 的 `type` 全集一次性定全（Phase 1 的 `docs/tool-spec.md` 冻结）：

| `type` | 用户侧控件 | MCP schema | 说明 |
|---|---|---|---|
| `string` | 文本框 | `string` | |
| `int` / `float` | 数字框 | `integer` / `number` | 可带 `min` / `max` |
| `bool` | 开关 | `boolean` | |
| `enum` | 下拉框 | `string` + `enum` | 需 `values: [...]` |
| `file` | 文件输入 | `string` | 绑定的存储路径 |
| `directory` / `dirpath` | 目录输入 | `string` | 绑定的存储路径 |
| **`path`** | **路径选择器** | `string` | **新增**：必须带 `from: <storage-id>`，受 Jail 约束（ADR-020） |

### 4.2 RunUnit（运行原语）

`service` 与 `task` 是**同一个原语**，差异只有三个字段：

| | `kind: service` | `kind: task` |
|---|---|---|
| k8s 类比（仅作理解，不用 k8s） | Deployment + Service | Job |
| 生命周期 | 无限，崩溃自动重启 | 跑完即止，不重启 |
| ingress | 有（网关代理端口） | 无（只有退出码） |
| **提交路径 / 资源限额 / 挂载隔离** | **完全相同** | **完全相同** |
| SGE 映射 | 长 `h_rt` 作业 + `ssh -L` 隧道 | 短 `h_rt` 作业，无隧道 |

`service` 还分两种部署位置：`node: login`（登录节点常驻，如 annovibe）与 `node: compute`（投递执行）。

### 4.3 Flow（云流程）

管理端编排产物，**只引用已注册工具，不写代码**：

```yaml
id: scrna_basic
version: 0.1.0
nodes:
  - { id: count,  tool: cellranger@1.2.3 }
  - { id: qc,     tool: sc_qc@0.3,      depends_on: [count] }
  - { id: report, tool: qc_report@1.0,  depends_on: [qc], when: always }
bindings:                                  # 画布上的连线（output → input）
  - { from: count.outputs.outs, to: qc.inputs.input_dir }
  - { from: qc.outputs.clean,   to: report.inputs.indir }
expose:                                    # 暴露给用户填的 input（+ 样本表来源）
  - { node: count, input: sample_id, from: sample.sample_id }
  - { node: count, input: fastq_dir, from: sample.fastq }
```

### 4.4 契约：`job.json` + `work.sh`

**SRCOS 不碰工具 UI。** 平台向工具 UI 进程注入环境变量：

```bash
SRCOS_JOB_DIR=/run/srcos/jobs/<user>/<instance>   # 提交落盘目录（目录即队列）
SRCOS_USER / SRCOS_TOOL / SRCOS_INSTANCE
SRCOS_WORKSPACE=/data/ws/alice/shiny-qc
SRCOS_API=http://127.0.0.1:30152/api              # 可选 REST 通道
```

工具 UI 往 `$SRCOS_JOB_DIR/<job-name>/` 写 `job.json` + `work.sh`，或调等价 CLI：

```bash
srcos job submit -n "cellranger S001" --cpu 8 --mem 32G --time 4:00:00 work.sh
```

```json
{
  "schemaVersion": 1,
  "name": "cellranger count S001",
  "command": ["bash", "work.sh"],
  "resources": { "cpu": 8, "memory": "32Gi", "walltime": "4:00:00", "gpu": 0 },
  "mounts": [{ "virtual": "/ref", "host": "/share/ref/GRCh38", "mode": "ro" }],
  "outputs": ["/workspace/out/S001"],
  "tags": { "sample": "S001", "step": "count" },
  "doneWhen": null
}
```

**`work.sh` 六条硬规范**：

| # | 规则 | 为什么 |
|---|---|---|
| 1 | **幂等**：先查输出/`.sign` 存在则退出 | 平台会重试 |
| 2 | **参数化**：可变值从 env 或 `$1` 取，不硬编码路径 | 跨环境复用 |
| 3 | **只写 `/workspace`** | 路径隔离与配额 |
| 4 | **退出码即状态**：0 成功，非 0 失败 | 平台据此重试/标记 |
| 5 | **日志走 stdout/stderr** | 平台自动收集，支持断线重看 |
| 6 | **必须同步阻塞**到所有实际工作完成（不要 `&` / `nohup`） | 否则平台误判"秒完"（见 ADR-006） |

### 4.5 存储与路径（Storage / Path）

`StorageProvider` 是 SRCOS 第一个正式 provider 面：管理端声明一次，工具声明需求，用户只能在交集里选路径。

```yaml
# config/storages.yaml —— 管理端声明
storages:
  - id: cluster-share
    name: "集群共享盘"
    type: posix                    # posix（第一期）| s3（留字段口子）
    host_root: /share              # 宿主真实根 → Jail 边界 + bind mount 源
    sandbox_path: /data/share      # 沙箱内可见路径 → path 参数的表述空间
    mode: ro                       # ro | rw
  - id: project-data
    name: "项目数据"
    type: posix
    host_root: "/share/projects/{grant}"
    sandbox_path: /data/project
    mode: rw
```

闭环约束（ADR-020，避免「UI 里选了、沙箱里看不到」）：

```
管理端声明 storages
  → 工具声明 requires_storages
  → 实例化时按需挂进 MountSpec
  → 用户 type: path 参数只能选已挂载的 storage（Jail 校验）
```

`/api/paths` 返回**沙箱路径**（可直接传给工具）；SRCOS 内部用 `Jail` 双向映射：
`ResolveExisting(sandboxPath) → hostPath` 读文件，`DisplayPath(hostPath) → sandboxPath` 返回 UI。
**这是移植 `Jail` 的第三个收益**（前两个：虚拟挂载、文件预览越权校验）。

工具 UI 接入方式两种：用 SRCOS 自动生成的表单（`type: path` 自动渲染成带浏览按钮的输入框），
或自建 UI 里放一行 `<srcos-path-picker storage="cluster-share">`（Web Component）/ 直接调 `GET /api/paths`。

> **边界澄清（重要）**：路径选择器是**平台提供的原语控件**（像 `<input type="file">`），不是「SRCOS 管业务 UI」。
> 这个边界不写清楚，「不碰工具 UI」会自相矛盾。

### 4.6 用户模型：OS 用户 ≠ SRCOS 注册用户

| | **OS 用户** | **SRCOS 注册用户** |
|---|---|---|
| 是什么 | 启动 `srcos serve` 的 Linux 账号 | `config/users/<name>.yaml` 里的一行 |
| 存在于系统 | 是，真实 `/etc/passwd` 条目 | **否，纯虚拟** |
| home | 真实（如 `/home/seqyuan`） | **虚拟，由 SRCOS 构造** |
| 权限来源 | 文件系统权限 + `config/` 属主 | SRCOS 认证 + `Grant` 授权 |

SRCOS 注册用户的运行时视图全部由 SRCOS 构造：

| 沙箱内路径 | 宿主路径 | 模式 | 来源 |
|---|---|---|---|
| `/workspace` | `data/ws/<user>/<tool>/` | rw | **内建**，每实例自动挂载 |
| `/home/<user>` | `data/homes/<user>/` | rw | **内建**，每实例自动挂载 |
| 工具声明的 storage | 见 `storages.yaml` | ro/rw | **声明式**，`requires_storages` 决定 |

- 沙箱内 `$HOME` = **虚拟 home**，所以 `.cache` / `.condarc` / `.config` / `.jupyter` 天然隔离，
  既不污染 OS 用户的真实 home，也不会在并发实例间互踩。
- **环境 vs 数据要分开**：conda env / `module` / `.sif` 属于**环境**（宿主真实路径，ro 挂载，
  管理员在 `tool.yaml` 里写）；共享参考数据与项目目录属于**数据 storage**（`storages.yaml` 声明）。

---

## 5. 关键决策记录（ADR）

### ADR-001：重命名为 srcos，不保留旧名兼容
- **背景**：项目从一个多用户反代网关演进为云工具平台，原名 goprox 不再达意。
- **决策**：`go.mod`、CLI、日志前缀、配置文件名、cookie 名（`srcos_session` / `srcos_2fa` / `srcos_route*`）、SSO 签名前缀（`srcos-sso:v1:`）全部改名，**不做兼容垫片**。
- **理由**：开发阶段、未上线。已对接 SSO 的后端需要同步改签名常量。

### ADR-002：后端只有 `local` 与 `sge`，不使用 k8s；隔离是属性不是后端
- **背景**：最初设想用 k8s 的 Pod/Deployment/Job 语义。
- **决策**：后端只有单机（`local`）与 qsub SGE 集群（`sge`）两种；`sandbox: none | bwrap | apptainer` 与 `image` 是 backend 上的正交属性。
- **理由**：部署环境是实验室单机或 HPC 登录节点，引入 k8s 控制面与「单二进制零依赖」定位冲突，且运维成本不成比例。抽象掉后 Controller 只剩两条提交路径。

### ADR-003：`service` 与 `task` 统一为 `RunUnit` 原语
- **背景**：曾认为「常驻服务如何与 Job 结合」是难点。
- **决策**：不结合 —— 它们是同一个原语的两组参数，差异只有 `kind` / `restartPolicy` / `ingress`；提交路径、资源限额、挂载隔离完全相同。`ServiceInstance` 与 `TaskInstance` 共用一张表。
- **理由**：k8s 的 `Deployment` 与 `Job` 共享 `PodTemplateSpec`，只是控制器不同。统一后 Controller 只有一份代码，且用户侧任务列表天然成为实例表的一个视图。

### ADR-004：SRCOS 不碰工具 UI，契约是 `job.json` + `work.sh`
- **决策**：工具界面由开发者自建；SRCOS 只提供「落盘目录 + 环境变量 + 等价 CLI」三件套，通过扫描目录发现提交。
- **理由**：① 工具 UI 的形态差异极大（shiny/python/R），平台化会拖死进度；② **目录即队列**——任何语言、任何位置（含计算节点）都能提交，SRCOS 重启不丢任务，天然幂等可重放；③ 共享文件系统在 HPC 上天然是登录节点与计算节点之间的消息总线，不需要 agent 或长连接。

### ADR-005：任务粒度 = 一个 `work.sh`；样本级并行归工具
- **决策**：一个工具的一次运行 = 一个 `work.sh` = 一个 TaskInstance。工具内部用 `ata`（本地并行）或 `annotask qsubsge`（SGE 并行）处理 N 个样本，SRCOS 不感知、不执行。**节点的资源声明是聚合需求**（如 `ata -t 5` × 每样本 4 核 → `cpu: 20`）。
- **理由**：分层清晰——SRCOS 管工具级编排，工具管样本级并行。避免 SRCOs 重新实现 annotask 已经做透的样本扇出、断点续跑、OOM 自适应重试。

### ADR-006：`work.sh` 必须同步阻塞；`doneWhen` 探针作为逃生口；不做子作业上报
- **背景**：`annotask qsubsge` 型 `work.sh` 是**提交器**——投递完立刻退出 0，但子作业仍在跑，SRCOS 会误判成功。
- **决策**：
  - **默认（规范硬性）**：`work.sh` 阻塞到所有实际工作完成（`annotask` 后轮询 `.sign`/`qstat`）。
  - **可选逃生口**：`job.json` 声明 `doneWhen`（如 `{type: file_exists, path: .../_SUCCESS}`），SRCOS 进入 `submitted` 中间态并轮询真实完成，配 `doneWhenTimeout`。
  - **明确不做**：SRCOS 解析 `qstat` 跟踪任意子作业（方案 C），除非确认大量工具无法阻塞。
- **理由**：同步契约语义最清晰、实现最少；`doneWhen` 覆盖无法阻塞的少数情况，代价只是一个中间态。

### ADR-007：`backend` 由工具声明而非编排时决定；`executor` 与 `backend` 必须交叉校验
- **决策**：工具在 `tool.yaml` 里声明 `backend`；管理端编排时可覆盖，但默认继承。
- **硬约束**：`internal.executor: qsubsge` + `backend: sge` → **注册时直接报错**。
- **理由**：① 工具最清楚自己内部怎么跑；② 大多数 SGE 站点禁止计算节点上嵌套 `qsub`，所以「`annotask qsubsge` 型工具必须在登录节点当提交器」不是风格偏好而是唯一可行组合。

| 工具内部 executor | 应声明的 backend | `work.sh` 跑在 | 资源请求 |
|---|---|---|---|
| `local`（ata） | `sge` | 计算节点 | `-pe smp 20 -l h_vmem=40G` |
| `qsubsge`（annotask） | `local` | 登录节点（提交器） | 1 核 / 少量内存 |
| 无并行单命令 | 任意 | 任意 | 单样本需求 |

### ADR-008：工具的 `interface` 必须机器可读
- **背景**：annopi 的 `interface` 节在文档里写明「仅作为文档」。
- **决策**：SRCOS 把它升级为**机器可读的类型签名**（`inputs[]` / `outputs[]`，含 `type` / `required` / `files` / `provides`）。
- **理由**：管理端要用鼠标连线，没有类型信息就无法校验兼容性、无法自动补全、无法自动推导 `expose` 列表。核心抽象是「**小工具 = 有类型签名的黑盒函数，流程 = 函数组合**」。

### ADR-009：流程编排在管理端，只引用已注册小工具
- **决策**：管理端提供画布，节点来自工具库，边 = 依赖 + `output → input` 绑定。管理员不写命令、不写代码。
- **理由**：把「定义」（工具开发者）与「组合」（流程管理员）与「使用」（用户填参数 + 样本表）三个角色分开；用户侧只需要渲染 `expose` 字段 + 样本表。
- **副产品**：既然 Flow 也有 `inputs`/`outputs`，它天然可被注册成工具（即子流程）。UI 不提供入口，但数据模型不需要为此增加任何字段。

### ADR-010：流程 schema 对齐 annopi，但不依赖 annopi 软件
- **决策**：`tool.yaml` 对齐 annopi 的 `task.yml`（含 `deps` 三层优先级、`resources`、`annotask` 节），`Flow` 对齐 `pipeline.yml` 的 `tasks`+`dependencies`。执行默认用 SRCOS 自己的最小 DAG；同时**把 annopi 本身注册成一个普通工具模块**作为逃生口。
- **理由**：① 不发明第二套规范，否则已有的 cellranger/rnaseq 模块要重写；② SRCOS 是 Go，没必要为一个可选能力引入 Python 运行时；③ 「元工具降级为模块」是这套原语的免费收益，保持了单一抽象。

### ADR-011：文件预览只借 dsh 的设计，不借代码
- **背景**：`deepseek-harness` 的「文件预览」实际是一套**资源地址协议 + viewer 注册表**（`dsh-resource://<protocol>/<scope>/<path>`，viewer 用 glob 认领地址，`open()` 返回帧流）。最初的设想是直接引入 dsh 的预览插件。
- **决定性证据（为什么不能直接 import）**：`packages/client/ui-sidebar-textpreview/package.json`：
  ```json
  "dsh": { "client": { "inject": [
      "@deepseek-ai/dsh-api-workspace-files",
      "@deepseek-ai/dsh-client-ui-sidebar-right",
      "@deepseek-ai/dsh-client-ui-session",
      "@deepseek-ai/dsh-api-remotes" ], "platform": "web" } },
  "peerDependencies": { "@deepseek-ai/cordis": "workspace:^" },
  "dependencies": { "react": "^18.2.0", "react-dom": "^18.2.0" }
  ```
  它**不是无状态组件**，而是 Cordis 插件，硬依赖：① Cordis 的 `ctx` + 依赖注入；② **session 模型**
  （文件树的根是 `useSessions().byId[sessionId].cwd`）；③ sidebar 的 tab 容器；④ Remote 传输层。
  且 `ui-renderer` 要求 React / ReactDOM / Cordis / ui-slots 保持**同一个浏览器实例**。
  → 在 SRCOS 前端里跑它，等于在 SRCOS 里重建 dsh client shell 并伪造 session，即「SRCOS 变成 dsh」，
  与 ADR-012 正面冲突。**一句话：dsh 的 viewer 不是库，是寄生在 dsh 运行时上的插件。**
- **决策**：**SRCOS 自己实现 viewer**（Phase 5），但地址语法**刻意与 `dsh-resource://` 同构**
  （含 `patterns` 认领规则照抄），使将来任何方向的适配器都退化成字符串重写。
- **dsh 本身的集成另立 ADR-016**（三层策略），与本条不冲突。
- **安全约束**：**HTML 预览必须 sandbox iframe + 独立 origin**。SRCOS 的 `README` 已说明代理后端与网关同源、同源脚本可调管理 API；同源渲染用户上传的 HTML 会直接放大这个风险。

### ADR-012：前端折中 —— Go 模板外壳 + 独立 Vite 包，`embed.FS` 打进同一二进制
- **决策**：dashboard/login/管理端框架保持 Go 内嵌模板；「文件预览 + 流程 DAG 编辑器 + 任务列表」做成独立 Vite 工程（`webui/`），构建产物输出到 `internal/web/dist/` 由 `//go:embed` 嵌入，挂载在 `/ui/`。
- **理由**：① 文件预览与画布用原生 JS 写会失控；② 但整体 React 化会打破「单二进制零依赖」；③ 折中后前端可独立演进，将来整体 React 化也不影响 Go 侧。
- **约束**：管理端 DAG 编辑器只在 `/admin/flows/:id/edit` 加载 React，其余页面仍是 Go 模板，避免网关被前端框架绑架。

### ADR-013：虚拟挂载借用 ennote 的三层抽象
- **决策**：移植 `ennoworker/internal/workspace/paths.go` 的 `Jail`（虚拟→宿主映射、最长前缀 + **段边界**校验、`EvalSymlinks` 规范化后再验、写路径逐级回溯、反向 `DisplayPath`），把硬编码挂载表升级为声明式 `MountSpec` 列表，物化交给 backend（`bwrap --bind` / `apptainer -B` / `systemd BindPaths=`）。
- **理由**：① `Jail` 解决的是文件系统层的越权（HTTP 层的 `/proxy/<user>/` 只隔离了 URL）；② 「一次声明、多后端物化」让同一份工具定义在 local/sge × bwrap/apptainer 下都能落地。
- **不借用**：bwrap 的硬编码参数、以及「bwrap 能管资源」的隐含假设——bwrap 不做 cgroup 资源限制。

### ADR-014：不用需要 root 的工具
- **决策**：排除 docker daemon、udocker/proot。隔离方案优先级：**apptainer/singularity**（HPC 事实标准，SIF 镜像 + `-B` + `--containall`）→ **bubblewrap**（无镜像、无镜像需求时的降级）。
- **理由**：部署在无 sudo 的登录节点。`proot` 是 ptrace 模拟、不是安全边界（`ennote` 的设计文档已明确此判断），不可退回去当隔离手段。
- **✅ 前提已确认（2026-09-22）**：这是**架构偏好**而非环境限制 —— node01 上免密 sudo 与 `docker` 组均可用，但选择不用。
  理由是可移植性 / 分发性 / 避免特权守护进程，以及真正的 SGE 登录节点（无 sudo）能同样工作。
  **推论：docker 最多作为可选 backend，不得成为 `local` 路径的必需项。**
- **✅ bwrap 可用性已解决（2026-09-22）**：Ubuntu 24.04 的 `apparmor_restrict_unprivileged_userns=1`
  会拦未带 setuid 的 bwrap；采用**按二进制单独授权**的 AppArmor profile 解决（与 Ubuntu 自带的
  `lxc-usernsexec` / `podman` / `runc` 同机制），**不全局关闭 sysctl**。node01 已修复并验证通过。
  重跑 `scripts/probe-env.sh` 可确认；探测记录见 [`environments.md`](environments.md)。
- **资源限制降级路径**（ADR 的实现细节）：`systemd-run --user --unit`（可用时首选；2026-09-24 前任务用 `--scope`，见 ADR-022）→ `prlimit`
  （`RLIMIT_AS`/`RLIMIT_CPU`/`RLIMIT_NPROC`，子进程继承）+ 轮询 RSS 超限 kill 整个进程组。
  node01 上 `systemd-run --user` **完全可用**（`CPUQuota`/`MemoryMax`/`TasksMax` 均被接受，`Linger=yes`），
  所以首选路径成立，`prlimit` 只作保险。

### ADR-015：HPC 上「实例是 Job 不是 Pod」
- **决策**：SGE 驱动的生命周期语义单独定义——资源限制交给 SGE（`-pe smp` / `-l h_vmem`），生命周期上限用 `-l h_rt`，停止用 `qdel`，**空闲回收默认关闭**（排队代价高），改为到期前预警 + `qalter` 续期。
- **必须处理**：SGE 会 `SIGSTOP` 挂起或 requeue 被抢占的作业，网关侧探活会失败——必须区分「作业被挂起（`qstat` 状态 `S`）」与「进程真的死了」，否则会误判 Failed 并疯狂重投。
- **网络接入**：**共享文件系统 rendezvous 文件当控制通道，SSH 本地转发当数据通道**——作业把 `node + port` 写进 `$HOME/.srcos/instances/<id>/endpoint`，登录节点读到后 `ssh -L <localport>:127.0.0.1:<toolport> <node>`，再注册进路由表。优先直连（网络允许时），`ssh -R` 反向隧道作兜底。
- **实现细节**：SGE 驱动用 `qsub`/`qstat`/`qdel` **命令行**而非 DRMAA（避免 CGO + `libdrmaa` 依赖），`qstat -xml` 解析比文本格式稳定。
- **合规提醒**：HPC 登录节点通常禁止跑长驻重负载进程。`local` 驱动应只用于轻量工具或管理员明确的例外，重工具一律走 `sge`。

### ADR-016：dsh 只保留「本地优先应用被代理」这一条已验证用法

- **背景**：dsh（DeepSeek Harness）是本项目最早的真实使用场景之一 —— 它是典型的本地优先应用
  （只监听回环、带 Host/Origin 信任栅栏、前端依赖安全上下文 API），经网关代理即可在局域网使用，
  `docs/dsh-demo.md` 有完整实测记录。此后探索过把 SRCOS 注册成 dsh 的资源协议（「路 B」：
  `integrations/dsh-plugin/` 的 `@seqyuan/srcos-dsh`）。
- **决策（路 B 已移除，2026-09-26）**：删除该插件。三条理由：
  1. **从未验证** —— 没有装进任何 dsh profile，全部测试都用替身，所以「dsh 真的加载它」不成立；
  2. **逆向 developer preview** —— dsh 明示会有破坏性变更，插件依赖的每个 API 都是读它当时源码得来的，
     是高度脆弱的依赖；把它挂在路线图里等于把维护成本押在一个会动的靶子上；
  3. **价值与自带 viewer 重叠** —— SRCOS 已有 `/view`（Phase 5），增量只有「不离开 agent 工作台」。
  代码可从 git 历史 `93351fc` 恢复；若将来出现真实的 dsh 互操作需求，应以**锁定版本 + 真实验证**重建，
  而不是让一个未验证的半成品挂在文档里冒充能力。
- **保留**：
  - **`srcos://` 地址语法与 `dsh-resource://` 同构**（原「路 D」）—— 这是**协议特性**，写在
    `internal/resource` 里（纯解析，无 dsh 依赖），对脚本与工具自建 UI 同样有用。
  - **dsh 作为本地优先应用被代理**（服务卡片）—— 已实测，是网关能力的真实证据；
    同样的配置思路适用于 Jupyter / code-server 等只认回环的应用。
- **不做路 A / 路 C**：原「路 A」（dsh 作为 `kind: service` 工具实例 + iframe 嵌入）一步未做，
  且它退化成「任何应用都可以是 service 工具」，不需要单列；原「路 C」（为 SRCOS 定制 dsh
  bundle/profile 剥离 agent 栈）回归风险大且逆着 dsh 的设计走。
- **禁止**：把 dsh 变成硬依赖或核心路径。没有 dsh 时 SRCOS 必须照常工作（对齐 `ennote` 的 degraded 模式）。

### ADR-017：战略定位 —— AI 平台的确定性执行后端
- **背景**：生信云平台可能势微，通用 AI 平台上升，但**确定性场景**（企业内部项目管理、非工程师使用、受控输入输出）有独立且持久的价值。
- **决策**：把定位固化为「**SRCOS 是 AI 平台的确定性执行后端。探索用 AI，执行用 SRCOS。**」并写入 [`AGENTS.md`](../AGENTS.md) 作为全部设计取舍的最终判据。
- **三个支柱**：
  1. **执行路径 0 token** —— 一次性 token 投入（把方法固化成工具）+ 零边际 token 成本（反复执行），本质是**把 AI 的探索成果资本化**。
  2. **确定性 / 可复现 / 可审计** —— `work.sh` + `.sign` + 版本化工具 + `interface` 签名；谁、何时、哪个版本、什么参数全部留痕。这是企业内场景的真正诉求。
  3. **不碰工具 UI** —— AI 让造 UI 的边际成本趋近于零，UI 因此不再是护城河；平台价值转移到**注册、授权、隔离、资源、编排、审计**。
- **推论链（推导出后面所有设计）**：
  ```
  UI 造起来便宜了 → UI 不再是护城河
    → 工具的核心资产 = work.sh（函数体）+ interface（类型签名），UI 是可替换的壳
    → 同一个工具可以有多个 UI：shiny / CLI / agent / 自动生成的表单
    → "注册什么"的答案不是 UI，而是 interface + entry
    → 一份 interface 派生三个前端（ADR-018）
  ```
- **必要补全（否则「不管 UI」自相矛盾）**：**SRCOS 从 `interface` 自动生成 fallback 表单**
  （`type: string` → 文本框，`type: path` → 路径选择器……）。
  工具想要更好看的 UI 就自己写 shiny 覆盖它，不想写就用自动生成的。**渐进增强。**
- **与 AI 平台的关系**：互补，不是竞争。SRCOS 应主动做 AI 平台的执行层，MCP 就是对接口（ADR-019）。
- **不把定位收窄到生信**：生信是第一个落地场景，但差异化三角（确定性 / 0 token / 可审计）适用于任何需要受控执行的场景。

### ADR-018：一份 `interface` 派生三个前端
- **决策**：`Tool.interface` 是唯一的机器可读契约，必须同时派生：
  ① **MCP tool schema**（给 agent，对话即 UI）；② **流程画布连线类型**（给管理员，`output → input` 类型校验 + `expose` 推导）；③ **用户侧参数表单**（给非工程师，自动生成，可被自建 UI 覆盖）。
- **理由**：这是 ADR-008 的收益兑现 —— 第一个收益是画布连线，第二个收益是「MCP 白送」。
  且它让「不碰工具 UI」这个策略真正成立：工具开发者只交付 `work.sh` + `interface`，平台自动补齐 agent 面、编排面、表单面。
- **推论**：**工具的 UI 可以替换，签名不可替换。** 所以工具注册的核心是 `interface` + `entry`。
- **约束**：`interface` 的 `type` 全集必须在 Phase 1 就**冻结**（含 `path`），否则后续加类型要改 schema 并回归三处派生。

### ADR-019：MCP Server 内置于网关，第一期只读
- **背景**：「能否兼容 agent 的 MCP 等服务」是云平台走向现代化的关键节点。
- **决策**：在网关内内置 `/mcp` 端点（Streamable HTTP），**复用单二进制，不引入新进程**。
- **第一期（read-only）**：`list_tools` / `describe_tool` / `list_storages` / `list_paths` / `list_instances` / `task_status` / `task_logs` / `list_artifacts` / `read_file`。
- **第二期（显式授权）**：`submit` / `run_flow` / `cancel`，scope = `submit`。
- **新增认证面**：agent 是程序，不能用浏览器 session cookie。
  新增 `agent token`（用户在 webui 自助生成，`Authorization: Bearer`，带 `scopes: [read]` / 过期时间 / 标签），
  并由 `config/agent-tokens.yaml` 存储（只存 hash）。**每次调用带 agent 身份审计。**
- **agent 作为 SRCOS 的 service 实例**：完全成立且**零新增概念** ——
  agent 就是「一个需要 workspace、需要资源、需要生命周期的长驻服务」，正是 `RunUnit` 的定义，
  SRCOS 现有实例化 + `/proxy/<user>/<agent>/` 代理全部复用。
  但**权限模型必须收紧**：托管 agent 调 MCP 时用**实例身份**，权限必须是所属用户权限的**子集**，否则会权限放大。
- **明确不做 —— SRCOS 作为 MCP Client**（去调工具自己的 MCP server）：
  工具的执行契约是 `work.sh`（确定性、有退出码、有产物），MCP 是「谁可以调它、怎么发现签名」的接口协议，
  两者不同层。混在一起会模糊「确定性执行」这个核心卖点。
- **同样不做**：让 agent 直接改 workspace（第一期只读）。
- **实现契约（2026-09-22 已实现认证面，`internal/agenttoken`）**：
  - 明文格式 `srcos_<8位 base32 小写 id>.<43位 base64url secret>`；id 是明文查找键，
    所以校验只需哈希一条记录（256 bit 熵 → 用 SHA-256 即可，不需要盐/KDF，与密码不同）。
    文件里只有 `sha256(整条明文)` 的 hex，明文仅在 `srcos token create` 打印一次。
  - **每次校验都重新读文件**（不做 mtime 缓存）：凭据文件很小，而
    「已撤销的 token 仍被接受」是这个面最不能出的 bug（粗粒度时间戳的 FS 上 mtime 缓存会漏掉撤销）。
  - **失败关闭**：文件缺失 = 无 token；文件格式错 = 全部失效并记日志（只记一次），修好即恢复。
  - **token 不放大权限**：以所属用户身份行事，Grant 照常生效；scope 只收窄。
    `read` 与 `submit` 都可签发；`submit` **蕴含 `read`**（只能开工不能看结果的凭据只会让持有者盲动）。
  - **API 层规则**：HTTP 非 GET/HEAD/OPTIONS 且无 `submit` scope → 403；
    Bearer 请求免 `Origin` 校验（token 不是浏览器自动携带的凭据）；
    每次认证都写审计日志行（被拒时另记一行）。

- **第二期实现契约（2026-09-24 完成，submit / cancel / run_flow）** ——
  **submit 授权 = 工具 × 用户双维度**（roadmap §8 #9 的决定）：
  - **按用户**：owner 的 Grant 依旧外层上界（未授权的工具对写入面同样是「未找到」），
    配额（`max_cpu` / `max_memory` / `max_instances`）照常生效。token 永远是自己用户的子集。
  - **按工具**：token 可带 `submit_tools` 白名单（`srcos token create --scope submit --tool X`），
    空 = 该用户可见的全部工具。白名单只能*收窄*：工具不在里面 → 403。
    跑流程时对**每个节点的工具**都校验（不能靠组合把另一个工具洗进来）。
  - **单一实现**：`internal/execute` 是写入面的唯一权威（校验 + 配额 + 投递 + 启动），
    REST（`POST /api/jobs`、`/api/jobs/<id>/cancel`、`/api/flows/<id>/run`）与 MCP
    （`srcos_submit_job` / `srcos_cancel_instance` / `srcos_run_flow`）都调它（ADR-018）。
  - **submit 的语义是「入队并开始执行」**：MCP 提交一律 `run=true`（agent 无法去 drain 队列）；
    REST 默认仍只入队（保持表单/队列语义），显式传 `"run": true` 才立即执行。
  - **实例在 submit 返回时就可供寻址**（先写 pending 记录再启动）：否则"刚提交就查"会得到
    误导性的 not found。
  - **取消是幂等的**：已终态的实例直接返回它的终态，而不是报错（agent 重试取消不应该失败）。
  - **提交幂等键**（2026-09-26，A2）：`submit_job` / `POST /api/jobs` 可选 `idempotency_key` / `idempotencyKey`。
    给了 key 就把该次提交钉在一个 job 目录上（`job.IdempotentID` 由 `user+tool+key` 派生），
    重试返回**第一次提交的结果**（包括失败的那次）—— key 的含义是「就是这个请求」，
    换一次尝试请换 key。key 按 (用户, 工具) 命名空间，所以两个人可以用同一个字符串。
    重放在审计里带 `idempotent_replay=true`。
  - **已知限制（未解决）**：执行等待者活在网关进程里（与 CLI `job run` 同构）。
    网关在任务运行中重启会丢掉退出码（记录由 Reconcile 按后端能看到的真实状态收尾）。
    真正的持久任务执行器（网关侧的队列 drainer）仍是待办。
  - **审计**：每次写入另记一行 `audit submit/cancel/run_flow`（谁、哪个 token/scope、
    哪个工具与版本、哪个 job/instance/run）。
  - **撤销**：`token revoke <id>` / `--user <name> --all`；`srcos del <user>` 一并撤销；
    网关侧还有「用户必须存在」的失败关闭兜底。
  - **使用时间**写在 `data/agent-token-usage.yaml`（运行态、网关写、按 token 降频、
    首次使用即时落盘），与 CLI 写的 `config/agent-tokens.yaml` 分开两个文件、两个写入者。
    它是提示而不是审计日志（审计流仍是独立的待办项）。
- **MCP 实现契约（2026-09-22 已完成第一期，`internal/mcp` + `internal/inspect`）**：
  - **端点**：`POST /mcp`，Streamable HTTP，**无状态**（不发 `Mcp-Session-Id`）；响应一律
    `application/json`（不用 SSE）；GET/DELETE 返回 405 + `Allow: POST`；通知（无 id）返回 202 空体；
    批量数组明确拒绝（2025-06-18 已移除批量）。
  - **协议版本**：`2025-06-18`，`initialize` 里客户端报的版本在支持集合内就回显（另含 2025-03-26 / 2024-11-05），
    否则回自己的版本（规范要求）。capabilities 只声明 `tools`（`listChanged: false`）。
  - **认证**：只认 `Authorization: Bearer`（agent token），session cookie 在 `/mcp` 无效；
    无凭据 401 + `WWW-Authenticate: Bearer`；有 `Origin` 时必须同源（规范要求校验 Origin）。
  - **错误分层**：协议错误用 JSON-RPC 错误码（未知方法 -32601、未知工具/坏参数 -32602、解析 -32700）；
    工具**执行**失败（未授权、路径越界、找不到实例）返回 `result.isError = true` 的文本
    —— 让 agent 看见原因并自己改正，而不是把它当成协议坏掉。
  - **读范围**：全部走 `internal/inspect`（Jail 唯一入口、只暴露沙箱路径、宿主路径永不外泄）；
    read_file 只覆盖 home / 指定工具的工作区 / 已声明 storage，二进制与逃逸一律拒绝。
  - **一份实现三个前端**：`internal/inspect` 同时供 REST API、HTML 工具页、MCP 使用；
    `internal/tool/schema.go` 的 JSON Schema 派生同时是 `inputSchema` 与后续画布/表单的共同来源。

### ADR-020：存储与路径 provider 化，并用闭环消除「选了却看不到」
- **背景**：工具需要访问集群/云路径并把路径作为参数，用户在工具 UI 里预览后选择。
  若「UI 里可选的路径」与「沙箱里可见的路径」不一致，就会出现选了却跑不了。
- **决策**：引入 **`StorageProvider`** 面（`config/storages.yaml`，管理端声明），
  `interface` 新增 **`type: path`**（必须带 `from: <storage-id>`），提供 **`/api/paths`** 与**原语控件 `srcos-path-picker`**。
- **闭环约束（核心）**：`path` 参数的可选范围 = 已挂载的 storage = 工具 `requires_storages` 声明的需求。
  实例化时按声明挂进 `MountSpec`，UI 选择时由 `Jail` 校验。**三者始终一致。**
- **路径表述空间**：`/api/paths` 与 `type: path` 的参数值一律用**沙箱路径**（工具在沙箱里直接可用）。
  SRCOS 内部用 `Jail` 双向映射（`ResolveExisting` ↔ `DisplayPath`）—— **这是移植 `Jail` 的第三个收益**。
- **边界澄清**：路径选择器是**平台提供的原语控件**（类比 `<input type="file">`），不是「SRCOS 管业务 UI」。
  这条必须写清楚，否则与 ADR-004 自相矛盾。
- **第一期只做 `type: posix`**；`type: s3` 留字段口子，物化方式（`work.sh` 内 s3 客户端 vs FUSE 挂载）待定。
- **`rw` 策略**：用户 home 与项目目录允许 rw，集群公共参考数据**强制 ro**；rw 的 storage 必须有配额，
  否则一个工具能写爆 `/share`。
- **同一个 provider 也是 MCP tool**：`/api/paths` ⇄ `srcos_list_paths` —— 一份实现，两个前端（对齐 ADR-018）。

### ADR-021：OS 用户 ≠ SRCOS 注册用户；隔离靠 mount namespace 而非 UID
- **背景**：必须区分「启动 `srcos serve` 的 OS 用户」与「SRCOS 注册用户」。
- **决策**：
  - **OS 用户**：真实的 Linux 账号，有真实 home；SRCOS 以它的权限运行，读 `config/`、写 `data/`。数量通常 1 个（或按角色少数几个）。
  - **SRCOS 注册用户**：只存在于 `config/users/<name>.yaml`，**没有系统账号、没有真实 home**。任意多个。
  - 因此注册用户的运行时视图由 SRCOS 构造：**内建挂载** `/workspace` ← `data/ws/<user>/<tool>/`（rw）、
    `/home/<user>` ← `data/homes/<user>/`（rw）；**声明式挂载**为工具 `requires_storages` 指定的 storage。
  - 沙箱内 `$HOME` = 虚拟 home，因此 `.cache` / `.condarc` / `.config` / `.jupyter` 天然隔离，
    既不污染 OS 用户的真实 home，也不会在并发实例间互踩。虚拟 home 首次使用时从模板初始化。
- **关键推论（已知限制，必须记录）**：所有实例都以**同一个 OS 用户**身份运行，
  所以**隔离不靠 Unix UID**。真正的边界有两个，且必须分开看待：
  - **沙箱内进程** ← **`MountSpec` 的粒度**（bind 了什么都看得见）+ 文件权限位
  - **SRCOS 的 API（用户 / agent）** ← **`Jail` 路径校验**

  而 `MountSpec` 粒度这条有一个实测确认的细节（2026-09-22，见 [`environments.md`](environments.md)）：
  bwrap 的 userns 把未映射的 gid 折叠成 `65534`，且沙箱进程就在 `65534` 组里，
  所以**宿主的 `group` 权限位在沙箱内等于公开可读**。
  即：**bind 粒度直接等于数据可见范围**。

  防护：**每个实例只挂自己的 workspace + 自己声明的 storage，绝不挂父目录。**
  需要多个 storage 就 bind 多个精确路径 —— 因为 bind 了父目录就等于把父目录下
  **所有 group-readable 内容**（含其他项目组的 `drwxrwx---`）一并交出。
- **明确不做**：为每个 SRCOS 注册用户建系统账号（真实 UID 隔离）—— 需要 root，与 ADR-014 冲突。
- **环境 vs 数据分离**：conda env / `module` / `.sif` 属于**环境**（宿主真实路径，ro 挂载，管理员在 `tool.yaml` 写）；
  共享参考数据与项目目录属于**数据 storage**（`storages.yaml` 声明）。两者不要混用同一套声明。
  ADR-020 的 `type: path` 只用于**数据**，不用于环境。

---

### ADR-022：任务队列的消费者在网关内，一次提交自动执行一次

- **背景**：「目录即队列」（ADR-004）在实现上只成立了一半：目录确实是队列，但**唯一的消费者是人手
  敲 `srcos job run`**。没有那个人的地方（agent、表单用户）提交即等于石沉大海 —— 对一个自称
  「确定性执行后端」的平台，这是最贵的一个缺口。
- **决策**：网关内置队列消费者（`internal/execute` 的 `Queue`），在启动时与周期 tick 上消费投递目录。
  配套规则：
  - **提交唤醒队列**：写 `job.json` 后立刻唤醒，所以常见延迟是毫秒而不是一个 tick；tick（默认 5s）
    只是兜底（网关不在时留下的提交、另一个进程写的提交）。
  - **启动即冲刷**：重启后第一轮就把「还没跑过」的提交捡起来。**排队状态是文件，不丢。**
  - **一次提交自动执行一次**：任务目录里留一个 O_EXCL 认领标记（`job.Claim`），所以网关的队列、
    `srcos job run`（另一进程）、流程执行器三方不会把同一个任务跑两遍；`--force` 是显式重跑入口。
    记录存在且已非 `pending` 就是「已经尝试过」，守护进程不再重试（一个会无限重试的 daemon 比
    只跑一次的更糟）。
  - **准入在启动时判**：每次真正启动前重新检查 Grant（撤权后不会偷偷跑）与配额（`--task-workers`
    是全局宿主保护，用户自己的上限仍是 grant 配额）。
  - **`pending` 是队列位置，不是资源占用**：`State.ConsumesResources` 把 `pending` 排除在配额之外 ——
    否则一条提交过不了它自己排队位置所蕴含的那道检查（队列里排着 10 个任务，不应该等于占了 10 份资源）。
  - **按 (用户, 工具) 串行** + 全局上限：同一工具的两个运行不会在同一个工作区里互相踩。
  - **流程节点不走队列**：`flowrun` 以依赖顺序调度自己的 job（并已认领），队列跳过带流程标签的提交。
    否则一个 DAG 会被当成一堆互不相干的队列项乱序执行 —— 这是实现这一条时差点踩进去的坑。
  - **队列跑过的任务一样有审计**：走的是同一条 `job.Validate` + 配额 + 实例记录 + 日志路径。
- **判定必须活得比等待者长（2026-09-24 补齐）**：重启**不会**杀掉正在跑的任务（systemd 拥有它），
  但「谁记下退出码」曾是那个死掉的网关进程。修法是**让 systemd 自己记**，分三步：
  1. 任务从 `systemd-run --scope` 改为**瞬时 unit**（`--unit=<name> --wait`）：`--wait` 仍把退出码
     同步交给等待者，unit 则给了稳定的名字（`stop` / `is-active` 按名操作）。
  2. **unit 自己写判定**：`-p ExecStopPost=-/bin/sh -c "echo $EXIT_STATUS $SERVICE_RESULT > <log>.verdict"`
     —— 系统把一个运行怎么结束写进一个文件（日志旁边，工作区之外，工具改不到）。
     **不能只靠 `systemctl show`**：瞬时 unit 退出后会被快速回收（实测 1.4s 后 `LoadState=not-found`），
     而 `ExecStopPost` 写下的文件永远在。
  3. `runtime.ReconcileTasks` 在**每个 tick** 上结算失去等待者的任务：进程还活着就收养，死了就
     **读判定文件**、用与实时路径**同一个** `applyExitStatus` 落库（成功/失败 + 真实退出码）；
     只有后端答不出判定时才写 `stopped` + 说明。只碰有进程的状态（不碰 `pending`、不碰 service）。
  另外两处副作用（都修了）：**unit 的 stdout 不继承我们的 fd**（`cmd.Stdout` 抓不到东西 —— 服务的
  日志一直是空的，现在用 `StandardOutput=append:<log>` 由 systemd 写）；**重跑同一个实例**要先
  `reset-failed` 旧 unit 并删掉旧判定文件，否则名字冲突、判定串味。
- **不做**：引入外部队列/消息中间件（与「文件系统即数据库」冲突）；把 `qstat` 子作业跟踪塞进队列
  （ADR-006 已排除）；为队列加优先级/公平调度（先按提交时间 FIFO，够用）。

---

### ADR-023：画布布局是旁挂文件；`expose` 由服务端推导

- **背景**：画布的两个「未做」——拖拽摆放与 `expose` 自动推导。前者需要一个落点，而 `flow.yaml`
  是契约（可手写、可 diff、注册期校验）；ADR-012 已经说了「布局不写进契约」，但没说写到哪。
- **决策（布局）**：手摆的坐标写在 **`<flows>/<id>/layout.yaml`**（`nodes: {<id>: {x, y}}`），
  由画布自己的写入口维护：`PUT /api/admin/flows/<id>/layout`，**不跑流程校验**（拖动一个节点
  不该让一个合法流程看起来坏掉）。文件缺失/损坏 = 「没有摆放记录」，画布退回**拓扑分层推导**
  （手写的流程照样打开）；只保留 `flow.yaml` 里仍存在的节点（旁挂数据不许无限积累）。
- **决策（expose）**：`GET /api/admin/flows/<id>` 的编辑器视图带上 `suggestedExpose` ——
  「必填、无绑定、无 expose、且工具没有默认值」的输入，建议 `from: sample.<输入名>`。
  规则与注册期校验的第 14 条**互补且写在一起**（`internal/flow.MissingExpose`），所以画布补的
  正是校验会拒的；它只产出草稿建议，写回仍然只能走「校验通过才保存」那条路。
- **顺带修正一处实现偏离**：校验的闭环原来**忽略工具默认值**（尽管提示语写着「或给工具一个默认值」，
  且 `job.Validate` 是按「有效参数」判定的）。现在 `default != nil` 也满足 required 输入 ——
  与 flow-spec §2.6 的原文一致。
- **不做**：把坐标写进 `flow.yaml`；在浏览器里重写类型规则（推导与校验都在 Go，画布只展示答案）。

---

### ADR-025：托管 agent 用「实例身份」——工具声明、运行时令牌文件、随实例生死

- **背景**：ADR-019 说「托管 agent 调 MCP 用实例身份，权限是所属用户的子集」，但一直没有这枚身份：
  agent 只能拿一枚用户级 token（权限放大，且审计里分不清「实例做的」与「人做的」）。
- **决策**：工具在 `tool.yaml` 里声明它托管的 agent 需要什么（只对 `kind: service` 合法）：

  ```yaml
  kind: service
  agent: { mcp: [read, submit], tools: [a, b] }   # tools 只能收窄 submit
  ```

  实例**启动时**由 SRCOS 签发一枚 token：
  - **scope 与白名单来自工具声明**（不是调用方），所以「是子集」由构造保证；owner 的 Grant 仍是外层
    上界，提交时的工具 × 用户检查照旧。
  - **存在运行态文件 `data/agent-tokens.yaml`**，与 `config/agent-tokens.yaml`（人签发的）分开 ——
    两个写入者、两个文件，而不是两个进程抢一个文件。认证用 `agenttoken.Chain`（用户令牌 + 实例
    令牌），REST 与 MCP 两面一致。
  - **投递是虚拟 home 里 0600 的 `$HOME/.srcos/agent-token`**，不是环境变量：所有实例共用一个 OS
    用户，`/proc/<pid>/environ` 在同 uid 的视野里可读；0600 文件正好只属于这个实例。
  - **生命周期 = 实例**：启动签发；停止（显式 stop / 回收 / 管理端强制停）撤销；网关启动时清扫
    「实例已不在」的令牌 —— 崩溃不会删任何东西，清扫是这条保证的兜底。
  - **审计**：actor 带 `instance`（「实例做的」与「人做的」可区分），签发/撤销记
    `agenttoken.mint` / `agenttoken.revoke`（kind=system）。
- **失败语义**：声明了 `agent` 却签发不出来 → **服务启动失败**：一个「没有凭据的托管 agent」
  不能被报成 running。
- **不做**：让 agent 用用户级 token（现状，权限放大）；把实例令牌放环境变量；跨主机共享令牌文件。

---

### ADR-024：审计流是结构化、只追加的 JSONL，读在 `srcos audit`

- **背景**：「可审计」是三个支柱之一（ADR-017），但实现一直是散落的 `log.Printf` 行 ——
  能被 `journalctl` 看见，却答不了「谁在何时用哪个版本的工具、什么参数、被允许还是被拒绝」，
  更没有「改配置」的记录（谁把哪个工具开给了谁），而这恰恰是企业内场景最先问的。
- **决策**：新增 `internal/audit`，能力分两半：
  - **写**：一个 JSON 对象一行，追加到 `data/audit/audit-YYYY-MM-DD.jsonl`（0600，按天轮转）。
    事件模型 `Event{Actor,Action,Target,Params,Decision,Reason,Request,Outcome,Refs}`，
    其中 `Actor` 区分 `session` / `agent_token`（带 token id 与 scopes）/ `cli` / `system`。
  - **读**：`srcos audit tail|list`（过滤 user/action/decision/since，`--json` 出原始行）。
- **两条硬约束**（写在包注释里，调用方依赖它们）：
  1. **记录绝不打断它描述的动作**：`nil *Recorder` 是 no-op，写失败只记 stderr，绝不进请求路径。
     `Recorder` 因此可以被调用方无条件持有，不需要每处 `if audit != nil`。
  2. **凭据永不落盘**：所有参数过 `Redact`（password/secret/token/api_key/… 掩码）。
- **埋点范围（第一期，scope A）**：
  - **写入面**（`internal/execute`）：`submit` / `cancel` / `run_flow` 的成功与拒绝 ——
    用 `defer` + 具名返回值，使每一条 early return 的拒绝路径都被记录。
  - **拒绝事件**（`internal/api`）：CSRF、未认证、只读 token 试写。拒绝是最有价值的审计材料。
  - **配置变更**：`grant.*` / `group.set` / `admins.set` / `token.create` / `token.revoke`
    （API 与 CLI 两条门都记 —— 漏了 CLI 就是漏了一扇门）；CLI `job submit` 也记。
- **第二期进展**：**管理端查询页已做**（`/admin/audit` + `GET /api/admin/audit`，仅管理员；
  服务端渲染 + GET 筛选表单，与 CLI 共用 `audit.Query`，非管理员 403）；**生命周期跃迁已审**
  （见下）；**保留策略已做**（`srcos audit prune --keep 90d`，**显式、不自动**，keep≤0 不删，
  删除本身记 `audit.prune`）。
- **生命周期跃迁**（`internal/runtime`）：`instance.done`（任务终态）、`instance.settled`
  （重启后凭判定文件结算）、`instance.reaped`（回收，带 reason）、`instance.orphaned` / `instance.adopted`
  （reconcile）、`instance.stopped`（管理端强制停 / CLI `svc stop`）—— actor 是该实例的用户、kind=system。
- **不做（仍待）**：无。（本地签名不做，理由见下。）
- **防篡改（hash chain，第二期收尾）**：每行带 `prev`/`hash`（`hash = SHA-256(prev + 该行字节)`），
  **每文件**一条链（不跨文件 —— 保留策略会合法删掉整天文件，跨文件链会被正常 prune 打断）。
  `srcos audit verify` 与管理页横幅都校验：能发现改行 / 删行 / 乱序 / 剥掉链字段；**不能**证明
  某个整文件从未被删除，也挡不住能重写全部文件的人。
- **外发 = 信任锚（2026-09-26）**：`state.yaml` 的 `audit.forward_url` 指向 HTTP 采集端，
  每个事件 POST 一次。先落本地 spool（`data/audit-forward/<ts>-<rand>.json`，一事件一文件），
  后台**按序**发送、成功才删；采集端故障则保留重试，超过 `forward_max` 丢最旧并**计数**
  （不静默）。管理页显示待发送/已丢弃。
- **本地签名明确不做**：签名密钥必须在网关读得到的地方（否则写的时候签不了），而同一个写域里的
  签名挡不住能重写全部文件的人 —— 它只会让人以为有防篡改。**锚必须在网关写不到的域里。**
- **实现要点（e2e 抓到的真 bug）**：网关与 CLI 是**两个进程**同时写同一日文件，各持一条链必断；
  所以每次追加要 `flock` + 重读文件尾再串链 —— 链属于文件，不属于进程。
- **为什么不是数据库 / 不是消息队列**：与「文件系统即数据库」一致（AGENTS.md）。
  一行一个 JSON 的文本流能被 `grep`、`jq`、`rsync`，也能在十年后被读懂；
  审计的可用寿命比任何查询 API 都长。
- **为什么放在 `data/` 而不是 `config/`**：`config/` 是声明态（谁被允许做什么），
  `data/` 是运行态（谁做了什么）。两者混在一起会让「备份配置」变成「备份审计」。

---

## 6. 路线图

### Phase 0：基线 ✅
- [x] 重命名为 srcos
- [x] 删除 `site/`，保留 `docs/tunnel.md` / `docs/dsh-demo.md`
- [x] `git init` + 首次提交
- [x] 建立本 roadmap 文档

### Phase 1：规范与最小闭环 ✅
目标：把一个真实工具从提交到产物跑通，并把契约**冻结**（schema 一旦定下就不反复改）。
- [x] `docs/tool-spec.md` —— 工具开发者契约（`interface` 类型全集已冻结）
- [x] 最小示例模块 `srcos-tools/hello-fanout/`：`work.sh` 用 `ata -t 5`（降级 `xargs -P5`）跑 5 个样本 + `.sign`
- [x] **端到端跑通**（`local` + `bwrap`，node01 实测）
- [x] 最小运行时：`internal/tool`（加载+13 条校验）、`internal/job`（加载+校验+扫描）、
      `internal/sandbox`（MountSpec + Jail + bwrap 物化）、`internal/runtime`（local backend）
- [x] CLI：`srcos tool validate|list`、`srcos job submit|run|list|status|logs`
- [x] `sandbox: none` 降级模式（含 `env -i` 内层清环境，见下）
- [x] **`docs/flow-spec.md`** —— 流程管理员契约（2026-09-22 建立：schema、类型兼容表、
      14 条注册期校验、执行语义（规划）、与 annopi 的对照、明确不做）
- [ ] `docs/storage-spec.md` —— 管理端契约（可推后到 Phase 2 存储实现时）

**Phase 1 实现中暴露的四个真问题**（已修，并已写回 `tool-spec.md`）：

| # | 问题 | 结论 |
|---|---|---|
| 1 | `.sign` 放在工具级 → 不同参数的任务互相误判为"已完成" | **幂等的单位是「一次任务」而非「一个工具」**，标记必须用 `$SRCOS_TASK_ID` 分派（§4.2.1） |
| 2 | `xargs -I{} bash -c '{}'` 会二次处理反斜杠，把生成的命令破坏掉 | 改用 `xargs -n 1` + `$1` + 环境变量传递（§4.2 示例） |
| 3 | 工具级 `interface.inputs[].default` 从未真正传给工具 | `EffectiveParams` 合并默认值后再生成 `SRCOS_PARAM_*`（§4.1） |
| 4 | `sandbox: none` 下契约路径（`/workspace`）在真实文件系统上不存在 | 降级时 env/cwd 改携带**宿主路径**；环境用内层 `env -i` 清空，外层 limiter 仍继承宿主环境（否则 `systemd-run` 找不到 DBUS）（§4.1） |

另一个实现期发现：`bwrap` **无法在已只读绑定的 `/usr` `/bin` `/lib*` 内创建挂载点**，
所以工具自带二进制统一放 `/opt/srcos/bin`（沙箱 `PATH` 第一项），注册期直接拒绝违规配置。

### Phase 2：实例化运行时（**主体完成**：剩 `storages` 管理端编辑、端口/路由审计、prlimit RSS 看门狗）
- [x] `Tool` / `RunUnit` / `Instance` 的 Go 类型定义 + `tool.yaml` 校验器（Phase 1）
- [x] `job.json` 落盘扫描器（目录即队列）+ `srcos job submit/run/list/status/logs` CLI（Phase 1）
- [x] `MountSpec` + `Jail` + **内建挂载**（`/workspace`、虚拟 `/home/<user>`）+ 模板初始化（Phase 1）
- [x] `Backend` / `Handle` 抽象 —— 一个原语两个 flavor（2026-09-24 起 task 也用 systemd 瞬时 unit，见 ADR-022）
- [x] `local` backend，含 cgroup（`systemd-run --user`）与 prlimit 降级路径
- [x] **`kind: service` 全链路**：端口池 → 物化 → 启动 → 探活 → 发布路由
- [x] **代理层接入动态路由表**（2026-09-22）：网关从 `data/instances/` 重建路由表
      （启动同步 + 每次扫描 + 查不到时按需读记录），`/proxy/<user>/<tool>/` 可达；
      实例优先于静态卡片；端点必须是回环地址（记录被手改也不能指向公网）；
      停止后第一个请求 502、随即丢弃路由（下次 404）；裸路径 / Referer / 路由 cookie 也回投到实例；
      裸短链接 `/<tool>/...` 与静态卡片一样 302 到 `/proxy/<user>/<tool>/...`
- [x] `StorageProvider`（ADR-020）：`storages.yaml` + Jail 复用 + `type: path` 闭环
- [x] `internal/route` 动态路由表（非回环目标被拒；`DeleteInstance` 有归属栅栏）
- [x] `internal/portpool` 端口池（真 bind 探测、并发安全、`Reserve` 供 reconcile）
- [x] **`Reconcile`**：重启后收养仍活着的实例（重新占端口 + 重发路由），把记录与真实状态对齐
- [x] **`Reaper`**：`maxLifetime` / `idleTTL`；有开放 WebSocket 时不算空闲；SGE 上默认不做空闲回收（ADR-015）。
      2026-09-22 补：`Reaper.LastActive` 让调用方（网关）提供「最近一次流量」，
      `idleTTL` 才真的是"没人用"而不是"启动久"；网关侧每次扫描 tick 自动回收
- [x] **降级模式的停止与探测**（2026-09-22 修）：无 user systemd 时把「SRCOS 直接启动的子进程」
      的 `pid` + `/proc/<pid>/stat` 的 `starttime` 记进实例记录，`StopUnit`/`UnitAlive` 用它停止/判断
      存活（发信号前校验 starttime，防 pid 复用）。降级模式下 `svc stop` / `svc reap` / `Reconcile`
      因此才真正成立
- [x] `doneWhen` 探针轮询（`file_exists` / `dir_nonempty`）
- [x] **`sge` backend 架构**（ADR-015，未接真集群）：
      `qsub` 参数翻译（`cpu`→`-pe smp`、内存→**按 slot 均分** `h_vmem`、`walltime`→`h_rt`）、
      `qstat -xml` 解析（**区分挂起 `s` 与真死**）、rendezvous 共享文件当控制通道、
      `ssh -L` 本地转发当数据通道、作业脚本内做端口冲突回退与就绪探活
- [x] **「启动中」进度页**（2026-09-22）：未就绪实例在浏览器里给出 503 + `Retry-After` +
      自动刷新的进度页（含日志末尾），失败/已停止给出 502 + 原因与日志 + 重启命令；
      程序客户端只拿状态码（按 `Accept: text/html` 区分）
- [x] `/api/paths` + `srcos-path-picker` 原语控件（Phase 3 完成）
- [x] **`svc reap` 定时调度 + 启动 reconcile**（2026-09-22）：网关启动时 reconcile
      （收养活着的、标记死掉的），每次扫描 tick（10s）执行一次回收；
      `idleTTL` 改为按**流量**判定（代理写 `data/service-activity.yaml`，`Reaper.LastActive` 读），
      并接了真实的 WebSocket 计数（`proxy.ActiveConns`）—— 修掉了「记录里只有启动时间，
      所以 idleTTL 实际是"启动多久"」这个会误杀在用的服务的缺陷
- [ ] storage 声明的管理端编辑（目前 `storages.yaml` 只有 CLI/手写；只读展示已在 MCP `srcos_list_storages`）
- [ ] 端口/路由的持久化审计
- [ ] prlimit 路径下的 RSS 看门狗（RLIMIT 无法表达"每单元进程数"，见 ADR-014 新增说明）
- [x] **任务队列的消费者**（2026-09-24，ADR-022）：网关启动/周期 tick 消费投递目录、提交唤醒队列、
      O_EXCL 认领标记（与 `job run`、流程执行器互斥）、`--task-workers` 全局上限、按 (用户,工具) 串行、
      `runtime.ReconcileTasks` 每 tick 结算失去等待者的任务记录；**任务改以 systemd 瞬时 unit 运行**，
      判定由 `ExecStopPost` 写入文件，所以重启后再结束的运行也有真实退出码

### Phase 3：注册、授权与管理端（**主体完成**：剩审计流与 `storages` rw 配额）
- [x] 工具注册：扫描工具目录（`--tools-dir` / `$SRCOS_TOOLS_DIR`）
- [x] **`Grant` 授权模型**（`internal/grant`）：**默认拒绝**、只有「允许」没有 deny、组 + 用户 + public + 通配兜底、管理员绕过
- [x] **用户级配额聚合**：`max_cpu` / `max_memory` / `max_instances`，按存活实例求和；终态实例不占名额
- [x] CLI `srcos grant list|show|set|rm|group|admin|allow|deny`
- [x] HTTP 面：`/api/tools`、`/api/tools/<id>`、`/api/paths`、`/api/jobs` + `/tools`、`/tools/<id>` 生成式表单
- [x] **原语控件 `srcos-path-picker`**（Web Component，工具自建 UI 也能用）
- [x] **生成式表单**（ADR-017 的必要补全：只有 `work.sh` + `interface` 的工具立即可用）
- [x] **agent token 认证面**（`config/agent-tokens.yaml`，只存 hash，scope + 过期 + 标签）：
      `internal/agenttoken`（创建/校验/撤销/失败关闭 + 使用时间）+ `srcos token create|list|revoke` +
      HTTP 面 `Authorization: Bearer`（第一期只读；第二期已可签发 `submit`，见下）+ 审计日志行
- [x] **管理端页面**（2026-09-22）：`/admin` —— 实例总览（全部用户 + CPU/内存快照 + 日志）、
      强制停止（走 `StopService`）、**启动服务**（2026-09-26，`POST /api/admin/instances` + `Runner.ForUser`，
      共用同一端口池与路由表）、工具/授权内联编辑（增删用户/组/public/配额，"删除授权" = 下架）、
      组与管理员管理；配套 `/api/admin/*`（仅管理员 + Origin 校验）
- [x] **授权变更的热加载**（2026-09-22）：`grant.Policy` 线程安全并支持原地 `ReplaceWith` ——
      管理端/API 的改动立即生效；手工 `vim grants.yaml` 在下一个扫描周期（10s）内生效，
      坏文件只记日志、内存策略不变
- [ ] 审计日志（含 agent token 调用）
- [ ] `storages.yaml` 的 rw 配额

### Phase 3.5：MCP Server（**招牌功能**）——✅ 完成（只读 2026-09-22；**第二期 submit/cancel/run_flow 2026-09-24**）
- [x] 网关内置 `/mcp` 端点（Streamable HTTP），复用单二进制与认证：`internal/mcp`，无会话状态
- [x] MCP tool schema 从 `Tool.interface` **自动派生**（ADR-018）：`Interface.JSONSchema()`，
      在 `srcos_describe_tool` 的 `inputSchema` 里对外
- [x] read-only tools（名字统一带 `srcos_` 前缀）：`srcos_list_tools` / `srcos_describe_tool` /
      `srcos_list_storages` / `srcos_list_paths` / `srcos_list_instances` / `srcos_task_status` /
      `srcos_task_logs` / `srcos_list_artifacts` / `srcos_read_file`
- [x] agent token scope 校验（`read`）+ 每次调用一行审计日志
- [x] 预留 `submit` scope 与第二期接口（**不实现**：不可签发，调用返回 -32602）
- [x] 端到端验证：官方 Python MCP SDK 客户端连上 → initialize / list_tools / call_tool 全通
- [x] 一份实现两个前端：`internal/inspect` 是只读答案的唯一实现，REST API、HTML 页面、MCP 共用

### Phase 4：云流程（**主体完成**：契约/校验/展开/并发/重试/取消/续跑/画布；剩流程级审计与 annopi 模块）
- [x] **`docs/flow-spec.md`** 契约冻结（2026-09-22）
- [x] **`Flow` 注册 + 校验**（2026-09-22）：`internal/flow` —— DAG 无环（报出环路径）、
      **连线即依赖**（下游必须直接或间接 depends_on 上游，否则可能先读不存在的路径）、
      节点 id 可寻址、`id@version` 精确匹配、节点工具必须 `kind: task`、
      连线地址/存在性/类型兼容（只允许路径流动）、一次报出全部问题 +
      闭环校验「每个必填输入有且仅有一个来源（默认值 / binding / expose）」+
      `srcos flow list|validate`
- [x] **样本展开与顺序调度**（2026-09-22）：`internal/flow` 的 planner（`expose` × 样本表 ×
      连线 → 每个 (节点 × 样本) 一个 job）+ `internal/flowrun`（拓扑序、`depends_on` AND、
      `when: always`、失败即停在第一个坏样本）+ `srcos flow run|resume|status`
      （`--dry-run` 打印每个 job 的参数与产物路径）
- [x] **节点产物路径由平台推导**（2026-09-22）：`/flow/runs/<run>/nodes/<节点>/<样本段>/<输出名>`，
      `/flow` 是继 `/workspace`、`/home/<用户>` 之后的**第三个内建挂载**（rw，每用户）——
      所以边不需要模板语法也能解析，`expose.from: output.<名>` 是同一机制的另一端
- [x] **断点续跑**（2026-09-22）：`.sign` 是人类逃生口（手工 `touch` 即视为完成），成功时自动写；
      样本表随运行留档，`flow resume <run-id>` 不需要再给
- [x] **流程级并发上限**（2026-09-22）：`--concurrency`（默认 4）限制整个运行的同时在跑任务数；
      节点级失败即停（不再投递该节点新样本，在跑的不打断）
- [x] **`retry.max` 自动重试 + 退避**（2026-09-22）：粒度是**任务**不是节点（重跑整个节点会白做已成功的样本），
      退避 30s/2m/5m/10m/20m，尝试次数持久化在运行记录里（`resume` 不重置预算）
- [x] **`flow cancel`**（2026-09-22）：跨进程标志文件 + 停掉在跑的 job（复用回收器的停止路径）
- [x] **流程级配额**（2026-09-22）：按 grant 的聚合配额（含本次运行中在跑的任务，所以并发也拦得住）
- [ ] 流程级审计（目前只有实例记录与 `flowrun.yaml`）
- [ ] 把 `annopi` 注册为普通工具模块（逃生口）

### Phase 5：前端（`webui/` Vite 包）+ 自带 viewer —— ✅ **全部完成**（2026-09-25）
- [x] **`webui/` 工程骨架 + `make webui` + `//go:embed dist`**（2026-09-22）：Vite + React + TS，
      产物嵌入 `internal/web/dist/`；整个 dist/ 都是产物（只有 `.gitkeep` 进版本库），
      所以没有 Node 的环境也能 `make build`（画布页退化为解释页）
- [x] **`srcos://` 地址协议 + `/api/resources` 统一解析**（2026-09-23）：`internal/resource`（地址语法
      + viewer 注册表，纯解析，**与 `dsh-resource://` 同构**，即路 D 保险）+ `internal/inspect`
      （唯一解析入口，复用 `sandbox.Spec`/Jail；scope 是用户相对的，地址里没有用户名）+
      `/api/resources`（元数据）/`raw`（字节，**永不 text/html**）/`html`（sandbox + CSP）
- [x] **viewer 注册表 + 首批 viewer**（2026-09-23）：`patterns` glob 认领 + 优先级；
      文本（行号）/Markdown（服务端渲染的子集）/表格（CSV/TSV）/图片/PDF/HTML（sandbox iframe
      + 不透明 origin）/目录；`/view` 页面 + 无 src 时的根选择页；全部 Go 模板，无 Node 依赖
- [x] **从 `interface` 自动生成参数表单**（fallback UI，含 `type: path` → `srcos-path-picker`）
      —— 已在 Phase 3 完成，此处仅登记（ADR-017）
- [x] **任务列表页 + 日志流（SSE）**（2026-09-23）：`/tasks`（实例列表 + 筛选）+ `/tasks/<id>`（详情：
      状态 / 资源 / 标签 / 产物 / 日志）+ `internal/inspect/logstream.go`（`FollowLogs`：回放尾部 →
      跟随追加 → 终态自动结束；半行不当作一行、静默 15s 心跳、日志未出现时等待、记录消失时收尾）+
      `GET /api/jobs/<id>/logs`（默认纯文本尾部）/`?follow=1`（SSE，事件名 `line`/`state`/`note`/`end`）；
      **产物直接链进 viewer**（`Artifact.Addr` = 沙箱路径 → `srcos://` 地址）
- [x] **流程编排画布**（2026-09-22；2026-09-24 补拖拽与 expose 推导）：`/admin/flows/<id>/edit` —— 拓扑分层自动布局（不写布局进契约）、
      两步点连线、**服务端校验**（画布不重复实现类型规则）、检查器编辑取值来源/依赖/when/retry、
      校验通过才写回 `flow.yaml`；只有这一页加载 React（ADR-012）。
      **拖拽摆放**（2026-09-24，ADR-023）：坐标落在旁挂 `layout.yaml`，单独写、不跑校验；
      **`expose` 自动推导**（2026-09-24）：服务端算出「必填且无来源」的输入，画布一键补齐
- [x] **用户自助生成 agent token 的页面**（2026-09-24）：`/tokens`（生成 / 列表 / 撤销）+ `/api/tokens`；
      列表服务端渲染（无 JS 也能看与撤），生成走 JSON API 以免明文进 URL；**只给自己签**、
      白名单只能从自己可见的工具里选、**只认 session**（agent token 不能管理凭据）

### Phase 6：HPC / SGE
- [ ] `sge` backend（`qsub` / `qstat -xml` / `qdel`）
- [ ] rendezvous 文件协议 + `ssh -L` 隧道管理（含重连与回收）
- [ ] `qstat` 状态映射：区分 `S`（挂起/抢占）与真死
- [ ] `h_rt` 到期预警 + `qalter` 续期；空闲回收默认关闭
- [ ] apptainer sandbox 物化（`-B` / `--containall` / SIF 镜像）
- [ ] 镜像预热（`apptainer pull` 到共享盘）

---

## 7. 明确不做

| 不做的事 | 原因 |
|---|---|
| Kubernetes / k3s 后端 | 与「单二进制零依赖」冲突，HPC 环境不适用（ADR-002） |
| 自研编排引擎替代 annopi/annotask 的断点续跑与 OOM 自适应重试 | 已解决，重写是浪费（ADR-010） |
| SRCOS 解析 `qstat` 跟踪任意子作业 | 复杂度高、收益低；用同步契约 + `doneWhen` 覆盖（ADR-006） |
| 条件分支 / 循环 / 动态 DAG / 嵌套子流程 | 「简单依赖串联」够用；Flow 可被引用已是免费副产品，UI 不开放（ADR-009） |
| 工具 UI 平台化（表单引擎 / 拖拽式参数面板） | 工具 UI 归开发者，SRCOS 只管投递（ADR-004） |
| docker daemon 依赖 / udocker / proot | 需要 root 或不是安全边界（ADR-014） |
| 多节点调度、跨机弹性 | 后端只有 `local` 与 `sge`；sge 的调度由 SGE 自己负责 |
| 原生 SaaS 多租户（组织/计费） | 面向实验室内部，非商用多租户 |
| **SRCOS 作为 MCP Client**（去调工具自己的 MCP server） | 工具的执行契约是 `work.sh`，MCP 是接口协议，不同层（ADR-019） |
| **为每个 SRCOS 注册用户建 OS 账号**（真实 UID 隔离） | 需要 root，与 ADR-014 冲突（ADR-021） |
| **让 agent 直接改 workspace**（第一期） | 确定性执行 + 可审计优先（ADR-019） |
| **把 dsh 变成硬依赖或核心路径** | dsh 只是「本地优先应用被代理」的一个已验证例子，没装 Node 时 SRCOS 必须照常工作（ADR-016） |
| **逆向 developer preview 写集成插件** | 未验证 + 上游会破坏性变更 + 与自带 viewer 重叠（ADR-016；2026-09-26 移除路 B） |
| 把 `type: path` 用于**环境**（conda / module / `.sif`） | 环境与数据要分开；`path` 只用于数据 storage（ADR-020/021） |

---

## 8. 待决策

| # | 问题 | 倾向 |
|---|---|---|
| 1 | 工具注册仓库：新建 `srcos-tools` 独立仓库，还是先放 `srcos/tools/`？ | 起步放 `srcos/tools/`，成熟后独立 |
| 2 | 容器化后端的具体选型：集群已装的是 apptainer 还是 enroot？ | 取决于集群现状；manifest 只差物化函数 |
| 3 | 集群的 `ssh 登录节点 → 计算节点` 是否免密可用？共享盘挂载点是哪个？ | 决定隧道方案与 rendezvous/镜像路径 |
| 4 | 登录节点是否允许长驻进程 / `systemd --user` 是否可用？ | 决定 `local` 驱动默认是否禁用、资源限制走 cgroup 还是 prlimit |
| 5 | `Flow` 的 `expose` 是否需要"多比较组"模式（对齐 annopi 的 `${cmp.*}`）？ | 首版不做，只做样本维度 |
| 6 | `storages.yaml` 的 `rw` 是否允许写共享盘？配额怎么做（XFS project quota / 单独卷 / 目录计数）？ | **数据盘是 ext4 不是 XFS → XFS project quota 排除**；倾向单独卷 + 目录计数（见 `docs/environments.md`） |
| 7 | `task` 的 `doneWhen` 探针是否需要内置常见类型（`file_exists` / `dir_nonempty` / `exit_code`）？ | 是，先内置这三个，其余留给工具自己写 |
| 9 | ~~MCP 第二期的 `submit` scope 粒度：按工具授权还是全局开关？~~ | ✅ **已决（2026-09-24）**：**工具 × 用户双维度** —— token 需 `submit` scope，`submit_tools` 白名单按工具收窄，owner 的 Grant 与配额仍是外层上界；实现见 ADR-019「第二期实现契约」 |
| 10 | ~~**「不用 root」是架构偏好还是环境限制？**~~ | ✅ **已决（2026-09-22）**：架构偏好。docker 最多作可选 backend，不得成为 `local` 必需项 |
| 11 | ~~**是否执行 bwrap 修复**（AppArmor profile）？~~ | ✅ **已执行并验证通过**（2026-09-22）；重跑 `scripts/probe-env.sh` 确认 |
| 12 | **真正的 SGE 登录节点在哪？** 需要在它上面也跑一次 `scripts/probe-env.sh` | 阻塞 `backend: sge` 的全部实现细节 |
| 13 | `runc 1.2.4` + `/etc/apparmor.d/runc` 已存在 → 是否能做无 root 容器化（ADR-014 的进阶方案）？ | 值得实测：`rootlesskit` + `runc` |
| 14 | **Phase 1 首个真实用例用哪个**：dsh(3080) / shiny-server(3838) / RStudio(8787)？ | 它们已在 node01 上运行，建议直接用现状验证，而非另造 `hello-fanout` |
| 15 | **`host_root` 可达性校验怎么做**：SRCOS 在注册 storage 时如何确认 OS 用户能读到（bind 不改变权限）？ | 倾向：注册时试读 + 报错时给 `setfacl` 建议（已实测 `setfacl -m u:$OS_USER:r-x` 有效） |

---

## 9. 变更记录

| 日期 | 变更 |
|---|---|
| 2026-09-22 | 建立 roadmap；完成 Phase 0（重命名 srcos、删 `site/`、`git init`）；定稿 ADR-001 ~ ADR-015 |
| 2026-09-22 | 定位固化（ADR-017）；补 ADR-011 的决定性证据；新增 ADR-016（dsh 三层集成）、ADR-018（一份 interface 三个前端）、ADR-019（MCP Server）、ADR-020（存储与路径）、ADR-021（OS 用户 vs 注册用户）；新建 `AGENTS.md`；Phase 重排（新增 3.5 MCP、5.5 dsh） |
| 2026-09-22 | 新增 `scripts/probe-env.sh` 与 `docs/environments.md`；完成 node01 探测（bwrap 被 AppArmor 拦、systemd-run --user 可用、数据盘是 ext4、node01 非 SGE 登录节点、shiny/RStudio/dsh 已在跑）；待决策扩到 13 项 |
| 2026-09-22 | **新增 `docs/handoff.md`（会话交接简报）** —— 目标 / 已验证事实 / 下一步 / **已踩的坑清单** / 代码地图 / 环境事实 / 验证脚本；roadmap §2 重写为准确状态并指向它；AGENTS.md 文档分工表加入 handoff 与 environments；README 增补「工具平台」章节（命令 / 配置 / 网页入口 / 工具开发者入口） |
| 2026-09-22 | **Phase 3 主体启动**：`Grant` 授权模型（默认拒绝 / 只有允许 / 组+用户+public / 通配 / 管理员绕过 / 聚合配额）+ `srcos grant` CLI；HTTP 面补齐（工具目录、机器可读 interface、路径浏览、提交、实例列表）+ 生成式表单 + `srcos-path-picker` 原语控件；`serve --tools-dir`；修掉 `parseFlagsLoose` 的假交错解析（Go flag 遇位置参数即停止） |
| 2026-09-22 | **Phase 2 主体完成**：`Backend`/`Handle` 抽象、`kind: service` 全链路、`StorageProvider`（`/Volumes/data`）、动态路由表、端口池、`Reconcile`、`Reaper`、`doneWhen` 探针、**SGE backend 架构**（qsub 翻译 / qstat -xml 解析含挂起区分 / rendezvous 控制通道 / ssh -L 数据通道）；新增 `internal/{storage,portpool,route}` 与 `internal/runtime/sge`；修掉 RLIMIT_NPROC 的语义错误（按 real UID 全系统计数，会连 bwrap 的 namespace 一起挡掉） |
| 2026-09-22 | **bwrap 修复已执行并验证通过**（AppArmor 按二进制授权，非全局关 sysctl）；确认「不用 root」是架构偏好（ADR-014 补前提）；**发现 userns 把 group 权限位变成人人可读** → `MountSpec` 粒度 = 数据可见范围，写入 `AGENTS.md` 安全不变式，并修正 ADR-021 的“Jail vs MountSpec 两个边界”表述；探测脚本修正误导（按二进制授权列表 + T4 权限折叠探测） |
| 2026-09-22 | **Phase 1 完成**：`tool-spec` 契约冻结、`hello-fanout` 示例、最小运行时（tool/job/sandbox/runtime 四包 + CLI）、node01 上 `local`+`bwrap` 端到端跑通；修掉实现期暴露的四个真问题（幂等粒度、xargs 转义、工具级 default 未生效、降级模式路径与 DBUS） |
| 2026-09-22 | **agent token 认证面完成（Phase 3.5 前置）** —— `internal/agenttoken`（只存 SHA-256、每次校验重读文件、失败关闭、使用时间落 `data/`）+ `srcos token create\|list\|revoke`（默认 90 天、`never`）+ `/api/*` 的 `Authorization: Bearer`（第一期只读，写接口需预留的 `submit`）+ 审计日志行 + `srcos del` 连带撤销；ADR-019 补「实现契约」小节 |
| 2026-09-22 | **修降级模式的「停不掉」**：`StartService` 成功后丢掉活跃 Handle（`StopService` 只能按名停），无 user systemd 时服务停不了、却被标 stopped —— 表现是 `go test ./internal/runtime/` 每轮泄漏 5 个 python3（连跑 4 次耗尽测试端口池）。改为在实例记录里持久化子进程 `pid` + `starttime`，`Local.StopUnit/UnitAlive` 在降级模式用它（信号前校验 starttime 防 pid 复用）；顺带让 `Reconcile` 在降级模式也能区分活着/已死 |
| 2026-09-22 | **Phase 3.5 完成：MCP Server（read-only）** —— `internal/mcp`（Streamable HTTP 无状态端点 /mcp、协议版本协商、JSON-RPC 错误分层、审计日志）+ `internal/inspect`（只读答案的唯一实现，REST API / HTML 页面 / MCP 三前端共用）+ `tool.Interface.JSONSchema()`（ADR-018 的第一处派生）+ 9 个只读工具（`srcos_` 前缀）；用官方 Python MCP SDK 客户端端到端验证；README 增补 MCP 章节与保留路径 |
| 2026-09-22 | **代理层接入动态路由表** —— 网关从实例记录重建路由表（启动同步 / 扫描同步 / 按需读记录），`/proxy/<user>/<tool>/` 可达；实例优先于静态卡片；`route.ParseTarget` 把「端点必须回环」变成单一入口（记录被手改也进不了表）；拨号失败即丢弃路由（`svc stop` 在另一进程执行时表现为 502 后 404）；裸路径 / Referer / 路由 cookie 三处解析统一走 `matchRouteForUser` |
| 2026-09-22 | **冷启动体验 + 自动回收**：`internal/activity`（write-behind 时间戳日志，agenttoken.Usage 改为它的薄封装）+ `Reaper.LastActive` + 网关写 `data/service-activity.yaml`（修掉 idleTTL 只看启动时间的缺陷，否则会回收正在使用的服务）+ `proxy.ActiveConns`（真实 WebSocket 计数）+ 网关启动 reconcile / 扫描 tick 自动 reap + 「启动中」进度页与失败说明页 |
| 2026-09-22 | **管理端**：`grant.Policy` 线程安全 + `ReplaceWith`（授权热加载）；`runtime.UnitUsage`/`UnitSampler`（systemd cgroup 或 /proc 的资源快照）+ `inspect.AdminInstances/AdminTools`（管理视图与只读模型共用）；`/api/admin/*`（实例总览/强制停止/日志/工具授权/组/管理员，仅管理员 + Origin 校验）；`/admin` 控制台（服务端渲染 + 少量 JS 动作）+ 仪表盘入口 |
| 2026-09-22 | **Phase 4 起步：流程契约 + 注册期校验** —— `docs/flow-spec.md`（schema / 类型兼容表 / 14 条校验 / 执行语义（规划）/ 与 annopi 对照 / 明确不做）+ `internal/flow`（DAG 拓扑序与环检测、连线类型兼容、必填输入闭环校验、`id@version` 精确匹配、节点必须 kind: task）+ `srcos flow list|validate` |
| 2026-09-22 | **Phase 4 主体：流程跑起来了** —— `internal/flow` 补 planner（样本表解析、`expose`×表×连线 → 每个 (节点×样本) 的 job、类型强制转换、`--param` 与 `output.<名>` 两种来源）+ 运行记录/布局（`/flow` 内建挂载、run id、样本段）+ `internal/flowrun`（拓扑序、AND、`when: always`、失败即停、`.sign` 续跑、样本表留档）+ `srcos flow run\|resume\|status --dry-run` |
| 2026-09-22 | **Phase 4 收尾**：流程级并发（`--concurrency`，默认 4，job 粒度）、`retry.max` 自动重试+退避（任务粒度）、`flow cancel`（跨进程标志文件 + 停掉在跑 job）、流程级配额；并发首次把运行时放到「两个 unit 同时开工」，因此修掉两个真 bug：**`BwrapProbe` 的缓存没有同步**（第二个调用者看到空的失败，报成无消息的 failed）与**任务不记录 pid**（当时任务用 `systemd-run --scope`，scope 的名字由 systemd 生成，按引用停不掉；2026-09-24 起任务改为自命名的瞬时 unit，见 ADR-022） |
| 2026-09-22 | **管理端流程画布**：`webui/`（Vite+React+TS，独立工程，产物 embed 进二进制，dist 全部 gitignore 只留 .gitkeep 所以无 Node 也能构建）+ `/admin/flows/<id>/edit` + `/api/admin/flows`（list/get/put/validate，仅管理员，路径由 id 推导）+ 拓扑分层自动布局 + 两步点连线（**服务端校验**，画布不重复实现类型规则）；顺带补一条契约规则：**连线即依赖**（画布自动补 depends_on，服务端拒绝手工删掉的情况） |
| 2026-09-23 | **Phase 5：`srcos://` 资源协议 + 自带 viewer** —— `internal/resource`（地址语法 + `patterns` 认领注册表，纯解析，与 `dsh-resource://` 同构 = 路 D 保险）+ `internal/inspect`（唯一解析入口：复用 `sandbox.Spec`/Jail，scope 用户相对、地址里无用户名，storage scope 走 ADR-020 闭包）+ `/api/resources`（元数据）/`raw`（字节，**永不 text/html**）/`html`（CSP sandbox）/`/view`（Go 模板 viewer：文本行号、服务端 Markdown、表格、图片、PDF、**sandbox iframe 的 HTML**、目录）+ viewer 注册表首批（`internal/web`）；storage id 保留 `home`/`workspace`；修掉一个真问题：`ResolveExisting` 把「文件不存在」报成「symlink escape」（见 handoff §4.2） |
| 2026-09-23 | **Phase 5：任务列表 + SSE 日志流** —— `internal/inspect/logstream.go`（`FollowLogs`：尾部回放 → 跟随 → 终态收尾；**半行不当作一行**、15s 心跳、日志未出现时等待）+ `GET /api/jobs/<id>/logs`（默认纯文本尾部，`?follow=1` 为 SSE）+ `/tasks`、`/tasks/<id>`（服务端渲染，无 JS 也能读；产物的 `srcos://` 地址一键进 viewer）；顺带修 `Address.Child` 造成的目录导航路径重复（浏览器 e2e 发现，见 handoff §4.6） |
| 2026-09-24 | **MCP 第二期：submit / cancel / run_flow** —— `internal/execute`（写入面唯一实现：校验 + 配额 + 投递 + 启动） `submit` scope 可签发（`srcos token create --scope submit --tool X` 按工具收窄，蕴含 read） REST `POST /api/jobs`（新增 `run`）/`/api/jobs/<id>/cancel`/`/api/flows/<id>/run` MCP 写入三件套（只对有 submit scope 的 token 列出） 每次写入一行审计；修掉两个真问题：实例在 submit 返回前不可寻址、取消被等待者盖成 failed（见 handoff §4.2） |
| 2026-09-24 | **任务队列消费者（ADR-022）** —— 「目录即队列」终于有了守护进程：`internal/execute/queue.go`（启动即时冲刷 + 提交唤醒 + 周期 tick；按 (用户,工具) 串行、`--task-workers` 全局上限）、`job.Claim`（O_EXCL 认领，网关/CLI/流程执行器互斥）、`pending` 不算资源占用（`State.ConsumesResources`）、队列跳过流程节点、`runtime.ReconcileTasks` 每 tick 结算失去等待者的任务；新增 `--task-workers` / `--no-task-drainer`；已知限制：重启后运行结束的运行拿不回退出码（scope 无 `ExecMainStatus`） |
| 2026-09-24 | **任务改以 systemd 瞬时 unit 运行 + 判定文件**（ADR-022 收尾）—— `systemd-run --unit --wait`（退出码仍同步返回，unit 给出稳定名字与 `is-active`/`stop` 句柄）、`-p StandardOutput=append:<log>`（**修掉一个一直存在的 bug**：unit 的 stdout 不继承我们的 fd，服务日志此前是空的）、`-p ExecStopPost=...` 把 `$EXIT_STATUS $SERVICE_RESULT` 写进 `<log>.verdict`（`systemctl show` 不能依赖：瞬时 unit 退出后 ~1s 就被回收）、`ReconcileTasks` 读判定文件并用同一个 `applyExitStatus` 落库（**重启后再结束的运行现在有真实退出码**）；顺带修掉每次启动泄漏一个日志 fd（task + service 两条路径） |
| 2026-09-24 | **agent token 自助页**（ADR-019 的自助那一半）—— `/tokens` 页面 + `/api/tokens`（GET 列表 / POST 新建 / DELETE 撤销）：列表服务端渲染，生成走 JSON 以免明文进 URL/历史；只给自己签、白名单只能收窄自己可见的工具、每次运行仍过 Grant 与配额；**管理凭据只认浏览器 session**（agent token 一律 403 —— 泄露的 submit token 不能给自己续期）；`ParseExpiry` 下沉到 `agenttoken` 供 CLI 与页面共用；每用户 token 上限 20 |
| 2026-09-24 | **画布：拖拽摆放 + `expose` 自动推导（ADR-023）** —— 坐标旁挂 `<flows>/<id>/layout.yaml`（不进契约、不跑校验、缺失即退回拓扑推导、只留现有节点）+ `PUT /api/admin/flows/<id>/layout`；编辑器视图带 `suggestedExpose`（与第 14 条闭环互补，写在同一个包）→ 画布一键补齐；前端改动见 `webui/src/{Canvas,layout,App}.tsx`（指针捕获拖动、去抖落盘、`touch-action: none`）；**顺带修正一处实现偏离**：校验的闭环原来忽略工具默认值（与 §2.6 原文及 `job.Validate` 不一致）；Playwright 验证：拖动 → 刷新后位置仍在、`layout.yaml` 有坐标而 `flow.yaml` 没有、补齐 expose → 草稿转合法 → 保存写回 |
| 2026-09-25 | **Phase 5.5 路 B：`@seqyuan/srcos-dsh`**（`integrations/dsh-plugin/`）—— 读 dsh 源码后实现的 `srcos` 资源协议插件：地址前缀替换（`srcos://X` ⇄ `dsh-resource://srcos/X`）、provider 帧语义（去重/失败帧/abort）、侧边栏 tab 类型 + 面板体、`build.mjs` 拼 `__ModuleLoader__` 信封（零 dsh 工具链）；SRCOS 侧给目录条目加 `addr`；ADR-016 补「路 B 实现注记」（**provider 在客户端侧**与原文措辞的更正、失败帧形状、bundle 形态、token 而非 cookie）；验证：16 个 Node 单测 + `test/live-run.sh` 对真实网关跑通 REST（in-dsh 加载未验证） |
| 2026-09-25 | 补 `integrations/dsh-plugin` 的**打包契约与装配测试**（替身 `__ModuleLoader__` 断言 dsh 会找的面、假 ctx 断言 `apply()` 注册 provider/tab/面板体），并加 `make dsh-plugin`；共 19 个 Node 用例 + 2 个 live（未配置网关自动跳过） |
| 2026-09-25 | **本轮小结（8 个提交）**：Phase 5 收工（`srcos://` 协议 + 自带 viewer、任务列表 + SSE 日志流、agent token 自助页、画布拖拽与 expose 推导）→ MCP 第二期（submit / cancel / run_flow，`submit` scope 工具×用户双维度）→ 任务队列消费者（ADR-022，提交即自动执行）→ 任务改 systemd 瞬时 unit + 判定文件（重启后仍有真实退出码）→ Phase 5.5 路 B（`@seqyuan/srcos-dsh`）。全程 node01 实测（curl / Playwright / 官方 MCP SDK），`go vet` + `go test ./... -race` 全绿。**唯一没验的**：在真实 dsh 里加载路 B 插件（§8 #16） |
| 2026-09-26 | **移除 dsh 路 B + 文档仓库卫生（技术债 P0）** —— 删除 `integrations/dsh-plugin/`（未验证 + 逆向 developer preview + 与自带 viewer 价值重叠；代码可从 git 历史 `93351fc` 恢复）与 `make dsh-plugin` 目标；**ADR-016 重写**为「dsh 只保留『本地优先应用被代理』这一条已验证用法」（`srcos://` 与 `dsh-resource://` 同构作为协议特性保留）；移除 Phase 5.5、待决策 #8/#16、下一步 #12/#13；goprox 时代的 7 份旧报告与 2 张截图归档到 `docs/archive/`（含 README 说明「不维护、不作为当前契约」） |
| 2026-09-26 | **`make e2e` 端到端回归网**（技术债 T5）—— `scripts/e2e.sh`：临时配置 + 临时端口（`mktemp` + 空闲端口），跑通 提交 → 网关队列消费 → systemd 瞬时 unit → `<log>.verdict` → 日志 → 实例记录 → HTTP 资源查看（`/api/resources` + `raw`）→ agent token 认证；退出时只停自己创建的 unit 并清理临时目录（不碰仓库 `config/` `data/`）。无 bwrap 时 `SRCOS_E2E_SANDBOX=none` |
| 2026-09-26 | **`sandbox: none` + `requires_storages` 注册期拒绝**（技术债 T3）—— storage 是 bind mount，只存在于 mount namespace；`none` 下沙箱路径在宿主上不存在，工具会以 `no such file` 失败并指向错误的问题。`tool.Validate` 直接拒绝该组合（tool-spec §9 / §5.4），连带修正 4 处自相矛盾的测试 fixture（补 `sandbox: bwrap`） |
| 2026-09-26 | **`make build` 前端产物守卫**（技术债 T4）—— `internal/web/dist/` 缺 `index.html` 时醒目警告（不阻断构建）：以前是构建静默通过、到画布页才发现没打包。修正 Makefile 里“占位 index.html 进版本库”的过时注释（实际只进 `.gitkeep`） |
| 2026-09-26 | **审计流第一期（scope A，ADR-024）** —— `internal/audit`（Event/Actor/Target/Request/Outcome + Recorder 追加写 `data/audit/audit-YYYY-MM-DD.jsonl`、按天轮转、nil no-op、参数脱敏、损坏行跳过）+ `srcos audit tail\|list`（--user/--action/--decision/--since/--limit/--json）；埋点：写入面 submit/cancel/run_flow（defer+具名返回值，拒绝路径不漏）、api 拒绝事件（CSRF/未认证/只读试写）、配置变更（grant/group/admins/token，API 与 CLI 两条门）、CLI job submit；`make e2e` 第 9 步断言 submit(allow)+scope(deny) 入流。**不做**：防篡改 / 管理页 / 生命周期审计 |
| 2026-09-26 | **拆 server.go + 消除隐式路由表契约**（技术债 T1/T7）—— `server.go` 1829 行拆为 server.go（组装/Options/mux，588）+ auth.go（登录/TOTP，326）+ proxy.go（代理路由，435）+ pages.go（页面/资源/任务/管理端，518）+ loops.go（后台循环，64）；`Options` 头注集中写清每个字段的零值语义；`NewWithOptions` 从 supervisor 派生 `route.Table` 并在传了另一张时明确日志（以前只有一句注释警告，编译期不保证）。纯重组，行为不变（`-race` + `make e2e` 全绿） |
| 2026-09-26 | **技术债 T6（拆 config 包）评估后不做** —— `UserConfig` 同时持有 `auth` 与 `services` 卡片是合理的（一个用户记录本就两样都有），不是「两种世界观混在一个包里」；拆包是纯命名 churn、风险大于收益。至此 P0/P1/P2 完成（T6 明确不做） |
| 2026-09-26 | **CI（`.github/workflows/ci.yml`）** —— 三个独立 job：`vet` + `test -race` + `make build`；`make e2e`（`SRCOS_E2E_SANDBOX=none`，GitHub runner 无 user systemd/bwrap）；前端 `pnpm install --frozen-lockfile` + `pnpm build`（lockfileVersion 9 / pnpm 10）。顺带修 e2e 的判定断言：改为按实例记录的 `limiter` 判断，而不是「systemd-run 二进制是否存在」——runner 上二进制在、user manager 不在，旧写法会误报 ADR-022 回归 |
| 2026-09-26 | **审计流第二期（1/4）：管理端查询页** —— `/admin/audit`（服务端渲染，GET 筛选表单：user/action/decision/since，最新在前；仅管理员，非管理员 403）+ `GET /api/admin/audit`（同一 `audit.Query`，脚本化）；`web.AuditPage`（自带样式块，与 admin 页同风格）+ 导航入口；`audit.ParseSince` 下沉供 CLI 与 API 共用。e2e 第 10 步：管理员登录 → API 返回 deny → 页面渲染 → 非管理员 403。剩：防篡改 / 生命周期审计 / 保留策略 |
| 2026-09-26 | **审计流第二期（2/4）：生命周期跃迁审计** —— `runtime.Options.Audit`；记录 `instance.done`（任务终态）、`instance.settled`（重启后凭 verdict 结算）、`instance.reaped`（回收 + reason）、`instance.orphaned`/`instance.adopted`（reconcile）、`instance.stopped`（管理端强制停 + CLI svc stop）；actor = 实例用户、kind=system；一个进程一个 Recorder（main.go 建，传给 supervisor 与 server）。e2e 断言 instance.done；runtime 单测锁定。剩：防篡改 / 保留策略 |
| 2026-09-26 | **审计流第二期（3/4）：保留策略** —— `audit.Stale`/`audit.Prune`/`audit.ParseKeep` + `srcos audit prune --keep 90d [--dry-run]`；**显式、不自动**（删除审计是运维的决定，不是 tick 的副作用；`keep<=0` 不删任何东西），删除这件事本身记 `audit.prune`（谁、keep、删了几个、oldest/newest）。单测锁定「只删过期整天文件」与 keep=0 no-op。剩：防篡改 |
| 2026-09-26 | **审计流第二期（4/4）：防篡改 hash chain** —— 每行 `prev`/`hash`（SHA-256(prev+该行字节)），**每文件**一条链；`srcos audit verify` + `/admin/audit` 横幅；能发现改行/删行/乱序/剥链字段，不能证明整文件未删、挡不住重写全部文件。**e2e 抓到的真 bug**：网关与 CLI 两个进程各持自己的「上一行」，交错写同一日文件链必断 —— 改为每次追加 `flock` + 重读文件尾（链属于文件不属于进程），单测 `TestChainSurvivesInterleavedRecorders` 锁定。审计流第二期完成；仅真实签名未做 |
| 2026-09-26 | **管理端启动服务（B/1）** —— 控制台以前能停不能起。`runtime.Runner.ForUser`（浅拷贝共享端口池/路由表/后端/存储/审计，只换 User）+ `POST /api/admin/instances`（{user,tool}，仅管理员，先停后起，审计 `instance.started`）+ `/admin` 表单（用户×service 工具下拉）。e2e 新增第 11 步：管理员启动 `e2e-web`（python http.server）→ 路由经 `/proxy/<user>/<tool>/` 200 → 审计 started → 停止 → 审计 stopped；**这是 e2e 首次覆盖 service 路径**。剩：storages 编辑 / 申请审批流 |
| 2026-09-26 | **B3：工具访问申请/审批流**（设计已确认 → 已实现）—— `grant.requestable` 显式开关（只公开存在性，默认 false；`Policy.Requestable` 里具体 grant 优先于 `*`）+ `internal/accessrequest`（`data/requests/*.yaml`，幂等/状态机/原子写/0600）+ API（`GET/POST /api/requests` 只认 session；`GET /api/admin/requests`、`POST /api/admin/requests/<id>/approve|deny`；批准先写 grant 并落盘再标记 decided）+ UI（`/tools` 可申请区 + `/requests` 用户页 + `/admin` 待审区）+ 审计 `request.*`。**e2e 抓到一处遗漏**：`/api/requests` 未注册进 server mux 的 srcos API 路径表 → 请求掉进 `/api/` 代理回退 404；第 12 步现覆盖 申请→待审→批准→可见→审计 |
| 2026-09-26 | **A2：提交幂等键** —— `job.IdempotentID(user,tool,key)` + `job.SubmitAs`（已有 job.json 则报 `ErrJobExists`，不覆盖）+ `execute.SubmitRequest.IdempotencyKey`（重放时直接返回首次的 job/instance + 终态）；API `POST /api/jobs` 的 `idempotencyKey`、MCP `srcos_submit_job` 的 `idempotency_key`；重放审计带 `idempotent_replay=true`。e2e 第 13 步：同 key 同 job、异 key 新 job |
| 2026-09-26 | **A1：托管 agent 的实例身份**（ADR-025）—— `tool.AgentSpec`（`agent: {mcp, tools}`，只对 service 合法，注册期校验）+ `agenttoken.MintInstance/RevokeInstance/InstanceTokens` + `agenttoken.Chain`（用户令牌 + 实例令牌；新增 `ErrUnknownToken` 哨兵让 Chain 能区分「这个 store 没有」与「凭据无效」）+ `runtime.Options.AgentTokens`（StartService 签发到 `$HOME/.srcos/agent-token` 0600、StopService 撤销）+ 网关启动清扫死实例的令牌 + `data/agent-tokens.yaml`（`config.InstanceTokensPath`，与人的令牌分文件）+ `audit.Actor.Instance`。e2e 第 11 步：启动 e2e-web（带 `agent:`）→ 凭证落在 home 且可用 → 停止后 401 → 审计有 `agenttoken.revoke` |
| 2026-09-26 | **第一个真实服务工具 `srcos-tools/shiny-demo`** —— R Shiny 应用作为 `kind: service` 实例。做它的时候撞出并修了**三个真问题**：① 路由的 `BackendPath` 取自 healthcheck 路径（探活不在根的服务经代理全 404，见上一条）；② `tool.yaml` 的 `env:` 从未生效（见上一条）；③ 沙箱里挂 `/pmo/miniforge3` 会让 R 的启动脚本报 `bin/sed: not found` —— `/pmo` 是指向 `/Volumes/data/pmo` 的符号链接，R 内部用真实路径，必须挂真实路径（记入 environments.md）。**关键结论：Shiny 在 `/proxy/<用户>/<工具>/` 前缀下可直接工作** —— 它从 `window.location.pathname` 推导 base，资源与 WebSocket 自动落回前缀，不需要 base-path 配置。验证：headless Chromium 打开 `/proxy/alice/shiny-demo/` → 标题、直方图（经 WebSocket 送达）、实例身份、零 console 错误；前缀下 WebSocket 101。CI 加 `tool validate --tools-dir srcos-tools` |
| 2026-09-26 | **`SRCOS_API` 注入 + shiny 用实例凭据调 SRCOS（A1 的首个真实消费者）** —— `SRCOS_API` 是 roadmap §4.4 写了、代码里一直**没有**的「工具回调平台的 REST 通道」：`config.GatewayAPIBase`（网关进程用自己的监听端口，否则读 state.yaml 的记录端口；都没有则不注入）+ `PathView.APIBase` → `SRCOS_API`。shiny-demo 声明 `agent: {mcp: [read]}`，app.R 用 `$HOME/.srcos/agent-token` 调 `$SRCOS_API/tools` 并把结果显示在页面上。**实测**：headless Chromium 显示「实例凭据可用（只读）——SRCOS 回了 2 个工具」；网关日志 `agent alice via token <id> (instance alice-shiny-demo-svc) [read] GET /api/tools` —— 调用归属到实例；停止后凭据文件被删 + 审计 `agenttoken.revoke` |
| 2026-09-26 | **审计外发（信任锚）** —— `audit.Sink`（`HTTPSink`：每事件 POST 一次）+ `audit.Forwarder`（本地 spool 一事件一文件 → 后台**按序**发送、成功才删 → 采集端故障保留重试 → 超 `forward_max` 丢最旧并计数）+ `Recorder.ForwardTo/ForwardStatus`；`state.yaml` 的 `audit.forward_url` / `forward_token` / `forward_max`；`/admin/audit` 显示待发送/已丢弃。**本地签名明确不做**（密钥在同一个写域，挡不住能重写全部文件的人，只是看起来像防篡改）。e2e 第 14 步：起 python 采集端 → 预写 state.yaml → 断言收到事件且带 `hash`（该步又抓到一个真 bug：端口变量带了尾空格 → URL 非法） |
| 2026-09-26 | **专题指南 `docs/agent-mcp-positioning.md`**（D）—— 把 agent/dsh/MCP 的定位讨论落成文件：agent 的三层含义（外部/托管/dsh 内）、两条通道（MCP vs `srcos://`+REST）、三条不做（MCP Client / 直改 workspace / 推理循环）、优缺点表（A1 托管 agent 无实例身份、A2 agent 无幂等键、M1 `srcos://` 而非 MCP resources…）与建议；dsh 四个接触点与「为什么删路 B」。结论指向 ADR-011/016/017/019/024，不新增决策 |
| 2026-09-26 | **B2（`storages.yaml` 管理端编辑）评估后不做** —— 网关在**构造时**把 storage provider 交给 api/execute/runtime，要即时生效需先把它做成可原地替换的（同 grants 的 `ReplaceWith`）；是可行的，但**价值偏低**（storages 很少变，重启一次可接受），而 UI 编辑会碰**安全边界**（`host_root` 的粒度就是数据可见范围，表单无法知道正确粒度）。结论：保留「手写文件 + 重启」；若将来真需要，做「编辑 + 明确提示需重启」而不做热加载 |
| 2026-09-26 | **shiny-demo 加 submit：托管 agent 用实例凭据真的提交任务** —— `agent: {mcp: [read, submit], tools: [hello-fanout]}`；app.R 点按钮 → `POST $SRCOS_API/jobs`（带**幂等键** A2）→ job `k-…` pending → 刷新显示 `succeeded`。**A1 + A2 + 审计在一个真实应用里合流**。实测（headless Chromium，两用户 ops/alice）：`agent alice via token <id> (instance alice-shiny-demo-svc) [read,submit] POST /api/jobs`；审计 `allow submit … tool hello-fanout@0.1.0 instance=… job=k-… {samples=S01,S02,S03}`；同一凭据提交白名单外的工具 → **403**（reason 带白名单）且审计记 `deny` |
| 2026-09-26 | **统一服务实例化 · 步骤 1：声明式 `command:`**（`docs/plans/2026-09-26-unified-service-instantiation-design.md`）—— `tool.yaml` 新增 `command`（argv，**直接 exec 不经 shell**），与 `entry` **恰好二选一**；`${NAME}` 从本单元自己的环境展开（**与环境同源**，所以两者不会对不上），且只允许 SRCOS 给这个 `kind` 注入的变量（service 有端口/实例 id，task 有 job root/task id），错名/未闭合括号 **注册期就报**。`ResolveArgv` 改为惰性解析（命令可能引用 `${SRCOS_PORT}`，而端口在 prepare 之后才分配）；优先级：`job.json.command` > job 目录 `work.sh` > 工具的 `command` > 工具的 `entry`。**首个样例 `shiny-demo` 删掉 work.sh 改为 `command:`**（`R --vanilla -e "shiny::runApp('${SRCOS_WORKSPACE}', …, port=${SRCOS_PORT})"`），真实浏览器端到端仍 PASS |
| 2026-09-26 | **统一服务实例化 · 步骤 2/3：`environment:` 具名环境** —— `internal/environment`（与 `storage` 同构的第二个 provider）：`config/environments.yaml` 由**管理端**声明 `root` / `env` / `provides`，工具只写 `environment: <id>`。**宿主路径从工具包里搬出去了** —— 工具包不再包含这台机器的事实（原来 shiny-demo 里写死 `/Volumes/data/pmo/miniforge3`）。root **按宿主路径本身**只读挂载（解释器内部用绝对路径；实测过挂 `/pmo` 会报 `bin/sed: not found`）；环境的 `env` 插在平台默认与工具 `env:` 之间；`provides` 把「R 装了但没 shiny」提前到注册期。**两级校验**（对齐 storage 的 `CheckReachable`）：未声明的 id → 注册期硬失败（机器无关，CI 能拦）；宿主不可达 → 警告 + 启动硬失败。接线覆盖 `buildRunner` / `execute` / `api` / `server`（网关启动时打印不可达环境）。首个样例 `shiny-demo` 删掉 `ro_mounts` + `env:` 改为 `environment: r-miniforge`，真实浏览器端到端仍 PASS。（`config/environments.example.yaml` 已进版本库） |
