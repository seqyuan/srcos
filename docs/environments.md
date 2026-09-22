# 部署环境探测记录

> 由 [`scripts/probe-env.sh`](../scripts/probe-env.sh) 实测产出。
> 用途：确定 `tool.yaml` 的 `sandbox` / `backend` 写法、ADR-014 的资源限制降级路径、
> 以及 `storages.yaml` 的落点与配额方案。
>
> **每次换主机（新的登录节点 / 计算节点）都要重跑一遍并追加到本文档。**

---

## node01（2026-09-22 探测）

`host: node01` · `kernel: 6.8.0-139-generic` · `os: Ubuntu 24.04 LTS` · `user: seqyuan (uid=1000)`

### 沙箱能力

| 项 | 结果 | 判定 |
|---|---|---|
| `unprivileged_userns_clone` | `1` | ✅ |
| **`apparmor_restrict_unprivileged_userns`** | **`1`** | ❌ **bwrap 挂掉的根因** |
| `user.max_user_namespaces` | `2061776` | ✅ |
| 实测 `unshare --user` | ❌ `写失败：/proc/self/uid_map: 不允许的操作` | ❌ userns 被拦 |
| `bwrap` 0.9.0（`/usr/bin/bwrap`） | 无 setuid → `setting up uid map: Permission denied` | ❌ **当前不可用** |
| `apptainer` / `singularity` | 未安装（注意 `/usr/games/singularity` 是 pygame 同名游戏） | ❌ |
| `/etc/apparmor.d/` userns profile 机制 | 有 90+ 个（含 `podman` `runc` `buildah` `rootlesskit` `lxc-usernsexec`） | ✅ **可照抄** |
| `apparmor_parser` | `4.0.0~beta3` | ✅ |
| 免密 sudo | ✅ 可用（用户在 `sudo` 组） | ✅ **可自行修复** |
| `runc` / `rootlesskit` / `slirp4netns` | `runc 1.2.4` 已装且 profile 存在 | 待验证 |

**结论**：`bwrap` 只需一次 root 操作即可启用，**推荐 AppArmor profile 而非全局 sysctl**：

```bash
sudo tee /etc/apparmor.d/bwrap >/dev/null <<'P'
abi <abi/4.0>,
include <tunables/global>

profile bwrap /usr/bin/bwrap flags=(unconfined) {
  userns,
  include if exists <local/bwrap>
}
P
sudo apparmor_parser -r /etc/apparmor.d/bwrap
bwrap --ro-bind / / -- /bin/true && echo OK
```

撤销：`sudo apparmor_parser -R /etc/apparmor.d/bwrap && sudo rm /etc/apparmor.d/bwrap`

### 资源限制通道（ADR-014）

| 通道 | 结果 |
|---|---|
| `systemd-run --user --scope` | ✅ **完全可用** —— `CPUQuota` / `MemoryMax` / `TasksMax` 三属性均被接受 |
| `Linger` | ✅ `yes`（SSH 登出后 user manager 仍在） |
| cgroup v2 | ✅ 全控制器 `cpuset cpu io memory hugetlb pids rdma misc` |
| 直接写 `/sys/fs/cgroup` | ❌ 不行（预期内） |
| `prlimit` | ✅ 可用；`RLIMIT_AS` 被子进程继承（`ulimit -v = 262144`） |

**结论**：**首选 `systemd-run --user --scope`**（cgroup 限制真正生效），`prlimit` 作为兜底。

### 存储

| 路径 | 设备 | 类型 | 容量/使用 | 判定 |
|---|---|---|---|---|
| `/`（含 `/home/seqyuan`） | `ubuntu--vg-ubuntu--lv` | ext4 | 394G / 28% | 系统盘，**不适合放 workspace** |
| `/work` | `/dev/sda` | ext4 | 7.2T / 74% | 独立大盘，可用 |
| `/Volumes/data` | `/dev/sdd` | ext4 `stripe=64` | **15.5T / 50%** | **最佳 storage 落点** |
| `/Volumes/data1` | `/dev/sdc` | ext4 | — | 独立盘 |
| `/Volumes/process` | `/dev/nvme0n1` | ext4 | — | **NVMe，最快** |

- ❌ **数据盘全是 ext4，不是 XFS** → 原来的「XFS project quota 做 workspace 配额」方案不成立。
  配额改用：单独卷 / ext4 `prjquota`（需改挂载选项 + 重挂，要 root）/ 目录计数。
- ✅ 有 `setfacl`。

### 已在运行的服务（SRCOS 的直接用例）

| 端口 | 服务 |
|---|---|
| `127.0.0.1:3080` | **dsh**（deepseek-harness） |
| `*:3838` | **shiny-server**（`/opt/shiny-server`，`/srv/shiny-server`，组 `shiny-apps`） |
| `0.0.0.0:8787` | **RStudio Server** |
| `127.0.0.1:8080` | 待确认 |

> **这是最有价值的发现**：SRCOS 想接管的三个目标工具**已经在这台机器上跑着**。
> 第一个真实用例可以直接用现状验证，不需要先造工具。

### 环境管理

- ❌ 无 `module`
- ✅ `conda` / `mamba`（`/pmo/miniforge3/bin/`）
- conda 环境散落多处：`/pmo/anaconda2023/envs/*`、`/pmo/miniforge3/envs/*`、`/home/seqyuan/anaconda3_2023/envs/*`、`/Volumes/data/pmo/anaconda2023/envs/*`
  → `sandbox: none` 模式下 `ro_mounts` 要列多个路径（这也说明 `sandbox: bwrap` 值得修）

### 调度器

❌ **无 `qsub` / `qstat` / `qdel` / `qconf` / `qalter`**

> **node01 不是 SGE 登录节点。** 它是实验室的一台服务机（跑 shiny/RStudio/dsh）。
> `backend: sge` 的目标环境需要另外找真正的登录节点再跑一次本探测。

### 容器运行时与权限边界 ⚠️

| 项 | 结果 |
|---|---|
| `docker` | ✅ 可用（`Server 27.5.1`, `overlay2`），**用户在 `docker` 组** |
| rootless docker socket | ❌ 无 `/run/user/1000/docker.sock` → **是系统 daemon，即 root 等价** |
| `/etc/subuid` | ✅ 已配置（`seqyuan:100000:65536`，另有 `yuan`/`deng`/`tennis` 三个真实系统用户） |

> **这动摇了 ADR-014 的前提**：本机免密 sudo + docker 组 = 环境上**并不缺 root**，
> 「不用需要 root 的工具」是**架构偏好**（可移植性 / 分发性 / 避免特权守护进程），
> 而不是环境限制。**已向用户确认中** —— 若确认是偏好，则 docker backend 可作候选；
> 若是"环境禁止 root"（例如真正的 SGE 登录节点），则 node01 只是宽松的例外。

---

## 待补

- [ ] **真正的 SGE 登录节点**探测（决定 `backend: sge` 的全部实现细节与隧道方案）
- [ ] 计算节点探测（apptainer 是否装、共享盘挂载点、`h_vmem` 语义）
- [ ] 验证 `runc` + `rootlesskit` 能否在 AppArmor profile 授权下做无 root 容器化
      （`runc 1.2.4` + `/etc/apparmor.d/runc` 已存在，可能零配置可用）
