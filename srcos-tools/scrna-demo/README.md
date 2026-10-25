# scrna-demo：共享包（UI + 两种 task）最小示例

一个工具包同时注册 **一个 UI 服务**和**两个重计算任务**，共享包根：

```
scrna-demo/
├── ui/tool.yaml       # kind: service, root: ..
├── ui/serve.py        # 静态页面服务（只监听 SRCOS_PORT）
├── ui/index.html      # 参数表单 + 状态 + 日志 + 产物 + 路径选择器
├── ui/app.js          # /api/jobs、/api/jobs/<id>、/logs、/artifacts、/cancel
├── task/tool.yaml     # 普通 task（不依赖共享数据）
├── task/work.sh
├── path/tool.yaml     # type: path + requires_storages: [data]
├── path/work.sh
└── shared/lib.sh      # 各组件共享的相对路径
```

`root: ..` 让 SRCOS 把这个包挂到 `/tool`，并把 `SRCOS_TOOL_DIR=/tool` 交给所有组件。
UI 只负责收集参数和呈现结果；执行仍然是一个普通的 `kind: task`，没有第三种 RunUnit。

## 三个工具

| 工具 id | kind | 作用 |
|---|---|---|
| `scrna-demo-ui` | service | 工具自己的页面；浏览器用**同源 session cookie** 调平台接口 |
| `scrna-demo-task` | task | 普通参数；写 `/workspace/out/<task id>/report.txt` |
| `scrna-demo-path` | task | `type: path` 输入 + `requires_storages: [data]`；用 `<srcos-path-picker>` 选择目录 |

**workspace 是 per `(用户, 工具)` 的**：UI 的 `/workspace` 和 task 的 `/workspace` 不是同一个目录。
页面提交任务时 `tool` 指向目标任务，由 `POST /api/jobs` 写进**目标任务**的投递目录。

> `scrna-demo-path` 需要部署声明 **id 为 `data` 的 storage**。可以直接复制
> [`config/storages.example.yaml`](../../config/storages.example.yaml) 为 `config/storages.yaml`，
> **把 `host_root` 改成自己机器上的目录**（示例里的 `/Volumes/data` 只是 node01 的值）。
> 没有这个 storage 时，普通 `scrna-demo-task` 仍然可以跑；path 组件的 picker 会显示服务端错误。

## 跑起来

```bash
# 1. 注册期校验（3 个包内组件都应出现；其他示例可能因本机环境缺失让整体退出 1）
./srcos tool validate --tools-dir srcos-tools | grep scrna-demo-

# 2. （path 组件需要）声明 data storage，host_root 换成你的目录
cp config/storages.example.yaml config/storages.yaml
$EDITOR config/storages.yaml

# 3. 起网关
./srcos serve --host 127.0.0.1 --port 30152

# 4. 授权（默认拒绝）：三个工具都要给用户
./srcos grant set scrna-demo-ui   --user alice
./srcos grant set scrna-demo-task --user alice
./srcos grant set scrna-demo-path --user alice

# 5. 启动 UI 服务（也可以从 /tools/scrna-demo-ui 页面点“启动服务”）
curl -b cookies -X POST http://127.0.0.1:30152/api/tools/scrna-demo-ui/start \
  -H 'Content-Type: application/json' -H 'Origin: http://127.0.0.1:30152' -d '{}'
```

浏览器登录后打开：

```
http://127.0.0.1:30152/proxy/alice/scrna-demo-ui/
```

- 选 `scrna-demo-task`：填样本/行数/前缀 → 运行 → `succeeded`，日志/产物可见。
- 选 `scrna-demo-path`：点“浏览…” → 从 `data` storage 里选目录 → 运行 → `succeeded`，
  日志里有 `input_dir=/data/...`，报告里列出该目录的前 50 个条目。

> 页面资源用相对路径（`app.js`），依赖网关注入的 `<base>`；平台接口用根路径
> （`/api/jobs`、`/assets/srcos-path-picker.js`），因为浏览器与网关同源。

## 等价命令行

```bash
./srcos job submit --user alice --tool scrna-demo-task \
  -n "scrna S001" --param sample_id=S001 --param copies=3 --param prefix=scrna \
  --output /workspace/out

# path 组件：参数值必须是 /data/... 下的已存在目录
./srcos job submit --user alice --tool scrna-demo-path \
  -n "path demo" --param input_dir=/data/ref --param prefix=demo \
  --output /workspace/out
```

## 校验点

- `tool validate`：三个组件的 `root` 指向同一个包根（`/…/scrna-demo`）。
- 任务日志第一行是 `tool dir = /tool`（证明共享包根与 `SRCOS_TOOL_DIR`）。
- `/workspace/out/<task id>/report.txt` 出现，`.sign` 只在全部写完后才写。
- path 组件的可浏览范围是 **task 工具**的 storage 闭包（不是 UI service 的）。
- 平台不知道页面状态：刷新页面不会恢复表单；页面重开只会重新提交或查看新任务。
