# 工具访问申请/审批流 —— 设计

> 状态:**设计已确认,待实现**。日期:2026-09-26。
> 相关:ADR-024(审计)、ADR-017(定位)、`docs/tool-spec.md`、`docs/agent-mcp-positioning.md`。
> 本文回答「一个非管理员的用户怎么拿到一个他还没有的工具」——今天是线下找管理员。

## 1. 问题

授权现在是**纯管理动作**:只有 `srcos grant`(CLI)与 `/admin` 的授权编辑器。
用户不能表达需求,只能线下找管理员,管理员再敲命令。对一个「非工程师用户也能用」的定位,这是缺的一环。

张力:授权是**默认拒绝**(未被 grant 提到的工具对非管理员不可见) ——
那么用户要如何申请一个**他看不见**的工具?

## 2. 决策(已确认)

| 问题 | 决定 |
|---|---|
| 用户能看见哪些工具? | **只列出被显式标为「可申请」的工具**(`requestable: true`)。保住默认拒绝;「可申请」本身是管理员的一次显式决定,与「只有允许,没有 deny」同构 |
| 批准后的授权粒度 | **永久**(不做有效期)。给 grant 加 `expires_at` 是第二期 |
| 通知 | **无邮件 / 无 webhook**。管理员看 `/admin` 的待审徽章,用户刷新看状态 |
| 多级审批 / 配额申请 | **不做**(过度设计) |

## 3. 数据模型

### 3.1 grant 新增一个开关

```yaml
# config/grants.yaml
grants:
  - tool: scrna_qc
    users: [alice]
    requestable: true      # ← 新增:非管理员可以在工具目录里申请它
```

- 语义:**「这个工具可以对未授权用户公开它的存在,并接受申请」**。它是**存在性公开**,不是授权。
- 默认 `false`:不写就等于不可申请(默认拒绝不变)。
- 申请一个已经可用的工具 → 拒绝(无事可做)。

### 3.2 申请记录(运行态)

```yaml
# data/requests/<id>.yaml
id: req-20260926-alice-scrna_qc-3f2a
user: alice
tool: scrna_qc
reason: "要跑一次 QC"
state: pending            # pending | approved | denied
created_at: 2026-09-26T12:00:00Z
decided_at: 2026-09-26T12:05:00Z     # 终态才有
decided_by: root                     # 终态才有
decision_note: ""                    # 拒绝理由
```

- 放在 `data/`(运行态),不是 `config/`(声明态)—— 与审计同理。
- **每个 (user, tool) 最多一条 pending**:重复申请返回已有那条,不新建(幂等,防刷屏)。
- `id` 由 user+tool+时间派生,文件系统安全且可读。

## 4. 接口

| 方法 | 路径 | 谁 | 语义 |
|---|---|---|---|
| `GET` | `/api/requests` | session | 我自己的申请(最旧在前) |
| `POST` | `/api/requests` | session | `{tool, reason}` —— 校验工具存在、`requestable`、我当前**不可用**、无 pending |
| `GET` | `/api/admin/requests?state=pending` | 管理员 | 全部申请(可按状态筛) |
| `POST` | `/api/admin/requests/<id>/approve` | 管理员 | → `AddUserToGrant(tool, user)` + 落盘策略 + 记录 approved |
| `POST` | `/api/admin/requests/<id>/deny` | 管理员 | `{note}` → 记录 denied |

**批准复用 `grant.Policy.AddUserToGrant` + `saveGrantPolicy`**:所以「为什么 alice 能用这个工具」
依旧指向 grants.yaml 里的一行,而不是一个隐式的旁路 —— 可审计性不被这个功能破坏。

## 5. 界面

- **用户侧 `/tools`(工具目录)**:除可用工具外,列出**可申请但当前不可用**的工具,
  带一个「申请」按钮(填理由)。已申请未决的显示「待审批」。
- **用户侧 `/requests`**:我自己的申请与状态(服务端渲染,无 JS 也能读)。
- **管理端 `/admin`**:新增「待审申请」区(计数徽章),逐条批准/拒绝。

## 6. 审计(复用 ADR-024,无需新机制)

| action | actor | 时机 |
|---|---|---|
| `request.create` | 申请用户 | 提交申请 |
| `request.approve` | 批准的管理员 | 批准(并写 grant) |
| `request.deny` | 拒绝的管理员 | 拒绝(带理由) |

## 7. 安全边界

- `requestable` 是**显式的管理员 opt-in**,默认关闭 —— 不改变「默认拒绝」。
- 申请本身**不授予任何东西**;它是一条待审记录。
- 批准走 `AddUserToGrant`,与 `srcos grant allow` 完全同一条路径(同一个配额与可见性语义)。
- 申请文件在 `data/`,不进配置备份。

## 8. 明确不做

- 有效期 / 自动回收授权
- 邮件 / webhook 通知;多级审批;配额或实例数申请
- agent(MCP)申请工具:agent 用所属用户的权限,不做 MCP 侧的申请工具
- 让 agent 看见「可申请」工具:`srcos_list_tools` 仍只列**可用**工具

## 9. 实现计划(每步一个提交)

1. `internal/accessrequest`:Request 类型 + Store(Save/Load/List/更新状态)+ 单测
2. `grant.Grant.Requestable` + 校验/往返 + CLI `grant set --requestable` + 管理端表单勾选
3. `inspect`:读侧暴露「可申请但不可用」的工具(带 `requestable` 标记);MCP 不变
4. `api`:`/api/requests` + `/api/admin/requests`(含 approve/deny)+ 审计埋点 + 测试
5. `web`:`/tools` 申请入口 + `/requests` 用户页 + `/admin` 待审区
6. `make e2e`:申请 → 批准 → 用户可见可用;拒绝路径;审计断言
7. 文档:tool-spec/README/roadmap/handoff + 变更记录
