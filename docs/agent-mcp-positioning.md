# SRCOS 与 Agent / dsh / MCP 的定位

> 专题指南。回答「agent 在 SRCOS 里是什么位置、dsh 在哪出现、MCP 怎么集成、各自优劣」。
> 不变式在 [`../AGENTS.md`](../AGENTS.md)，决策记录在 [`roadmap.md`](roadmap.md)（ADR-011 / 016 / 017 / 019 / 024）。
>
> 本文于 2026-09-26 由一次设计讨论整理，**结论已落入 ADR**，这里只保留推理与权衡。

---

## 0. 一句话

> **SRCOS 不是 agent，是 agent 的执行层。** agent 做探索（消耗 token、试错、写代码），
> SRCOS 做执行（0 token、确定性、可复现、可审计）。

代码印证：`grep -ri agent internal/` 只命中 `agenttoken`（程序凭据）与注释——
SRCOS **没有**任何 LLM、tool-calling loop、推理循环。这是定位决定的，不是缺失。

---

## 1. Agent 的三层含义（不要混为一谈）

| 说法 | 指什么 | 现状 |
|---|---|---|
| **外部 agent** | Claude / Cursor / 自研 agent，通过 `/mcp` 调 SRCOS | ✅ 已实现，官方 MCP SDK 实测 |
| **托管 agent** | agent 作为 `kind: service` 的 `RunUnit` 跑在 SRCOS 里 | ⚠️ **设计成立、代码为零** |
| **dsh 里的 agent** | dsh 自身带 agent 栈；路 B 曾让它的 agent 看见 SRCOS 资源 | ⚠️ 插件已移除（见 §3） |

**外部 agent 是最清晰、也是真正被实现的那条。** 分工来自 ADR-017：AI 负责「探索出方法」，
SRCOS 负责「把方法资本化」（`work.sh` 固化 + 反复执行，边际 token 成本为 0）。

### 1.1 两条进入 SRCOS 的通道，按用途分工

| 通道 | 做什么 | 凭据 | 为什么是这个凭据 |
|---|---|---|---|
| **MCP `/mcp`** | 发现工具、执行（submit / cancel / run_flow） | **只认 agent token** | 程序没有浏览器；session cookie 会让「谁在调用」不可审计 |
| **`srcos://` + REST** | 读资源（目录、文件、产物、日志） | session 或 agent token | 同一个 `inspect` 实现，浏览器与 agent 共用（ADR-018） |

### 1.2 三条明确不做（边界就是定位）

1. **不做 MCP Client**（ADR-019）—— 工具的执行契约是 `work.sh`（确定性、有退出码、有产物），
   MCP 是「谁可以调它、怎么发现签名」的接口协议；两者不同层，混用会模糊核心卖点。
2. **不让 agent 直接改 workspace** —— 想改数据必须 `submit` 一个 task（可审计、有版本、有产物、可重放）。
   更准确的措辞是「agent **不直接写**，所有写都经 job」。
3. **不实现推理循环** —— 那是 AI 平台的事。

---

## 2. Agent 定位的优缺点

**优点**

- **分工干净**：探索归 AI，执行归 SRCOS；`interface` 是唯一契约，`work.sh` 是执行契约。
- **协议收敛**：MCP 是标准协议，不需要为每个 agent 写适配器。
- **凭据模型正确**：agent token ≠ session cookie；只收窄不放大、只存哈希、撤销立即生效、失败关闭。
- **agent 面是「白送」的**：`interface` 机器可读 + `inspect` 共用，适配 MCP 的边际成本极低（ADR-018）。

**缺点 / 风险**

| # | 问题 | 说明 |
|---|---|---|
| A1 | **「托管 agent」只有设计** | ADR-019 说「托管 agent 调 MCP 时用实例身份，权限是所属用户的子集」，但**实例身份 token 不存在**；agent 实例只能拿用户级 token |
| A2 | ~~**agent 侧没有幂等键**~~ ✅ **已解决（2026-09-26）** | `submit_job` / `POST /api/jobs` 可选 `idempotency_key`：同一个 key 钉在同一 job 目录（`user+tool+key` 派生），重试返回第一次的结果；重放在审计里标 `idempotent_replay` |
| A3 | **MCP 无状态 ⇒ 只能轮询** | 无 progress notification；`srcos_task_status` 轮询是唯一手段 |
| A4 | **审计曾是洞** | ✅ 已补（ADR-024 第一期+第二期） |
| A5 | **MCP 只用 tools，没用 resources** | 见 §4.3（M1） |

---

## 3. dsh 在本项目里的角色

dsh（DeepSeek Harness）曾出现在**四个位置**，角色完全不同；拆开看耦合很轻：

| # | 位置 | 角色 | 状态 |
|---|---|---|---|
| 1 | `docs/dsh-demo.md` + 服务卡片 | **被代理的「本地优先」应用**（只监听回环、Host/Origin 信任栅栏、安全上下文 API） | ✅ 实测 |
| 2 | ADR-011 + `internal/resource` | **设计样板**（`dsh-resource://` 地址语法与 viewer 认领优先级，只借设计不借代码） | ✅ 同构 |
| 3 | ADR-016 路 A | 把 dsh 当 `kind: service` 实例、SRCOS 前端 iframe 嵌它 | ❌ 未做，已从路线图移除 |
| 4 | ADR-016 路 B + `integrations/dsh-plugin/` | 让 SRCOS 成为 dsh 的资源协议（dsh 里的 agent 能读 SRCOS 资源） | ❌ **已移除** |

### 3.1 为什么删掉路 B

未验证（从未装进任何 dsh profile，全部测试用替身）+ 逆向 developer-preview API（dsh 明示会破坏性变更）
+ 价值与自带 `/view` 重叠（增量只有「不离开 agent 工作台」）。代码可从 git `93351fc` 恢复。
**设计洞察保留**：`srcos://` 与 `dsh-resource://` 地址同构（原「路 D」）是协议特性，写在
`internal/resource`，对脚本与工具自建 UI 同样有用。

### 3.2 dsh 集成的普遍教训

- dsh 的 viewer 不是库，是**寄生在 dsh 运行时上的 Cordis 插件**（ADR-011 的决定性证据）——
  在 SRCOS 里跑它等于重建 dsh client shell。
- **不借代码**：把 preview 产品的内部 API 写进自己的东西，等于把维护成本押在会动的靶子上。
- dsh 本身**只绑回环**，与 SRCOS 安全模型天然一致 —— 它天生该待在 SRCOS 后面（作为被代理的应用）。

---

## 4. MCP 怎么集成

### 4.1 形态

```
POST /mcp   (Streamable HTTP, 无状态, application/json)
  认证   只认 Authorization: Bearer <agent token>；有 Origin 必须同源
  工具   12 个：9 只读 + 3 写（submit_job / cancel_instance / run_flow，需 submit scope）
  读     全部走 internal/inspect（与 REST / HTML 同一实现）
  写     全部走 internal/execute.Controller（REST / MCP 同一权威）
```

**错误分层**：协议错误用 JSON-RPC 码；工具**执行**失败（未授权、越界、实例不存在）返回
`result.isError = true` 的文本 —— 让 agent 看见原因并自己改正。写授权 = **工具 × 用户双维度**
（`submit_tools` 白名单 ∩ owner 的 Grant ∩ 配额）；跑流程时对每个节点的工具分别校验。

### 4.2 优点

- 复用单二进制，不引入新进程；MCP 是标准协议，任何客户端可接。
- 凭据面正确（Bearer only、撤销立即生效、不做 mtime 缓存）。
- 写入单一权威（`execute.Controller`），REST 与 MCP 不可能行为分叉。
- 只读面「白送」：`interface` 机器可读 + `inspect` 共用。

### 4.3 缺点 / 风险

| # | 问题 | 说明 / 建议 |
|---|---|---|
| **M1** | **`srcos://` 是自定义协议，不是 MCP resources** | MCP 客户端无法原生「浏览」SRCOS，只能逐个调 `list_paths`/`read_file`。同一件事有两套资源抽象（自研 `srcos://` 与闲置的 MCP resources）。**建议：保持现状** —— MCP resources 各客户端支持参差，投入产出比存疑 |
| **M2** | **无状态 ⇒ 轮询** | 与 A3 同源。加 SSE + progress 会破坏无状态、增加连接管理复杂度。**除非有真实长任务 agent 场景，否则保持** |
| **M3** | **REST 与 MCP 的 `submit` 语义不对称** | MCP 一律 `run=true`（agent 无法 drain 队列），REST 默认只入队。文档写了，但对调用者是隐藏语义 |
| **M5** | **工具数量增长无分级** | 12 个平铺；到 30 个时 agent 上下文里全是 schema。可考虑命名空间 |

---

## 5. 待决 / 建议

1. **托管 agent 的实例身份**（A1）：要做就得给 service 实例发一枚「权限 ≤ 所属用户」的 token。
   目前不做，但它是「SRCOS 里跑 agent」的前提。
2. **MCP resources vs `srcos://`**（M1）：保持现状，除非出现明确要求。
3. **dsh 路 A/B**：已移除。若将来出现真实的 dsh 互操作需求，以**锁定版本 + 真实验证**重建，
   不要让未验证的半成品挂在文档里冒充能力。
4. **agent 提交的幂等键**（A2）：✅ 已做（`idempotency_key`，见 ADR-019 第二期契约）。
