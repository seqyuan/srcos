# 统一的服务实例化 —— 设计

> **状态:已实现（2026-09-26）。** 决策已落成 **ADR-026 / ADR-027 / ADR-028**（roadmap §5），
> 本文保留为实现层面的详细设计（字段、语义、比较表、取舍）。
>
> 进度:步骤 1（声明式 `command:`）✅ · 步骤 2/3（`environment:` 具名环境 + 沙箱装配 + shiny-demo 迁移）✅ ·
> 步骤 4（`ingress` 补齐：websocket / bwlimit / port 可选）✅ ·
> 七步全部完成：命令 ✅ · 环境 ✅ · ingress ✅ · external ✅ · digest ✅ · ADR 与文档 ✅。
> 目标服务类型:R Shiny / Python Shiny / Jupyter / dsh(+ RStudio,见 §2.5)。
>
> 相关:ADR-002(backend 与 isolation 正交)、ADR-003(一个运行原语)、
> ADR-014(不用需要 root 的组件)、ADR-020(storage provider 与闭环)、
> ADR-021(虚拟 home)、ADR-025(实例身份)、AGENTS.md「环境与数据要分开」。

## 0. 一句话

把「服务」收敛成**一条**路径:**声明式清单(具名环境 + 启动命令 + ingress/lifecycle)+ SRCOS 实例化**;
而「转发一个已经跑着的后端」退化为这条路径上的一种 backend(`external`),不再是第二个子系统。

## 1. 现状与出入(为什么需要这份 plan)

### 1.1 已经成立的(有代码为证)

| 能力 | 证据 |
|---|---|
| **一条代理管线,两种路由来源** | `serviceForEntry(route.Entry)` 把实例渲染成与卡片同一个 `config.ServiceConfig`;注释:*"…therefore share one path through the proxy: base rewriting, route cookies, WebSocket support and bandwidth limits are written once and apply to both."* |
| **工具不需要知道多用户** | 工具只 `listen $SRCOS_PORT`(回环)。端口池 / 路由表 / `<base>` 注入 / SSO 头 / cookie 隔离全在 SRCOS |
| **每 (用户,工具) 一个实例** | `route.Key(user,tool)`、`InstanceID(user,tool,…)`、`data/ws/<user>/<tool>`、`data/homes/<user>`;`/proxy/<user>/<tool>/` 跨用户不可达 |
| **授权与工具正交** | `Grant`(默认拒绝 · 组/用户/public/通配 · 配额)+ agent token 的 scope × 白名单 |

### 1.2 出入

| 类别 | 出入 | 证据 |
|---|---|---|
| **授权** | **卡片不受 Grant 管辖**:靠「在你自己的 `config/users/<u>.yaml` 里」;用户还能自助加卡片 | `handleListServices` 直读 user config、无 `Allowed()`;`handleAddService` 不查 Grant |
| **启动方式** | **被写死**:`entry: work.sh` → `["bash", "<tool>/<entry>"]`;解释器/PATH/locale 全靠工具在 `work.sh` + `ro_mounts` + `env` 里手拼,每个服务类工具重复一遍 | `ResolveArgv`(`internal/runtime/local.go`) |
| **环境可移植** | 宿主路径**写死在工具包**(`ro_mounts: /Volumes/data/pmo/miniforge3`);且 `ro_mounts` 只校验绝对性**不校验存在** → 缺挂载点要等到 bwrap 启动才炸 | `tool.Validate` 的 ro_mounts 段 |
| **能力对齐** | 实例声明不了带宽上限(代理侧**已支持** `Entry.BWLimit`,`serviceForEntry` 也映射了,但 `StartService` 从不设、`tool.yaml` 也没字段);WebSocket 恒 `true` | `StartService` 的 `Routes.Put` |
| **复用单位** | 只有「每 (用户,工具) 一个进程」一种;没有「一后端多用户 + 每请求身份」(那是卡片 + SSO)、也没有「一用户多同类实例」 | `route.Key`;`svc start` 先停后起 |
| **协议覆盖** | **非 HTTP 转发完全没有**(代理是 `httputil.ReverseProxy` + 路径路由) | `internal/proxy` |
| **认知** | 两套目录(仪表盘 cards vs `/tools`+`/tasks`)、命名撞车(`config.ServiceConfig` 与 `tool.KindService` 都叫「服务」) | — |

**结论**:「实例化分发」这条腿完整;「权限」这条腿只有工具那半落地;「转发既存后端」有位置但属于另一个世界。本 plan 处理前两条,并把第三条并进来。

---

## 2. 设计一:统一的服务清单

目标:一个服务类工具只需要回答三个问题 —— **跑什么、用什么解释器、怎么探活**。

```yaml
# srcos-tools/r-shiny/tool.yaml（目标形态）
schemaVersion: 1
id: r-shiny
version: 0.2.0
kind: service
backend: local
sandbox: bwrap

environment: r-miniforge            # ← 新增：具名环境（解释器/依赖从哪来）
command: ["Rscript", "app.R"]       # ← 新增：启动命令（argv，不做 shell）
# entry: work.sh                    #    逃生口：需要真脚本时才用（与 command 二选一）

ingress:
  healthcheck: {path: "/", timeout: "60s", startup_grace: "180s"}
  websocket: true                   # ← 新增（默认 true）
  bwlimit: 0                        # ← 新增：字节/秒，0 = 不限
lifecycle: {restart: on-failure, max_lifetime: "12h", idle_ttl: "1h"}
resources: {cpu: 1, memory: "2Gi"}
workspace: {init_from: workspace-template/}
```

### 2.1 `environment:` —— 具名环境(管理端声明,工具引用)

**问题**:`/Volumes/data/pmo/miniforge3` 是**这台机器的事实**,不是工具的属性。写在工具包里,
换一台机器就要改工具包 —— 「工具只考虑实现」在多主机时破功。

**设计**:与 `storages.yaml` **同构**的一个 provider(ADR-020 的形状照搬)。

```yaml
# config/environments.yaml —— 管理端声明（宿主事实）
environments:
  - id: r-miniforge
    name: "R 4.4 (miniforge)"
    root: /Volumes/data/pmo/miniforge3     # ro 挂进沙箱，且**路径与宿主一致**
    env:
      - "PATH=/opt/srcos/bin:/Volumes/data/pmo/miniforge3/bin:/usr/local/bin:/usr/bin:/bin"
      - "LANG=C.UTF-8"                     # 干净沙箱没有 locale（实测过）
      - "LC_ALL=C.UTF-8"
    provides: [R, Rscript]                 # 注册期断言 <root>/bin/<name> 存在且可执行
  - id: py-3.11
    root: /opt/conda/envs/py311
    env: ["PATH=/opt/srcos/bin:/opt/conda/envs/py311/bin:/usr/bin:/bin"]
    provides: [jupyter, shiny]
```

- **为什么 `root` 挂载路径必须与宿主一致**:解释器内部用绝对路径(实测:R 的启动脚本引用
  `/Volumes/data/…/bin/sed`,把 `/pmo` 挂进去就报 `not found`)。这条要写进文档,否则每个人都会踩。
- **`provides`** 把「R 装了没」「shiny 装了没」提前到**注册期**,而不是等实例起来再失败(实测撞过一次:
  PATH 上先找到的 `/usr/bin/R` 没有 shiny)。
- **校验的两级**,对齐 storage 的 `CheckReachable()`:
  1. **声明层(硬失败,机器无关)**:工具引用了一个 `environments.yaml` 里不存在的 id → 拒绝注册;
     环境之间 id 重复、`root` 非绝对路径 → 拒绝。
  2. **宿主层(警告)**:声明的 `root` 在这台机器上不存在 / `provides` 缺失 → `tool validate` 与网关启动时
     **警告**(不阻断,否则 CI 上无法校验带 R 的工具);**启动实例时硬失败**并给出明确消息。
- **`env` 的顺序**:平台默认(`PATH`/`TMPDIR`/`SRCOS_*`) → 环境 → 工具自己的 `env:`(工具最后,同名以工具的为准)。
  环境里的 `PATH` **必须自己带上 `/opt/srcos/bin`**(它是契约的第一项),注册期校验这一点。
- **与 `ro_mounts` 的关系**:`environment` 是**环境**的具名声明(生成挂载 + env);`ro_mounts` / `env:`
  保留为**逃生口**(单机、临时、工具自带二进制)。三者合并后的最终挂载表仍走 `MountSpec`,粒度规则不变。

### 2.2 `command:` —— 声明式启动命令

```yaml
command: ["jupyter", "lab", "--no-browser", "--ip", "127.0.0.1",
          "--port", "${SRCOS_PORT}", "--notebook-dir", "${SRCOS_WORKSPACE}"]
```

- **argv 直接 exec,不经 shell**(无 `sh -c`):不做引号/注入处理,也就没有注入面。
- **`${SRCOS_*}` 白名单替换**:只展开平台自己注入的那些变量(`SRCOS_PORT` / `SRCOS_WORKSPACE` /
  `SRCOS_HOME` / `SRCOS_USER` / `SRCOS_TOOL` / `SRCOS_TOOL_VERSION` / `SRCOS_INSTANCE_ID` / `SRCOS_API`),
  其余 `${...}` **注册期直接拒绝**(拼错在注册时就报,而不是运行时)。这些变量的值同样在环境里,
  所以 `command` 只是省掉「自己从 env 读」这一步。
- **与 `entry` 二选一**:`command` 覆盖常见形态;需要循环/条件/多进程的仍写 `entry: work.sh`。
  **两者都给 → 拒绝注册;都不给 → 拒绝注册**(现在的 `entry` 是必填,改成二选一)。
- **task 也开放 `command`**(对称:`command: ["python", "analyze.py"]`),规则同样适用 —— ADR-006 的
  「必须同步阻塞到所有实际工作完成」不变(命令必须阻塞)。
- **优先级**(沿用 `ResolveArgv` 的现有形状,只插一层):
  1. job 目录里的 `work.sh`(task 的提交期脚本覆盖,现状保留)
  2. `command:`
  3. `entry:`

### 2.3 `ingress` 的补齐

| 字段 | 状态 | 动作 |
|---|---|---|
| `healthcheck` | 已有 | 不动 |
| `backend_path` | 已有(2026-09-26 修:不再取自 healthcheck) | 不动 |
| `websocket` | **新增**,默认 `true` | `StartService` 用它填 `Entry.WebSocket`(现在硬编码 true) |
| `bwlimit` | **新增**,字节/秒,0=不限 | `StartService` 填 `Entry.BWLimit`(代理与 `serviceForEntry` 早已支持) |
| `port` | 现存但**对 local/sge 无意义**(校验要求、实际用 `$SRCOS_PORT`) | 改为**可选**;文档写明「instantiated 后端一律用平台分配的 `$SRCOS_PORT`;`port` 只对 `external` 与将来的 apptainer 有意义」 |

### 2.4 目标服务类型的映射(统一模型能否覆盖)

| 服务 | `environment` | `command` | 备注 |
|---|---|---|---|
| **R Shiny** | `r-miniforge` | `["Rscript", "app.R"]`(app 读 `$SRCOS_PORT`) | 已实测可跑(见 `srcos-tools/shiny-demo`) |
| **Python Shiny** | `py-3.11` | `["shiny", "run", "--host", "127.0.0.1", "--port", "${SRCOS_PORT}", "app.py"]` | 前缀下可直接工作(同 shiny 的 base 推导) |
| **Jupyter** | `py-3.11` | `["jupyter", "lab", "--no-browser", "--ip", "127.0.0.1", "--port", "${SRCOS_PORT}", "--notebook-dir", "${SRCOS_WORKSPACE}"]` | 需注意 token/口令:建议 `--IdentityProvider.token=''` 关掉它自己的认证(平台已鉴权),或用 SSO 头 |
| **dsh** | `node-22` | `["dsh", "web", "--port", "${SRCOS_PORT}"]` | 本地优先应用的 Host/Origin 栅栏由网关的 Origin 改写消化(实测) |
| **RStudio Server** | `r-rstudio` | `["rserver", "--www-port", "${SRCOS_PORT}", "--server-user", "…"]` | ⚠️ 它自带用户/会话模型(要系统账号),与「虚拟用户」冲突。**单独评估**,可能永远不做 —— 见 §8 |

结论:**四种目标服务落在同一个模型里**;RStudio 是唯一需要单独判断的(它的多用户模型与 ADR-021 相冲)。

### 2.5 新增校验规则(注册期)

| 规则 | 违反 |
|---|---|
| `command` 与 `entry` 恰好给一个 | 拒绝注册 |
| `command[0]` 非空;`command` 里出现的 `${...}` 必须在白名单内 | 拒绝注册 |
| `environment` 必须已在 `environments.yaml` 声明 | 拒绝注册(宿主无关) |
| `environment` 声明的 `root` 不存在 / `provides` 缺失 | **警告**;启动实例时硬失败 |
| 环境 `env` 里的 `PATH` 必须含 `/opt/srcos/bin` | 拒绝注册(否则平台的二进制不可达) |
| `bwlimit` ≥ 0;`websocket` 是布尔 | 拒绝注册 |

---

## 3. 设计二:`external` backend —— 转发既存后端成为工具的一种后端

**动机**(来自讨论):有些需求是「这个服务已经在跑,给我一条受管、可授权的路径」,而不是「给我起一个」。
现状是卡片,但它不受 Grant 管(§1.2),于是平台有两个授权真相。

**设计**:`backend` 是「单元在哪跑」的轴(ADR-002),`external` 是它的一个取值。

```yaml
kind: service
backend: external
external: {host: 127.0.0.1, port: 3838}   # 目标后端（已存在）
ingress: {backend_path: /, websocket: true, healthcheck: {path: "/"}}
# 不需要 environment / command / entry / resources
```

- **`Start` 的语义**:不启动任何进程,只把 `external` 里的端点**发布到路由表**,返回一个
  `WantEndpoint() == true` 的 Handle(先例:SGE 也用 `WantEndpoint` —— 后端自己决定端点)。
- **不 reap、不 stop**:SRCOS 不是它的父进程。`StopService` 只撤路由与记录,`Reaper` 跳过
  `backend: external`(`isReapable` 加一条)。这是**必须写清楚的边界**:能停的只有自己起的。
- **安全护栏沿用**:端点仍过 `ResolveAllowedIPs`(只 private/loopback)+ 自环拒绝,与卡片同一套。
- **与卡片的关系**:卡片**不删、不改**,保持「用户自己的、可自助添加的转发」。但从此**新东西走工具**,
  且**转发实现只有一份**(`external` backend);卡片可以逐步容器化成一个「把卡片合成成 external 工具」
  的适配器(实现阶段再定),两条路最终共享同一个转发内核与同一个授权模型。
- **收益**:Grant / 配额 / 审计 / 目录 / 画布 全部复用;「转发」不再是第二个世界。

---

## 4. 设计三:工具的源码与版本(取舍)

### 4.1 问题

今天 `version: 1.2.3` 是**自报字符串**:目录内容变了版本号可以不变,流程 pin 的 `tool@1.2.3` 指向的
是一份可变内容。对「可复现 / 可审计」这是真的缺口 —— 审计里「哪个版本的工具」目前不可验证。

### 4.2 取舍

| 方案 | 「版本」是不可变的吗 | 多机分发 | 成本 | 结论 |
|---|---|---|---|---|
| **目录扫描**(现状) | ❌ 自报字符串 | 手工 rsync | 0 | 作为兜底保留 |
| **+ content digest**(底线) | ✅ 变了就看得出来 | 同现状 | **低** | **做** |
| **+ git 仓库**(可选) | ✅ commit 不可变 | `git pull` | 低(要求 tools-dir 是仓库) | **可选做** |
| 打包 / OCI registry | ✅ | registry | 高 | **不做**(与「单二进制零依赖」冲突) |

**推荐:digest 为底线 + git 为可选增强。**

- **digest**:加载工具包时按「相对路径 + 模式 + 内容」排序哈希,得到包级 digest(排除 `.git`、`node_modules`、
  `__pycache__` 之类)。**记进实例记录与审计**(`tool=r-shiny@0.2.0#ab12cd34`),`tool list` / `/api/tools` 也显示。
- **git(可选)**:若工具目录(或其祖先)是 git 仓库,额外记 `HEAD` 短 sha + dirty 标志。
  策略开关 `tools.require_clean_git`(默认 **只警告**,可配成拒绝)。这是「源码管理」的实际答案,但不强加。
- **digest ≠ 签名**:它证明「内容和记录一致」,不证明「内容可信」。信任锚的事在 ADR-024(审计外发)那一层,
  不要把两件事混起来。

### 4.3 工具源码 vs 应用源码的边界(必须点名的一个取舍)

| | 工具源码 | 应用源码 |
|---|---|---|
| 是什么 | `tool.yaml` + `work.sh`/`command` + 只读模板 | 用户在 workspace 里改的 `app.R` / `app.py` / `*.ipynb` |
| 归属 | 平台资产,应版本化(digest/git) | 用户数据,不版本化 |
| 现状 | `workspace-template/` 里的 `app.R` 被**一次性拷贝**进用户工作区 | 拷贝之后用户可改,但**不再跟随工具升级** |

**这是取舍,要明说**:
- **一次性拷贝(现状)**:用户可改;代价是工具升级不带过去(每个用户的工作区是分叉)。
- **每次同步 / 符号链接**:跟随升级;代价是用户改不了(或改了会被覆盖)。
- 两种都合理,**取决于工具是「产品」还是「脚手架」**。建议默认仍是拷贝,并允许工具声明
  `workspace: {sync: true}`(强制覆盖)或 `{template: true}`(不拷,只读挂载一个模板目录)。

---

## 5. 安全边界

- **环境是环境,不是数据**。`environment.root` 是**只读**挂载(AGENTS.md:环境 vs 数据要分开);
  数据仍走 `storages`,用户可选的路径仍由 `requires_storages` 决定(ADR-020 的闭环不变)。
- **`MountSpec` 的粒度规则不变**:环境只挂它自己的前缀,绝不挂父目录(userns 的 group 位折叠)。
- **`command` 无 shell**:argv 直接 exec,`${SRCOS_*}` 只做白名单文本替换。
- **`external` 不越权**:不启动、不停止、不回收别人的进程;端点仍受 `ResolveAllowedIPs` 与自环拒绝。
- **`environment` 是管理端声明的**:工具作者**不能**在包里写任意宿主路径 —— 这既是可移植,也是安全收益
  (今天 `ro_mounts` 允许工具作者声明任意宿主路径,只是受挂载粒度约束)。

---

## 6. 兼容与迁移

- **不破坏现有工具**:`ro_mounts` / `env:` / `entry:` 全部保留(`environment` / `command` 是**新增的可选路径**)。
  `hello-fanout` 与 `shiny-demo` 现状可跑。
- **卡片不动**:`config/users/<u>.yaml` 的 `services` 语义不变;`external` 是**新增**的注册方式。
- **第一个迁移样例**:把 `shiny-demo` 迁到 `environment: r-miniforge` + `command: ["Rscript","app.R"]`,
  work.sh 退化成不存在 —— 用它证明「新模型能把 host 事实从工具包里搬出去」。
- **`ingress.port` 变可选**:同时更新 e2e 与示例工具。

---

## 7. 实现计划(每步一个提交,可独立 build + test)

| # | 做什么 | 验收 |
|---|---|---|
| 1 | `command:` + `${SRCOS_*}` 白名单替换 + 与 `entry` 二选一 + 校验 | 单测:argv 展开、未知 `${X}` 被拒、两者都给/都不给被拒;一个示例工具改用它 |
| 2 | `config/environments.yaml` + provider(声明/校验/可达性) | 单测:未声明的 id 硬失败、宿主缺失只警告、`PATH` 缺 `/opt/srcos/bin` 被拒 |
| 3 | 沙箱装配接入 `environment`(挂载 + env 顺序)+ 启动前 `provides` 检查 | 集成:`shiny-demo` 用 `environment` 跑起来,日志无 locale 警告 |
| 4 | `ingress.websocket` / `ingress.bwlimit` 接线;`port` 变可选 | 单测:`StartService` 把它们写进 `route.Entry`;e2e 里断言一次 |
| 5 | `backend: external` + `external: {host,port}`(不启动/不 reap)+ 校验 | 单测:Start 返回 `WantEndpoint` Handle、路由可发布、Reaper 跳过它;手工 e2e:转发一个已跑的 shiny-server(3838) |
| 6 | content digest(实例/审计/列表)+ 可选 git 记录 | 单测:改一个字节 digest 变化;`tool list` 显示;审计带 digest |
| 7 | `shiny-demo` 整体迁移 + 文档(tool-spec 主战场、README、AGENTS 明确不做非 HTTP、roadmap 加 ADR) | `tool validate`、`make e2e`、真实浏览器各过一次 |

**明确不做**(写进 AGENTS.md):非 HTTP(TCP/UDP)转发;工具 registry 服务;OCI 打包;让 `command` 跑任意 shell。

---

## 8. 待你拍板

| # | 问题 | 我的倾向 |
|---|---|---|
| D1 | 具名环境叫 `environment:` + `config/environments.yaml`?还是别的名字/位置 | 倾向这样(与 `storages` 同构,名字直白) |
| D2 | `command:` 对 **task** 也开放吗(对称) | 开放;省掉简单任务的样板 |
| D3 | `ingress.port`(对 local/sge 无意义)**保留还是删除** | 改可选 + 文档说明;留给 `external`/apptainer |
| D4 | git:默认「只警告 dirty」还是提供「拒绝 dirty」的开关 | 都做,默认只警告 |
| D5 | 应用源码:`init_from` 一次性拷贝(可改不跟升级)vs 同步(跟升级不可改)—— **默认哪个** | 默认保持拷贝;加 `sync`/`template` 两个可选项 |
| D6 | digest 是否进入 `tool@version` 引用语义(流程 pin 会变) | 先只记录与展示,**不改** `tool@version` 的匹配语义 |
| D7 | **RStudio 要不要做**(它自带系统账号与会话模型,与「虚拟用户」冲突) | 倾向不做,或只做「单实例共享 + SSO 头」那种形态 |

定了 D1/D5/D7 就能开工第 1 步。
