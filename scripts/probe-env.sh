#!/usr/bin/env bash
# SRCOS 部署环境探测 —— 在目标主机（登录节点 / 单机）上运行。
#
# 用途：确定 sandbox 与资源限制通道的可用性，从而确定 tool.yaml 里
#       sandbox / backend 该怎么写，以及 ADR-014 的降级路径走哪一条。
#
# 特点：不需要 root；只读探测 + 一个 /tmp 临时目录，退出时自动清理；
#       不改动系统任何状态。
#
# 用法：
#   bash scripts/probe-env.sh
#
# 判定表：
#   bwrap ✓  apptainer ✓/✗ → sandbox: bwrap 用于 local；apptainer 用于 sge
#   bwrap ✗  apptainer ✓   → local 用 apptainer 或 sandbox: none
#   bwrap ✗  apptainer ✗   → 只能 sandbox: none，隔离全靠 Jail + MountSpec（弱）

set +e

pass() { printf '  \033[32m✓\033[0m %s\n' "$1"; }
fail() { printf '  \033[31m✗\033[0m %s\n' "$1"; }
warn() { printf '  \033[33m!\033[0m %s\n' "$1"; }
hdr()  { printf '\n\033[1m== %s ==\033[0m\n' "$1"; }

WS=$(mktemp -d /tmp/srcos-probe.XXXXXX) || exit 1
trap 'rm -rf "$WS"' EXIT

printf 'host:   %s\n' "$(hostname)"
printf 'kernel: %s\n' "$(uname -r)"
printf 'os:     %s\n' "$( (. /etc/os-release 2>/dev/null && echo "$PRETTY_NAME") || echo unknown)"
printf 'user:   %s (uid=%s)\n' "$(id -un)" "$(id -u)"
printf 'home:   %s\n' "$HOME"

# ─────────────────────────────────────────────── 1. 内核 userns 开关
# 这三项决定「非特权 user namespace」能否创建。它是 bubblewrap 的前提，
# 也是 apptainer 非 setuid 模式的前提。
hdr "1. user namespace 开关"

check_sysctl() { # $1=path $2=期望值 $3=说明
  [ -r "$1" ] || { warn "$3：$1 不存在（正常，视内核补丁而定）"; return; }
  local v; v=$(cat "$1")
  if [ "$v" = "$2" ]; then pass "$3: $v"
  else fail "$3: $v  ← 期望 $2"; fi
}
check_sysctl /proc/sys/kernel/unprivileged_userns_clone           1 "unprivileged_userns_clone"
check_sysctl /proc/sys/kernel/apparmor_restrict_unprivileged_userns 0 "apparmor_restrict_userns"

if [ -r /proc/sys/user/max_user_namespaces ]; then
  v=$(cat /proc/sys/user/max_user_namespaces)
  [ "${v:-0}" -gt 0 ] 2>/dev/null && pass "user.max_user_namespaces: $v" \
                                   || fail "user.max_user_namespaces: $v  ← userns 全禁"
fi

# 直接实测一次 userns 创建（比读 sysctl 更可靠 —— AppArmor 限制不在 sysctl 里体现）
if command -v unshare >/dev/null 2>&1; then
  if unshare --user --map-root-user /bin/true 2>"$WS/e0"; then
    pass "实测 unshare --user 可用 → 非特权 userns 确实能创建"
  else
    fail "实测 unshare --user 失败 → 无 profile 的二进制无法创建 userns"
    sed 's/^/      /' "$WS/e0"
    warn "注意：这不等于 bwrap 不能用 —— Ubuntu 支持按二进制单独授权（见下）"
  fi
fi

# Ubuntu 24.04+ 的机制：在 /etc/apparmor.d/ 里给某个二进制加一份带 `userns,` 的 profile，
# 即可单独解除 apparmor_restrict_unprivileged_userns 对它的限制（不影响全局）。
# 官方为 90+ 个二进制这幺做了（lxc-usernsexec / podman / runc / buildah / flatpak / chrome …）。
if [ -d /etc/apparmor.d ]; then
  GRANTED=$(grep -l '^[[:space:]]*userns,' /etc/apparmor.d/* 2>/dev/null \
            | xargs -r -n1 basename | sort | tr '\n' ' ')
  if [ -n "$GRANTED" ]; then
    pass "已通过 AppArmor profile 单独授予 userns 的二进制（$(printf '%s' "$GRANTED" | wc -w) 个）:"
    printf '        %s\n' "$GRANTED" | fold -s -w 100 | sed 's/^/      /'
    case " $GRANTED " in
      *" bwrap "*) pass "  ✅ bwrap 已在其中 → 不受 apparmor_restrict_userns 限制" ;;
      *)           warn "  ❌ bwrap 不在其中 → 需要按上面的方法加一份 profile" ;;
    esac
  else
    warn "未发现任何 userns profile（非 Ubuntu 或未启用 AppArmor）"
  fi
  if [ -r /proc/sys/kernel/apparmor_restrict_unprivileged_userns ] \
     && [ "$(cat /proc/sys/kernel/apparmor_restrict_unprivileged_userns)" = "1" ] \
     && ! printf '%s' " $GRANTED " | grep -q " bwrap "; then
    echo
    echo "      ── 修复 bwrap 的两种方法 ──"
    echo "      ✅ 推荐（精准，只给 bwrap 一个二进制授权）:"
    echo "         sudo tee /etc/apparmor.d/bwrap >/dev/null <<'P'"
    echo "         abi <abi/4.0>,"
    echo "         include <tunables/global>"
    echo "         profile bwrap /usr/bin/bwrap flags=(unconfined) {"
    echo "           userns,"
    echo "           include if exists <local/bwrap>"
    echo "         }"
    echo "         P"
    echo "         sudo apparmor_parser -r /etc/apparmor.d/bwrap"
    echo "         # 撤销: sudo apparmor_parser -R /etc/apparmor.d/bwrap && sudo rm /etc/apparmor.d/bwrap"
    echo
    echo "      ⚠️  降级（全局，降低纵深防御）:"
    echo "         sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0"
  fi
fi

# ─────────────────────────────────────────────── 2. bubblewrap
hdr "2. bubblewrap"

if command -v bwrap >/dev/null 2>&1; then
  pass "已安装 $(command -v bwrap)  ($(bwrap --version 2>&1 | head -1))"
  if [ -u "$(command -v bwrap)" ]; then
    warn "带 setuid 位（老式部署；此时不依赖非特权 userns）"
  else
    pass "无 setuid 位 → 依赖非特权 userns（见第 1 节）"
  fi

  BWRAPOK=no
  if bwrap --ro-bind / / -- /bin/true 2>"$WS/e1"; then
    pass "T1 最小调用 --ro-bind / /"
    BWRAPOK=yes
  else
    fail "T1 失败 —— bwrap 无法工作"
    sed 's/^/      /' "$WS/e1"
    case "$(cat "$WS/e1")" in
      *"uid map"*) warn "错误特征 = userns 创建被拦（AppArmor 或 userns 禁用），见第 1 节的解除方法" ;;
    esac
  fi

  # T2：SRCOS 的实际挂载集（含 unshare + tmpfs + bind workspace + cwd）
  if [ "$BWRAPOK" = yes ]; then
    if bwrap --die-with-parent --new-session --unshare-pid --unshare-ipc --unshare-uts \
          --proc /proc --dev /dev --tmpfs /tmp \
          --ro-bind /usr /usr --ro-bind /bin /bin \
          $([ -d /lib ]   && echo --ro-bind /lib /lib) \
          $([ -d /lib64 ] && echo --ro-bind /lib64 /lib64) \
          --bind "$WS" /workspace --chdir /workspace \
          -- /bin/sh -c 'pwd && echo hi > t.txt && cat t.txt' 2>"$WS/e2"; then
      pass "T2 SRCOS 同款挂载集（unshare-pid/ipc/uts + tmpfs + bind /workspace）"
    else
      fail "T2 失败 —— bwrap 不可用于 SRCOS"
      sed 's/^/      /' "$WS/e2"
      BWRAPOK=no
    fi
  else
    warn "T2 已跳过（T1 未通过）"
  fi

  # T3：隔离性 —— 没挂进来的路径应该看不见（仅在 T2 通过后才有意义）
  if [ "$BWRAPOK" = yes ]; then
    out=$(bwrap --die-with-parent --new-session --unshare-pid --unshare-ipc --unshare-uts \
          --proc /proc --dev /dev --tmpfs /tmp \
          --ro-bind /usr /usr --ro-bind /bin /bin \
          $([ -d /lib ] && echo --ro-bind /lib /lib) \
          --bind "$WS" /workspace --chdir /workspace \
          -- /bin/sh -c 'ls -d '"$HOME"' 2>&1 | head -1' 2>&1)
    case "$out" in
      *"No such file"*|*"cannot access"*)
        pass "T3 未挂载的 \$HOME 在沙箱内不可见（隔离生效）";;
      *"uid map"*|*"bwrap:"*)
        fail "T3 无效 —— bwrap 自身起动失败，输出不是隔离结果";;
      *)
        warn "T3 \$HOME 仍可见：$out" ;;
    esac

    # T4：权限位折叠 —— userns 会把未映射 gid 折叠为 65534(nogroup)，
    # 而沙箱进程本身就在 65534 组，所以宿主的 group 权限位在沙箱内等于"人人可读"。
    # 这不是 bug 是 userns 的固有语义，但它把“OS 用户可读”扩大为“任何 group 可读”，
    # 因此 MountSpec 必须 bind 最小必要路径，绝不 bind 父目录。
    echo "  沙箱内 id: $(bwrap --die-with-parent --new-session --unshare-pid \
          --ro-bind / / -- /bin/sh -c 'id' 2>/dev/null)"
    if bwrap --die-with-parent --new-session --unshare-pid \
         --ro-bind / / -- /bin/sh -c 'grep -q 65534 /proc/self/status' 2>/dev/null; then
      warn "T4 沙箱内组列表含 65534 → group 权限位在沙箱内等同于公开可读"
      warn "   → MountSpec 必须 bind 到最小必要路径（storages.yaml 的 host_root 不要再加父层）"
    else
      pass "T4 未观察到 65534 组折叠"
    fi
  else
    warn "T3/T4 已跳过（bwrap 未通过 T1/T2）"
  fi
else
  fail "未安装 bwrap"
  warn "可用 conda 装：conda install -c conda-forge bubblewrap"
fi

# ─────────────────────────────────────────────── 3. apptainer / singularity
hdr "3. apptainer / singularity"

# 注意：必须排除同名干扰（例如 PyPI 上那个叫 Singularity 的 pygame 游戏/库）。
# 判据：二进制名匹配 + 版本串含 semver + 输出里不出现 pygame/SDL + 不在 /usr/games。
APPT=""
APPTBIN=""
for c in apptainer singularity; do
  bin=$(command -v "$c" 2>/dev/null) || continue
  case "$bin" in /usr/games/*) warn "$bin 在 /usr/games/ → 不是容器运行时，跳过"; continue;; esac
  ver=$("$bin" --version 2>&1 | head -20)
  if printf '%s' "$ver" | grep -qiE 'pygame|SDL|Hello from'; then
    warn "$bin 是同名干扰库（pygame 的 Singularity），不是容器运行时，跳过"; continue
  fi
  if printf '%s' "$ver" | grep -qiE '(apptainer|singularity(-ce)?).*[0-9]+\.[0-9]+'; then
    APPT=$c; APPTBIN=$bin; APPTVER="$ver"; break
  fi
  warn "$bin 版本串不像容器运行时，跳过：$(printf '%s' "$ver" | head -1)"
done

if [ -n "$APPT" ]; then
  pass "已安装 $APPT ($APPTBIN)  $(printf '%s' "$APPTVER" | head -1)"
  if [ -u "$APPTBIN" ]; then
    pass "带 setuid 位 → 即使非特权 userns 被拦也可能可用"
  else
    warn "无 setuid 位 → 依赖非特权 userns（见第 1 节）"
  fi
  # T4：注意可能因「无外网 / 无镜像缓存」而失败，不一定是权限问题
  if timeout 120 "$APPTBIN" exec docker://alpine:3.19 /bin/echo "apptainer exec OK" 2>"$WS/e4"; then
    pass "T4 exec docker:// 可用（含拉镜像，需外网或缓存）"
  else
    warn "T4 失败（可能是无外网 / 无缓存，而非权限问题）"
    sed 's/^/      /' "$WS/e4" | head -5
  fi
  if timeout 120 "$APPTBIN" exec --containall --cleanenv docker://alpine:3.19 /bin/true 2>"$WS/e5"; then
    pass "T5 --containall --cleanenv（SRCOS 推荐的强隔离参数）"
  else
    warn "T5 失败"; sed 's/^/      /' "$WS/e5" | head -5
  fi
else
  fail "未找到 apptainer / singularity 容器运行时"
  warn "集群常见位置：/usr/bin/apptainer、/usr/local/bin/apptainer；或需 module load"
  if command -v module >/dev/null 2>&1; then
    for m in apptainer singularity; do
      module avail "$m" 2>&1 | grep -qi "$m" && warn "module 里有 $m（需 module load 后才有）"
    done
  fi
fi

# ─────────────────────────────────────────────── 4. 资源限制通道
hdr "4. 资源限制通道（ADR-014 降级路径：systemd-run --user > prlimit）"

if command -v systemd-run >/dev/null 2>&1; then
  if systemd-run --user --scope -p MemoryMax=64M -- /bin/true >/dev/null 2>"$WS/e6"; then
    pass "systemd-run --user --scope 可用 ← 首选（cgroup 限制真生效）"
    if systemd-run --user --scope -p CPUQuota=50% -p MemoryMax=64M -p TasksMax=64 \
         -- /bin/true >/dev/null 2>&1; then
      pass "CPUQuota / MemoryMax / TasksMax 均被接受"
    else
      warn "部分属性被拒（降级为只用被接受的属性）"
    fi
  else
    fail "systemd-run --user 不可用（HPC 登录节点常见）"
    sed 's/^/      /' "$WS/e6" | head -3
    warn "systemctl --user is-system-running → $(systemctl --user is-system-running 2>&1 | head -1)"
  fi
  if loginctl show-user "$(id -un)" 2>/dev/null | grep -q 'Linger=yes'; then
    pass "Linger=yes（SSH 登出后 user manager 仍在）"
  else
    warn "Linger 未开启 → systemd --user 可能在 SSH 会话结束后消失"
  fi
else
  fail "无 systemd-run"
fi

[ -w /sys/fs/cgroup/cgroup.procs ] 2>/dev/null \
  && pass "可直接写 /sys/fs/cgroup" \
  || warn "不能直接写 cgroup v2（预期内；只能走 systemd-run 或 prlimit）"
v=$(cat /sys/fs/cgroup/cgroup.controllers 2>/dev/null)
[ -n "$v" ] && pass "cgroup v2 已挂载，controllers: $v" || warn "非 cgroup v2 或未挂载"

if command -v prlimit >/dev/null 2>&1; then
  if prlimit --as=268435456 --nproc=64 --cpu=5 -- /bin/true 2>"$WS/e7"; then
    pass "prlimit 可用（RLIMIT_AS / NPROC / CPU）← 兜底方案"
  else
    fail "prlimit 失败"; sed 's/^/      /' "$WS/e7"
  fi
  v=$(prlimit --as=268435456 -- /bin/sh -c 'ulimit -v' 2>/dev/null)
  [ -n "$v" ] && pass "RLIMIT_AS 被子进程继承（ulimit -v = $v）" \
              || warn "无法确认 RLIMIT 是否被子进程继承"
else
  fail "无 prlimit（util-linux）"
fi

# ─────────────────────────────────────────────── 5. 存储环境
hdr "5. 存储环境（决定 storage 与配额方案）"

# 注意：stat -f -c %T 对 ext4 会报 "ext2/ext3"，不可靠；用 df -T 拿真实类型。
for d in "$HOME" "$WS" "$(pwd)" /data /share /scratch /work; do
  [ -e "$d" ] || continue
  info=$(df -T "$d" 2>/dev/null | tail -1)
  fstype=$(printf '%s' "$info" | awk '{print $2}')
  size=$(printf '%s' "$info" | awk '{print $3}')
  avail=$(printf '%s' "$info" | awk '{print $5}')
  used=$(printf '%s' "$info" | awk '{print $6}')
  printf '  %-22s fs=%-7s avail=%-8s used=%-5s %s\n' \
    "$d" "${fstype:-?}" "${avail:-?}" "${used:-?}" \
    "$([ -w "$d" ] && echo rw || echo ro)"
done

# XFS project quota 需要一个 XFS 文件系统；ext4 需要改挂载选项（要 root + 重挂）
DATA_FS=$(df -T "$HOME" 2>/dev/null | tail -1 | awk '{print $2}')
case "$DATA_FS" in
  xfs)  pass "数据盘是 XFS → 可用 XFS project quota 做 workspace 配额" ;;
  ext*) warn "数据盘是 $DATA_FS → 不能用 XFS project quota；
              配额改用: 单独卷 / ext4 prjquota（需改挂载选项+重挂）/ 目录计数" ;;
  *)    warn "数据盘类型 $DATA_FS → 配额方案待定" ;;
esac

command -v setfacl >/dev/null 2>&1 && pass "有 setfacl" || warn "无 setfacl"
for d in "$HOME" /work /Volumes/data; do
  [ -d "$d" ] && mount | grep -qE " on $d " && printf '  挂载点 %-22s %s\n' "$d" "$(mount | grep -E " on $d " | head -1 | sed 's/^.* on /on /')"
done

# ─────────────── 5.5 现已存在的工具与容器运行时
hdr "5.5 现在已在跑的东西（SRCOS 可能直接接管）"

for port in 3080 3838 8787 8888 8080; do
  line=$(ss -tlnp 2>/dev/null | grep -E ":$port\\b" | head -1)
  [ -n "$line" ] && printf '  :%-5s %s\n' "$port" "$(printf '%s' "$line" | awk '{print $4}')"
done
warn "3080≈dsh  3838=shiny-server  8787=RStudio Server  8888=Jupyter（对照看）"

# 容器运行时（docker 可用 = 用户在 docker 组 = 事实上的 root 等价）
for c in docker podman nerdctl enroot; do
  bin=$(command -v "$c" 2>/dev/null) || continue
  if timeout 10 "$bin" info >/dev/null 2>&1; then
    ver=$(timeout 10 "$bin" info 2>/dev/null | grep -i version | head -1 | tr -d ' ' | tr '\n' ' ')
    pass "$bin 可用（${ver}）"
    [ -S "/run/user/$(id -u)/$c.sock" ] && pass "  且是 rootless 模式" \
      || warn "  不是 rootless → 用户能用它 = root 等价（与 ADR-014 冲突，需确认）"
  else
    warn "$bin 存在但当前用户不可用（无权限）: $bin"
  fi
done
id | grep -q 'docker' && warn "当前用户在 docker 组"

# ─────────────── 5.6 包管理与环境
hdr "5.6 环境管理（sandbox: none 时的环境来源）"
command -v module >/dev/null 2>&1 && pass "有环境模块系统（module）" || warn "无 module"
for c in conda mamba micromamba; do
  command -v $c >/dev/null 2>&1 && pass "有 $c: $(command -v $c)"
done
command -v conda >/dev/null 2>&1 && conda env list 2>/dev/null | grep -v '^#' | head -8 | sed 's/^/  /'

# ─────────────────────────────────────────────── 6. 调度器
hdr "6. 调度器（SRCOS backend）"

for c in qsub qstat qdel qconf qalter; do
  command -v "$c" >/dev/null 2>&1 && pass "有 $c  ($(command -v "$c"))" || warn "无 $c"
done

if command -v qstat >/dev/null 2>&1; then
  if qstat -xml 2>/dev/null | head -c 200 | grep -q '<'; then
    pass "qstat -xml 可用（SRCOS 靠它解析作业状态）"
  else
    warn "qstat -xml 无输出或格式异常"
  fi
  qconf -sq 2>/dev/null | sed -n 's/^pe_list *//p' | head -1 | sed 's/^/  队列 PE: /'
  qconf -sql 2>/dev/null | tr '\n' ' ' | sed 's/^/  可用队列: /'
  echo
fi

# ─────────────────────────────────────────────── 结论
hdr "结论速查"
cat <<'NOTE'
  第 1 节决定第 2/3 节能否工作（userns 是共同前提）。
  例外：bwrap / apptainer 若「带 setuid 位」，即使 userns 被禁也可能可用。

  ── 修复 bwrap 的两种方法（Ubuntu 24.04+）──
  ✅ 推荐（精准，与 Ubuntu 自带的 lxc-usernsexec 同机制，只给 bwrap 一个二进制授权）:
      sudo tee /etc/apparmor.d/bwrap >/dev/null <<'P'
      abi <abi/4.0>,
      include <tunables/global>
      profile bwrap /usr/bin/bwrap flags=(unconfined) {
        userns,
        include if exists <local/bwrap>
      }
      P
      sudo apparmor_parser -r /etc/apparmor.d/bwrap
      验证: bwrap --ro-bind / / -- /bin/true && echo OK
      撤销: sudo apparmor_parser -R /etc/apparmor.d/bwrap && sudo rm /etc/apparmor.d/bwrap

  ⚠️  降级（全局，降低纵深防御）:
      sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0
      持久化: echo 'kernel.apparmor_restrict_unprivileged_userns=0' \
                | sudo tee /etc/sysctl.d/99-userns.conf && sudo sysctl --system

  判定表：
    bwrap ✓  apptainer ✓/✗ → sandbox 用 bwrap（local）；apptainer 用于 sge
    bwrap ✗  apptainer ✓   → local 用 apptainer 或 sandbox: none
    bwrap ✗  apptainer ✗   → 只能 sandbox: none
                             隔离全靠 Jail + MountSpec 的逻辑校验（弱）
                             必须在 UI 标出降级状态（对齐 ennote 的 degraded 模式）

  资源限制：第 4 节通过 systemd-run --user 就用它；否则只能用 prlimit
            （RLIMIT_AS 对 JVM / 某些 Python 库不友好，需要"超限 kill 进程组"兜底）

  请把完整输出贴回 SRCOS 会话，以便确定 tool.yaml 的 sandbox/backend 写法。
NOTE
