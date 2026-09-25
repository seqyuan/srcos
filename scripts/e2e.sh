#!/usr/bin/env bash
#
# scripts/e2e.sh — SRCOS 端到端回归网（`make e2e`）
#
# 覆盖一条真实链路：
#   提交（job submit）→ 队列消费（网关内，ADR-022）→ 执行（systemd 瞬时 unit）
#   → 判定（<log>.verdict，ADR-022）→ 日志 → 实例记录 → 资源查看（HTTP /api/resources）
#   外加第二认证面：agent token（Bearer）打 /api/tools。
#
# 全部在临时目录 + 临时端口里跑，不碰仓库的 config/ 与 data/，退出时清理。
#
# 环境：
#   SRCOS_E2E_SANDBOX=none   在没有 bwrap 的机器上跑（默认 bwrap）
#   SRCOS_E2E_TIMEOUT=60     等待实例成功的秒数
#   SRCOS_E2E_KEEP=1         保留临时目录（排错用）
#
# 依赖：go（先跑 make build）、curl、bwrap（默认）、python3（可选，仅用于挑空闲端口）
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$ROOT/srcos"

USER_NAME=alice
ADMIN_USER=e2eops
PASSWORD=e2e-pass
TOOL=e2e-ticker
SVC=e2e-web
ASK=e2e-ask
SANDBOX="${SRCOS_E2E_SANDBOX:-bwrap}"
TIMEOUT="${SRCOS_E2E_TIMEOUT:-60}"

TMP="$(mktemp -d "${TMPDIR:-/tmp}/srcos-e2e.XXXXXX")"
CFG="$TMP/config"
TOOLS="$TMP/tools"
DATA="$TMP/data"
JOB_ID=""
GATEWAY_PID=""

info()  { printf '\033[36m▸\033[0m %s\n' "$1"; }
pass()  { printf '\033[32m  ✓\033[0m %s\n' "$1"; }
step()  { info "[$1] $2"; }

fail() {
  printf '\033[31m  ✗ %s\033[0m\n' "$1" >&2
  if [ -f "$TMP/gateway.log" ]; then
    echo "--- gateway.log (tail 30) ---" >&2
    tail -30 "$TMP/gateway.log" >&2 || true
  fi
  exit 1
}

cleanup() {
  local rc=$?
  # Stop only the transient units *this* run created — never a blanket 'srcos-*'
  # (the developer machine may be serving real instances).
  if [ -d "$DATA/instances" ]; then
    for f in "$DATA/instances"/*.yaml; do
      [ -e "$f" ] || continue
      local id; id="$(basename "$f" .yaml)"
      systemctl --user stop "srcos-$id" >/dev/null 2>&1 || true
      systemctl --user reset-failed "srcos-$id" >/dev/null 2>&1 || true
    done
  fi
  if [ -n "$GATEWAY_PID" ]; then
    kill "$GATEWAY_PID" >/dev/null 2>&1 || true
    wait "$GATEWAY_PID" 2>/dev/null || true
  fi
  if [ -n "$COLLECTOR_PID" ]; then
    kill "$COLLECTOR_PID" >/dev/null 2>&1 || true
    wait "$COLLECTOR_PID" 2>/dev/null || true
  fi
  if [ "${SRCOS_E2E_KEEP:-0}" = "1" ]; then
    echo "[e2e] kept temp dir: $TMP"
  else
    rm -rf "$TMP"
  fi
  exit "$rc"
}
trap cleanup EXIT INT TERM

[ -x "$BIN" ] || { echo "srcos not built — run 'make build' first" >&2; exit 2; }
command -v curl >/dev/null 2>&1 || { echo "curl is required" >&2; exit 2; }

mkdir -p "$CFG" "$TOOLS"

PORT="$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()' 2>/dev/null || true)"
[ -n "$PORT" ] || PORT=$(( 28000 + ($$ % 2000) ))
BASE="http://127.0.0.1:$PORT"

# An external audit collector, when python3 is available: the forwarding path is
# where bugs hide, so the e2e exercises config -> state.yaml -> forwarder ->
# collector, not just the forwarder's unit tests.
COLLECTOR_LOG="$TMP/collector.jsonl"
COLLECTOR_PID=""
COLLECTOR_PORT=""
if command -v python3 >/dev/null 2>&1; then
  COLLECTOR_PORT="$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()')"
  python3 - "$COLLECTOR_PORT" "$COLLECTOR_LOG" <<'PY' >/dev/null 2>&1 &
import sys, http.server, socketserver
port = int(sys.argv[1]); out = sys.argv[2]
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get('Content-Length') or 0)
        body = self.rfile.read(n)
        with open(out, 'ab') as f:
            f.write(body + b"\n")
        self.send_response(204); self.end_headers()
    def log_message(self, *a): pass
socketserver.TCPServer.allow_reuse_address = True
with socketserver.TCPServer(("127.0.0.1", port), H) as srv:
    srv.serve_forever()
PY
  COLLECTOR_PID=$!
  mkdir -p "$CFG"
  cat > "$CFG/state.yaml" <<YAML
audit:
  forward_url: http://127.0.0.1:$COLLECTOR_PORT/audit
YAML
fi

info "SRCOS e2e  temp=$TMP  port=$PORT  sandbox=$SANDBOX  collector=${COLLECTOR_PORT:-none}"

# ── 1. 工具包（临时，不进 srcos-tools/）────────────────────────────────
step 1 "写入临时工具 $TOOL（sandbox=$SANDBOX）"
mkdir -p "$TOOLS/$TOOL"
cat > "$TOOLS/$TOOL/tool.yaml" <<YAML
schemaVersion: 1
id: $TOOL
version: 0.0.1
name: "E2E Ticker"
description: "端到端回归用的最小任务：打印 N 行并写一个产物"
kind: task
backend: local
sandbox: $SANDBOX
entry: work.sh

interface:
  inputs:
    - name: ticks
      type: int
      label: "行数"
      required: true
      default: 3
      min: 1
      max: 10
  outputs:
    - name: outs
      type: directory
      provides: [result.txt]

requires_storages: []

resources:
  cpu: 1
  memory: "1Gi"
  walltime: "0:05:00"
YAML

cat > "$TOOLS/$TOOL/work.sh" <<'SH'
#!/usr/bin/env bash
# 六条硬规范的最小实现：幂等 / 参数化 / 只写 workspace / 退出码 / 日志 / 同步阻塞。
set -euo pipefail
TICKS="${SRCOS_PARAM_TICKS:-3}"
: "${SRCOS_WORKSPACE:?SRCOS_WORKSPACE not set}"
OUT="${SRCOS_WORKSPACE}/out"
SIGN="${OUT}/.sign"
mkdir -p "${OUT}"
if [[ -f "${SIGN}" ]]; then
  echo "[skip] ${SIGN} exists — already done"
  exit 0
fi
for i in $(seq 1 "${TICKS}"); do echo "tick ${i}"; done
echo "e2e-ok" > "${OUT}/result.txt"
touch "${SIGN}"
echo "[done] ticks=${TICKS} -> ${OUT}/result.txt"
SH
chmod +x "$TOOLS/$TOOL/work.sh"

# A service tool, so the e2e also covers the path the task tool cannot: start
# (via the admin API), a published route, and a stop.
mkdir -p "$TOOLS/$SVC"
cat > "$TOOLS/$SVC/tool.yaml" <<YAML
schemaVersion: 1
id: $SVC
version: 0.0.1
name: "E2E Web"
description: "端到端回归用的最小 service：监听分配到的端口"
kind: service
backend: local
sandbox: none
entry: work.sh
resources:
  cpu: 1
  memory: "256Mi"
ingress: {port: 8080}
lifecycle: {max_lifetime: "5m", idle_ttl: "2m"}
YAML
cat > "$TOOLS/$SVC/work.sh" <<'SH'
#!/usr/bin/env bash
# A service is a unit that keeps running: listen on the port SRCOS assigned.
set -euo pipefail
: "${SRCOS_PORT:?SRCOS_PORT not set}"
exec python3 -m http.server "${SRCOS_PORT}" --bind 127.0.0.1
SH
chmod +x "$TOOLS/$SVC/work.sh"

# A tool alice does NOT have and CAN ask for (requestable), so the e2e exercises
# the request -> approve -> access path.
mkdir -p "$TOOLS/$ASK"
cat > "$TOOLS/$ASK/tool.yaml" <<YAML
schemaVersion: 1
id: $ASK
version: 0.0.1
name: "E2E Ask"
description: "可申请但未授权的工具（B3 用）"
kind: task
backend: local
sandbox: none
entry: work.sh
resources:
  cpu: 1
  memory: "256Mi"
  walltime: "0:05:00"
YAML
printf '#!/usr/bin/env bash\nexit 0\n' > "$TOOLS/$ASK/work.sh"
chmod +x "$TOOLS/$ASK/work.sh"

"$BIN" tool validate -d "$CFG" --tools-dir "$TOOLS" >/dev/null || fail "tool validate failed"
pass "tool package valid"

# ── 2. 用户 + 授权 ──────────────────────────────────────────────────────
step 2 "创建用户 $USER_NAME + 管理员，并授权 $TOOL"
printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | "$BIN" user "$USER_NAME" -d "$CFG" >/dev/null \
  || fail "user create failed"
printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | "$BIN" user "$ADMIN_USER" -d "$CFG" >/dev/null \
  || fail "admin user create failed"
cat > "$CFG/grants.yaml" <<YAML
admins: [$ADMIN_USER]
grants:
  - tool: $TOOL
    users: [$USER_NAME]
    quota:
      max_cpu: 4
      max_memory: 4Gi
      max_instances: 2
  - tool: $SVC
    users: [$USER_NAME]
    quota:
      max_cpu: 2
      max_memory: 1Gi
      max_instances: 1
  - tool: $ASK
    users: [$ADMIN_USER]
    requestable: true
YAML
pass "users + grants ready (default-deny lifted for $TOOL; $ADMIN_USER is admin)"

# ── 3. 启动网关（队列消费者默认开启）────────────────────────────────────
step 3 "启动网关（$BASE）"
"$BIN" serve -d "$CFG" --tools-dir "$TOOLS" --host 127.0.0.1 --port "$PORT" \
  --title "srcos-e2e" > "$TMP/gateway.log" 2>&1 &
GATEWAY_PID=$!

ready=0
for _ in $(seq 1 100); do
  code="$(curl -s -o /dev/null -w '%{http_code}' "$BASE/login" 2>/dev/null || true)"
  [ "$code" = "200" ] && { ready=1; break; }
  kill -0 "$GATEWAY_PID" 2>/dev/null || fail "gateway exited during startup"
  sleep 0.2
done
[ "$ready" = "1" ] || fail "gateway did not become ready in 20s"
pass "gateway ready (pid $GATEWAY_PID)"

# ── 4. 提交（写投递目录 = 入队）─────────────────────────────────────────
step 4 "提交任务（job submit）"
SUBMIT_OUT="$("$BIN" job submit -d "$CFG" --tools-dir "$TOOLS" --user "$USER_NAME" \
  -n "e2e $(date +%H:%M:%S)" --tool "$TOOL" --param ticks=3 --output /workspace/out)" \
  || fail "job submit failed"
JOB_ID="$(printf '%s\n' "$SUBMIT_OUT" | awk '/^submitted /{print $2; exit}')"
[ -n "$JOB_ID" ] || fail "could not read job id from submit output"
pass "submitted job $JOB_ID"

# ── 5. 等队列把它跑完（网关进程自己消费，无人敲 CLI）──────────────────
step 5 "等待网关队列把任务跑完（最多 ${TIMEOUT}s）"
INSTANCE=""
for _ in $(seq 1 $((TIMEOUT * 2))); do
  for f in "$DATA"/instances/*.yaml; do
    [ -e "$f" ] || continue
    if grep -q '^state: succeeded' "$f"; then
      INSTANCE="$(basename "$f" .yaml)"; break 2
    fi
    if grep -qE '^state: (failed|stopped)' "$f"; then
      fail "instance ended as $(grep '^state:' "$f") — $(grep '^error:' "$f" || true)"
    fi
  done
  kill -0 "$GATEWAY_PID" 2>/dev/null || fail "gateway died while the queue was running"
  sleep 0.5
done
[ -n "$INSTANCE" ] || fail "timed out waiting for a succeeded instance"
pass "instance $INSTANCE succeeded (state from its own record)"

# ── 6. 判定文件（ADR-022：退出码活得比等待者长）───────────────────────
step 6 "校验 systemd 判定文件与日志"
LOGDIR="$DATA/logs/$USER_NAME/$TOOL"
VERDICT_FILE="$(ls "$LOGDIR"/*.log.verdict 2>/dev/null | head -1 || true)"
# Judge by the limiter the instance actually used, not by whether the
# systemd-run binary exists: on a CI runner the binary is present but there is
# no user manager, so the runtime legitimately falls back to prlimit and
# writes no verdict.
LIMITER="$(grep -m1 '^limiter:' "$DATA/instances/$INSTANCE.yaml" | awk '{print $2}')"
if [ -n "$VERDICT_FILE" ]; then
  verdict="$(cat "$VERDICT_FILE")"
  [ "$verdict" = "0 success" ] || fail "verdict = '$verdict' (want '0 success')"
  pass "verdict: $verdict (limiter=$LIMITER)"
elif [ "$LIMITER" = "systemd-run" ]; then
  fail "limiter=systemd-run but no <log>.verdict (ADR-022 regression)"
else
  info "  (limiter=${LIMITER:-none} — degraded mode, verdict assertion skipped)"
fi

grep -q 'tick 3' "$LOGDIR"/*.log 2>/dev/null || fail "log does not contain 'tick 3'"
pass "log contains the tool's output"

[ -f "$DATA/ws/$USER_NAME/$TOOL/out/result.txt" ] || fail "declared output not written"
pass "produced $DATA/ws/$USER_NAME/$TOOL/out/result.txt"

# ── 7. 资源查看（HTTP + session cookie）─────────────────────────────────
step 7 "资源查看（/api/resources）"
CJ="$TMP/cookies"
code="$(curl -s -o /dev/null -w '%{http_code}' -c "$CJ" -X POST "$BASE/login" \
  -H "Origin: $BASE" --data-urlencode "username=$USER_NAME" \
  --data-urlencode "password=$PASSWORD")"
case "$code" in 200|302) ;; *) fail "login returned $code" ;; esac
pass "session login ok"

curl -fsS -b "$CJ" "$BASE/api/resources" | grep -q 'workspace' \
  || fail "/api/resources did not list the workspace scope"
pass "resource roots listed"

ADDR="srcos%3A%2F%2Ffile%2Fworkspace%2Fout%2Fresult.txt%3Ftool%3D$TOOL"
curl -fsS -b "$CJ" "$BASE/api/resources?src=$ADDR" | grep -q 'result.txt' \
  || fail "resource metadata missing result.txt"
RAW="$(curl -fsS -b "$CJ" "$BASE/api/resources/raw?src=$ADDR")"
[ "$RAW" = "e2e-ok" ] || fail "raw content = '$RAW' (want 'e2e-ok')"
pass "read artifact through srcos:// ($RAW)"

# ── 8. agent token（第二认证面）───────────────────────────────────────
step 8 "agent token 认证"
TOK="$("$BIN" token create -d "$CFG" --user "$USER_NAME" --label e2e 2>/dev/null \
  | grep -o 'srcos_[a-z2-7]*\.[A-Za-z0-9_-]*' | head -1 || true)"
[ -n "$TOK" ] || fail "token create produced no plaintext"
code="$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOK" "$BASE/api/tools")"
[ "$code" = "200" ] || fail "agent token /api/tools = $code"
pass "Bearer token accepted (200)"
code="$(curl -s -o /dev/null -w '%{http_code}' -b "$CJ" "$BASE/api/tools")"
[ "$code" = "200" ] || fail "session /api/tools = $code"
pass "session and Bearer both accepted"

# ── 9. 审计流（结构化落盘 + 拒绝事件）───────────────────────────────
step 9 "审计流（data/audit）"
# A read-only token must be refused a write — and the refusal is audited.
code="$(curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $TOK" \
  -H 'Content-Type: application/json' -d "{\"tool\":\"$TOOL\"}" "$BASE/api/jobs")"
[ "$code" = "403" ] || fail "read-only token write = $code, want 403"

AUDDIR="$DATA/audit"
grep -rq '"action":"submit"' "$AUDDIR" 2>/dev/null || fail "no submit event in the audit stream"
grep -rq '"action":"scope"' "$AUDDIR" 2>/dev/null || fail "no scope denial in the audit stream"
grep -rq '"action":"instance.done"' "$AUDDIR" 2>/dev/null || fail "no instance.done (lifecycle) in the audit stream"
"$BIN" audit list -d "$CFG" | grep -q 'submit' || fail "srcos audit list read nothing"
"$BIN" audit verify -d "$CFG" | grep -q '^ok' || fail "audit hash chain does not verify"
pass "submit allow + scope deny + instance.done recorded, readable, chain verifies"

# ── 10. 管理端审计页 / API（仅管理员）──────────────────────────────
step 10 "管理端审计流（/admin/audit + /api/admin/audit）"
CJ2="$TMP/cookies-admin"
code="$(curl -s -o /dev/null -w '%{http_code}' -c "$CJ2" -X POST "$BASE/login" \
  -H "Origin: $BASE" --data-urlencode "username=$ADMIN_USER" \
  --data-urlencode "password=$PASSWORD")"
case "$code" in 200|302) ;; *) fail "admin login returned $code" ;; esac

curl -fsS -b "$CJ2" "$BASE/api/admin/audit?decision=deny" | grep -q '"decision":"deny"' \
  || fail "/api/admin/audit did not return the denial"
pass "admin audit API returns the denial"

curl -fsS -b "$CJ2" "$BASE/admin/audit?decision=deny" | grep -q '审计流' \
  || fail "/admin/audit page did not render"
curl -fsS -b "$CJ2" "$BASE/admin/audit" | grep -q '链校验通过' \
  || fail "/admin/audit did not report a verified chain"
pass "admin audit page renders, chain verified"

# A non-admin must not read the audit stream.
code="$(curl -s -o /dev/null -w '%{http_code}' -b "$CJ" "$BASE/api/admin/audit")"
[ "$code" = "403" ] || fail "non-admin /api/admin/audit = $code, want 403"
pass "non-admin refused (403)"

# ── 11. 管理员启动 service（能停也能起）──────────────────────────
step 11 "管理端启动 service（POST /api/admin/instances）"
code="$(curl -s -o /dev/null -w '%{http_code}' -b "$CJ2" -X POST "$BASE/api/admin/instances" \
  -H "Origin: $BASE" -H 'Content-Type: application/json' \
  -d "{\"user\":\"$USER_NAME\",\"tool\":\"$SVC\"}")"
[ "$code" = "201" ] || fail "admin start service = $code, want 201"

# The route must be reachable through the proxy as that user.
code="$(curl -s -o /dev/null -w '%{http_code}' -b "$CJ" "$BASE/proxy/$USER_NAME/$SVC/")"
[ "$code" = "200" ] || fail "proxied service = $code, want 200"
pass "service running and reachable at /proxy/$USER_NAME/$SVC/"

grep -rq '"action":"instance.started"' "$AUDDIR" 2>/dev/null || fail "admin service start not audited"

SVC_INST="$("$BIN" job list -d "$CFG" 2>/dev/null | awk -v t="$SVC" '$2==t{print $1}' | head -1)"
[ -n "$SVC_INST" ] || fail "could not find the service instance id"
code="$(curl -s -o /dev/null -w '%{http_code}' -b "$CJ2" -X POST "$BASE/api/admin/instances/$SVC_INST/stop" \
  -H "Origin: $BASE")"
[ "$code" = "200" ] || fail "admin stop = $code, want 200"
grep -rq '"action":"instance.stopped"' "$AUDDIR" 2>/dev/null || fail "admin service stop not audited"
pass "started and stopped, both audited"

# ── 12. 申请 → 审批（B3）──────────────────────────────────────────
step 12 "工具访问申请/审批"
# alice asks for the requestable tool she does not have.
RESP="$(curl -s -w '\n%{http_code}' -b "$CJ" -X POST "$BASE/api/requests" \
  -H "Origin: $BASE" -H 'Content-Type: application/json' \
  -d "{\"tool\":\"$ASK\",\"reason\":\"e2e\"}")"
code="$(printf '%s' "$RESP" | tail -1)"
[ "$code" = "201" ] || fail "request create = $code: $(printf '%s' "$RESP" | head -1)"

REQ_ID="$(curl -fsS -b "$CJ" "$BASE/api/requests" | grep -o '"id":"req-[^"]*"' | head -1 | cut -d'"' -f4)"
[ -n "$REQ_ID" ] || fail "could not read the request id"

# A tool alice already has needs no request.
code="$(curl -s -o /dev/null -w '%{http_code}' -b "$CJ" -X POST "$BASE/api/requests" \
  -H "Origin: $BASE" -H 'Content-Type: application/json' -d "{\"tool\":\"$TOOL\"}")"
[ "$code" = "400" ] || fail "already-granted request = $code, want 400"

# The admin sees it pending, and a non-admin cannot read the queue.
curl -fsS -b "$CJ2" "$BASE/api/admin/requests?state=pending" | grep -q "$REQ_ID" \
  || fail "admin does not see the pending request"
code="$(curl -s -o /dev/null -w '%{http_code}' -b "$CJ" "$BASE/api/admin/requests")"
[ "$code" = "403" ] || fail "non-admin request queue = $code, want 403"

# Approve: access becomes real, and both acts are audited.
code="$(curl -s -o /dev/null -w '%{http_code}' -b "$CJ2" -X POST "$BASE/api/admin/requests/$REQ_ID/approve" \
  -H "Origin: $BASE" -H 'Content-Type: application/json' -d '{}')"
[ "$code" = "200" ] || fail "approve = $code, want 200"
curl -fsS -b "$CJ" "$BASE/api/tools" | grep -q "\"id\":\"$ASK\"" \
  || fail "approved tool is still not visible to alice"
grep -rq '"action":"request.create"' "$AUDDIR" 2>/dev/null || fail "request.create not audited"
grep -rq '"action":"request.approve"' "$AUDDIR" 2>/dev/null || fail "request.approve not audited"
pass "申请 → 待审 → 批准 → 可用，且入审计"

# ── 13. 幂等键（A2）──────────────────────────────────────────
step 13 "提交幂等键"
sub() {
  curl -fsS -b "$CJ" -X POST "$BASE/api/jobs" -H "Origin: $BASE" -H 'Content-Type: application/json' \
    -d "{\"tool\":\"$TOOL\",\"params\":{\"ticks\":\"1\"},\"idempotencyKey\":\"e2e-$1\"}" \
    | grep -o '"jobId":"[^"]*"' | head -1 | cut -d'"' -f4
}
J1="$(sub key)"
J2="$(sub key)"
[ -n "$J1" ] && [ "$J1" = "$J2" ] || fail "idempotency key did not dedupe: $J1 vs $J2"
J3="$(sub other)"
[ "$J3" != "$J1" ] || fail "a different key must be a different job"
pass "同一 key → 同一 job（$J1）；不同 key → 新 job"

# ── 14. 审计外发（信任锚）──────────────────────────────────────────
step 14 "审计外发到采集端"
if [ -n "$COLLECTOR_PID" ]; then
  got=0
  for _ in $(seq 1 40); do
    if grep -q '"action":"submit"' "$COLLECTOR_LOG" 2>/dev/null; then got=1; break; fi
    sleep 0.5
  done
  [ "$got" = "1" ] || fail "collector received no events (forwarder may be stuck)"
  # The forwarded copy carries prev/hash, so a remote collector can verify too.
  grep -q '"hash":"' "$COLLECTOR_LOG" || fail "forwarded events lost their chain fields"
  pass "事件已外发，且带链字段"
else
  info "  （无 python3，跳过采集端验证）"
fi

echo
printf '\033[32m[e2e] PASS\033[0m  提交 → 队列 → 执行 → 判定 → 日志 → 资源查看 → agent token → 审计 → 管理端 → 启动服务 → 申请审批 → 幂等键 → 审计外发\n'
