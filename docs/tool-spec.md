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
├── workspace-template/       # 可选，首次实例化时 copy 进 /workspace
├── home-template/            # 可选，首次实例化时 copy 进 /home/<user>
├── ui/                       # 可选，工具自建 UI（SRCOS 不解析，只在文档里说明怎么起）
│   └── app.py
├── README.md                 # 建议
└── data/                     # 可选，工具自带的静态数据（会被挂进沙箱）
```

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
entry: work.sh                      # 必需，工具目录下的入口文件

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
  port: 3838                        # 容器/沙箱内监听的端口
  healthcheck:
    path: "/"                       # 探活路径
    timeout: "120s"                 # 单次探活超时
    startupGrace: "180s"            # 冷启动宽限期
lifecycle:                          # kind=service 必需
  restart: always                   # always | on-failure | never
  maxLifetime: "12h"
  idleTTL: "1h"                     # sge backend 下被忽略（改由 h_rt 决定）

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

---

## 3. `job.json` 规范

工具 UI 往 `$SRCOS_JOB_DIR/<job-name>/` 写 `job.json` + `work.sh`，SRCOS 扫描目录发现任务。

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

## 4. `work.sh` 六条硬规范

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
| `SRCOS_PARAM_<NAME>` | 参数值 | `name` 大写、`-`→`_`。如 `sample_id` → `SRCOS_PARAM_SAMPLE_ID` |
| `TMPDIR` | `/tmp` | 沙箱内 tmpfs |

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

### 5.5 工具 UI 怎么接

| 工具 UI 是什么 | 怎么接 |
|---|---|
| 用 SRCOS 自动生成的表单 | 什么都不做 —— `type: path` 自动渲染成带「浏览」按钮的输入框 |
| 自建 shiny / python / R 页面 | 放一行 `<srcos-path-picker storage="cluster-share" select="directory">`，或直接调 `GET /api/paths?storage=…&path=…` |

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
| `kind: service` 必须有 `ingress` + `lifecycle` | 拒绝注册 |
| `kind: task` 不能有 `ingress` / `lifecycle`，必须有 `resources.walltime` | 拒绝注册 |
| `sandbox: apptainer` 必须有 `image` | 拒绝注册 |
| `internal.executor: qsubsge` + `backend: sge` | 拒绝注册（§6） |
| `type: path` 必须有 `from` | 拒绝注册 |
| `from` 必须是 `requires_storages` 的子集 | 拒绝注册 |
| `type: enum` 必须有 `values` | 拒绝注册 |
| `interface` 的 `name` 在同一工具内唯一 | 拒绝注册 |
| `outputs[].type` 只能是 `file` / `directory` | 拒绝注册 |
| `job.json.params` 含未声明的键 | 拒绝提交 |
| `job.json.resources` 超过 `Grant` 配额 | 拒绝提交 |

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
