# scrna-demo：共享包（UI + task）最小示例

一个工具包同时注册 **一个 UI 服务**和一个**重计算任务**，两者共享包根：

```
scrna-demo/
├── ui/tool.yaml       # kind: service, root: ..
├── ui/serve.py        # 静态页面服务（只监听 SRCOS_PORT）
├── ui/index.html      # 参数表单 + 状态 + 日志 + 产物
├── ui/app.js          # 调 /api/jobs、/api/jobs/<id>、/logs、/artifacts、/cancel
├── task/tool.yaml     # kind: task, root: .., entry: work.sh
├── task/work.sh       # 读 SRCOS_PARAM_*，source $SRCOS_TOOL_DIR/shared/lib.sh
└── shared/lib.sh      # 两边共享的相对路径
```

`root: ..` 让 SRCOS 把这个包挂到 `/tool`，并把 `SRCOS_TOOL_DIR=/tool` 交给两个组件。
UI 只负责收集参数和呈现结果；执行仍然是一个普通的 `kind: task`，没有第三种 RunUnit。

## 两个工具

| 工具 id | kind | 作用 |
|---|---|---|
| `scrna-demo-ui` | service | 工具自己的页面；浏览器用**同源 session cookie** 调平台接口 |
| `scrna-demo-task` | task | 真正干活：按参数写 `/workspace/out/<task id>/report.txt` |

**workspace 是 per `(用户, 工具)` 的**：UI 的 `/workspace` 和 task 的 `/workspace` 不是同一个目录。
页面提交任务时 `tool` 指向 `scrna-demo-task`，由 `POST /api/jobs` 写进**任务工具**的投递目录。

## 跑起来

```bash
# 1. 注册期校验（两个组件都应出现）
./srcos tool validate --tools-dir srcos-tools

# 2. 起网关
./srcos serve --host 127.0.0.1 --port 30152

# 3. 授权（默认拒绝）：两个工具都要给用户
./srcos grant set scrna-demo-ui   --user alice
./srcos grant set scrna-demo-task --user alice

# 4. 启动 UI 服务（也可以从 /tools/scrna-demo-ui 页面点“启动服务”）
curl -b cookies -X POST http://127.0.0.1:30152/api/tools/scrna-demo-ui/start \
  -H 'Content-Type: application/json' -H 'Origin: http://127.0.0.1:30152' -d '{}'
```

浏览器登录后打开：

```
http://127.0.0.1:30152/proxy/alice/scrna-demo-ui/
```

填参数 → 运行 → 状态徽章从 `pending`/`running` 走到 `succeeded`，日志实时出现，
产物列表给出 `/workspace/out` 的 `/view?src=srcos://…` 链接。

> 页面资源用相对路径（`app.js`），依赖网关注入的 `<base>`；平台接口用根路径
> （`/api/jobs`），因为浏览器与网关同源。

## 等价命令行

```bash
./srcos job submit --user alice --tool scrna-demo-task \
  -n "scrna S001" --param sample_id=S001 --param copies=3 --param prefix=scrna \
  --output /workspace/out
```

## 校验点

- `tool validate`：`scrna-demo-ui` 与 `scrna-demo-task` 都在列表里，`root` 指向同一个包根。
- 任务日志第一行是 `tool dir = /tool`（证明共享包根与 `SRCOS_TOOL_DIR`）。
- 任务记录里 `/workspace/out/<task id>/report.txt` 出现，`.sign` 只在全部写完后才写。
- 平台不知道页面状态：刷新页面不会恢复表单；页面重开只会重新提交或查看新任务。
