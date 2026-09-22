# hello-fanout workspace

这个目录在实例第一次运行时从 `workspace-template/` 初始化（只初始化一次）。

## 沙箱内可见的路径

| 沙箱内路径 | 说明 | 模式 |
|---|---|---|
| `/workspace` | **就是本目录**，跨任务持久 | rw |
| `/workspace/jobs/<job-id>` | 本次任务目录，也是 `work.sh` 的 cwd | rw |
| `/workspace/out` | 产物目录（由 `work.sh` 创建） | rw |
| `/home/<user>` | 虚拟 home（**不是** OS 用户的真实 home） | rw |
| `/tool` | 工具包本身（只读） | ro |
| `/tmp` | tmpfs，实例结束即丢 | rw |

## 试一下

```bash
srcos job submit -d <config> --tools-dir srcos-tools \
  -n "hello demo" --tool hello-fanout \
  --param samples=S001,S002,S003 \
  --output /workspace/out

srcos job run -d <config> --tools-dir srcos-tools --tool hello-fanout
srcos job logs -d <config> <instance-id>
```

第一次跑完后再跑一次，`work.sh` 会因为 `/workspace/out/.sign` 已存在而直接跳过
—— 这就是 tool-spec 规范 #1（幂等）的实际效果。

想重跑：`rm -rf /workspace/out/.sign`（对应宿主路径 `<data>/ws/<user>/hello-fanout/out/.sign`），
或改 `--param` 让它成为一个新任务。
