# SRCOS 代码评审发现（2026-09-29）

> **状态：已审核（2026-09-29）。** 本文原为外部评审输入；现已逐条对照 HEAD `4a91085` 核对并接受。
> 审核结论：**#1 复现成立（P0，已按本文建议修复：版本输出校验 + probe 可注入 + 回归测试）**；
> #2/#3/#4 代码层面成立，#5.1/#5.3 成立；但 #2 的计数与归因、#5.2 的前提有误，已就地修正。
> 审核修正以 `> 【审核修正】` 标注，未标注处为原评审文本，保留以便对照。
>
> | 想核实什么 | 看哪 |
> |---|---|
> | 评审怎么做的、范围边界 | §1 |
> | 评审人对设计目的的理解（先校对这个，理解错了后面全错） | §2 |
> | 发现清单总表 | §3 |
> | 每个发现的详细论证 | §4 |
> | 评审人认为做得好、修复时不要破坏的部分 | §5 |
> | 建议的落地顺序 | §6 |
> | 逐条复现命令 | 附录 A |

---

## 1. 评审范围与方法

- **对象**：工作区 HEAD `4a91085` 时的全仓库；重点在执行链路（`internal/execute` / `internal/runtime` / `internal/server`）与工具加载（`internal/tool`）。
- **方法**：通读 `AGENTS.md` / `roadmap.md` / `handoff.md` 全文与 29 条 ADR → 通读核心包源码 → 在本机实际运行 `go vet` 与 `go test ./...` → 对可疑点逐个写最小复现。
- **环境**：Linux（Ubuntu 24.04 系），有 `bwrap` 与 `systemd-run --user`，**装有 `/usr/games/singularity`（ pygame 游戏，非容器运行时）**——这个环境事实直接催生了发现 #1。
- **范围边界（评审人请注意，不要把没查的说成查了）**：
  - 未深入审计 `internal/runtime/sge` 的隧道/rendezvous 状态机（已有真机验证记录）；
  - 未做渗透测试式的安全审计——只核对了文档声称的安全不变式与实现是否一致（见 §5）；
  - 前端 `webui/`（约 950 行）只看了结构，未逐行审；
  - P1 两条性能问题的量化是**推算**（基于调用点计数与文件规模），不是大规模压测结果。

---

## 2. 设计目的的理解（请先校对本节）

评审人请先确认这一节。如果这里的理解有偏差，后面的发现可能建立在对定位的误读上。

SRCOS 的定位是一条可推导的价值主张：**「AI 平台的确定性执行后端——探索用 AI，执行用 SRCOS」**。全部架构决策都能从它推出来：

```
AI 使造 UI 的边际成本趋近 0 → UI 不再是护城河
  → 工具的核心资产 = work.sh（函数体）+ interface（类型签名），UI 是可替换的壳
  → 一份 interface 派生三个前端（MCP schema / 画布连线 / 生成式表单）——签名不可替换
  → 平台的价值面 = 注册、授权、隔离、资源、编排、审计
  → 成本结构 = 一次性 token 投入 + 执行路径 0 token（把 AI 的探索成果资本化）
```

工程支柱四条：单二进制零依赖分发；文件系统即数据库（声明式 YAML + 目录扫描，无外部数据库）；工具实例只监听回环、网关是唯一入口；编排层不碰 HTTP、代理层不碰容器。

两条用户模型（OS 用户 ≠ SRCOS 注册用户）与两个安全边界（`Jail` 管 API 面、`MountSpec` 粒度管沙箱面）分开维护，且文档明确记录了 bwrap userns 把 group 权限位折叠为「人人可读」的实测事实——bind 粒度即数据可见范围。

**评审人对本节的判断**：☐ 准确 ☐ 有偏差（请指出：____________）

---

## 3. 发现清单总表

| # | 优先级 | 一句话 | 位置 | 类型 |
|---|---|---|---|---|
| 1 | **P0** | apptainer probe 会被同名游戏二进制骗过，`sandbox: apptainer` 在装了 Singularity 游戏的机器上必然失败且报因误导 | `internal/sandbox/apptainer.go:118` | 真 bug（已复现） |
| 2 | **P1** | 每个 10s 扫描 tick 做 6 次全量实例扫描（另：命中回收/对账时再触发 `syncRoutes`）；flowrun 每个 unit 派发再全量扫描一次 | `internal/server/loops.go` 等多处 | 规模化热点 |
| 3 | **P1** | 每次工具列表/详情请求都对所有工具包全树重算 sha256 | `internal/tool/tool.go:432` | 规模化热点 |
| 4 | P2 | `StartService` 并发双启动会杀掉刚起的服务（停旧在锁内、启动在锁外） | `internal/execute/execute.go:484-495` | 竞态（体验级） |
| 5 | P3 | 杂项：`warnedInstances` 无界增长、`audit.forward_url` 凭据明文落盘、handoff §3.1 轻微漂移 | 见 §4.5 | 小项 |

---

## 4. 发现详述

### 4.1 #1（P0）apptainer probe 被同名游戏二进制骗过

> 【审核修正】本节结论**成立且已复现**（`go test ./internal/runtime/ -run TestUnusableSandboxProducesFailedInstance` 在本机 FAIL）；已按建议修复，见本节末。

**位置**：`internal/sandbox/apptainer.go:118-134`（`probeApptainer`），缓存于 `apptainerProbe`（`sync.Once`，进程生命周期）。

**证据**（本机实测，2026-09-29）：

```console
$ which singularity
/usr/games/singularity                 # Ubuntu 官方源里的 pygame 策略游戏 "Singularity"
$ /usr/games/singularity --version; echo $?
1.00
0                                      # 退出 0 → probe 的唯一判据被满足
$ go test ./internal/runtime/ -run TestUnusableSandboxProducesFailedInstance -v
    local_test.go:320: unexpected error: tool exited with code 2
--- FAIL
```

> 【审核修正】实测该游戏 `--version` 的输出是**多行**（首行 `Singularity 1.00 (commit: 2ebc…)`，
> 末尾才是一个孤立的 `1.00`），不是单行 `1.00`。结论不变（正则仍能区分），但校验应**在整段输出中
> 逐行匹配**，不要只在首行锚定。

**根因**：`probeApptainer` 先 `exec.LookPath("apptainer")`、失败则 `LookPath("singularity")`，然后只检查 `--version` 的**退出码**，不检查输出内容。游戏二进制对 `--version` 退出 0（打印 `1.00`），probe 判定「可用」，随后真正运行时 `singularity exec <image> ...` 才失败——错误被归因为**工具自己的失败**（`tool exited with code 2`），而不是「沙箱运行时不可用」。且结果被 `sync.Once` 缓存整个进程生命周期，一次误判无法自愈。

**影响面**：

- 任何安装了这个游戏（Ubuntu `universe` 源，桌面机常见）的开发机上，声明 `sandbox: apptainer` 的工具全部以误导性错误失败；
- `PATH` 含 `/usr/games` 是 Ubuntu 默认，这不是构造出来的场景；
- **CI 恰好掩盖了它**：GitHub runner 没装该游戏也没有 apptainer → probe 如期失败 → 测试通过。这与 handoff §4.2 记录的 systemd `--working-directory` 一案同类——「CI 环境与真实宿主的差异躲过了自动化」。而 handoff §6 已确认 annuo 集群验证时 singularity 是真运行时，所以生产集群不受影响，**受影响的是开发机与未来新接入的宿主**。

**建议修法**（两层，均小改动）：

1. **校验版本输出内容**，不只退出码：

   ```go
   out, err := exec.Command(p, "--version").CombinedOutput()
   if err != nil || !runtimeVersionLooksReal(string(out)) {
       // runtimeVersionLooksReal: 输出须匹配 (?i)^(apptainer|singularity)\s+version
       // 真 apptainer 打印 "apptainer version 1.x.x"；真 singularity 打印
       // "singularity version 4.x.x"；游戏打印 "1.00"，不匹配
   }
   ```

2. **让 probe 可注入**：目前是包级 `sync.Once` 全局，单测只能依赖宿主机碰巧装了什么（CI 通过纯属侥幸）。把 probe 抽成可替换的函数（如包级 `var probeApptainerFn = probeApptainer`，或经由 `ApptainerOptions` 传入），测试注入假 probe；并把本机这个案例固化为回归测试（假 probe 返回「存在但版本输出不对」的二进制）。

**待审核人确认的问题**：

- 版本输出校验的正则是否足够？真 Singularity CE 4 的输出确认为 `singularity version 4.x`（annuo 实测记录可查），apptainer 为 `apptainer version x.y`；是否有站点自定义 wrapper 会改变输出前缀？
- 是否需要顺带在 `environments.md` 补一条「宿主 PATH 含 /usr/games 时的探测行为」？

> 【审核修正·落实结果】已取「整段输出匹配 `(?i)\b(apptainer|singularity)\s+version\s+\S`」而非首行锚定（对带前置 banner 的 wrapper 更稳）；
> probe 通过 `sandbox.SetApptainerProbeForTest` 可注入，新增三个单测（拒绝游戏、接受 apptainer 与 singularity 真 banner）；
> `internal/runtime` 的 `TestUnusableSandboxProducesFailedInstance` 改为注入假 probe，不再依赖宿主 PATH。

---

### 4.2 #2（P1）扫描 tick 的 6 次全量实例扫描 + flowrun 每单元派发全量扫描

**位置与调用计数**（全部已核实，非测试代码）：

`internal/server/loops.go:17` 的 `ScanLoop`（默认 10s tick）顺序调用六个消费者，**每个都独立调用 `ListInstances`**（readdir + 逐文件读 + YAML 解析全部 `data/instances/*.yaml`）：

| 消费者 | ListInstances 位置 |
|---|---|
| `syncRoutes` | `internal/server/routes.go:249` |
| `Reaper.Sweep`（经 `reapServices`） | `internal/runtime/run.go:940` |
| `ReconcileServices` | `run.go:1139` |
| `RenewServices` | `run.go:1222` |
| `WarnExpiringLeases` | `run.go:1277` |
| `ReconcileTasks` | `run.go:1332` |

即 **6 次全量扫描 / tick ≈ 0.6 次/秒**，互相不知道对方已经读过。

> 【审核修正】原表把 `sweepInstanceCredentials`（`routes.go:69`）算作每 tick 的第七个消费者，并称它
> 「由 `syncRoutes` 顺带调用」——**不成立**：它只在启动期的 `reconcile()`（`routes.go:50-51`，由 `server.go:377`
> 调用一次）里执行；`run.go:1010`（`Runner.Reconcile`）同样是启动态。两者都不在 ScanLoop 路径上，
> 故每 tick 是 **6 次**、不是 7 次。
>
> 另一方面，`reapServices` / `reconcileServices` 命中时会**再调一次 `syncRoutes`**
> （`routes.go:202`、`:145`），所以忙 tick 上实际会 >6 次——原表漏了这处放大。

雪上加霜的是两处按事件的全量扫描：

- **flowrun 的 `checkQuota` 每个 unit 派发都全量扫描一次**（`internal/flowrun/flowrun.go:377` 调用 `:615`，`:624` 内 `ListInstances`）——一个 200 样本 × 多节点的流程，运行期间就是数百次全量扫描；
- 每次 task submit 的配额检查（`internal/execute/execute.go:723`）也是全量扫描。

**成本量化（推算，非实测）**：实例记录是**只增不减的历史**（`prune.go` 顶部注释自认「scan tick lists all of them several times a minute」；清理是显式 `srcos prune`）。设 1,000 条记录、每条 ~1.5KB：

- 本机 NVMe：每 tick ≈ 10MB 读 + 6,000 次 YAML 解析，勉强可忍；
- **`data/` 在 NFS/共享盘上（实验室常态）**：每 tick 6,000 次网络文件读，tick 时长被拉长，路由收敛、任务结算全部变慢——这类退化没有告警，只会「越来越钝」。

**建议修法**（三层，按投入排序；均不违反「文件系统即数据库」——路由表已经是同一思路的先例）：

1. **每 tick 一份快照，六个消费者共享**（最小改动，收益立现）：`ScanLoop` 开头做一次 `ListInstances`，把 `[]*Instance` 作为参数传入六个消费者。函数签名变化限于包内 + server。立刻 6× → 1×。

   > 【审核修正】原文称「语义不变」需要附加条件：`Reaper.Sweep`、`RenewServices`、`WarnExpiringLeases`、
   > `ReconcileTasks`、`ReconcileServices` **都会修改 `inst` 再 `SaveInstance`**。共享同一批指针时，
   > 它们拿到的不是只读快照，后一个消费者可能把前一个消费者尚未落盘的改动一并写回。落地时须明确
   > 快照**只读**（写者先复制）或按消费者深拷贝，否则会引入跨消费者串写。
2. **配额检查改用「活跃实例索引」**：`usageFor`（execute）与 flowrun `checkQuota` 只关心**非终态**实例，而活跃数有硬上界（`max_instances` 配额）。维护进程内 map（`SaveInstance`/`DeleteInstance` 时同步更新 + 每 tick 与磁盘对账一次），两处配额检查变 O(活跃数)。
3. **长期（现在不做，仅记录）**：目录分片 `data/instances/<user>/<id>.yaml`，把「读全量」的 API 收窄成「读某用户」。

**待审核人确认的问题**：

- 六消费者共享快照会引入「tick 内读旧值」——`ReconcileServices` 在结算后立即 `syncRoutes`（`routes.go:145`），需要确认传参化后这条「立即收路由」的路径不被快照的旧值回退；
- 活跃索引的权威方是谁：网关进程是唯一写入者吗？（CLI `job run` / `svc start` 是另一进程，靠 tick 对账兜底是否够？）
- 是否应该在 `docs/roadmap.md` 把「实例记录保留策略」从纯显式升级为「终态记录 N 天后自动归档」的待决策项？（现状：只靠人记得跑 `srcos prune`。）

---

### 4.3 #3（P1）每次请求重算所有工具包的 sha256

**位置**：`internal/tool/tool.go:406`（`Load`）→ `:432` `Digest(dir)`；`Discover`（`:451`）逐目录调 `Load`。

**调用面**（均已核实为每请求路径，非仅启动时）：

- `GET /api/tools` → `internal/api/tools.go:67` → `inspect.Reader.Tools` → `VisibleManifests` → `tool.Discover`（`internal/inspect/inspect.go:259`）；
- 工具详情、`/tools/<id>` 页面、MCP `list_tools` / `describe_tool` 同源（`VisibleManifest` 也走 `Discover`，`inspect.go:235`）；
- 申请页 `RequestableTools`（`inspect.go:181`）、画布保存（`internal/api/flows.go:285`）。

即**每一个列表/详情请求 = N 个工具包 × 全树 sha256**。ADR-028 已推论「工具包不要放大文件（走 `ro_mounts`/`environment`），否则每次加载都要重算摘要」——这个约束写在 ADR 里，但防线只是约定俗成；一个新同事往工具包里塞一个 200MB 的数据文件，dashboard 就从毫秒级变成秒级，且没有任何告警。

**建议修法**：digest 的消费场景有三处——实例记录（要求真实）、审计（要求真实）、列表展示（只要求展示）。前两处发生在任务运行时，频率低；第三处是高频路径。加 **stat 签名缓存**：

```go
// 包级缓存：key = dir, value = {statSig, digest}
// statSig = 树内每个文件的 (relpath, size, mtime) 的稳定哈希
// stat 未变 → 直接返回缓存 digest；变了 → 重算
```

正确性不变（内容改动必然带 mtime 变化；stat 遍历本身 O(inode 数)，无内容读取），成本从 O(字节数) 降到 O(文件数)。工具包是管理端部署的静态资产，「用 mtime 检测变更」与 grants.yaml 热加载是同一信任级别。

**待审核人确认的问题**：

- 是否接受「mtime 粒度之内的改动检测不到」（与 grants 热加载同级别）？对审计语义是否有异议——严格派可能要求实例记录处的 digest 永远现算、只有**展示**走缓存；
- 或者更简单的替代：`Discover` 结果按 tools-dir mtime 整体缓存（目录树任一变更会更新父目录 mtime 吗？**不会**——子目录内文件改动不更新祖先 mtime，所以必须逐文件 stat，这正是上面 statSig 方案的原因。这个坑请评审人一并确认）。

---

### 4.4 #4（P2）`StartService` 并发双启动的杀启竞态

**位置**：`internal/execute/execute.go:434`（`StartService`），锁段 `:484-493`，真正启动在 `:495`（锁外）。

**场景**：锁内做「停旧实例 + 查配额」（注释明确解释了为什么 start 必须在锁外：健康检查可阻塞一分钟，不能持锁）。但两个并发 `StartService`（用户双击「启动服务」、agent 超时重试）：

```
T1: 锁内停旧 → 配额过 → 解锁 → 开始 start（健康检查中，记录已写 running）
T2: 拿到锁 → 读到 T1 刚写的 running 记录 → 把它停了 → 配额过 → 解锁 → 起自己的
T1: 健康检查失败（unit 被停）→ 报错
```

终态收敛没问题（converge-on-one 最终成立），但用户视角是「服务被莫名杀了一次」。服务实例 id 由 `(user, tool)` 派生（`runtime.InstanceID(user, tool, "")`），所以 T2 的停旧**必然**命中 T1 的在途实例。

**建议修法**：per-`(user,tool)` 的启动互斥——`sync.Map[toolID]*sync.Mutex`（Controller 级），把「停旧 → 配额 → 投递」整体串行；锁在 unit 投递后即释放（不必等健康检查），既消除竞态又不长时间持锁。任务侧不受影响（task 的 id 含 job id，天然不冲突）。

**待审核人确认的问题**：并发双启动的语义期望是什么——「第二个请求等第一个完成再替换」还是「第二个直接 409」？上面的修法隐含前者。

---

### 4.5 #5（P3）小项

1. **`warnedInstances` 无界增长**（`internal/runtime/instance.go:246`，`sync.Map`）：为「损坏记录只告警一次」而生，key 是文件路径且从不删除。量级极小（只有损坏记录才进），可不修；若修，TTL 化即可。
2. **`audit.forward_url` 的凭据**：~~外发信任锚（ADR-024）目前只支持 URL~~ 【审核修正】**`audit.forward_token` 已经存在**（`internal/config/state.go:88` 定义，`main.go:625` 已接进 `audit.HTTPSink.Token`，`forward.go:52-55` 用作 Bearer 头）。真正成立的**是**「凭据明文进 `state.yaml`（虽 0600）」。建议仍可采纳：支持从环境变量读取（`SRCOS_AUDIT_FORWARD_TOKEN`，Bearer 头注入），凭据不落盘。
3. **handoff §3.1 与 §2.3 的漂移**：#12「审计流第二期」在 §2.3 已标完成，但 §3.1 的下一步表仍将其列为待办（附完成注记）。【审核修正】还有一处原评审未提：§3.1 #13 仍称 Phase 6 SGE「唯一一个『架构在、从未真跑』的部分」，而 §2.3 已记录 2026-09-28 在 `annuo` 真集群跑通（`realcluster_test.go`）。两行都需对齐。

---

## 5. 评审人认为做得好、修复时不要破坏的部分

以下各项是本次评审的**加分项**。任何为修上述问题而提出的改动，如果与它们冲突，应优先保它们：

1. **单一写路径**（`internal/execute`）+ 错误分类在源头决定（`ErrNotFound`/`ErrForbidden`/…），REST 与 MCP 只翻译不决策。「两个前端对授权产生分歧」这个最危险的安全漂移被结构性消除。
2. **失败关闭的一贯性**：agent token 只存哈希 + 每次校验重读文件（注释解释了为什么不做 mtime 缓存）；Grant 默认拒绝且只有 allow 没有 deny；`/mcp` 不认 session cookie；digest 在 ADR-028 中明确「不是签名」。安全语义的表述从不夸大。
3. **「判定活得比等待者长」**（ADR-022）：systemd `ExecStopPost` 把退出码写进 verdict 文件，网关重启不丢退出码。用最小机制解决真问题，是全仓库最优雅的设计。
4. **`cancel` 不要求工具包存在、不做 Grant 检查**（`execute.go` Cancel 内注释）：撤权后仍能停自己的实例——权限边界想得很细。
5. **显式 prune 而非自动清理**（`internal/runtime/prune.go` 与 `audit prune` 同构）：历史保留是运营者的决定。因此 **#2 的性能修复应走读面快照，绝不要顺手改成自动删除记录**。
6. **handoff §4「已踩的坑」**：本仓库最值钱的资产。本评审发现的 #1 建议修完后按同一格式补录（含「CI 为什么没拦住」）。
7. **文档分工纪律**：AGENTS（不变式）/ roadmap（决策与进度）/ handoff（状态）/ specs（对外契约）四层不互相渗透，本次评审定位信息零障碍。

---

## 6. 建议的落地顺序（待审核通过后执行）

| 步骤 | 内容 | 预估 | 备注 |
|---|---|---|---|
| 1 | #1 probe 加固（版本输出校验 + probe 可注入 + 回归测试） | ~30 行 + 测试，半天 | **建议最先**：已让 `make test` 在常见开发机变红，且是 CI 盲区 |
| 2 | #2-a ScanLoop 快照共享（6×→1×） | ~100 行，1 天 | 函数加参；注意快照只读（消费者会改 `inst` 再落盘） |
| 3 | #3 tool digest 的 stat 签名缓存 | ~50 行，半天 | 注意 §4.3 的「祖先目录 mtime 不随子文件变」坑 |
| 4 | #4 StartService per-(user,tool) 启动互斥 | ~40 行，半天 | 语义需审核人先拍板（等或 409） |
| 5 | #2-b 活跃实例索引（配额 O(1)） | ~150 行，1-2 天 | 可延后；等 #2-a 落地后观察是否仍需要 |

每步独立提交（Conventional Commits），提交前 `make vet && make test`，#1/#2 涉及执行链路另跑 `make e2e`。修完后：#1 补录 handoff §4；#2/#3 若接受，在 roadmap §7「明确不做」或 §8「待决策」补对应条目（快照缓存与「文件系统即数据库」的关系值得一句话说清）。

---

## 附录 A：逐条复现命令

```bash
# 发现 #1：probe 误判（需要一台装有游戏 singularity 的 Ubuntu）
$ which singularity                 # → /usr/games/singularity
$ /usr/games/singularity --version  # → 打印 1.00，退出 0
$ go test ./internal/runtime/ -run TestUnusableSandboxProducesFailedInstance -v
# → FAIL: unexpected error: tool exited with code 2
# 对照：注释掉游戏二进制或从 PATH 移除 /usr/games 后，同测试通过（probe 报 "neither apptainer nor singularity is on PATH"）

# 发现 #2：调用点计数
$ grep -rn "ListInstances(" internal/ --include='*.go' | grep -v _test | grep -v "func ListInstances"
# → server/routes.go:69, routes.go:249, runtime/run.go:940,1010,1139,1222,1277,1332,
#   flowrun/flowrun.go:624, execute/execute.go:723, inspect/admin.go:51, instances.go:150
# 其中 run.go 五处（940/1139/1222/1277/1332）与 routes.go 一处（249）在 ScanLoop（loops.go:17）每 tick 执行；
# run.go:1010（启动期 Reconcile）与 routes.go:69（启动期 sweepInstanceCredentials）不在 tick 路径上

# 发现 #3：每请求 Discover → Load → Digest
$ grep -n "digest, err := Digest" internal/tool/tool.go        # → tool.go:432（Load 内）
$ grep -rn "tool.Discover" internal/ --include='*.go' | grep -v _test
$ grep -n "VisibleManifests\|Discover" internal/inspect/inspect.go  # → :259（每请求路径）

# 发现 #4：锁段与启动的相对位置
$ sed -n '480,500p' internal/execute/execute.go
# → c.mu.Lock() 停旧+配额，c.mu.Unlock() 后 :495 runner.StartService（健康检查在锁外）
```

## 附录 B：评审方法说明（供审核人评估可信度）

- 所有文件行号在 HEAD `4a91085` 上逐一核实过（本文行号若漂移，以 git blame 为准）；
- #1 是**运行时复现**，非静态推断；#2/#3 的调用点逐一 grep 核实，但**性能影响是推算**，未做压测——审核人若要求，可先写一个 10k 记录的基准测试再动 #2；
- #4 是**代码推演**的竞态（未写并发复现测试）；若审核人接受修法，应要求附带一个确定性回归测试（参照 `execute` 包 `TestWaiterDoesNotOverwriteADeliberateStop` 的先例：去掉修复它必失败）。
