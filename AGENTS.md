# SRCOS 仓库约束

本文件是整个仓库的**持久工程约束**（invariants），不是状态文档。
状态、优先级、进度看 [`docs/roadmap.md`](docs/roadmap.md)；面向使用者的说明看 [`README.md`](README.md)；
工具的对外契约看 [`docs/tool-spec.md`](docs/tool-spec.md)。

---

## 定位（不要偏离）

> ### SRCOS 是 AI 平台的确定性执行后端。探索用 AI，执行用 SRCOS。

展开来说：

- **SRCOS 不碰工具 UI。** 有了 AI agent，造一个带参数输入的 shiny / python / R 页面成本趋近于零。
  UI 不再是平台的护城河，所以工具的核心资产是 **`work.sh`（函数体）+ `interface`（类型签名）**，UI 是可替换的壳。
- **执行路径 0 token。** 一次性 token 投入（把探索出的方法固化成工具）+ 零边际 token 成本（反复执行）。
  本质是**把 AI 的探索成果资本化**。
- **确定性、可复现、可审计**是企业内场景的真正诉求，也是相对 AI 平台的差异化所在。
- **与 AI 平台互补，不做竞争。** 应该主动做 AI 平台的执行层（MCP 就是对接口）。

### 差异化三角

| | 传统生信云平台 | 通用 AI Agent 平台 | **SRCOS** |
|---|---|---|---|
| 工具 UI | 平台托管（护城河） | 无，对话即 UI | 工具自己出；平台只提供**原语控件** |
| 交互 | 参数表单 | 自然语言 | 表单为主（确定性）+ agent 问答为辅（显式 scope） |
| token 消耗 | 0 | 每步都消耗 | 执行路径 0；探索与问答路径消耗 |
| 确定性 / 可复现 | 中 | 差 | 强（`work.sh` + `.sign` + 版本化工具 + `interface`） |
| 可审计 | 中 | 弱 | 强（谁、何时、哪个版本的工具、什么参数） |

**推论（这条链推导出后面所有设计）**：

```
UI 造起来便宜了 → UI 不再是护城河
  → 工具的核心资产 = work.sh + interface
  → 同一个工具可以有多个 UI：shiny 一个、CLI 一个、agent 一个、自动生成的表单一个
  → "注册什么"的答案不是 UI，而是 interface + entry
  → 一份 interface 派生三个前端（见下）
```

---

## 两条用户模型不能混淆（最容易搞错的地方）

| | **OS 用户** | **SRCOS 注册用户** |
|---|---|---|
| 是什么 | 启动 `srcos serve` 的那个 Linux 账号 | `config/users/<name>.yaml` 里的一行 |
| 存在于系统 | 是，真实的 `/etc/passwd` 条目 | **否，纯虚拟** |
| home | 真实（如 `/home/seqyuan`） | **虚拟，由 SRCOS 构造** |
| 数量 | 通常 1 个（或按角色少数几个） | 任意多个 |
| 权限来源 | 文件系统权限 + `config/` 属主 | SRCOS 的认证 + Grant 授权 |

**所有工具实例都以同一个 OS 用户身份运行**（无 root，见 ADR-014）。因此：

- **隔离不靠 Unix UID，靠 mount namespace + `Jail` 路径校验。**
- 如需真实 UID 隔离，必须为每个 SRCOS 注册用户建系统账号 —— 需要 root，**明确不做**。

### 虚拟 home 与虚拟 workspace

SRCOS 注册用户的运行时视图由 SRCOS 构造：

| 沙箱内路径 | 宿主路径 | 模式 | 来源 |
|---|---|---|---|
| `/workspace` | `data/ws/<user>/<tool>/` | rw | **内建**，每实例自动挂载 |
| `/home/<user>` | `data/homes/<user>/` | rw | **内建**，每实例自动挂载 |
| 工具声明的 storage | 见 `storages.yaml` | ro/rw | **声明式**，工具 `requires_storages` 决定 |

- **内建挂载**（`/workspace`、`/home/<user>`）任何工具都有，不可选、不可覆盖。
- **声明式挂载**（共享参考数据等）由工具在 `tool.yaml` 声明需求，用户只能在其中选路径（见 ADR-020 闭环）。
- 沙箱内的 `$HOME` 指向**虚拟 home**，所以 `.cache` / `.condarc` / `.config` / `.jupyter` 天然隔离，
  不会污染 OS 用户的真实 home，也不会在并发实例间互相踩。
- 虚拟 home 首次使用时从模板初始化（对齐 `workspace-template` 的做法）。

> **环境 vs 数据要分开**：conda 环境、`module`、`.sif` 镜像属于**环境**（宿主真实路径，ro 挂载，管理员在
> `tool.yaml` 里写）；共享参考数据、项目目录属于**数据 storage**（管理端在 `storages.yaml` 声明，
> 工具的 `type: path` 参数从里面选）。两者不要混用同一套声明。

---

## 不变式（Invariants）

违反以下任一条，都是架构回退，需要先改文档再改代码。

### 分发与依赖

- **单二进制、零依赖分发。** 拷贝一个文件就能跑；配置落在二进制旁边的 `config/`，可整体备份迁移。
  前端资源通过 `embed.FS` 打进同一个二进制。
- **不引入外部数据库。** 声明式 YAML + 目录扫描（「文件系统即数据库」）。
  运行态（实例、任务、流程 run）也用 YAML，启动时 reconcile。
- **不引入需要 root 的组件。** 排除 docker daemon、udocker、proot。

### 安全

- **工具实例只监听回环。** 网关是唯一入口，这是全部安全模型的基石，也是「后端零改动挂到子路径」的前提。
- **`Jail` 是唯一的路径解析入口。** 任何「用户可控参数 → 文件系统路径」都必须过 `Jail`：
  禁止 `..`、绝对宿主路径、symlink 逃逸。
- **`MountSpec` 的粒度就是隔离的粒度。** bwrap 的 userns 会把未映射的 gid 折叠成 `65534`，
  而沙箱进程本身就在 `65534` 组里 —— 因此宿主的 `group` 权限位在沙箱内**等于公开可读**
  （实测：`-rw-r----- root:somegroup` 宿主不可读、沙箱可读；`-rw-------` 两边都拒绝）。
  所以：**必须 bind 到最小必要路径，绝不 bind 父目录。**
  需要多个 storage 就 bind 多个精确路径 —— bind 了 `/share` 就等于把 `/share` 下
  **所有 group-readable 内容**（含其他项目组的 `drwxrwx---`）给了沙箱。依据见
  [`docs/environments.md`](docs/environments.md)。
- **`Jail` 与 `MountSpec` 是两个不同的边界，不要混为一谈：**
  `Jail` 保护 **SRCOS 自己的 API**（`/api/paths`、文件预览、MCP）；
  `MountSpec` 保护 **沙箱内的进程**。混用会产生虚假的安全感。
- **HTML 预览必须 sandbox iframe + 独立 origin。** 代理后端与网关同源，同源脚本可以调用管理 API；
  同源渲染用户上传的 HTML 会直接放大这个风险。
- **身份头由网关覆盖，绝不透传。** SSO / agent token 一律先删客户端传入的同名头再写入。
- **agent token 独立于浏览器 session。** agent 是程序，用 `Authorization: Bearer`；
  托管在 SRCOS 里的 agent 实例调 MCP 时用**实例身份**，权限必须是所属用户权限的**子集**。

### 架构

- **编排层不碰 HTTP，代理层不碰容器。** 两者只通过「动态路由表」耦合。
- **一个运行原语。** 常驻服务与一次性任务是同一个 `RunUnit`，差异只有 `kind` / `restartPolicy` / `ingress`（ADR-003）。
- **能力即 provider。** 凡是「可能有多种来源或多种实现」的能力，都抽象成 provider，平台只依赖接口：
  `StorageProvider`、`ViewerProvider`、`RuntimeProvider`、`AuthProvider`…
  这条原则与 dsh 的 "everything is a plugin" 同源，也是 SRCOS 里最喜欢复用的一种结构。
- **一份 `interface`，三个前端。** `Tool.interface` 必须机器可读，它同时派生：
  ① MCP tool schema（给 agent）② 流程画布的连线类型（给管理员）③ 用户侧参数表单（给非工程师）。
  **工具的 UI 可以替换，签名不可替换。**
- **`interface` 是唯一契约。** 平台不认识工具的实现细节；工具不需要知道平台的内部结构。
- **流程 schema 对齐 annopi，但不依赖 annopi 软件。** 不发明第二套规范，避免已有模块要重写。

### 工具契约

- **SRCOS 不管工具 UI。** 只提供原语控件（路径选择器、文件预览、进度）与投递/状态契约。
- **`work.sh`（或声明式 `command:`）必须同步阻塞到所有实际工作完成。** `doneWhen` 探针只作逃生口（ADR-006）。
- **任务粒度 = 一个 `work.sh`/`command`。** 样本级并行（`ata` / `annotask`）归工具自己，SRCOS 不感知、不执行；
  节点的资源声明是**聚合需求**（ADR-005）。
- **`backend` 由工具声明。** `internal.executor: qsubsge` + `backend: sge` 是非法组合，
  注册时交叉校验直接报错（ADR-007）。

---

## 明确不做

完整清单见 [`docs/roadmap.md`](docs/roadmap.md) §7。几条最容易"顺手做了"的：

- ❌ Kubernetes / k3s 后端
- ❌ 重写 annopi/annotask 的断点续跑与 OOM 自适应重试
- ❌ SRCOS 解析 `qstat` 跟踪任意子作业
- ❌ 条件分支 / 循环 / 动态 DAG / 嵌套子流程
- ❌ 工具 UI 平台化（表单引擎、拖拽式参数面板）
- ❌ **非 HTTP（TCP/UDP）的端口转发** —— 代理层是 HTTP 反向代理，`backend: external` 也只转发 HTTP；
  裸露 TCP 端口是另一个协议栈，与「单二进制零依赖」不成比例
  （设计见 [`docs/plans/2026-09-26-unified-service-instantiation-design.md`](docs/plans/2026-09-26-unified-service-instantiation-design.md)）
- ❌ **SRCOS 作为 MCP Client**（去调工具自己的 MCP server）——
  工具的执行契约是一个**同步阻塞、以退出码报状态**的命令（`entry: work.sh` 或声明式 `command:`），
  MCP 是"谁可以调它、怎么发现签名"的接口协议，
  两者不同层，混在一起会模糊"确定性执行"这个核心卖点
- ❌ 让 agent 直接改 workspace（**只读**；执行是提交任务，走显式 `submit` scope，见 ADR-019）

---

## 参考项目的借用边界

| 项目 | 借 | 不借 |
|---|---|---|
| `../ennote`（`ennoworker/internal/workspace/`） | `MountSpec` 三层抽象、`Jail` 解析算法、`TrustStore` 的「信任存于被信任对象之外」原则 | bwrap 的硬编码挂载参数、"bwrap 能管资源"的假设 |
| `../annopi` | `task.yml` / `pipeline.yml` 的 schema、`deps` 三层优先级、模块复用形态 | Python 运行时依赖 |
| `../annotask`、`../ata` | 样本级并行、`.sign` 完成标记、OOM 自适应重试的**设计** | 作为 SRCOS 的组件（它们在工具内部跑） |
| `../goqsub` | SGE 提交参数取值 | DRMAA / CGO 依赖 |
| `deepseek-harness` | `dsh-resource://` 地址语法与「viewer 注册表 + 认领优先级」设计、provider 契约形态 | 代码（Cordis 插件无法脱离 dsh 运行时，见 ADR-011/016） |

---

## 仓库卫生

### 文档分工（不要重复，也不要放错）

| 文件 | 回答什么问题 | 谁维护 |
|---|---|---|
| `AGENTS.md`（本文件） | **不变的**：架构约束、不变式、明确不做 | 架构变更时改 |
| `docs/handoff.md` | **重开会话先读**：目标 / 已验证事实 / 下一步 / 已踩的坑 / 代码地图 | 每轮工作结束时改 |
| `docs/roadmap.md` | **变化的**：设计思想、ADR、Phase 进度、待决策 | 每次实质推进都改 |
| `docs/tool-spec.md` | 工具开发者要遵守的对外契约 | `interface`/`job.json`/`work.sh` 变更时改 |
| `docs/flow-spec.md` | 流程管理员要遵守的对外契约 | `Flow` schema 变更时改 |
| `README.md` | 使用者怎么装、怎么用（代理网关部分） | 用户可见行为变更时改 |
| `docs/environments.md` | 各主机的实测环境事实（沙箱/资源/存储/调度器） | 换主机或环境变化时追加 |
| `docs/*.md` 其余 | 专题指南（`tunnel.md`、`dsh-demo.md`、`reverse-proxy-tls.md`、`agent-mcp-positioning.md`…） | 对应主题变更时改 |
| `docs/archive/` | goprox→SRCOS 过渡期的历史记录（命令与路径已过时） | **不维护**，不作为当前契约 |

**规则：本文件写"为什么不能这么做"，roadmap 写"我们决定这么做"和"做到哪了"，
handoff 写"现在到哪了、下一步做什么、哪些坑踩过了"。**
不要把进度写进本文件，也不要把不变式写进 roadmap。

### 提交约定

- Conventional Commits + 范围：`feat(tool):`、`fix(proxy):`、`docs(roadmap):`、`chore:`。一个提交一个逻辑变更。
- `git add` 限定在本次变更的文件，**不要用 `git add -A` 扫入无关的工作区状态**。
- 每个提交要能独立 build + test 通过。不通过就不是可提交状态。
- 提交前跑：`make vet && make test`（涉及前端时另加 `make webui`；改到执行链路时跑 `make e2e`）。

### 不要提交的东西

- `config/`（运行态配置，含 `session_secret`、用户 `password_hash`、`totp_secret`）
- `data/`（虚拟 home、workspace、镜像、实例运行态）
- `internal/web/dist/`（前端构建产物）
- 任何凭据、agent token 明文、SSO `hmac_secret`
- `docs/公众号软文*`（草稿文章，本地保留）

### 开发命令

```bash
make build     # 构建 srcos 二进制（版本取自最近的 git tag）
make test      # go test ./...
make vet       # go vet ./...
make fmt       # gofmt -w .
make webui     # 构建独立前端包到 internal/web/dist/（Phase 5 起）
make e2e       # 端到端回归网（临时配置+临时端口：提交→队列→执行→判定→日志→资源查看）
```

CI（`.github/workflows/ci.yml`）跑同样的三件事：`vet` + `test -race`、`make e2e`
（降级沙箱：`SRCOS_E2E_SANDBOX=none`）、前端 `pnpm build`。**本地提交前仍要自己跑** ——
CI 是防回退，不是代替品。

---

## 当前阶段

见 [`docs/roadmap.md`](docs/roadmap.md) §6。**Phase 1（规范与最小闭环）在进行中。**

Phase 0 已完成：重命名为 srcos、删除 `site/` 静态文档站、`git init`、建立 roadmap。
