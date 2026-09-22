#!/usr/bin/env bash
#
# hello-fanout —— SRCOS 最小示例工具
#
# 演示 tool-spec 的四件事：
#   1. 六条硬规范（幂等 / 参数化 / 只写 workspace / 退出码 / 日志走 stdout / 同步阻塞）
#   2. 样本级并行归工具自己（优先 ata，退化 xargs）
#   3. .sign 完成标记做幂等
#   4. 产物落在 /workspace/out，供 SRCOS 登记
#
set -euo pipefail

# ── 1. 取参数（规范 #2：所有可变值从 SRCOS_PARAM_* 取，不硬编码）──────────
SAMPLES="${SRCOS_PARAM_SAMPLES:?samples is required}"
PARALLEL="${SRCOS_PARAM_PARALLEL:-5}"
PREFIX="${SRCOS_PARAM_PREFIX:-hello}"

: "${SRCOS_WORKSPACE:?SRCOS_WORKSPACE not set — 必须由 SRCOS 提供}"

# 产物目录按【任务】分派，不是按工具。
#
# 这是幂等标记的正确粒度：如果 .sign 放在工具级（如 /workspace/out/.sign），
# 那么第二次带不同参数的任务会被上一次的标记误判为"已完成"而直接跳过。
# 幂等的单位是「一次任务」，用 $SRCOS_TASK_ID 区分 —— 同一个任务重跑会
# 命中自己的标记（正确跳过），新任务则有全新的目录。
OUT="${SRCOS_WORKSPACE}/out/${SRCOS_TASK_ID:-manual}"
SIGN="${OUT}/.sign"
mkdir -p "${OUT}"

# ── 2. 幂等（规范 #1）──────────────────────────────────────────────────
if [[ -f "${SIGN}" ]]; then
  echo "[skip] ${SIGN} exists — already done"
  exit 0
fi

echo "[info] workspace = ${SRCOS_WORKSPACE}"
echo "[info] outdir    = ${OUT}"
echo "[info] home      = ${HOME:-<unset>}"
echo "[info] job root  = ${SRCOS_JOB_ROOT:-<unset>}"
echo "[info] user/tool = ${SRCOS_USER:-?}/${SRCOS_TOOL:-?}"
echo "[info] samples   = ${SAMPLES}"
echo "[info] parallel  = ${PARALLEL}"

# ── 3. 把样本展开成清单（样本级 fanout 的输入）──────────────────────────
# 两份产物：
#   SAMPLEFILE —— 一行一个样本（xargs 兑底路径用）
#   CMDFILE    —— 一行一条完整命令（ata 用，ata 吃的是命令清单）
SAMPLEFILE="$(mktemp)"
CMDFILE="$(mktemp)"
trap 'rm -f "${SAMPLEFILE}" "${CMDFILE}"' EXIT

IFS=',' read -ra ARR <<< "${SAMPLES}"
COUNT=0
for raw in "${ARR[@]}"; do
  s="$(printf '%s' "$raw" | tr -d '[:space:]')"
  [[ -z "$s" ]] && continue
  printf '%s\n' "${s}" >> "${SAMPLEFILE}"
  # 生成：printf "%s\n" 'hi S001' > '/workspace/out/S001.txt'
  printf 'printf "%%s\\n" %q > %q\n' "${PREFIX} ${s}" "${OUT}/${s}.txt" >> "${CMDFILE}"
  COUNT=$((COUNT + 1))
done

if [[ "${COUNT}" -eq 0 ]]; then
  echo "[error] no samples after parsing" >&2
  exit 2
fi
echo "[info] fanout count = ${COUNT}"

# ── 4. 样本级并行：优先 ata，退化 xargs（规范 #5：日志走 stdout）──────────
if command -v ata >/dev/null 2>&1; then
  echo "[run] ata -t ${PARALLEL}"
  ata -t "${PARALLEL}" -i "${CMDFILE}"
else
  echo "[run] xargs -P ${PARALLEL}  (ata not found — 降级)"
  # 坑：不要用 `xargs -I{} bash -c '{}'`。xargs 自己会处理引号与反斜杠，
  # 会把已经生成好的命令再破坏一层（`\n` 变成 `n`）。
  # 正解：用 -n 1 把样本作为 $1 传给子 shell，其余通过环境变量传递。
  export SRCOS_FANOUT_PREFIX="${PREFIX}"
  export SRCOS_FANOUT_OUT="${OUT}"
  xargs -P "${PARALLEL}" -n 1 bash -c \
    's="$1"; printf "%s\n" "${SRCOS_FANOUT_PREFIX} ${s}" > "${SRCOS_FANOUT_OUT}/${s}.txt"' _ \
    < "${SAMPLEFILE}"
fi

# ── 5. 完成标记必须在所有样本结束后才出现（规范 #6 的另一半）──────────────
touch "${SIGN}"

echo "[done] ${COUNT} samples → ${OUT}"
ls -1 "${OUT}"
