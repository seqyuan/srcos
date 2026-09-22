# SRCOS 流程契约（Flow Spec）

> 本文是**流程管理员**要遵守的对外契约。工具开发者看 [`tool-spec.md`](tool-spec.md)，
> 架构约束看 [`../AGENTS.md`](../AGENTS.md)，决策记录看 [`roadmap.md`](roadmap.md)。
>
> 状态：**Phase 4 主体 —— schema、注册期校验、样本展开与顺序调度（`flow run` / `resume` / `status`）
> 已实现。** 标「规划」的小节是已定契约、待实现的语义（并发、重试退避、MCP `run_flow`），
> 实现进度以 roadmap §6 为准。

---

## 0. 一句话理解

> **流程 = 把已注册的工具按 `output → input` 连起来的一张 DAG。**
> 管理员**不写命令、不写代码**；用户只需填 `expose` 出来的参数 + 一张样本表。

三个角色，三份产物，互不越界（ADR-009）：

| 角色 | 产物 | 谁写 | 负责什么 |
|---|---|---|---|
| 工具开发者 | `tool.yaml` + `work.sh` | 会写代码的人 | **一个**函数体：怎么把输入变成输出 |
| 流程管理员 | `flow.yaml` | 生信/流程人员 | **怎么串**：节点、依赖、连线、暴露哪些参数 |
| 使用者 | 参数 + 样本表 | 非工程师 | **填什么**：每个样本一行，点运行 |

推论（也是本契约的全部理由）：流程里**没有 shell**。命令属于工具（`work.sh`），
编排属于流程。一旦允许流程写命令，工具契约、`interface` 类型校验、沙箱挂载、
授权配额全都会出现第二个定义。

---

## 1. 流程目录结构

与工具包同构：**一个流程一个目录**，目录名 = 流程 id。

```text
<flows-dir>/
└── scrna_basic/
    └── flow.yaml          # 唯一必需文件
```

- 默认 `<flows-dir>` = `<程序目录>/srcos-flows`（与 `srcos-tools` 并列），
  可用 `--flows-dir` / `$SRCOS_FLOWS_DIR` 覆盖（解析顺序与工具一致）。
- 流程只**引用**工具，不打包工具：节点指向工具目录里已注册的 `id@version`。

---

## 2. `flow.yaml` 规范

### 2.1 完整字段

```yaml
schemaVersion: 1
id: scrna_basic                 # 必填，小写字母/数字/-/_，与目录名一致
version: 0.1.0                  # 必填
name: "单细胞基础流程"            # 必填
description: "count → qc → report"   # 可选

# ── 节点：每个节点 = 一个已注册工具的一次运行 ──────────────────
nodes:
  - id: count                   # 必填，节点内唯一；**不能含 "."**（地址用它分段）
    tool: cellranger@1.2.3      # 必填，工具 id（可带 @版本，见 §2.3）
    depends_on: []              # 可选，上游节点 id 列表；AND 语义
    when: on_success            # 可选，on_success（默认）| always
    retry:                      # 可选，见 §5.3
      max: 0                    #   默认 0（不重试）

# ── 连线：上游 output → 下游 input ───────────────────────────
bindings:
  - from: count.outputs.outs    # <节点>.outputs.<输出名>
    to: qc.inputs.input_dir     # <节点>.inputs.<输入名>

# ── 暴露：哪些输入由用户（或样本表）提供 ─────────────────────
expose:
  - node: count
    input: sample_id
    from: sample.sample_id      # sample.<字段> = 样本表的一列
  - node: count
    input: transcriptome
    from: user                  # user = 运行时让用户填一次（所有样本共用）
```

### 2.2 节点字段

| 字段 | 必填 | 说明 |
|---|---|---|
| `id` | ✅ | 节点标识，`[a-z0-9][a-z0-9_-]*`，流程内唯一。**不允许 `.`** |
| `tool` | ✅ | 工具 id，可选 `@version` 后缀（§2.3）。工具必须是 `kind: task` |
| `depends_on` | — | 上游节点 id 列表。缺省/空 = 第一层 |
| `when` | — | `on_success`（默认）：上游全部成功才跑；`always`：无论上游成败都跑（报告类节点）。**不支持条件表达式** |
| `retry` | — | `{max: N}`，同一节点失败后最多重试 N 次（0 ≤ N ≤ 5）。**规划**：自动重试与退避尚未实现 |

> **服务不能当节点。** `kind: service` 的工具是长驻实例（有端口、生命周期、探活），
> 与 DAG 的「跑完就结束」语义不同；把它放进流程是注册期错误（§4）。

> **规划（未实现）**：节点级的 `resources` 覆盖（只能**下调**、不得超过工具声明的上限）。
> 首版不支持，工具声明即上限。

#### 节点的产物落在哪（平台决定，不是流程作者写）

流程**不允许**写路径模板（没有 `${...}`），所以每个节点产物的路径由**平台**按运行 id 推导：

```text
沙箱视角：/flow/runs/<run>/nodes/<节点>/<样本段>/<输出名>
宿主视角：data/flows/<用户>/runs/<run>/nodes/<节点>/<样本段>/<输出名>
```

- `/flow` 是**内建挂载**（rw，每用户一个），与 `/workspace`、`/home/<用户>` 同级 —— 它由 SRCOS 创建，
  不是 `storages.yaml` 里声明的数据 storage，也不出现在路径选择器里（ADR-021 / ADR-020 的边界）。
- `<样本段>` = `s01-S001` 这样的「行号-样本名」：行号保证唯一（两行同名样本不会互相覆盖产物），
  样本名让人看得懂。
- 因此**每条边都能解析**：`count.outputs.outs` 就是 `<该样本目录>/outs`，下游拿到的是同一个路径。
- 需要「告诉工具往哪写」时用 `expose[].from: output.<输出名>`（见 §2.5）。
- 同一个运行目录在**续跑**时被复用：路径只由 run id 推导，所以重跑读到的还是同一批输入。

### 2.3 `tool` 引用与版本

- `tool: cellranger` —— 用部署里该 id 的当前版本。
- `tool: cellranger@1.2.3` —— 要求**精确**匹配，否则注册期报错（流程的可复现性靠它）。
- 版本不匹配时报错而不是回退：静默用别的版本会让「同一流程两次运行结果不同」变得无法解释。

### 2.4 `bindings[]`：连线

地址语法两段固定：

```text
<节点 id>.outputs.<输出名>     上游
<节点 id>.inputs.<输入名>      下游
```

规则：

1. `from` / `to` 都必须指向**存在的**节点，且名字必须在该工具的 `interface` 里存在。
2. **一个输入最多被一条 binding 提供**；同一个输出可以喂多个下游（扇出）。
3. 方向必须是 `outputs → inputs`（不能反过来、不能 outputs→outputs）。
4. **类型必须兼容**（§3）。

### 2.5 `expose[]`：暴露给用户的输入

| 字段 | 说明 |
|---|---|
| `node` / `input` | 被暴露的输入，必须存在 |
| `from` | `user`（运行时填一次，所有样本共用）、`sample.<字段>`（样本表的一列），或 `output.<输出名>`（**本节点自己的产物路径**，用来告诉工具往哪写） |

- `sample.<字段>` 意味着**这个流程按样本展开**：用户上传样本表，一列 `sample_id`，每个样本一次运行。
- 一个流程可以混合：`sample.*` 的参数按样本变化，`user` 的是全局参数（参考基因组、阈值）。
> `from: output.<输出名>` 是「同一个连线机制的另一端」：`bindings` 把上游的产物交给下游，
> 它把**本节点**的产物路径交给本节点自己的参数（工具总得被告知往哪写）。
> 两者都不需要模板语法，因为路径由平台按 run id 推导（见 §2.2 末）。

- **规划（未实现）**：`${cmp.*}` 式的「多比较组」维度（对齐 annopi 的层次 2）。
  首版只做样本维度（roadmap §8 #5）。

### 2.6 输入必须被满足（闭环）

每个节点的**每个 required 输入**，必须恰好被下面三者之一提供：

| 来源 | 写法 | 谁决定 |
|---|---|---|
| 工具默认值 | `interface.inputs[].default` | 工具开发者 |
| 上游连线 | `bindings[]` | 流程管理员 |
| 暴露给用户 | `expose[]`（`user` / `sample.<字段>`） | 使用者（或样本表） |
| 节点自己的产物路径 | `expose[].from: output.<名>` | 平台（路径推导，见 §2.2） |

**三者都没有 → 注册期报错**；**两个以上都提供 → 注册期报错**（`default` 与显式来源同时存在时以显式来源为准，
但显式来源之间不许重叠）。

这条闭环是「用户侧零代码」的前提：流程一旦注册成功，就一定能被实例化成一组可执行的任务。

---

## 3. 类型兼容表

工具 `interface` 的类型集合是冻结的（[`tool-spec.md`](tool-spec.md) §2.2）。
**输出只有 `file` / `directory` 两种**，所以连线规则很小：

| 上游输出 | 可连的下游输入 |
|---|---|
| `directory` | `directory`、`dirpath`、`path`（`select: directory` 或无 `select`） |
| `file` | `file`、`path`（`select: file` 或无 `select`） |

- **`path` 的 `select` 必须与上游类型一致**：`select: directory` 不能接 `file` 输出。
- **输出不能接标量输入**（`string` / `int` / `float` / `bool` / `enum`）。SRCOS 不做数据搬运与解析：
  标量要么来自用户（`expose`）、要么来自工具默认值。需要「上游产出一个数、下游读它」时，
  正确做法是**下游用 `type: path` 读上游写出的文件**（或把两步合成一个工具）。
- **`from` 必须存在**：`type: path` 输入必须声明 `from: <storage-id>`（ADR-020 闭环），
  连线不改变这一点：连上去的仍然是一个路径值，工具侧的挂载需求由**下游工具自己的
  `requires_storages`** 决定。

> 为什么不做「数据类型」搬运：见 [`tool-spec.md`](tool-spec.md) §4.3 与 ADR-005/006 ——
> 样本级并行与中间数据格式属于工具；流程只保证**依赖顺序**与**路径传递**。

---

## 4. 注册期校验清单

`srcos flow validate` 与网关启动时的流程扫描执行**同一套**校验（实现：`internal/flow`）。
任何一条不过 = 流程不可用，且报出全部问题（不一次只报一个）。

**结构**

1. `schemaVersion`、`id`、`version`、`name` 必填；`id` 与目录名一致。
2. 节点 id 合法（`[a-z0-9][a-z0-9_-]*`）、**不含 `.`**、流程内唯一。
3. `depends_on` 指向存在的节点；不许自依赖。
4. **DAG 无环**（否则报出环上的节点）。
5. `when` ∈ {`on_success`, `always`}；`retry.max` ∈ [0,5]。

**节点与工具**

6. `tool` 引用的工具存在于工具目录；带 `@version` 时版本必须精确匹配。
7. 节点工具必须 `kind: task`（服务不能进流程）。
8. 工具自身的注册期校验必须通过（`tool.yaml` 合法：13 类检查，含 `executor × backend` 交叉校验 —— 见 ADR-007）。

**连线**

9. `from` / `to` 地址语法正确，节点存在，输出/输入名存在于对应 `interface`。
10. 一个输入最多一条 binding。
11. 类型兼容（§3）。

**暴露与闭环**

12. `expose` 的 `node`/`input` 存在；`from` 形如 `user` 或 `sample.<字段>`。
13. 同一输入不被两条 `expose` 暴露，也不与 binding 重叠。
14. **每个 required 输入有且仅有一个来源**（§2.6）。

---

## 5. 执行语义

### 5.1 `FlowRun` 与展开（**已实现**）

一次运行 = 一个 `FlowRun`：

```text
data/flows/<user>/runs/<run-id>/
├── flowrun.yaml        # 运行记录：节点状态、job id、尝试次数、时间
├── samples.csv         # 用户上传的样本表（留档 → 续跑不用再给）
└── nodes/<节点 id>/
    ├── .sign           # 该节点完成标记（人类逃生口）
    └── <样本段>/       # 该样本的工作目录，产物落在这里（/flow 里看到的是同一处）
```

- `expose.from: sample.<字段>` 的参数**按样本行展开**：N 行 → 该节点产生 N 个任务
  （`job` 名形如 `count · S001`），一行一个 `work.sh`。
- `expose.from: user` 的参数在实例化时填一次（`--param`），所有行共用。
- 展开后每个节点实例就是一个**普通 SRCOS 任务**：同样的 `job.json`、沙箱、实例记录、审计与配额
  —— 流程不是第二套执行路径，只是任务的生产者（`job list` 里能看到它们，带 `flow/run/node/sample` 标签）。

### 5.2 调度（**顺序版已实现**）

- **拓扑序投递**：一个节点的所有 `depends_on` 都成功后才投递（AND）。
- `when: always` 的节点即使上游失败也投递（用于出报告，让失败也留下可读的产物）；
  其他下游节点标 `skipped` 并写明原因。
- **当前是顺序执行**（一次一个 job）。这是刻意的第一版：登录节点上不适合拿别人的工作去
  试探 SRCOS 自己的并行度。**规划**：流程级并发上限（默认 4）待实现。
- 样本展开后的 N 个任务**属于该节点的内部并行**：由工具自己用 `ata` / `annotask` 决定并发
  （ADR-005）；流程不下钻到样本内部。
- 一个节点在**第一个失败的样本**上停下，不再投递后续样本（失败要看得见，不要雪崩）。

### 5.3 失败、重试与续跑（续跑已实现；自动重试规划）

- 节点失败 → `flow resume <run-id>` 只重跑**该节点及其下游**；已成功的节点被跳过（上游不重跑）。
- **断点续跑**：`.sign` 文件是「这一步做完了」的**权威**（手工 `touch` 即视为完成，对齐 annopi 的
  `is_signed`）—— 这是唯一的逃生口，因为有些步骤确实没法重跑。成功时自动写 `.sign`。
  续跑读运行记录（`flowrun.yaml`）里的 job id 与节点状态；样本表已随运行留档，不需要再给。
- `retry.max: N` → **规划**：同一节点失败后自动重试 N 次（退避 30s、2m、5m…）尚未实现，
  目前失败即停、由人决定是否 `resume`。
- 终态：`succeeded`（全部节点成功）/ `failed`（有节点失败）。
  **规划**：`cancelled`（取消未开始的节点）未实现。

### 5.4 产物与「流程也是一次任务」（已验证）

- 节点的产物就是它引用的工具声明的产物；流程级 `outputs` 是各节点产物的**引用集合**（不复制）。
- 既然流程自己也有 `inputs`/`outputs`，它**可以**被当成一个工具被别人引用（子流程，ADR-009 的免费副产品）。
  **首版不提供入口**（UI 与 CLI 都不注册流程为工具），数据模型不需要为此增加字段。

---

## 6. 与 annopi 的异同（ADR-010）

| annopi | SRCOS | 说明 |
|---|---|---|
| `pipeline.yml` 的 `tasks` + `dependencies` | `flow.yaml` 的 `nodes` + `depends_on` | 同构，改名是为了与 `tool.yaml` 的词汇一致 |
| `task.yml` 的 `params` + `command` | `tool.yaml` 的 `interface` + `work.sh` | **命令不再出现在流程里**：命令属于工具 |
| `deps` 三层优先级（task 默认 < pipeline 覆盖 < 未声明报错） | 环境挂载由**工具声明**（`ro_mounts`），数据由**storage**声明 | SRCOS 把 annopi 的 `deps` 拆成「环境」与「数据」两件事（AGENTS.md：环境与数据要分开） |
| `${sample.x}` / `${sample[field=v].x}` / `${cmp.x}` | `expose.from: sample.<字段>` / （规划）比较组 | 首版只做样本维度；交叉引用靠下游工具自己读上游产物 |
| `annotask: {lines, threads}` | 工具自己的 `internal.parallelism` / 自带执行器 | **样本级并行归工具**（ADR-005），流程不下钻 |
| `executor: local \| qsubsge` | `backend: local \| sge` 由**工具**声明 | 流程不声明 backend；SGE 禁嵌套 qsub（ADR-007） |
| `tasks.yml` + `.sign` 状态 | 实例记录 + `flowrun.yaml` + `.sign` | 同一套语义：`.sign` 是人类逃生口，终态以标记为准 |

**逃生口**：把 annopi 本身注册成一个**普通工具模块**（一个 `tool.yaml` + `work.sh` 调 `annopi run`），
流程里就能直接用 annopi 的 pipeline —— 不需要 SRCOS 变成 Python 运行时（ADR-010）。

---

## 7. 明确不做

| 不做 | 为什么 |
|---|---|
| 条件分支 / 循环 / 动态 DAG | 「简单依赖串联」够用；要复杂控制流就写进工具（AGENTS.md「明确不做」） |
| 嵌套子流程（UI 入口） | 数据模型支持，入口不开（ADR-009） |
| 节点之间的数据搬运 / 类型转换 | SRCOS 不解析业务数据；要转换就写一个工具 |
| 流程里写 shell / `${}` 模板 | 命令属于 `work.sh`；模板会绕过 `interface` 校验与沙箱 |
| 节点声明 `backend` / `sandbox` | 由工具声明；流程覆盖会让「同一个工具两种隔离」变得不可审计 |
| 每节点资源上调 | 允许下调（规划）才是安全的；上调等于流程绕过工具的资源声明 |
| 多比较组（`${cmp.*}`） | 首版只做样本维度（roadmap §8 #5），等真实需求 |

---

## 8. CLI

```bash
srcos flow list     --flows-dir srcos-flows     # 列出流程（节点数、DAG 层数、样本列）
srcos flow validate --flows-dir srcos-flows      # 校验全部流程（或指定一个）

srcos flow run <flow-id> --samples samples.csv [--param k=v] [--dry-run]
srcos flow resume <run-id>      # 续跑：跳过已签名/已成功的节点
srcos flow status [run-id]      # 本用户的流程运行列表 / 单个运行的节点进度
```

`--dry-run` 打印**每个 (节点 × 样本) 的 job 及其参数与产物路径**，不提交任何东西 ——
这是「展开结果可审查」的那一条（对齐 annopi 的 `.sh` 可读性）。

**规划**：`flow cancel`、网关侧 `/api/flows`、管理端画布（Phase 5，只在编辑器页面加载 React —— ADR-012 的折中）、
MCP `run_flow`（第二期，`submit` scope 生效之后）。

网关侧：`/api/flows`、`/api/flows/<id>`、管理端画布（Phase 5，只在编辑器页面加载 React —— ADR-012 的折中）。
MCP 第二期加 `run_flow`（`submit` scope 生效之后）。

---

## 9. 变更记录

| 日期 | 变更 |
|---|---|
| 2026-09-22 | 建立本契约（Phase 4 起步）：schema、类型兼容表、注册期校验 14 条、执行语义（规划）、与 annopi 的对照、明确不做 |
| 2026-09-22 | **Phase 4 主体**：补「节点产物由平台按 run id 推导」+ `expose.from: output.<名>`（路径不靠模板、不靠 flow 作者手写）；`/flow` 内建挂载；样本展开、顺序调度、`.sign` 续跑、失败即停与 `when: always` 全部落地并端到端验证 |
