# `@seqyuan/srcos-dsh`

把 **SRCOS 当作 dsh 的一等资源协议**：在 DeepSeek Harness 里浏览与预览某个 SRCOS 网关上的
home、各工具的工作区、已授权的共享数据（ADR-016 的「路 B」）。

它做的事情很小、很明确：

| 它在 dsh 里注册什么 | 用的是什么 |
|---|---|
| 一个 **resource protocol provider**（协议名 `srcos`） | `ctx.resources.register({protocol, open})` |
| 一个 **侧边栏 tab 类型**（认领 `dsh-resource://srcos/**`） | `ctx.sidebarRightTabs.register(definition)` |
| 一个 **面板体**（目录浏览 / 文本预览 / 失败原因） | `ctx.slots.register('sidebar.right.pane.tab')` |

## 地址映射：一次前缀替换

dsh 的资源地址是 `dsh-resource://<protocol>/…`（host 就是协议名），SRCOS 的是
`srcos://<provider>/<scope>/<path>`（host 是 provider）——两者恰好**错开一层**，所以映射是纯粹的
前缀替换，这也是 ADR-016「路 D 保险」当初要的形状：

```
srcos://file/workspace/out/x.txt?tool=demo
  ⇄ dsh-resource://srcos/file/workspace/out/x.txt?tool=demo
```

`file` 段被保留（而不是丢掉），所以将来 SRCOS 多一个 provider（`artifact` 等）不需要改这里。

## 安装

插件就是**一个文件**（`lib/client.js`），由 dsh 的 client module 系统按
`package.json` 的 `exports["./client"]` 读字节后挂在 `/plugins/<id>/client.js`。构建只需要 Node，
不需要 dsh 的构建工具链：

```bash
cd integrations/dsh-plugin
npm run build            # 生成 lib/client.js（也挂在 prepack 上）
```

然后二选一：

```bash
# ① 官方 out-of-tree 安装（推荐）
dsh plugin --profile web add /path/to/srcos/integrations/dsh-plugin

# ② 或者手工放进 profile：把本目录链接到 ~/.dsh/profiles/<profile>/node_modules/@seqyuan/srcos-dsh
```

重启 dsh（或等 HMR）后，侧边栏右侧的「引导」页里会出现 **SRCOS 资源** 胶囊；点开即可浏览。

## 配置

两份来源，按优先级：

1. **dsh 注入的全局**（profile 里声明插件配置时由 host 填）：`window.__SRCOS_DSH_CONFIG__`
2. **`localStorage`**：`localStorage['srcos-dsh']` = 同样的 JSON（方便先试，不必改 profile）

```json
{ "baseUrl": "http://192.168.0.106:30152", "token": "srcos_xxxxxxxx.yyyy", "pollMs": 5000 }
```

- `baseUrl` **必须显式写**：dsh 若被 SRCOS 反代，页面的 origin 提供的是 **dsh 自己的** `/api`，
  不是网关的，所以这里推断不出网关地址。
- `token` 是**你的 agent token**（在网关的 `/tokens` 页自助生成，选 `read` 即可；
  一个 `submit` token 也能读，但没必要）。
- `pollMs` 是元数据轮询间隔（默认 5 秒）。SRCOS 没有文件变更推送（只有日志端点有 SSE），
  所以这里用轮询：**只在 size/mtime 变化时才发新的一帧**，安静的文件不会让面板反复重绘。

## 为什么用 agent token，而不是浏览器 session

dsh 页面的 origin 不是网关的，session cookie 根本不会被带上；而「程序用什么凭据」在 SRCOS 里
早有答案：**agent token**（ADR-019，`Authorization: Bearer`）。它是你自己的、只读的、可在
`/tokens` 随时撤销的凭据。

## 能力与边界（不要误以为有）

- ✅ 目录逐级浏览、文本/Markdown/CSV 预览（读 `/api/resources/raw`）、失败原因直接显示。
- ⚠️ 图片 / PDF 只给一个「原始文件」链接：`raw` 端点的 URL **不带凭据**（把 token 放进
  query string 会进浏览器历史与代理日志），所以没有会话时那个链接会 401。
- ⚠️ 变更靠轮询，不是推送。
- ❌ 不写 SRCOS（本插件只读）；提交任务请用 SRCOS 的 MCP 端点（`srcos_submit_job`）。

## 验证状态（照实说）

| 部分 | 怎么验的 |
|---|---|
| 地址映射、配置读取 | `npm test`（纯 Node，无 dsh） |
| provider 的帧语义（去重、失败帧、abort） | `npm test`（假 gateway） |
| **打包契约**：manifest 形状、`exports["./client"]`、bundle 的自注册信封 | `npm test`（把产物塞进一个替身 `__ModuleLoader__`，断言 dsh 会找的那个面） |
| **装配**：`apply()` 是否按文档调 `ctx.resources.register` / `sidebarRightTabs.register` / `slots.register` | `npm test`（给一个假 ctx，断言注册的是 provider / tab 定义 / 面板体） |
| **真实的 SRCOS API**（roots / 目录 / 文本 / 缺资源 / 401） | `test/live-run.sh`：起一个临时网关、签一个 read token，用真 HTTP 跑同一个 provider |
| **在 dsh 里实际加载与渲染** | ⛔ **未验证** —— 没有往任何 profile 里装过。dsh 是 developer preview，插件 API 会变；`src/client.js` 里每个 API 都标了它读到的是哪个源文件 |

```bash
cd integrations/dsh-plugin
npm test                 # 19 个用例 + 2 个 live（未配置网关时自动跳过）
sh test/live-run.sh      # 起临时网关跑真实 API（需要仓库根目录的 ./srcos）

# 或从仓库根目录：
make dsh-plugin          # = node build.mjs && node --test test/*.test.mjs
```
