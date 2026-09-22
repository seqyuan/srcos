# SRCOS 会话交接简报

> **重开会话后先读这个。** 本文是交接简报，不是唯一状态来源。
>
> | 想知道什么 | 看哪 |
> |---|---|
> | 为什么这么设计（不变的） | [`../AGENTS.md`](../AGENTS.md) |
> | 我们决定了什么、做到哪了 | [`roadmap.md`](roadmap.md) |
> | 工具开发者要遵守的契约 | [`tool-spec.md`](tool-spec.md) |
> | 这台机器的实测环境事实 | [`environments.md`](environments.md) |
> | **目标 / 现状 / 下一步 / 已踩的坑** | **本文** |
>
> 最后更新：2026-09-22（Phase 0/1 完成，Phase 2/3 主体完成）

---

## 0. 30 秒版

**SRCOS 是 AI 平台的确定性执行后端。探索用 AI，执行用 SRCOS。**

现状：一个 Go 单二进制（`github.com/seqyuan/srcos`，17 个包，约 2.1 万行，250 个测试用例），
在**网关**（继承自 goprox 的多用户认证反向代理）之上长出了**工具平台**四层：
工具契约、实例化运行时、存储 provider、授权模型。全部在 node01 上端到端实测过。

下一步：**agent token → MCP Server（read-only）**。它是"走向现代化"的招牌，且全部前置件已就绪。

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
│ ✅ 工具注册：扫描 --tools-dir / $SRCOS_TOOLS_DIR                    │
│ ⛔ 流程注册（Flow）：Phase 4                                        │
│ ✅ 实例编排：Backend/Handle + 端口池 + 动态路由表 + Reconcile/Reaper│
└─────────────────────────────────────────────────────────────────────┘
┌─ 数据面（继承自 goprox）────────────────────────────────────────────┐
│ ✅ 反向代理：<base> 注入 · WebSocket · 带宽限制 · SSO 头 · cookie 隔离│
│ ⛔ 代理层尚未接入动态路由表（路由表就绪，但 handleProxy 还只读静态卡片）│
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
│ ⛔ agent token + MCP Server：Phase 3.5（下一步）                    │
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

### 2.3 尚未实现（明确边界，不要误以为有）

- ⛔ **流程编排（Flow / FlowRun）** —— Phase 4，`docs/flow-spec.md` 也未写
- ⛔ **MCP Server** —— Phase 3.5
- ⛔ **agent token 认证面** —— `config/agent-tokens.yaml` 还不存在
- ⛔ **管理端页面** —— 工具上架/下架/实例总览/强制停止都只有 CLI 与 HTTP API
- ⛔ **代理层接入动态路由表** —— `internal/route` 已就绪且已测，但 `handleProxy` 仍只读静态卡片，
  所以 service 实例目前**还不能通过网关访问**（只能直连 endpoint）
- ⛔ **授权变更热加载** —— 改 `grants.yaml` 需重启网关
- ⛔ **审计日志** —— 只有实例记录与日志文件，没有专门的审计流
- ⛔ **`apptainer` sandbox** —— 声明了会明确报错
- ⛔ **SGE 未接真集群** —— 架构与测试都在（用 fake runner），但从未在真登录节点上跑过
- ⛔ **前端打包（`webui/`）** —— ADR-012 的 Vite 包还没建；目前前端是 Go 内嵌模板 + 一个原语控件
- ⛔ **dsh 集成** —— ADR-016 的三层策略已定，一步未做

---

## 3. 下一步

### 3.1 建议的下一步（按顺序，理由在右）

| # | 做什么 | 为什么现在做 |
|---|---|---|
| **1** | **agent token 认证面**：`config/agent-tokens.yaml`（只存 hash、scope、过期、标签）+ `srcos token create\|list\|revoke` | MCP 的前置；agent 是程序，不能用浏览器 session cookie |
| **2** | **MCP Server（read-only）**：网关内置 `/mcp`（Streamable HTTP），工具 schema 从 `interface` **自动派生** | 全部前置件已就绪（机器可读 interface、`/api/paths`、`/api/jobs`）。这是"走向现代化"的招牌，让「探索用 AI，执行用 SRCOS」可演示 |
| **3** | **代理层接入动态路由表** | 目前 service 实例还不能通过网关访问，`/proxy/<user>/<tool>` 只对静态卡片生效。这是"平台"缺的最后一块 |
| **4** | 启动中进度页 + `svc reap` 定时调度 | 冷启动体验与自动回收 |
| **5** | 管理端页面（上架/授权/实例总览） | 让非 CLI 用户能运维 |
| **6** | Flow 编排（Phase 4） | 需要先定 `docs/flow-spec.md` |

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

### 4.2 进程与资源

| 坑 | 结论 |
|---|---|
| **RLIMIT_NPROC 是按 real UID 全系统计数的，不是按单元** | node01 上该计数已达 1169，钳到 512 会让所有 `clone()` 返回 EAGAIN —— 连 bwrap 建 namespace 都失败，报成误导性的 `Creating new namespace failed: Resource temporarily unavailable`。每单元进程数上限本质是 cgroup 能力（systemd `TasksMax` 语义正确），**prlimit 路径下已移除 `--nproc`** |
| **`systemctl list-units <pattern>` 不按裸名匹配**（需要 glob） | "先问存在吗"的预检会静默返回 false 并跳过 stop —— 服务于是活过了自己的停止命令。正解：**直接 `stop` 并解释它自己的错误**（`not loaded`/`not found` 视为成功） |
| **降级模式下替换外层 `cmd.Env` 会弄丢 `DBUS_SESSION_BUS_ADDRESS`** | `systemd-run --user` 起不来，报 `Failed to connect to bus`。正解：用**内层 `env -i`** 清环境，让外层 limiter 继承宿主环境 |
| **`pkill -f '<pattern>'` 会匹配到执行它的 shell 自身** | 如果 pattern 出现在 shell 的命令行里（通常都会），pkill 会杀掉自己。用不自匹配的模式（如 `srcos[-]dev`）或改用 `lsof -ti:<port> \| xargs kill` |

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
| 019 | MCP Server 内置网关，第一期 read-only；新增 agent token；**不做** MCP Client |
| 020 | 存储与路径 provider 化；**闭环：path 可选范围 == 已挂载 storage == `requires_storages`** |
| 021 | **OS 用户 ≠ SRCOS 注册用户**；虚拟 home/workspace 由 SRCOS 构造；隔离靠 mount namespace 而非 UID |
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
```

### 清理测试残留（容易漏）

```bash
systemctl --user stop 'srcos-*' && systemctl --user reset-failed 'srcos-*'
lsof -ti:20000-30000 2>/dev/null | xargs -r kill    # 端口池范围
lsof -ti:31111,31112,31113 2>/dev/null | xargs -r kill
```

---

## 7. 代码地图

```
main.go                     CLI 入口与一级子命令分发（serve/user/sso/tool/job/svc/grant）
cmd_job.go                  tool / job / svc 子命令 + 通用 flag 解析
cmd_grant.go                grant 子命令

internal/config/            用户/状态/路径/存储与授权文件位置；registry 扫描 users/*.yaml
internal/auth/              密码（bcrypt）、TOTP、会话 cookie、SSO、ClientIP/SameOrigin
internal/proxy/             httputil.ReverseProxy 的 Rewrite/ModifyResponse 全部逻辑
internal/server/            网关 mux、登录/TOTP 页面、工具页面、/assets、代理路由
internal/api/               管理 API（services）+ 工具/存储/任务 API（tools.go, jobs.go）
internal/web/               内嵌模板（Go html 字符串）+ toolpages.go（生成式表单）
  └ templates/srcos-path-picker.js   原语控件（Web Component）

internal/tool/              tool.yaml 类型 + 13 类注册期校验 + input 值校验
internal/job/               job.json 类型 + 对工具的校验 + 目录扫描 + Slug/NewID/Submit
internal/grant/             授权策略（默认拒绝 · 组/用户/public/通配 · 配额）+ yaml 往返
internal/storage/           StorageProvider（列表/浏览/Jail 复用/可达性检查）
internal/sandbox/           MountSpec + Jail（symlink 逃逸防护）+ bwrap argv 物化
internal/portpool/          loopback 端口池（真 bind 探测、Reserve 供 reconcile）
internal/route/             动态路由表（非回环拒绝、归属栅栏、段边界最长前缀）
internal/runtime/           编排层
  ├ instance.go             Instance 记录（task/service 共用）+ Paths + PathView
  ├ backend.go              Backend / Handle 接口 + Limiter(spawn) + Runner(build)
  ├ local.go                local backend（task=scope，service=unit）+ BuildInner
  ├ run.go                  RunTask / StartService / StopService / Reap / Reconcile
  └ sge/                    SGE backend：qsub 翻译 / qstat -xml 解析 / rendezvous / ssh -L
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
- ✅ **`systemd-run --user --scope` 完全可用**（`CPUQuota`/`MemoryMax`/`TasksMax` 均接受，`Linger=yes`）
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
读 AGENTS.md、docs/roadmap.md、docs/handoff.md，然后从 handoff §3.1 的第 1 项
（agent token 认证面）开始。
```

如果要继续做**已规划的**工作，说「继续」+ 指向 `handoff §3.1` 的编号即可。
如果要**换方向**，先说清要改哪一条不变式（`AGENTS.md`）或哪一条 ADR，
因为按仓库约定，架构回退必须先改文档再改代码。
