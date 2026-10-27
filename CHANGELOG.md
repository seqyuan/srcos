# Changelog

## v0.2.0 — 2026-10-27

首个面向人的工具平台版本：工具自带页面，平台拥有执行、挂载、资源、实例与审计。

### 工具契约与 Task UI

- 共享包：`tool.yaml` 的 `root` 指向包根，UI 与 task 可以同目录注册；平台把包根只读挂到
  `/tool` 并注入 `SRCOS_TOOL_DIR`。不做 bundle、digest pin、多版本共存。
- Task UI 交接协议（`docs/tool-ui-spec.md`）：`POST /api/jobs`、`GET /api/jobs/<id>`、
  `/logs?follow=1`（SSE）、`/artifacts`、`/cancel`；状态以 `/api/jobs/<id>` 为准。
- `GET /api/jobs/<id>/artifacts` 只列 `interface.outputs` 声明的产物，带 `srcos://` 地址。
- 新示例 `srcos-tools/scrna-demo/`：UI service + 普通 task + `type: path` task，共用包根；
  UI 用 `srcos-path-picker` 选择 `data` storage 里的目录。
- 默认 `serve`（只设 `SRCOS_TOOLS_DIR`、不传 `--tools-dir`）也会启动任务队列；
  `srcos job submit <script>` 只产生一个 job，按 `work.sh` → `job.json` 顺序发布。

### 可靠性

- 实例记录新增 flock 串行化的 `runtime.UpdateInstance`（CAS 语义）：故意停止不会被迟到的
  verdict 覆盖；service 用 `StartedAt` 区分上一代停止与本次启动取消。
- claim 改为 flock 租约：`Claim` 返回 `*Lease`，进程死亡自动释放；权威文件移到
  `data/claims/<user>/<tool>/<job>.lock`，不再受工具可写的 workspace 影响。
- `job.SubmitAs`、`flow.SaveFlow/SaveLayout`、`grant.Save`、`activity.Flush` 改为原子写；
  `job.json` 用唯一临时文件发布。
- flow run 记录 owner pid/start；网关启动时把 owner 已死的 `running` 标记为 `interrupted`，
  节点回 `pending`，并提示用 `flow resume` 继续。`flow resume` 拒绝仍被 live owner 持有
  或残留 cancel flag 的 run。
- 修复队列测试在 `prepare` 创建 `data/` 子目录时的清理竞态。

### 运维

- 无需认证的 `GET /-/healthz`、`GET /-/readyz`、`GET /-/metrics`（实例状态、队列 in-flight、
  build info）；readyz 的 bwrap 检查按工具声明触发。
- `X-Request-ID`：合法客户端值回写，否则生成；网关自有请求记录结构化访问日志。
- `--log-format text|json`：JSON 模式把现有 `log.Printf` 桥接到 `slog`，一行一个对象。
- 新命令 `srcos doctor`：离线检查 config/data 可写、claims 的 flock 是否可用、按工具检查
  bwrap/apptainer、storages/environments 可达性、flow 包校验。
- 修复 `/-/metrics` 字面 `\n` 导致指标完全不输出的问题。

### 文档

- 新增 `docs/tool-ui-spec.md`、`srcos-tools/scrna-demo/README.md`；更新 `tool-spec.md`
  的 `/api/jobs/<id>` 与 `/artifacts`、README 的保留路径/`doctor`/`--log-format`；
  `docs/handoff.md` 记录各轮已验证事实与踩坑。

### 兼容性

- 认领标记的权威位置从 job 目录移到 `data/claims/`；旧 `.srcos-claimed` 仅在一个兼容窗口内
  （job walltime + 10 分钟）被尊重，之后忽略。不要同时用两个版本的二进制操作同一份 `data/`。
- 状态/日志/产物接口是增量新增；`POST /api/jobs` 与 `job.json` 契约不变。
