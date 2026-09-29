#!/usr/bin/env bash
#
# scripts/e2e-shiny.sh — Shiny for Python 服务回归（`make e2e-shiny`）
#
# 用 srcos-tools/shiny-py-* 两个工具跑一遍 kind: service 的真实链路：
#   注册 → 启动（声明式 command + 具名环境 + workspace 模板）→ healthcheck
#   → HTTP 200 → WebSocket 握手 → 停止（只凭实例记录，无需 --tools-dir）
#
# 需要机器上有一个装了 Shiny for Python 的 python：
#   SRCOS_SHINY_PYTHON=/path/to/python   （默认先试 python3）
#   `python -c 'import shiny'` 必须成功；否则脚本 SKIP（退出 0），
#   所以它不会因为 CI 机器没有 shiny 而失败 —— 正因如此它不在 `make e2e` 里。
#
# 全部在临时目录 + 临时端口里跑，不碰仓库的 config/ 与 data/，退出时清理。
#   SRCOS_E2E_KEEP=1   保留临时目录（排错用）
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$ROOT/srcos"
TOOLS="$ROOT/srcos-tools"
USER_NAME=alice
PASSWORD=e2e-shiny-pass
TMP="$(mktemp -d /tmp/srcos-e2e-shiny.XXXXXX)"
CONFIG="$TMP/config"
# srcos 把运行态放在 config 的兄弟目录：<parent>/data
DATA="$TMP/data"
KEEP="${SRCOS_E2E_KEEP:-}"

red()   { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
step()  { printf '\033[36m▸\033[0m %s\n' "$*"; }
die()   { red "  ✗ $*"; exit 1; }

cleanup() {
  if [ -d "$CONFIG" ]; then
    for t in shiny-py-hello shiny-py-plot; do
      "$BIN" svc stop --tool "$t" --user "$USER_NAME" -d "$CONFIG" >/dev/null 2>&1 || true
    done
  fi
  if [ -z "$KEEP" ]; then rm -rf "$TMP"; else echo "kept: $TMP"; fi
}
trap cleanup EXIT

[ -x "$BIN" ] || die "missing $BIN — run: make build"

# ── pick a python that can import shiny ─────────────────────────────────
PY="${SRCOS_SHINY_PYTHON:-python3}"
if ! "$PY" -c 'import shiny' >/dev/null 2>&1; then
  red "SKIP: no python with Shiny for Python (tried: $PY)"
  echo "      set SRCOS_SHINY_PYTHON=/path/to/python (module 'shiny' must import)"
  exit 0
fi
PREFIX="$("$PY" -c 'import sys;print(sys.prefix)')"
SITE="$("$PY" -c 'import shiny,os;print(os.path.dirname(os.path.dirname(shiny.__file__)))')"
green "python: $PY  (shiny $("$PY" -c 'import shiny;print(getattr(shiny,"__version__","?"))'))"

# ── temp config: the site's interpreter + a permissive grant ────────────
mkdir -p "$CONFIG"
cat > "$CONFIG/environments.yaml" <<YAML
environments:
  - id: py-shiny
    name: "Shiny for Python (e2e)"
    root: $PREFIX
    env:
      - "PATH=/opt/srcos/bin:$PREFIX/bin:/usr/local/bin:/usr/bin:/bin"
      - "PYTHONPATH=$SITE"
      - "LANG=C.UTF-8"
      - "LC_ALL=C.UTF-8"
    provides: [python3]
YAML
printf 'default_allow: true\n' > "$CONFIG/grants.yaml"
printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | "$BIN" user "$USER_NAME" -d "$CONFIG" >/dev/null 2>&1 \
  || die "could not create the test user"

check_one() {
  local tool="$1"
  local inst="$USER_NAME-$tool-svc"

  step "start $tool"
  if ! "$BIN" svc start --tool "$tool" --user "$USER_NAME" -d "$CONFIG" --tools-dir "$TOOLS" \
        --param 'title=SRCOS e2e' > "$TMP/$tool.start" 2>&1; then
    cat "$TMP/$tool.start"; die "start $tool failed"
  fi
  local port
  port="$(sed -n 's/^endpoint: .*://p' "$DATA/instances/$inst.yaml")"
  [ -n "$port" ] || die "no endpoint recorded for $tool"

  local code
  code="$(curl -s -o "$TMP/$tool.html" -w '%{http_code}' "http://127.0.0.1:$port/")"
  [ "$code" = "200" ] || die "$tool HTTP $code"
  grep -qi shiny "$TMP/$tool.html" || die "$tool does not look like a Shiny page"
  green "  ✓ $tool HTTP 200 (127.0.0.1:$port)"

  if "$PY" -c 'import websockets' >/dev/null 2>&1; then
    if ! "$PY" - "$port" <<'PY'
import asyncio, sys, websockets
async def main():
    async with websockets.connect(f"ws://127.0.0.1:{sys.argv[1]}/websocket/"):
        pass
asyncio.run(main())
PY
    then
      die "$tool WebSocket handshake failed"
    fi
    green "  ✓ $tool WebSocket /websocket/ OPEN"
  fi

  step "stop $tool (record-only: no --tools-dir)"
  if ! "$BIN" svc stop --tool "$tool" --user "$USER_NAME" -d "$CONFIG" > "$TMP/$tool.stop" 2>&1; then
    cat "$TMP/$tool.stop"; die "stop $tool failed"
  fi
  green "  ✓ $tool stopped"
}

check_one shiny-py-hello
check_one shiny-py-plot

green "shiny e2e: PASS"
