# SRCOS 工具契约（Tool Spec）

> 本文档面向**工具开发者**，定义工具与 SRCOS 之间的全部契约。
> 设计取舍与理由见 [`roadmap.md`](roadmap.md)；仓库不变式见 [`../AGENTS.md`](../AGENTS.md)。
>
> **状态：Phase 1 草稿 —— `interface` 的 `type` 全集在此冻结，之后只增不改。**

---

## 0. 一句话理解

> 一个工具 = **一个 `work.sh`（函数体）+ 一份 `interface`（类型签名）+ 一份 `tool.yaml`（执行约束）**。

SRCOS 不认识工具的实现，工具也不需要知道 SRCOS 的内部结构。两者唯一的接触面就是这三个文件。

### 0.1 职责边界

| 谁负责 | 什么 |
|---|---|
| **工具开发者** | ① `work.sh` 的实际工作逻辑；② `interface` 的类型签名；③ 工具的业务 UI（可选，shiny / python / R / 静态页皆可）；④ 声明真实的资源需求与 backend |
| **SRCOS 平台** | ① 实例化与生命周期（起停、探活、回收、崩溃重启）；② 资源限制与配额；③ 沙箱与挂载（虚拟 home / workspace / storage）；④ 认证、授权、审计；⑤ 投递与状态、日志收集、产物登记；⑥ 从 `interface` 自动派生 agent（MCP）、编排画布、用户表单三个前端 |
| **两者都不做** | 平台不管工具 UI；工具不管进程管理、资源限制、路径挂载 |

### 0.2 两种 `kind`

| | `kind: service` | `kind: task` |
|---|---|---|
| 形态 | 长驻服务（shiny、Jupyter、agent…） | 一次性任务（跑完即止） |
| 生命周期 | 无限（`lifecycle.maxLifetime` 封顶），崩溃自动重启 | `resources.walltime` 封顶，跑完即止，不重启 |
| 对外端口 | 有（`ingress.port`，网关代理） | 无（只有退出码） |
| 提交路径 / 资源限额 / 挂载隔离 | **完全相同** | **完全相同** |

**一个工具可以同时提供两者**：例如 `shiny-qc` 是 `service`（用户在页面上配参数），而它产生的运行请求是 `task`。
两者共享同一个 workspace，因此 `service` 写下的数据，`task` 直接可读。

> **注意**：`service` 与 `task` 是**两个独立的工具注册项**（两个 `tool.yaml`）还是一个 `tool.yaml` 带两个 `kind`，
> 由 Phase 1 的实现决定；本 spec 按**一个 `tool.yaml` 一个 `kind`** 描述（更简单）。
> 需要"UI + 执行"成对时，注册两个工具（如 `shiny-qc` 与 `qc-run`），让前者产生后者的任务。

---

## 1. 工具目录结构

```
srcos-tools/<tool-id>/
├── tool.yaml                 # 必需，唯一契约入口
├── work.sh                   # 必需，entry 指向的执行入口
├── workspace-template/       # 可选，首次实例化时 copy 进 /workspace（只做一次）
├── home-template/            # 可选，首次实例化时 copy 进 /home/<user>（只做一次）
├── ui/                       # 可选，工具自建 UI（SRCOS 不解析，只在文档里说明怎么起）
│   └── app.py
├── README.md                 # 建议
└── data/                     # 可选，工具自带的静态数据（会被挂进沙箱）
```

`workspace-template/` 与 `home-template/` 只在**首次**初始化时展开：SRCOS 会在目标目录写下
`.srcos-initialized` 标记，之后不再覆盖。这样用户有意删掉的种子文件不会被静默恢复。

**检验标准**：把这个目录单独拷给别人，对方能否在不安装 SRCOS 的情况下理解它在干什么、需要什么环境？

---

## 2. `tool.yaml` 规范

### 2.1 完整字段

```yaml
schemaVersion: 1                    # 必需，当前为 1
id: cellranger                      # 必需，[a-z0-9][a-z0-9-]*，全局唯一
version: 1.2.3                      # 必需，semver
name: "Cell Ranger count"           # 必需，展示名（支持中文）
description: "10X 单细胞比对计数"     # 可选，≤500 字

kind: task                          # 必需，task | service

# ── 执行环境 ──────────────────────────────────────────────
backend: sge                        # 必需，local | sge（见 §6）
sandbox: apptainer                  # 可选，none | bwrap | apptainer（默认 none）
image: /share/srcos/img/x-7.2.0.sif # sandbox=apptainer 时必需
env:                                # 可选，执行前的环境准备（sandbox 为 none/bwrap 时生效）
  - "module load cellranger/7.2.0"
ro_mounts:                          # 可选，只读环境挂载（宿主真实路径 → 沙箱路径）
  - { host: /opt/conda,           sandbox_path: /opt/conda }
  - { host: /share/ref/GRCh38,    sandbox_path: /ref/GRCh38 }

# 解释器/依赖从哪来：引用 config/environments.yaml 里的一个具名环境。
# root 按宿主路径本身只读挂进沙箱（解释器内部用绝对路径），它的 env 插在
# 平台默认与工具自己的 env: 之间。宿主路径因此不再写进工具包。
environment: r-miniforge

# 启动方式：`command` 与 `entry` **二选一**。
#
# command：声明式 argv，**直接 exec、不经 shell**。`${NAME}` 从本单元自己的
# 环境展开（值就是 SRCOS 注入的那些，所以命令与环境不会对不上）。服务类工具的
# 常见形态——“起一个 app server”——用它就够了，不必写一个只为了读端口/找解释器
# 的 work.sh。
command:
  - "jupyter"
  - "lab"
  - "--no-browser"
  - "--ip"
  - "127.0.0.1"
  - "--port"
  - "${SRCOS_PORT}"
  - "--notebook-dir"
  - "${SRCOS_WORKSPACE}"
# entry：脚本逃生口，工具有循环/条件/多进程时用它（六条硬规范见 §4）
# entry: work.sh

# ── 契约 ──────────────────────────────────────────────────
interface:
  inputs:  [...]                    # 见 §2.2
  outputs: [...]                    # 见 §2.3
requires_storages: [cluster-share]  # 可选，数据 storage 需求（见 §5）

# ── 资源 ──────────────────────────────────────────────────
resources:
  cpu: 8                            # 必需，核数（整数）
  memory: "32Gi"                    # 必需，支持 Gi/Mi/G/M
  walltime: "4:00:00"               # task 必需，H:MM:SS
  queue: "sci.q"                    # 可选，sge 队列名
  gpu: 0                            # 可选，GPU 数（默认 0）

internal:                           # 可选，仅供校验，SRCOS 不执行
  executor: local                   # local | qsubsge
  parallelism: 5                    # executor=local 时的内部并行度

# ── kind=service 专属 ─────────────────────────────────────
ingress:                            # kind=service 必需
  # port 可选：它只对 sge 有意义（作业在计算节点上**偏好**的端口；两个服务作业
  # 可能落在同一节点，所以脚本会退回一个空闲端口，并把实际端口公布出去）。
  # local 下一律用端口池分配的 $SRCOS_PORT —— 工具必须监听它，省略即可。
  # port: 3838
  healthcheck:
    path: "/"                       # 探活路径（不要与 backend_path 搞混）
    timeout: "120s"                 # 单次探活超时
    startupGrace: "180s"            # 冷启动宽限期
  backend_path: "/app"               # 可选：后端自己期望的前缀（默认在它自己的根）
  websocket: true                   # 可选，默认 true：是否允许协议升级穿过代理
  bwlimit: 10485760                 # 可选，字节/秒，0 = 不限（与静态卡片共用同一套限速）
lifecycle:                          # kind=service 必需
  restart: always                   # always | on-failure | never
  maxLifetime: "12h"
  idleTTL: "1h"                     # sge backend 下被忽略（改由 h_rt 决定）

# ── 转发一个已经跑着的后端（另一种 kind: service 的形态）──────
# backend: external
# external: {host: 127.0.0.1, port: 3838}
# ingress: {healthcheck: {path: "/"}}
# 详见 §6.1：SRCOS 只发布路由，不启动/不停止/不回收

# ── 托管 agent 的凭据（仅 kind=service，可选）──────────────
# 声明后，SRCOS 在实例启动时签发一枚 agent token（scope 与白名单来自这里，
# 所以它是 owner 权限的子集），放到虚拟 home 的 $HOME/.srcos/agent-token
# （0600），实例停止时撤销。agent 拿它调 /mcp。见 ADR-025。
agent:
  mcp: [read]                       # read | submit（至少一个）
  tools: [scrna_qc]                 # 可选：收窄 submit 到这些工具（需含 submit）

# ── 完成判定（仅 task，逃生口）─────────────────────────────
completion:                         # 可选，见 §7.3
  doneWhen: { type: file_exists, path: "/workspace/out/S001/_SUCCESS" }
  doneWhenTimeout: "12:00:00"

# ── 初始化模板 ────────────────────────────────────────────
workspace:
  init_from: workspace-template/     # 可选，首次实例化时 copy
home:
  init_from: home-template/          # 可选

# ── 授权（也可集中放 config/grants.yaml）───────────────────
grants:
  allow_groups: [bio-team]
```

### 2.2 `interface.inputs[]`

| 字段 | 必需 | 说明 |
|---|---|---|
| `name` | ✅ | `[a-z0-9_][a-z0-9_-]*`，同一工具内唯一 |
| `type` | ✅ | 见下表 |
| `label` | — | UI 显示名（缺省用 `name`） |
| `description` | — | ≤500 字 |
| `required` | — | 默认 `false` |
| `default` | — | 默认值 |

**`type` 全集（本文档冻结，之后只增不改）：**

| `type` | 用户侧控件 | MCP schema | 附加字段 |
|---|---|---|---|
| `string` | 文本框 | `string` | — |
| `int` | 数字框 | `integer` | `min` / `max` |
| `float` | 数字框 | `number` | `min` / `max` |
| `bool` | 开关 | `boolean` | — |
| `enum` | 下拉框 | `string` | `values: [...]` **必需** |
| `file` | 文件输入 | `string` | `from`（可选） |
| `directory` | 目录输入 | `string` | `from`（可选） |
| `dirpath` | 目录输入 | `string` | 同 `directory`（annopi 兼容别名） |
| **`path`** | **路径选择器** | `string` | **`from`（必需）**、`select` |

`type: path` 的附加字段：

| 字段 | 必需 | 说明 |
|---|---|---|
| `from` | ✅ | 一个或多个 storage id（逗号分隔）；必须是 `requires_storages` 的子集 |
| `select` | — | `directory`（默认）\| `file` |

> **为什么需要 `path` 而不是复用 `directory`**：`path` 强制绑定 storage，从而保证
> 「UI 里能选到的路径」=「沙箱里挂载了的路径」=「工具声明的需求」。三者不一致就会出现"选了却跑不了"。
> 详见 [`roadmap.md`](roadmap.md) ADR-020。

### 2.3 `interface.outputs[]`

| 字段 | 必需 | 说明 |
|---|---|---|
| `name` | ✅ | 同上 |
| `type` | ✅ | `file` \| `directory`（输出只允许这两类） |
| `label` / `description` | — | 同上 |
| `provides` | — | 该输出目录下**保证产出**的文件名列表，用于编排连线校验与产物校验 |

### 2.4 版本与内容摘要（`version` 与 `digest`）

| | 是什么 | 谁写 | 记录在哪 |
|---|---|---|---|
| `version` | 人写的版本字符串（`0.1.0`） | 工具作者 | 流程可 pin `tool@version` |
| **`digest`** | 工具包内容的 sha256（manifest + 脚本 + 模板，按路径+权限+内容） | SRCOS 算 | 实例记录、审计事件、`tool list` / `/api/tools` |

**为什么两个都要**：`version` 是人写的声明，**它变了才说明问题**；而包里的文件可以改了而不改版本号。
`digest` 让「到底跑的哪份代码」变成可答的：审计里 `tool demo@0.1.0#a58e88795f2a` 前半是声称的版本，
后半是实际的字节。

**三条边界**：

- **`digest` 不是签名。** 它证明「内容与记录一致」，不证明「内容可信」—— 后者是审计的外部锚（ADR-024），
  两件事混在一起会把它说大。
- **包内不要放大文件。** 镜像 / conda 前缀走 `ro_mounts` 或 [`environment`](#541-环境挂载ro_mounts与数据-storage-的区别)；
  大文件让每次加载都要重算摘要。
- **构建缓存与版本控制元数据不参与摘要**（`__pycache__` / `node_modules` / `.git` / `.DS_Store` / `*.pyc`）——
  否则摘要在代码没动时也会变。

---

## 3. `job.json` 规范

工具 UI 往 `$SRCOS_JOB_DIR/<job-name>/` 写 `job.json` + `work.sh`，SRCOS 扫描目录发现任务。

`work.sh` 的来源有两条路，SRCOS 按以下顺序决定（**Phase 1 实现已冻结此规则**）：

| 顺序 | 条件 | 执行什么 | cwd |
|---|---|---|---|
| 1 | `job.json` 显式给了 `command` | 该 `command` | `$SRCOS_JOB_ROOT` |
| 2 | 任务目录里有 `work.sh` | `bash work.sh` | `$SRCOS_JOB_ROOT` |
| 3 | 都没有 | `bash /tool/<tool.yaml 的 entry>` | `$SRCOS_JOB_ROOT` |

规则 2 就是"工具 UI 为本次运行生成 `work.sh`"的通道；规则 3 是"直接用工具包里的入口"。
两者都以任务目录为 cwd，所以脚本可以引用同目录的兄弟文件而不写绝对路径。

```json
{
  "schemaVersion": 1,
  "name": "cellranger count S001",
  "command": ["bash", "work.sh"],
  "params": {
    "sample_id": "S001",
    "fastq_dir": "/workspace/raw/S001",
    "ref": "/data/share/ref/GRCh38"
  },
  "resources": { "cpu": 8, "memory": "32Gi", "walltime": "4:00:00" },
  "outputs": ["/workspace/out/S001"],
  "tags": { "sample": "S001", "step": "count" },
  "doneWhen": null
}
```

| 字段 | 必需 | 说明 |
|---|---|---|
| `schemaVersion` | ✅ | 当前 `1` |
| `name` | ✅ | 任务展示名，≤200 字 |
| `command` | — | 默认 `["bash", "work.sh"]`。**不要用 `sh -c` 包字符串** |
| `params` | — | 键必须匹配 `interface.inputs[].name`；SRCOS 会**校验**并拒绝未声明的键 |
| `resources` | — | 覆盖 `tool.yaml` 的默认值，**只能降不能升**（超出 `Grant` 配额直接拒绝） |
| `outputs` | — | 沙箱绝对路径列表，登记为产物供预览/连线 |
| `tags` | — | 键值对，用于任务列表筛选（如按 sample） |
| `doneWhen` | — | 覆盖 `tool.yaml` 的完成判定（§7.3） |

`outputs` 目前是**字面沙箱路径**，不支持 `${SRCOS_TASK_ID}` 之类的模板。
需要"每任务一个产物目录"时，把它做成子目录（如产物写到 `/workspace/out/${SRCOS_TASK_ID}/`，
`outputs` 仍声明父目录 `/workspace/out`）—— 见 §4.3 关于幂等粒度的说明。

**不提供的字段（有意为之）**：

- ❌ `mounts` —— 挂载由 `tool.yaml` 的 `requires_storages` + `ro_mounts` 决定，**工具不能临时加挂**（安全边界）
- ❌ `image` / `sandbox` / `backend` —— 执行环境由 `tool.yaml` 决定，不允许任务级覆盖
- ❌ `env` —— 不允许任务级注入环境变量（避免绕过配额或注入敏感信息）

### 3.1 CLI 等价通道

```bash
srcos job submit \
  -n "cellranger count S001" \
  --cpu 8 --mem 32G --time 4:00:00 \
  --param sample_id=S001 \
  --param fastq_dir=/workspace/raw/S001 \
  --tag sample=S001 \
  --output /workspace/out/S001 \
  work.sh
```

CLI 内部等价于往 `$SRCOS_JOB_DIR/<job-name>/` 写同一份 `job.json`，两者可混用。

---

## 4. 启动方式：`command:` 或 `entry:`，以及六条硬规范

**`command:`（声明式 argv，推荐）**

- argv **直接 exec，不经 shell**：不做引号/展开处理，也就没有注入面。
- `${NAME}` 只引用 SRCOS 为这个 `kind` 注入的变量（见 §4.1）；引用其它名字、或写错
  `${` 括号，**注册期就报错**（错误在注册时而不是运行时露出来）。
- 与 §4.1 的环境同源：展开值就是环境里的值。
- **服务类必读**：端口是 `${SRCOS_PORT}`（端口池分配的），命令必须监听它、绑 `127.0.0.1`；
  当开发现在本单元自己的根路径（SRCOS 转发前会剥掉 `/proxy/<用户>/<工具 id>` 前缀，
  所以应用**不需要** base-path 配置——Shiny 这类从 `location.pathname` 推导 base 的天然可用）。

**`entry:`（脚本逃生口）**

需要循环、条件、多个进程、或自己决定解释器时用它；SRCOS 以 `bash <工具目录>/<entry>` 运行它。
两者**恰好给一个**，都给或都不给都拒绝注册。

**优先级**（谁覆盖谁）：

1. `job.json` 的 `command`（提交期显式 argv，最高）
2. job 目录里的 `work.sh`（task 的提交期脚本）
3. 工具的 `command:`
4. 工具的 `entry:`

**六条硬规范**（对 `command:` 与 `entry:` 同样适用）：

| # | 规则 | 为什么 |
|---|---|---|
| 1 | **幂等** —— 先查输出或 `.sign` 是否存在，存在则直接退出 0 | SRCOS 会重试失败任务；非幂等会重复计算或损坏产物 |
| 2 | **参数化** —— 所有可变值从 `$SRCOS_PARAM_*` 或 `$1` 取，**不硬编码路径** | 同一份 `work.sh` 要在不同用户、不同集群、不同 storage 下复用 |
| 3 | **只写 `/workspace`**（和 `/home/<user>`） | 沙箱只挂载这两个可写路径；写别处会失败或污染环境 |
| 4 | **退出码即状态** —— `0` 成功，非 `0` 失败 | SRCOS 据此判定重试或标记失败 |
| 5 | **日志走 stdout/stderr** | SRCOS 自动收集并支持断线重看；不要自己写日志文件（写了也行，但 stdout 是规范） |
| 6 | **必须同步阻塞到所有实际工作完成** | 否则 SRCOS 看到"退出码 0"就立刻标记成功，而真正的工作还在跑（§7） |

**额外约束**：

- **不要后台化**（`&`、`nohup`、`disown`）—— 与第 6 条冲突
- **不要 `cd` 到 `/workspace` 之外**
- **不要依赖网络出站**（除非集群允许；HPC 计算节点常无外网）
- **临时文件放 `$TMPDIR` 或 `/tmp`**（沙箱内是 tmpfs），**大文件放 `/workspace`**

### 4.1 注入的环境变量

| 变量 | 沙箱内值 | 说明 |
|---|---|---|
| `SRCOS_WORKSPACE` | `/workspace` | 本工具的专属可写目录（跨任务持久） |
| `SRCOS_HOME` | `/home/<user>` | **虚拟 home**（跨工具持久，见 §8） |
| `HOME` | 同 `SRCOS_HOME` | 让 `.cache` / `.condarc` / `.config` 自动落进虚拟 home |
| `SRCOS_JOB_DIR` | `/workspace/jobs` | 任务提交扫描根（工具 UI 往这里写） |
| `SRCOS_JOB_ROOT` | `/workspace/jobs/<job-name>` | 本次任务目录，**也是 `work.sh` 的 cwd** |
| `SRCOS_USER` | 如 `alice` | SRCOS 注册用户名 |
| `SRCOS_TOOL` | 如 `cellranger` | 工具 id |
| `SRCOS_TASK_ID` | 如 `t-01J8…` | 本次任务 id |
| `SRCOS_TOOL_VERSION` | 如 `1.2.3` | 工具版本，便于脚本记录到产物里 |
| `SRCOS_INSTANCE_ID` | 如 `alice-web-svc` | 服务实例 id（service 注入，task 不注入） |
| `SRCOS_API` | 如 `http://127.0.0.1:30152/api` | **SRCOS 自己的 REST 基址**，供工具 UI / 托管 agent 回调平台；**本机无已知网关时不注入**（工具应能区分「没网关」与「调用失败」） |
| `SRCOS_PARAM_<NAME>` | 参数值 | `name` 大写、`-`→`_`。如 `sample_id` → `SRCOS_PARAM_SAMPLE_ID` |
| `TMPDIR` | `/tmp` | 沙箱内 tmpfs |
| `PATH` | 见下 | 含 `/opt/srcos/bin`，供工具自带的二进制 |

**两条容易踩的规则**：

1. **宿主环境不会被继承。** SRCOS 用 `--clearenv` 清空后只注入上表的内容——SRCOS 自己的进程环境
   可能含密钥，且工具不应该依赖它没有声明的环境。所以 `PATH`、`LANG`、`LD_LIBRARY_PATH`
   之类**都要显式声明**（`PATH` 由平台给，其余用 `env:` 或 `ro_mounts`）。

   > 注：`env:` 里**不得**出现 `HOME` 或 `SRCOS_*`（平台契约，注册期直接拒绝）。工具声明的条目
   > 会**追加在最后**，所以同名键以工具的值为准。

2. **托管 agent 的凭据**（ADR-025）：工具声明 `agent: { mcp: [read] }` 后，SRCOS 在实例启动时
   签发一枚**只读**实例凭据到 **`$HOME/.srcos/agent-token`**（0600），实例停止时撤销。
   用它调 `$SRCOS_API` 的只读端点——这类调用在审计里记为「这个实例做的」。
2. **工具自带的二进制放在 `/opt/srcos/bin`。** 它是沙箱 `PATH` 的第一项。
   不要挂到 `/usr/local/bin`、`/usr/bin` 之类的位置——那些目录（`/usr` `/bin` `/sbin` `/lib*`）
   是**只读绑定**的，bubblewrap 无法在其中创建挂载点，会报
   `Can't create file at ...: Read-only file system`。SRCOS 会在注册/运行前直接拒绝这种配置。

3. **`sandbox: none` 时上表的路径值是宿主路径**，不是 `/workspace` 这种契约路径
   —— 没有 mount namespace，那些路径在真实文件系统上并不存在。`PATH` 也沿用宿主的。
   **这正是不许硬编码路径的原因**：只用 `$SRCOS_*` 的 `work.sh` 在两种模式下都能跑。

**参数默认值**：`interface.inputs[].default` 由 SRCOS 填充进 `$SRCOS_PARAM_*`。
参数留空（空字符串）视为"用默认值"，不会把默认值覆盖成空。

参数同时以 `params.json` 落在 `$SRCOS_JOB_ROOT/params.json`（供不便读环境变量的场景）。

### 4.2 `work.sh` 模板

```bash
#!/usr/bin/env bash
set -euo pipefail

# ── 1. 取参数（规范 #2）──────────────────────────────────
SAMPLE_ID="${SRCOS_PARAM_SAMPLE_ID:?sample_id is required}"
FASTQ_DIR="${SRCOS_PARAM_FASTQ_DIR:?fastq_dir is required}"
REF="${SRCOS_PARAM_REF:?ref is required}"

OUT="${SRCOS_WORKSPACE}/out/${SAMPLE_ID}"
SIGN="${OUT}/.sign"

# ── 2. 幂等检查（规范 #1）────────────────────────────────
if [[ -f "${SIGN}" ]]; then
  echo "[skip] ${SAMPLE_ID} already done"
  exit 0
fi

mkdir -p "${OUT}"

# ── 3. 干活，日志走 stdout（规范 #5）──────────────────────
echo "[run] cellranger count ${SAMPLE_ID}"
cellranger count \
  --id="${SAMPLE_ID}" \
  --transcriptome="${REF}" \
  --fastqs="${FASTQ_DIR}" \
  --localcores="${SRCOS_CPU:-8}" \
  --localmem="${SRCOS_MEM_GB:-32}"

# ── 4. 只写 /workspace（规范 #3）─────────────────────────
mv "${SAMPLE_ID}"/* "${OUT}/"
rmdir "${SAMPLE_ID}"

# ── 5. 打完成标记（规范 #1 的另一半）─────────────────────
touch "${SIGN}"
echo "[done] ${SAMPLE_ID}"
```

### 4.2.1 幂等标记的粒度：按**任务**，不按**工具**

这是实现时踩到的第一个真坑：如果 `.sign` 放在**工具级**位置（例如
`/workspace/out/.sign`），那么第二次带**不同参数**的任务会被上一次的标记误判为"已完成"
而直接跳过 —— 静默地什么都没做，却报告成功。

**幂等的单位是「一次任务」**：

```bash
# ✅ 正确：按任务分派产物目录与标记
OUT="${SRCOS_WORKSPACE}/out/${SRCOS_TASK_ID}"
SIGN="${OUT}/.sign"

# ❌ 错误：工具级标记 —— 不同参数的任务会互相误判
# OUT="${SRCOS_WORKSPACE}/out"
# SIGN="${OUT}/.sign"
```

同一个任务重跑会命中自己的标记（正确跳过）；新任务则得到全新的目录。
注意 `SRCOS_TASK_ID` 在**同一次任务**下是稳定的（等于任务目录名），所以重跑语义正确。

### 4.3 样本级并行归工具自己

`work.sh` **可以在内部并行处理 N 个样本**，SRCOS 不感知也不干预：

```bash
# 用 ata（本地并行执行器）
ata -t 5 -i samples.cmds

# 或退化方案
xargs -P 5 -I{} bash -c 'process {}' < samples.txt
```

**但资源声明必须是聚合需求**：如果 5 个样本各要 4 核，节点要声明 `cpu: 20`。
SRCOS 只保证"给了你一个 20 核 / 40Gi 的沙箱"，内部怎么切分是工具的事。

---

## 5. 存储与路径（Storage）

### 5.1 概念

| 概念 | 谁声明 | 作用 |
|---|---|---|
| **storage** | 管理端在 `config/storages.yaml` 声明一次 | 一个可访问的数据根（集群共享盘、项目目录、云路径） |
| **`requires_storages`** | 工具在 `tool.yaml` 声明需求 | 让 SRCOS 在实例化时把 storage 挂进沙箱 |
| **`type: path` 参数** | 工具在 `interface.inputs` 声明 | 让用户在**已挂载的 storage** 里选一个路径 |

**闭环**：`path` 参数可选范围 = 已挂载的 storage = 工具声明的需求。三者始终一致。

### 5.2 用法

```yaml
# tool.yaml
requires_storages: [cluster-share]

interface:
  inputs:
    - name: ref
      type: path
      from: cluster-share          # 必须是 requires_storages 的子集
      select: directory
      required: true
      default: /data/share/ref/GRCh38
```

```bash
# work.sh 里直接用沙箱路径
REF="${SRCOS_PARAM_REF}"           # 如 /data/share/ref/GRCh38
cellranger count --transcriptome="${REF}" ...
```

### 5.3 路径的表述空间

**`/api/paths` 与参数值一律用沙箱路径**（工具在沙箱里直接可用）。
SRCOS 内部用 `Jail` 双向映射到宿主真实路径，**工具不需要知道宿主路径**。

### 5.4 storage 声明的硬要求（重要）
1. **`host_root` 必须是 bind 的最小粒度的路径。**
   bwrap 的 userns 会把未映射的 gid 折叠成 `65534`，而沙箱进程本身就在 `65534` 组里，
   因此**宿主的 `group` 权限位在沙箱内等于公开可读**。
   把 `host_root` 写成 `/share` 就等于把 `/share` 下**所有 group-readable 内容**
   （含其他项目组的 `drwxrwx---` 目录）都交给了沙箱，与 `Jail` 的子路径限制无关。
   → **粒度就是数据可见范围。需要多个目录就声明多个 storage。**
2. **`host_root` 必须对 SRCOS 的 OS 用户可达。** bind 不改变权限，只让路径可见；
   宿主不可读的内容，沙箱内也读不到（而 SRCOS 也无法预览）。
   若目标是组独占目录（`drwxrwx--- group`），推荐用 ACL 而非改组：

   ```bash
   sudo setfacl -R -m u:<SRCOS 的 OS 用户>:r-x /share/projectA
   ```
   （已实测有效；比 `chmod o+rx` 安全，比改组简单。）
3. `mode: ro` 的 storage 在沙箱内同时是 `ro` 挂载，工具无法写入。
4. **声明了 storage 就必须用 `sandbox: bwrap`。** storage 是 bind mount，只存在于 mount
   namespace 里；`sandbox: none` 下沙箱路径（如 `/data/ref`）在宿主上根本不存在，工具会以
   `no such file` 失败 —— 指向错误的问题。注册期直接拒绝这个组合（§9）。

### 5.4.1 环境挂载（`ro_mounts`）与数据 storage 的区别

`ro_mounts` 是**环境**，`requires_storages` 是**数据**，两者不要混用（AGENTS.md）：

| | `ro_mounts` | `requires_storages` |
|---|---|---|
| 写在哪 | 工具包的 `tool.yaml` | 管理端的 `storages.yaml` |
| 谁声明宿主路径 | 工具作者 | 管理员 |
| 典型内容 | conda 环境、`module` 树、`.sif`、工具自带二进制 | 共享参考数据、项目目录 |
| 用户可选吗 | 不可选 | `type: path` 参数从中选择 |
| 粒度 | 允许较粗（不含用户数据） | **必须最小粒度**（§5.4 第 1 条） |

`ro_mounts[].sandbox_path` **不能落在系统只读目录内**（`/usr` `/bin` `/sbin` `/lib*` 及
`/etc` 下的白名单文件），否则 bubblewrap 无法创建挂载点。工具自带二进制请用
`/opt/srcos/bin/<name>`（沙箱 `PATH` 第一项）。

**推荐用 `environment:` 而不是手写 `ro_mounts` + `env:`。** 宿主路径是**管理端的事实**，不是
工具的属性；写进工具包就意味着换一台机器要改工具包（包变成这台机器的副本）。两者同构：

| | `requires_storages`（数据） | `environment`（环境） |
|---|---|---|
| 声明处 | 管理端 `storages.yaml` | 管理端 `config/environments.yaml` |
| 工具侧 | `requires_storages: [id]` | `environment: id` |
| 挂载 | `host_root` → `sandbox_path`（可不同） | `root` → **同一路径**（解释器内部用绝对路径） |
| 环境变量 | — | 环境的 `env:` 插在平台默认与工具 `env:` 之间 |
| 注册期断言 | 声明的 storage 必须存在 | `provides: [R, Rscript]` 必须可执行 |
| 宿主不可达时 | 警告 + 启动硬失败 | 警告 + 启动硬失败 |

`ro_mounts` + `env:` 仍保留为**逃生口**（单机、临时、工具自带二进制）。
完整示例与三条硬要求见 [`config/environments.example.yaml`](../config/environments.example.yaml)。

### 5.4.2 HTTP 面（工具 UI 与 agent 共用）

平台把工具契约和路径浏览暴露成三个只读 GET 与一个提交 POST。工具自建 UI 直接调它们即可，
不需要懂 SRCOS 的内部结构：

| 方法 | 路径 | 作用 |
|---|---|---|
| `GET` | `/api/tools` | 当前用户可见的工具目录 |
| `GET` | `/api/tools/<id>` | **机器可读的 `interface`** + 该工具可选的 storages |
| `GET` | `/api/paths?tool=&input=&path=&select=&limit=&storage=` | 列目录（沙箱路径空间） |
| `POST` | `/api/jobs` | 提交任务（写 `job.json` 到投递目录；网关的任务队列会自动执行一次） |
| `GET` | `/api/jobs?tool=&kind=` | 本用户的实例列表（含 service 的 endpoint/route） |
| `POST` | `/api/jobs/<id>/cancel` | 停掉一个实例（幂等） |
| `POST` | `/api/flows/<id>/run` | 用 CSV 样本表展开并启动一个流程 |

**`/api/paths` 的安全约束（ADR-020 在 API 边缘的执行）**：可浏览的 storage 由
`tool` + `input` **推导**，不接受调用方自由指定。`storage=` 只能在**该 input 声明的**
`from` 列表内切换根，否则 403。所以「UI 能选到的」永远不可能超过「沙箱挂载的」。

`select` 缺省取自 input 的 `select`/`type`，因此 `select: directory` 的参数在 UI 里
根本看不到文件 —— 非法值不会被提供，而不是提交后被拒。

## 5.5 工具 UI 怎么接

| 工具 UI 是什么 | 怎么接 |
|---|---|
| 用 SRCOS 自动生成的表单 | 什么都不做 —— 访问 `/tools/<id>` 或从 `/tools` 进入；`type: path` 自动渲染成路径选择器 |
| 自建 shiny / python / R 页面 | 一行 `<script src="/assets/srcos-path-picker.js">` + 一个 `<srcos-path-picker tool="<工具>" input="<参数名>" name="<参数名>">`，或直接调 `GET /api/paths?tool=…&input=…&path=…` |

**只读参考数据强制 `mode: ro`**；`rw` 的 storage 有配额，不要往共享盘写大中间文件（写 `/workspace`）。

---

## 6. `backend` 选择

| 工具内部 executor | 应声明的 `backend` | `work.sh` 跑在 | 资源请求 | 同步阻塞 |
|---|---|---|---|---|
| `local`（ata / xargs 并行） | **`sge`** | 计算节点 | `-pe smp N`（N = 聚合核数） | 是（前台） |
| `qsubsge`（annotask 提交） | **`local`** | 登录节点（提交器） | 1 核 / 少量内存 | **否** → 必须用 `doneWhen`（§7.3） |
| 无并行单命令 | 任意 | 任意 | 单样本需求 | 是 |

> ⚠️ **非法组合**：`internal.executor: qsubsge` + `backend: sge`。
> 大多数 SGE 站点**禁止计算节点上嵌套 `qsub`**，所以"用 annotask 投递"的工具**必须**在登录节点当提交器。
> 注册时会交叉校验并直接报错。

### 6.1 `backend: external` —— 转发一个已经跑着的后端

有些需求不是「给我起一个」，而是「这个服务已经在跑，给我一条受管、可授权的路径」：

```yaml
kind: service
backend: external
external: {host: 127.0.0.1, port: 3838}   # 目标后端（已经存在）
ingress: {healthcheck: {path: "/"}}
# 不需要 command/entry/environment/resources/lifecycle
```

SRCOS **只发布路由**，不启动、不停止、不回收：

- `command` / `entry` / `environment` / `requires_storages` / `agent` 在这里是**死配置**，
  注册期直接拒绝（没有人会去跑它们）；`lifecycle` 也不需要（生命周期不归 SRCOS 管）
- **`svc stop` 只撤路由 + 把记录标 `stopped`**，进程继续跑 —— SRCOS 不杀不是它起的进程
- **空闲回收不适用**（`idleTTL`/`maxLifetime` 描述的是平台拥有的生命周期）
- 健康检查仍然做：探不到就启动失败（后端不在就是不在）
- 审计里记一条 `instance.forwarded`（带 endpoint）

**为什么它比静态卡片好**：同一条转发管线、同一个 `Grant` 授权模型、同一个审计流、同一个目录。
卡片仍然可用（用户可自助添加），但新东西应当写成工具。

**`external.host` 目前必须是回环**（`127.0.0.1` / `::1` / `localhost`）：路由表只收回环端点
（记录是本地进程可改的文件，回环限制是一道兵防线）。要转发**另一个主机**上的后端，
今天用静态卡片，或在前面放一个 `ssh -L` 转发器。

---

## 7. 完成判定

### 7.1 默认路径（推荐）

`work.sh` **同步阻塞到所有实际工作完成**，然后 `exit 0`。SRCOS 看到退出码 `0` 即标记成功。

### 7.2 为什么这条必须是硬规范

如果 `work.sh` 里是 `annotask qsubsge ...`（**提交器**），它会投递完立刻退出 `0`，
而 5 个样本的子作业还在跑 —— SRCOS 会**误判成功**。这是本文档里最容易踩的坑。

### 7.3 逃生口：`doneWhen` 探针

确实无法阻塞时（例如必须在登录节点投递后立刻释放），声明探针：

```yaml
completion:
  doneWhen: { type: file_exists, path: "/workspace/out/S001/_SUCCESS" }
  doneWhenTimeout: "12:00:00"
```

| `doneWhen.type` | 含义 |
|---|---|
| `file_exists` | `path` 存在即完成 |
| `dir_nonempty` | `path` 是非空目录即完成 |
| `exit_code` | 仅看退出码（等价于默认路径） |

语义：`work.sh` 退出码 `0` 只表示**投递成功**；SRCOS 进入 `submitted` 中间态并轮询探针，
探针满足才标记成功，超过 `doneWhenTimeout` 标记失败。

**要求**：完成标记必须在**所有子作业结束后**才出现（如 annotask 的 `.sign`）。

---

## 8. 沙箱内的文件系统视图

> **重要前提**：启动 `srcos serve` 的 **OS 用户** 与 SRCOS 的**注册用户**是两个概念。
> 注册用户没有系统账号、没有真实 home，其运行时视图完全由 SRCOS 构造。

| 沙箱内路径 | 内容 | 模式 | 来源 |
|---|---|---|---|
| `/workspace` | 本工具专属目录（跨任务持久） | rw | **内建** |
| `/home/<user>` | 虚拟 home（跨工具持久） | rw | **内建** |
| `/tmp` | tmpfs（任务结束即丢） | rw | **内建** |
| `/opt/conda`、`/ref/GRCh38`… | `ro_mounts` 声明 | ro | **环境挂载** |
| `/data/share`、`/data/project`… | `requires_storages` 对应 | ro/rw | **数据 storage** |

**推论**：

- `$HOME` 是**虚拟 home**，所以 `.cache` / `.condarc` / `.config` / `.jupyter` 天然隔离
  —— 不会污染 OS 用户的真实 home，也不会在并发实例间互踩
- **不要指望系统用户目录**：`/home/seqyuan` 之类的真实路径在沙箱里**不存在**
- **临时文件优先放 `$TMPDIR`**（快且自动清理），大文件放 `/workspace`

**已知限制**：所有实例以同一个 OS 用户身份运行，隔离靠 mount namespace + 路径校验，**不靠 Unix UID**。
SRCOS 保证"每个实例只挂自己的 workspace + 自己声明的 storage，绝不挂父目录"；
但工具**不要试图猜测或访问其他实例的路径** —— 即使某些情况下能够到，那也是 bug 不是特性。

---

## 9. 校验规则（注册时会拒绝的配置）

| 规则 | 违反后果 |
|---|---|
| `id` 全局唯一且合法 | 拒绝注册 |
| `command` 与 `entry` **恰好给一个**（都给/都不给都不行） | 拒绝注册 |
| `command[0]` 非空；`${...}` 只能引用 SRCOS 给这个 `kind` 注入的变量，括号必须闭合 | 拒绝注册（错在注册时，不是运行时） |
| `environment` 必须是 `config/environments.yaml` 里已声明的 id | 拒绝注册（机器无关，所以 CI 能拦） |
| `environment` 的 `root` 在本机不可读 / `provides` 缺失 | **警告**（不阻断）；启动实例时硬失败 |
| `kind: service` 必须有 `ingress` + `lifecycle`（`port` 可选） | 拒绝注册 |
| `kind: task` 不能有 `ingress` / `lifecycle`，必须有 `resources.walltime` | 拒绝注册 |
| `ingress.backend_path` 必须绝对；`ingress.port` ≥ 0；`ingress.bwlimit` ≥ 0 | 拒绝注册 |
| `backend: external` 必须有 `external: {host, port}`，且 host 为回环、port 在范围内 | 拒绝注册 |
| `backend: external` 不得声明 `command`/`entry`/`environment`/`requires_storages`/`agent`/`resources` | 拒绝注册（跑了也没人用） |
| `external` 只能配 `backend: external` | 拒绝注册 |
| `sandbox: apptainer` 必须有 `image` | 拒绝注册 |
| `sandbox: none` 不能有 `requires_storages` —— 声明的 storage 要 bind 进 mount namespace，而 `none` 不建 namespace | 拒绝注册 |
| `agent` 只对 `kind: service` 合法（托管 agent 是长驻单元） | 拒绝注册 |
| `agent.mcp` 必须含 `read`/`submit` 至少一个；`agent.tools` 需同时含 `submit` | 拒绝注册 |
| `internal.executor: qsubsge` + `backend: sge` | 拒绝注册（§6） |
| `type: path` 必须有 `from` | 拒绝注册 |
| `from` 必须是 `requires_storages` 的子集 | 拒绝注册 |
| `type: enum` 必须有 `values` | 拒绝注册 |
| `interface` 的 `name` 在同一工具内唯一 | 拒绝注册 |
| `outputs[].type` 只能是 `file` / `directory` | 拒绝注册 |
| `job.json.params` 含未声明的键 | 拒绝提交 |
| `job.json.resources` 超过 `Grant` 配额 | 拒绝提交 |
| `interface.inputs[].default` 不满足自己的 `type` / `min` / `max` / `values` | 拒绝注册 |
| `ro_mounts[].sandbox_path` 落在系统只读目录内（`/usr` `/bin` `/sbin` `/lib*`、`/etc` 白名单） | 拒绝运行（bubblewrap 无法在其中创建挂载点） |
| `MountSpec` 中出现嵌套/重叠的沙箱路径 | 拒绝运行（bind 父目录 = 交出父目录下所有 group-readable 内容） |
| `workspace.init_from` / `home.init_from` 指向不存在的目录 | 拒绝运行 |

---

## 10. 完整示例：`hello-fanout`

一个最小可跑的 `task` 工具，演示六条规范、样本级并行、`.sign` 幂等、产物声明。

```
srcos-tools/hello-fanout/
├── tool.yaml
├── work.sh
├── samples.txt
└── workspace-template/
    └── README.md
```

**`tool.yaml`**

```yaml
schemaVersion: 1
id: hello-fanout
version: 0.1.0
name: "Hello Fanout"
description: "最小示例：把 N 个样本各写一个文件，演示样本级并行与幂等"

kind: task
backend: sge                  # 内部用 local 并行（ata / xargs），所以投到计算节点
sandbox: bwrap
entry: work.sh

interface:
  inputs:
    - name: samples
      type: string
      label: "样本列表（逗号分隔）"
      required: true
      default: "S001,S002,S003,S004,S005"
    - name: parallel
      type: int
      label: "并行度"
      default: 5
      min: 1
      max: 20
  outputs:
    - name: outs
      type: directory
      label: "输出目录"
      provides: [S001.txt, S002.txt, S003.txt, S004.txt, S005.txt]

resources:
  cpu: 5                      # 聚合需求：5 个样本 × 1 核
  memory: "5Gi"
  walltime: "0:10:00"

internal:
  executor: local
  parallelism: 5

workspace:
  init_from: workspace-template/
```

**`work.sh`**

```bash
#!/usr/bin/env bash
set -euo pipefail

SAMPLES="${SRCOS_PARAM_SAMPLES:?samples is required}"
PARALLEL="${SRCOS_PARAM_PARALLEL:-5}"

OUT="${SRCOS_WORKSPACE}/out"
SIGN="${OUT}/.sign"
mkdir -p "${OUT}"

# 幂等：已完成则直接退出（规范 #1）
if [[ -f "${SIGN}" ]]; then
  echo "[skip] already done"
  exit 0
fi

# 生成任务清单
CMDS="$(mktemp)"
IFS=',' read -ra arr <<< "${SAMPLES}"
for s in "${arr[@]}"; do
  printf 'printf "hello %%s\\n" %q > %q\n' "${s}" "${OUT}/${s}.txt"
done > "${CMDS}"

# 样本级并行：优先用 ata，退化用 xargs（规范 #2 允许退化）
if command -v ata >/dev/null 2>&1; then
  echo "[run] ata -t ${PARALLEL}"
  ata -t "${PARALLEL}" -i "${CMDS}"
else
  echo "[run] xargs -P ${PARALLEL} (ata not found)"
  xargs -P "${PARALLEL}" -I{} bash -c '{}' < "${CMDS}"
fi
rm -f "${CMDS}"

# 完成标记：所有样本结束后才出现（规范 #1 的另一半）
touch "${SIGN}"
echo "[done] ${#arr[@]} samples → ${OUT}"
```

**触发方式**

```bash
# 方式一：CLI（等价于写 job.json）
srcos job submit -n "hello S001-S005" \
  --param samples=S001,S002,S003,S004,S005 \
  --param parallel=5 \
  --cpu 5 --mem 5G --time 0:10:00 \
  --output /workspace/out \
  --tag demo=1 \
  work.sh

# 方式二：工具 UI 往 $SRCOS_JOB_DIR/demo/ 写 job.json + work.sh
```

**预期结果**：任务列表出现一条 `hello S001-S005`，状态 `succeeded`，产物里有 `/workspace/out/S001.txt … S005.txt`，日志含 `[run]` 与 `[done]`。

---

## 11. 检查清单（提交前自查）

- [ ] `work.sh` 幂等吗？重跑第二次会不会重复计算或损坏产物？
- [ ] 所有可变值都从 `$SRCOS_PARAM_*` 取了吗？有没有硬编码 `/share/...` 这种宿主路径？
- [ ] 只写 `/workspace`（和虚拟 home）吗？有没有往 `/tmp` 写大文件？
- [ ] 退出码语义对吗？失败时返回非 0 吗？
- [ ] 日志走 stdout/stderr 吗？
- [ ] `work.sh` 会阻塞到所有工作完成吗？如果内部是提交器，声明 `doneWhen` 了吗？
- [ ] 没有 `&` / `nohup` 后台化吧？
- [ ] `resources` 是**聚合需求**吗（内部并行度乘过了）？
- [ ] `backend` 与 `internal.executor` 的组合合法吗（§6）？
- [ ] `type: path` 的 `from` 在 `requires_storages` 里吗？
- [ ] 把目录单独拷给别人，对方能看懂它在干什么、需要什么吗？

---

## 12. CLI 速查（Phase 1 已实现）

```bash
# 校验工具包（不传目录则扫描整个 tools-dir）
srcos tool validate --tools-dir srcos-tools
srcos tool validate --tools-dir srcos-tools hello-fanout
srcos tool list     --tools-dir srcos-tools

# 提交任务（写 job.json 到 <workspace>/jobs/<job-id>/，目录即队列）
srcos job submit --tools-dir srcos-tools \
  -n "hello S001-S005" --tool hello-fanout \
  --param samples=S001,S002,S003,S004,S005 --param prefix=hi \
  --tag demo=1 --output /workspace/out \
  --cpu 3 --mem 3Gi --time 0:05:00 \
  [work.sh]          # 可选：把这份脚本作为本次任务的 work.sh 拷进任务目录

# 执行（扫描投递目录）
srcos job run --tools-dir srcos-tools --tool hello-fanout
srcos job run --tools-dir srcos-tools --tool hello-fanout --job <job-id> --force
srcos job run --tools-dir srcos-tools --tool hello-fanout --sandbox none   # 调试降级模式

# 查看
srcos job list
srcos job status <instance-id|job-id>
srcos job logs   <instance-id|job-id>
```

- `--config-dir` / `-d` 决定 `config/` 与 `data/` 的位置（默认在二进制旁边）。
- `--tools-dir` 默认取 `$SRCOS_TOOLS_DIR`，再取二进制旁边的 `srcos-tools/`，最后 `tools/`。
- `--user` 默认取 `$SRCOS_USER` → `$USER` → `$LOGNAME`。
- `<job-id>` 支持松匹配：完整 id、slug 前缀、或末尾的 8 位十六进制后缀都可以。

## 13. Phase 1 实现状态

已实现（`internal/tool`、`internal/job`、`internal/sandbox`、`internal/runtime`、`cmd_job.go`）：

| 能力 | 状态 |
|---|---|
| `tool.yaml` 加载 + 全部 §9 校验规则 | ✅ |
| `job.json` 加载 + 参数/资源校验 + 目录扫描 | ✅ |
| 工具级 `default` 填充进 `SRCOS_PARAM_*` | ✅ |
| `MountSpec` + 路径解析（含 symlink 逃逸防护） | ✅ |
| `sandbox: bwrap` 物化（含 `--clearenv` 与 `/opt/srcos/bin`） | ✅ |
| `sandbox: none` 降级（并在实例记录里标注） | ✅ |
| 资源限制：systemd 瞬时 unit（`systemd-run --user --unit`）优先，`prlimit` 兜底 | ✅ |
| 实例记录、日志分离、`--force`、松匹配 | ✅ |
| 按任务分派的产物与 `.sign`（幂等粒度） | ✅ |
| 数据 storage（`requires_storages`） | ⛔ Phase 2 —— 声明了会明确报错 |
| `sandbox: apptainer` | ⛔ Phase 6 —— 声明了会明确报错 |
| `kind: service` 的 ingress/healthcheck/生命周期 | ⛔ Phase 2 |
| `doneWhen` 探针的轮询 | ⛔ Phase 2（字段已解析并校验） |
| `internal.executor: qsubsge` 的交叉校验 | ✅（注册期拒绝非法组合） |
