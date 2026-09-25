#!/usr/bin/env bash
#
# shiny-demo —— 把一个 R Shiny 应用作为 SRCOS service 实例跑起来。
#
# SRCOS 交给我们两样东西：
#   $SRCOS_PORT       端口池分配的端口（**必须**监听在它上面，代理按它转发）
#   $SRCOS_WORKSPACE  这个用户这个工具的工作区（`/workspace`）
#
# 关于路径前缀：SRCOS 把实例挂在 /proxy/<用户>/<工具 id>/ 下，而 Shiny 的
# 客户端从 window.location.pathname 推导它自己的 base —— 所以资源与 WebSocket
# 天然落回同一个前缀，**不需要** base-path 配置。工具只需要监听根路径。
#
# 规范 #6：必须同步阻塞到所有实际工作结束 —— 对服务来说就是「一直跑着」。
set -euo pipefail

: "${SRCOS_PORT:?SRCOS_PORT not set — the port pool must hand one over}"
: "${SRCOS_WORKSPACE:?SRCOS_WORKSPACE not set}"

APP_DIR="${SRCOS_WORKSPACE}"
if [ ! -f "${APP_DIR}/app.R" ]; then
  echo "[srcos] no app.R in ${APP_DIR} — the workspace template did not arrive" >&2
  exit 2
fi

# 找一个**装了 shiny 的** R：PATH 上可能有多个（系统 R 常常没有 shiny）。
# 顺序：先平台给的 PATH（工具可用 env: 把 miniforge 前缀放进去），再退回已知路径。
R_BIN=""
for candidate in "$(command -v R 2>/dev/null || true)" /Volumes/data/pmo/miniforge3/bin/R /pmo/miniforge3/bin/R /usr/bin/R; do
  [ -n "${candidate}" ] && [ -x "${candidate}" ] || continue
  if "${candidate}" --vanilla -e 'quit(status = !requireNamespace("shiny", quietly = TRUE))' >/dev/null 2>&1; then
    R_BIN="${candidate}"
    break
  fi
done
if [ -z "${R_BIN}" ]; then
  echo "[srcos] no R with the 'shiny' package found (tried PATH, the miniforge prefixes, /usr/bin/R)" >&2
  exit 3
fi

export SRCOS_SHINY_APP="${APP_DIR}"
echo "[srcos] R=${R_BIN} app=${APP_DIR} port=${SRCOS_PORT}"
echo "[srcos] instance=${SRCOS_INSTANCE_ID:-?} user=${SRCOS_USER:-?}"

# exec：让 R 成为本单元的主进程，SIGTERM 直接到达它（svc stop / 回收靠这个）。
exec "${R_BIN}" --vanilla -e \
  "shiny::runApp(Sys.getenv('SRCOS_SHINY_APP'), host='127.0.0.1', port=as.integer(Sys.getenv('SRCOS_PORT')), launch.browser=FALSE)"
