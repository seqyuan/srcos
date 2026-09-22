# dsh 配置示例

DeepSeek Harness（dsh）是典型的**本地优先应用**：只监听回环地址、自带 Host/Origin 信任栅栏、
前端还依赖只在安全上下文（HTTPS/localhost）可用的 Web API。这类应用直接暴露给局域网往往跑不起来
—— 但通过 SRCOS 一行服务卡片即可兼容。

## 为什么 dsh「本地优先」却连不上局域网

dsh 默认 `dsh web` 只监听 `127.0.0.1`，且出于安全做了三层限制：

- **只认回环地址** —— 绑定的 `127.0.0.1:3080` 让局域网其他机器根本连不上；
- **`/api` 信任栅栏** —— `isTrustedApiRequest` 要求请求的 Host 是回环或受信主机，
  且浏览器附加的 Origin 必须与 Host 一致，否则返回 `403 forbidden`；
- **安全上下文 Web API** —— 客户端 JS 每个 RPC 都调用 `crypto.randomUUID()`，
  该 API 在纯 HTTP（非安全上下文）下不存在，直接报 `crypto.randomUUID is not a function`。

SRCOS 的价值：**你不需要改 dsh 的源码、配置或启动方式**。网关把「浏览器 ⇄ 本地后端」之间的
所有差异都消化掉，dsh 保持本地优先、零改动。

## 配置 demo

### ① dsh 照常本地启动（不变）

```bash
# dsh 依然是本地服务，只监听回环
cd /path/to/deepseek-harness
nohup pnpm dsh web > /tmp/dsh-web.log 2>&1 &
# dsh web: http://127.0.0.1:3080
```

### ② 在 srcos 里加一张服务卡片

`config/users/<用户名>.yaml`：

```yaml
auth:
  password_hash: "$2a$10$..."   # bcrypt

services:
  # dsh：后端与网关同一台机器，host 用 127.0.0.1
  - id: dsh
    name: "dsh"
    description: "DeepSeek Harness Web GUI"
    host: "127.0.0.1"        # 本地优先应用，网关在本机转发
    port: 3080
    path: "/dsh"             # 前端访问路径
    websocket: true          # 事件流走 WebSocket/SSE，必须开启
    category: "AI"
    order: 0
    bwlimit: 10000000        # 可选：10 MB/s 带宽上限（上行/下行各限）
```

### ③ 访问

浏览器打开 `http://网关:7658/proxy/<用户名>/dsh/`（示例：`http://192.168.0.106:7658/proxy/alice/dsh/`），
登录网关即可使用，无需记住 dsh 的端口，也无需把 dsh 暴露到局域网。

## SRCOS 自动消化了哪些兼容问题

| dsh 的限制 | 不经过网关时的现象 | SRCOS 的兼容机制 |
|---|---|---|
| 只监听 `127.0.0.1` | 局域网其他机器无法访问 | 网关在本机转发，浏览器只与网关通信 |
| `/api` 信任栅栏（Host/Origin 必须一致） | RPC 全部 `403 forbidden`，页面空白 | 同源 Origin 改写：浏览器 Origin 改写成后端 authority，栅栏通过 |
| `crypto.randomUUID` 仅安全上下文可用 | `crypto.randomUUID is not a function` | HTML 注入 polyfill：用 `getRandomValues` 重建，HTTPS 下自动失效 |
| WebSocket / SSE 事件流 | `502`，101 升级失败 | 101 升级响应不做带宽限速包装，隧道直连 |
| 页面内绝对路径资源（`/assets`、`/api`、`/plugins`） | 资源请求落到网关根路径 → 404 | 「最近访问的服务」cookie 兜底转发到对应后端 |

以上全部自动生效，无需在 dsh 侧做任何配置（如 `base_url`、`--trusted-host` 等）。dsh 源码零改动。

## 常见问题排查

| 症状 | 原因 | 处理 |
|---|---|---|
| `crypto.randomUUID is not a function` | 纯 HTTP 非安全上下文缺少该 API | 确认走的是 `/proxy/<用户>/dsh/` 代理路径（polyfill 只注入到代理的 HTML） |
| `/api/*` 返回 `403 forbidden` | dsh 信任栅栏判定 Origin/Host 不一致 | 升级到含 Origin 改写的 srcos 版本；确认不是直连后端 |
| WebSocket 连接报 `502` | 带宽限速包装了 101 升级响应 | 升级到含 101 修复的 srcos 版本；`bwlimit` 可正常保留 |
| 页面能打开但工作区/会话是空的 | RPC 请求被栅栏或 polyfill 缺失阻断 | 浏览器控制台无报错则正常；否则对照上两行 |

## 同类「本地优先」应用

同样的配置思路适用于其他只认回环地址的内网应用：Jupyter（`--ip=127.0.0.1`）、
code-server（`--bind-addr 127.0.0.1`）、本地大模型 API 服务等。只要后端跑在网关能访问的地址上，
加一张服务卡片即可通过统一入口访问。

进阶：**根路径 SPA + HTTPS**。若应用是 Nuxt/Next 这类客户端路由 SPA（无法理解 `/proxy/` 前缀），
在用户配置加 `default_service: <服务ID>` 即可在网关根路径下零改动访问。
