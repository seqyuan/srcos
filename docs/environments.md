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
| **`apparmor_restrict_unprivileged_userns`** | **`1`** | ⚠️ 全局处于限制状态，但已按二进制单独豁免（见下） |
| `user.max_user_namespaces` | `2061776` | ✅ |
| 实测 `unshare --user` | ❌ `写失败：/proc/self/uid_map: 不允许的操作` | 预期内（`unshare` 未获授权，**不代表 bwrap 不行**） |
| **`bwrap` 0.9.0（`/usr/bin/bwrap`）** | **✅ 已修复并验证通过（2026-09-22）** | ✅ **可用** |
| `apptainer` / `singularity` | 未安装（注意 `/usr/games/singularity` 是 pygame 同名游戏） | ❌ |
| `/etc/apparmor.d/` userns profile 机制 | **90 个二进制已获单独授权**（含 `bwrap` `runc` `podman` `buildah` `rootlesskit` `lxc-usernsexec`） | ✅ |
| `apparmor_parser` | `4.0.0~beta3` | ✅ |
| 免密 sudo | ✅ 可用（用户在 `sudo` 组） | ✅ |
| `runc` / `rootlesskit` / `slirp4netns` | `runc 1.2.4` 已装且 profile 存在 | 待验证 |

#### 修复记录（已执行）

Ubuntu 24.04 的 `apparmor_restrict_unprivileged_userns=1` 会拦掉未带 setuid 的 `bwrap`。
采用**按二进制单独授权**（与 Ubuntu 自带的 `lxc-usernsexec` / `ch-run` / `podman` / `runc` 同机制），
**而非全局关闭 `kernel.apparmor_restrict_unprivileged_userns`**（不降低纵深防御）：

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
```

撤销：`sudo apparmor_parser -R /etc/apparmor.d/bwrap && sudo rm /etc/apparmor.d/bwrap`

验证结果：T1 最小调用 ✓ · T2 SRCOS 同款挂载集 ✓ · T3 隔离性 ✓（沙箱内 `/` 仅剩 `bin dev lib lib64 proc tmp usr workspace`，`/home/seqyuan` 不可见）。

#### ⚠️ 重要发现：userns 把 group 权限位变成"人人可读"

```
宿主:  uid=1000(seqyuan) gid=1000 组=1000,4,24,27,30,46,101,110,985,1001   ← 10 个补充组
沙箱:  uid=1000         gid=1000 组=1000,65534(nogroup)                    ← 只剩主组 + overflow
```

bwrap 的 userns 把**所有未映射的 gid 折叠成 `65534`**，而沙箱进程本身就在 `65534` 组里。
因此宿主的 `group` 权限位在沙箱内**等于公开可读**。实测对照：

| 文件权限 | 宿主（`seqyuan`） | 沙箱 | 判定 |
|---|---|---|---|
| `-rw-------` owner-only | 仅 root | **拒绝** | ✅ 有效隔离 |
| `-rw-r-----` group | 仅 root / `docker` 组 | **可读** | ⚠️ **权限被放宽** |
| `-rw-r--r--` other | 所有人 | 可读 | — |
| `--uid 0 --gid 0`（userns 内 root） | — | 与默认相同 | ✅ 不额外提权 |

**对 SRCOS 的硬约束**：

1. **MountSpec 的粒度就是隔离的粒度。** bind 了 `/share`，沙箱内就能读 `/share` 下
   **所有 group-readable** 的内容（包含其他项目组的 `drwxrwx---` 目录），绕过 `Jail` 的子路径限制。
   → **必须 bind 到最小必要路径，绝不 bind 父目录。** 需多个 storage 就 bind 多个精确路径。
2. **`Jail` 与 `MountSpec` 是两个不同的边界**，不要混为一谈：
   - `Jail` 保护 **SRCOS 自己的 API**（`/api/paths`、文件预览、MCP）
   - `MountSpec` 保护 **沙箱内的进程**
3. `storages.yaml` 的 `host_root` **就是** bind 源，粒度不能更粗；且必须保证 SRCOS 的 OS 用户可达
   （bind 不改变权限，只让路径可见）。

### 资源限制通道（ADR-014）

| 通道 | 结果 |
|---|---|
| `systemd-run --user`（`--scope` 与 `--unit`） | ✅ **完全可用** —— `CPUQuota` / `MemoryMax` / `TasksMax` 三属性均被接受；瞬时 unit 可用 `ExecStopPost` 记判定（`$EXIT_STATUS`/`$SERVICE_RESULT`），退出后 ~1s 被回收（`LoadState=not-found`），所以判定要落文件 |
| `Linger` | ✅ `yes`（SSH 登出后 user manager 仍在） |
| cgroup v2 | ✅ 全控制器 `cpuset cpu io memory hugetlb pids rdma misc` |
| 直接写 `/sys/fs/cgroup` | ❌ 不行（预期内） |
| `prlimit` | ✅ 可用；`RLIMIT_AS` 被子进程继承（`ulimit -v = 262144`） |

**结论**：**首选 `systemd-run --user`**（cgroup 限制真正生效；2026-09-24 起任务与 service 都用瞬时 unit），`prlimit` 作为兜底。

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

### R / Shiny（做 `shiny-demo` 工具时实测，2026-09-26）

- `R` 与 `shiny` 在 **miniforge 前缀** `/pmo/miniforge3`（`/pmo` 是指向 `/Volumes/data/pmo`
  的**符号链接**）；`/usr/bin/R` 是另一个 R，**没有 shiny**。
- **沙箱里必须挂真实路径**：挂 `/pmo/miniforge3` 会让 R 的启动脚本报
  `.../bin/sed: not found` —— 脚本内部引用的是 `/Volumes/data/pmo/...`。
  `tool.yaml` 的 `ro_mounts` 写 `host: /Volumes/data/pmo/miniforge3`。
- 干净环境没有 locale，R 解析中文标签会警告 `unable to translate ... to native encoding`；
  `env: ["LANG=C.UTF-8", "LC_ALL=C.UTF-8"]` 即可（glibc 自带，不需额外文件）。
- `shiny` 的客户端从 `window.location.pathname` 推导 base URL 与 WebSocket 路径，因此
  **在 `/proxy/<用户>/<工具>/` 前缀下可直接工作**（实测：页面 200、前缀下 WebSocket 101、
  headless Chromium 里直方图渲染成功、无 console 错误）。

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

> **已确认（2026-09-22）：「不用需要 root 的工具」是架构偏好，不是环境限制。**
> node01 上免密 sudo + docker 组均可用，所以这是**主动选择** ——
> 为了可移植性、分发性、避免特权守护进程，也为了在真正的 SGE 登录节点（无 sudo）上能同样工作。
> 推论：docker 最多作为**可选 backend**，不得成为 `local` 路径的必需项；
> 主线仍是 `bwrap`（已修好）+ `systemd-run --user`。

---

## node060-gpu / SGE 集群 `annuo`（2026-09-28 探测）

`host: node060-gpu` · `user: yuanzan (uid=560)` · **SGE 8.1.9**（`SGE_ROOT=/opt/gridengine`，`SGE_CELL=default`，`SGE_CLUSTER_NAME=annuo`）

### 调度器

| 项 | 结果 |
|---|---|
| SGE 客户端 | ✅ `qsub/qstat/qdel/qconf/qalter/qhost/qacct` 全在 `/opt/gridengine/bin/lx-amd64/` |
| `qhost` | ✅ 数十台执行主机（`node000`…`node060-gpu`、`cloud00x`、`hsy-test01`…） |
| PE | `make` / `mpi` / **`smp`** |
| 队列 | `all.q` `gpu.q` `asm.q` `asm2.q` `auto.q` `bs.q` `denovo.q` `filter.q` `hbk.q` `TR.q` … |
| `qstat`（本机） | ✅ 可用；`qstat -xml` 默认只列**当前用户**作业（`<queue_info>`+`<job_info>`，SRCOS 解析的就是这个格式） |
| **`qsub`（本机）** | ❌ `denied: host "node060-gpu" is not a submit host` |
| **`qdel`（本机）** | ❌ 同样被 submit-host 限制（删除也要在提交主机上） |
| `qconf -sm`（管理员） | `admin` / `guanhuajin` / `root` —— **yuanzan 不是 manager**，无法 `qconf -as` 把自己加成提交主机 |
| 提交主机（`qconf -ss`） | `bj-sci-login` `bj-sci-login02` `bj-sci-master` `hsy-test01` `node010` |

> ⚠️ **关键事实：`qstat -xml -j <id>` 返回的是 `<detailed_job_info>`（没有 `<job_list>`），不是 SRCOS 解析的标准格式。**
> SRCOS 的 `queryState`/`UnitAlive` 已改为用 `qstat -xml`（按当前用户过滤、按 id 匹配）。这是真集群才暴露的 bug。

### 提交路径（本机不能直接 qsub 时）

| 主机 | ssh 免密 | 说明 |
|---|---|---|
| `hsy-test01`（192.168.200.3） | ✅ `yuanzan` | **提交主机且可执行**，用它转发 `qsub`/`qdel` |
| `bj-sci-master`（10.1.1.66） | ❌ `Permission denied (publickey)` | 提交主机，但无 key |
| `bj-sci-login`（10.1.1.67） | ❌ Permission denied | 提交主机 |
| `bj-sci-login02` / `node010` | ❌ 连接超时 | 提交主机，网络不可达 |
| `node060-gpu`（本机） | ✅ 自 ssh | `ssh -L` 隧道在本机→本机可用（service 测试靠它） |

**可行做法**：把 `sge.qsub` / `sge.qdel` / `sge.qalter` 指向转发到 `hsy-test01` 的包装脚本，`qstat` 用本机。
实测包装目录：`/annogene/data2/bioinfo/PMO/yuanzan/srcos_sge_probe/bin/{qsub-remote,qdel-remote,qalter-remote}`。

### 共享文件系统与计算节点

- `/annogene/data2` = NFS（`10.4.1.170:/ifs/data`），`/home` = NFS（`bj-sci-master:/home`）——**计算节点可见**（作业写出的文件可在登录侧读到）。
- 作业默认落在 `gpu.q@node060-gpu`（本机）；`python3` 用 miniforge 的（`/annogene/.../miniforge3/bin/python3`）。
  ⚠️ 本机 `/usr/bin/python3` 是 3.6，`import _random` 会 `failed to map segment from shared object`，别用它。
- **容器运行时**：计算节点普遍有 **Singularity CE 4.0.1（`/usr/local/bin/singularity`，setuid 安装）**，**无 apptainer**；
  `node060-gpu`（gpu.q）两者都无，`node001`（all.q）**可以跑**。
- ⚠️ **队列默认 `h_vmem` = 1 GiB 会把容器运行时压死**：SGE 的 `h_vmem` 以 `RLIMIT_AS` 强制，1 GiB 虚拟内存
  不够 singularity 的 Go 进程，表现为 `pthread_create failed: Resource temporarily unavailable`，
  或看起来毫不相干的 `FATAL: Couldn't determine user account information: user: lookup userid 560: invalid argument`。
  作业显式 `-l h_vmem=8G` 后，`singularity exec --contain --no-home --cleanenv --pwd ... --bind ... --env ...`
  正常工作（已用 `rst_report_1.0.sif` 在 `all.q@node001` 端到端验证）。
- 现成 SIF：`/annogene/data2/bioinfo/PMO/yuanzan/project/comm/commander_test/RD/rst_report/rst_report_1.0.sif`（46MB，可读）、
  `/annoroad/data1/software/install/images/SIF/*.sif`。

### 实测结论（2026-09-28）

`internal/runtime/sge/realcluster_test.go`（`SRCOS_SGE_REAL=1` 才跑）在 `annuo` 上全绿：

- task `exit 0`（产出文件在共享盘、作业跑在 `node060-gpu`）
- task `exit 7`（退出码经 rendezvous 正确回传）
- **service + `ssh -L` 隧道：登录侧经隧道 `GET` 到计算节点的 `python3 -m http.server` 返回 HTTP 200**，Stop 后 `qdel` 干净回收；隧道存活时 `ReattachUnit` 采纳
- **`qalter -l h_rt=...` 续期成功**（注意：`qalter -l` 会**替换**资源列表，必须重述 `h_vmem`，否则 SGE 拒绝改运行中作业）
- **`sandbox: apptainer`（实为 Singularity CE）跑通**：SGE 作业在计算节点上解析 `apptainer||singularity`，
  按声明挂载绑定并执行 SIF，容器内写回 `/workspace`；失败过的“用户信息查询”其实是默认 `h_vmem=1G` 的假象

发现的真 bug（已修）：`qstat -xml -j` 查询格式错误；task 体 `exec` 导致 `exit_code` 永不落盘；`qalter` 未重述 `h_vmem`。

---

## Phase 1 可直接用的真实用例

node01 上**已经在跑** SRCOS 想接管的三个目标工具，因此 Phase 1 的端到端验证可以直接用现状，
不必先造 `hello-fanout`：

| 工具 | 现状 | 建议的 `kind` / `backend` / `sandbox` |
|---|---|---|
| dsh | `127.0.0.1:3080` | `service` / `local` / `bwrap`（只绑回环，与 SRCOS 安全模型一致） |
| shiny-server | `*:3838`，`/srv/shiny-server` | `service` / `local` / `bwrap`（注意 `run_as shiny` 与组权限） |
| RStudio Server | `0.0.0.0:8787` | `service` / `local` / `bwrap`（**绑了全网卡，需改配置只为回环**） |

---

## 待补

- [x] **SGE 提交/执行环境探测**（2026-09-28）—— 集群 `annuo`，本机 `node060-gpu` 是 SGE 客户端但非提交主机；经 `hsy-test01` 转发可投递（见上节）
- [ ] `backend: sge` 的**正式部署点**（真正长期运行 `srcos serve` 的登录/提交节点，天然可 qsub/qdel）
- [ ] 计算节点探测（apptainer 是否装、共享盘挂载点、`h_vmem` 语义）
- [ ] 验证 `runc` + `rootlesskit` 能否在 AppArmor profile 授权下做无 root 容器化
      （`runc 1.2.4` + `/etc/apparmor.d/runc` 已存在，可能零配置可用）
