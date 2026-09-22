# SRCOS Roadmap

> **本文档是 SRCOS 开发状态、优先级和明确延期范围的唯一来源。**
> 设计与实施历史散落在对话和草稿中的部分，以本文档为准。
> 文档只记录「为什么这么设计」和「做到哪了」，不替代 README（面向使用者）。

---

## 1. 定位与设计思想

**一句话**：SRCOS 把「一台服务器、一个 Web 入口、每人管理自己的转发」升级为
「**一个入口，上架工具，授权使用，人人有隔离工作区，工具被实例化管理**」——
一个面向实验室/HPC 的小型云工具平台。

### 1.1 沿用自原 goprox 的四条不变原则

| 原则 | 含义 |
|---|---|
| **单二进制、零依赖分发** | 拷贝一个文件就能跑，配置落在二进制旁边的 `config/`，可整体备份迁移 |
| **文件系统即数据库** | 声明式 YAML 配置 + 目录扫描，不引入外部数据库 |
| **后端只监听回环** | 网关是唯一入口，这是全部安全模型的基石 |
| **代理层与编排层解耦** | 编排层不碰 HTTP，代理层不碰容器，两者只通过「动态路由表」耦合 |

### 1.2 新增的三条

| 原则 | 含义 |
|---|---|
| **不碰工具 UI** | 工具界面由开发者用 python/R/shiny/任意方式自建；SRCOS 只提供投递与状态契约 |
| **不用需要 root 的工具** | 排除 docker daemon；优先 apptainer / bubblewrap 这类无 root 方案 |
| **一个运行原语** | 常驻服务与一次性任务是同一个 `RunUnit`，差异只有三个字段（见 ADR-003） |

### 1.3 参考过的项目

| 项目 | 借了什么 |
|---|---|
| `../ennote`（`ennoworker/internal/workspace/`） | 虚拟挂载三层抽象：挂载表 + `Jail` 路径解析 + 物化器；信任必须存于被信任对象之外 |
| `../annopi` | `task.yml` 模块规范、`pipeline.yml` 的 `tasks`+`dependencies`、`deps` 三层优先级、`annopi install xxx@v1.2.3` 的模块复用 |
| `../annotask`、`../ata` | 样本级并行执行器（工具内部用，SRCOS 不执行）、断点续跑 `.sign` 标记、OOM 自适应重试 |
| `../goqsub` | SGE 提交参数（`-pe smp`、`-l h_vmem`、`-q`、`-p`）的取值参考 |
| `deepseek-harness` | 文件预览的「资源地址协议 + viewer 注册表」设计（`dsh-resource://`），只借设计不借代码 |

---

## 2. 现状

### 2.1 已完成

- [x] **多用户认证反向代理网关**（继承自 goprox，功能完整）
  - 多用户共享端口，路径前缀隔离：`/proxy/<用户>/<服务路径>/`
  - bcrypt 密码 + 可选 TOTP 两步验证 + 登录限速
  - 轻量 SSO（身份头 + HMAC-SHA256 签名）
  - 原生 TLS（自签或自有证书）/ 可信反代 / Cloudflare 隧道
  - HTML `<base>` 注入、`crypto.randomUUID` polyfill、`Location` 改写
  - WebSocket/SSE 透传、per-service 带宽限制（`bwlimit`）
  - `backend_path` 三种映射形态、`default_service` 根路径托管、Referer/路由 cookie 兜底转发
  - 管理 API 的 CSRF（Origin）校验、`SafeDialContext` 防 SSRF/自环
- [x] **重命名为 srcos**（2026-09-22）：`go.mod` → `github.com/seqyuan/srcos`，CLI/日志/配置文件名/cookie 名/SSO 前缀全部改名
- [x] **删除 `site/` 静态文档站**；其中 `tunnel` 与 `dsh-demo` 两篇正文已转为 `docs/tunnel.md`、`docs/dsh-demo.md`
- [x] **建立 git 仓库**，首次提交
- [x] 基线校验：`go build` / `go vet` / `go test ./...` 全绿（8 个包）

### 2.2 进行中

- [ ] 无

### 2.3 下一步（Phase 1）

见 §6。当前阻塞点：需确认 §8 的待决策项，然后开始 Phase 1。

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
    - { name: ref,       type: dirpath, default: /share/ref/GRCh38 }
  outputs:
    - { name: outs, type: directory,
        provides: [filtered_feature_bc_matrix.h5, metrics_summary.csv] }

resources: { cpu: 8, memory: "32Gi", walltime: "4:00:00", queue: sci.q }
internal: { executor: local, parallelism: 5 }   # 供校验，SRCOS 不执行
```

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
- **背景**：`deepseek-harness` 的「文件预览」实际是一套**资源地址协议 + viewer 注册表**（`dsh-resource://<protocol>/<path>`，viewer 用 glob 认领地址，`open()` 返回帧流）。
- **决策**：借用地址协议与认领优先级规则，SRCOS 侧实现 `srcos://file/...` 与 `/api/resources` 统一解析（复用 `Jail` 做越权校验）+ 前端 viewer 注册表。**不引入 dsh 的代码**。
- **理由**：① dsh 的 viewer 是 Cordis 插件，依赖 `ctx.resources.register` / `useResource` / dsh 的 slot+renderer，与 SRCOS 的 Go 内嵌模板栈不通；② dsh 实际只实现了 text preview 与 attachment，缺表格/PDF/HTML，覆盖度不足。
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
- **决策**：排除 docker daemon、udocker/proot。隔离方案优先级：**apptainer/singularity**（HPC 事实标准，SIF 镜像 + `-B` + `--containall`）→ **bubblewrap**（无镜像、无镜像需求时的降级）→ 可选 Enroot。
- **理由**：部署在无 sudo 的登录节点。`proot` 是 ptrace 模拟、不是安全边界（`ennote` 的设计文档已明确此判断），不可退回去当隔离手段。
- **已知缺口**：无 root 下的资源限制需降级路径 —— `systemd-run --user --scope`（可用时首选）→ `prlimit`（`RLIMIT_AS`/`RLIMIT_CPU`/`RLIMIT_NPROC`，子进程继承）+ **轮询 RSS 超限 kill 整个进程组**。HPC 登录节点常禁用 `systemd --user`，所以降级路径不是可选项。

### ADR-015：HPC 上「实例是 Job 不是 Pod」
- **决策**：SGE 驱动的生命周期语义单独定义——资源限制交给 SGE（`-pe smp` / `-l h_vmem`），生命周期上限用 `-l h_rt`，停止用 `qdel`，**空闲回收默认关闭**（排队代价高），改为到期前预警 + `qalter` 续期。
- **必须处理**：SGE 会 `SIGSTOP` 挂起或 requeue 被抢占的作业，网关侧探活会失败——必须区分「作业被挂起（`qstat` 状态 `S`）」与「进程真的死了」，否则会误判 Failed 并疯狂重投。
- **网络接入**：**共享文件系统 rendezvous 文件当控制通道，SSH 本地转发当数据通道**——作业把 `node + port` 写进 `$HOME/.srcos/instances/<id>/endpoint`，登录节点读到后 `ssh -L <localport>:127.0.0.1:<toolport> <node>`，再注册进路由表。优先直连（网络允许时），`ssh -R` 反向隧道作兜底。
- **实现细节**：SGE 驱动用 `qsub`/`qstat`/`qdel` **命令行**而非 DRMAA（避免 CGO + `libdrmaa` 依赖），`qstat -xml` 解析比文本格式稳定。
- **合规提醒**：HPC 登录节点通常禁止跑长驻重负载进程。`local` 驱动应只用于轻量工具或管理员明确的例外，重工具一律走 `sge`。

---

## 6. 路线图

### Phase 0：基线 ✅
- [x] 重命名为 srcos
- [x] 删除 `site/`，保留 `docs/tunnel.md` / `docs/dsh-demo.md`
- [x] `git init` + 首次提交
- [x] 建立本 roadmap 文档

### Phase 1：规范与最小闭环（**当前**）
目标：用一个真实工具把 `job.json` / `work.sh` 规范验证一遍，再扩接口。
- [ ] `docs/tool-spec.md` —— 工具开发者视角的完整规范
      （`tool.yaml` + `job.json` + `work.sh` 六条规范 + 同步契约 + `doneWhen` 逃生口 + `backend` 选择对照表）
- [ ] `docs/flow-spec.md` —— 流程管理员视角（`Flow` 的 nodes/bindings/expose 语义与画布交互约定）
- [ ] 最小示例模块 `srcos-tools/hello-fanout/`：`work.sh` 用 `ata -t 5`（退化可用 `xargs -P5`）跑 5 个样本 + `.sign`
- [ ] `local` + `bwrap` 驱动上跑通端到端：提交 → 任务列表 → 日志 → 产物

### Phase 2：实例化运行时
- [ ] `Tool` / `Flow` / `RunUnit` / `TaskInstance` / `FlowRun` 的 Go 类型定义
- [ ] `Runtime` 接口 + `local` backend（含 cgroup/prlimit 降级路径）
- [ ] `MountSpec` + `Jail`（自 ennote 移植改造：去 skills 硬编码、加 ingress）
- [ ] `job.json` 落盘扫描器（目录即队列）+ `srcos job submit/list/status/logs/cancel` CLI
- [ ] 动态路由表 + 实例 registry + **启动时 reconcile**（清孤儿实例与端口）
- [ ] 端口池分配器（`127.0.0.1:20000-30000`）
- [ ] 「启动中」进度页 + 探活（端口就绪 + HTTP 200）
- [ ] 空闲回收 reaper（WebSocket 活跃期间视为不空闲）

### Phase 3：注册、授权与管理端
- [ ] 工具注册：扫描 `srcos-tools/`（→ 后续支持 registry 拉取）
- [ ] `Grant` 授权模型（用户/组 × 工具/流程 + 配额：max_cpu / max_memory / max_instances）
- [ ] 用户级配额聚合（防单用户开满）
- [ ] 管理端：工具上架/下架/授权、实例总览（CPU/内存实时采样、日志、强制停止）
- [ ] 审计日志

### Phase 4：云流程
- [ ] `Flow` 注册 + 校验（类型兼容、DAG 无环、`executor`×`backend` 交叉校验）
- [ ] 用户侧实例化：渲染 `expose` + 样本表 → `FlowRun` → 展开 `TaskInstance`
- [ ] 最小 DAG 调度器：拓扑序投递、`depends_on` AND、`when: on_success|always`
- [ ] 失败只重跑失败节点及下游；每流程并发上限；重试计数
- [ ] 断点续跑（状态表 + `.sign`）
- [ ] 把 `annopi` 注册为普通工具模块（逃生口）

### Phase 5：前端（`webui/` Vite 包）
- [ ] `webui/` 工程骨架 + `make webui` + `//go:embed dist`
- [ ] `srcos://` 地址协议 + `/api/resources` 统一解析
- [ ] viewer 注册表 + 首批 viewer：文本/代码（分页 + 行号）、Markdown、表格、图片、PDF、HTML（sandbox）
- [ ] 任务列表页 + 日志流（SSE）
- [ ] 流程编排画布（拖拽 + 类型校验连线 + `expose` 推导）

### Phase 6：HPC / SGE
- [ ] `sge` backend（`qsub`/`qstat -xml`/`qdel`）
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

---

## 8. 待决策

| # | 问题 | 倾向 |
|---|---|---|
| 1 | 工具注册仓库：新建 `srcos-tools` 独立仓库，还是先放 `srcos/tools/`？ | 起步放 `srcos/tools/`，成熟后独立 |
| 2 | 容器化后端的具体选型：集群已装的是 apptainer 还是 enroot？ | 取决于集群现状；manifest 只差物化函数 |
| 3 | 集群的 `ssh 登录节点 → 计算节点` 是否免密可用？共享盘挂载点是哪个？ | 决定隧道方案与 rendezvous/镜像路径 |
| 4 | 登录节点是否允许长驻进程 / `systemd --user` 是否可用？ | 决定 `local` 驱动默认是否禁用、资源限制走 cgroup 还是 prlimit |
| 5 | `Flow` 的 `expose` 是否需要"多比较组"模式（对齐 annopi 的 `${cmp.*}`）？ | 首版不做，只做样本维度 |

---

## 9. 变更记录

| 日期 | 变更 |
|---|---|
| 2026-09-22 | 建立 roadmap；完成 Phase 0（重命名 srcos、删 `site/`、`git init`）；定稿 ADR-001 ~ ADR-015 |
