#!/usr/bin/env bash
#
# Recompute half of the shared package. Both manifests (ui/ and task/) use
# root: .., so SRCOS mounts this package at /tool and exports SRCOS_TOOL_DIR.
#
# Hard rules this follows: idempotent, parameterised, writes only /workspace,
# exits non-zero on failure, logs to stdout, and blocks until the work is done.

set -euo pipefail

: "${SRCOS_TOOL_DIR:?SRCOS_TOOL_DIR is not set — root: .. should expose the package root}"
: "${SRCOS_WORKSPACE:?SRCOS_WORKSPACE is not set}"
# Relative to the package root, not to this script: that is the point of the
# shared-package layout.
# shellcheck source=/dev/null
source "${SRCOS_TOOL_DIR}/shared/lib.sh"

SAMPLE_ID="${SRCOS_PARAM_SAMPLE_ID:?sample_id is required}"
COPIES="${SRCOS_PARAM_COPIES:-3}"
PREFIX="${SRCOS_PARAM_PREFIX:-scrna}"

OUT="${SRCOS_WORKSPACE}/out/${SRCOS_TASK_ID:-manual}"
SIGN="${OUT}/.sign"

echo "[info] tool dir = ${SRCOS_TOOL_DIR}"
echo "[info] user/tool = ${SRCOS_USER:-?}/${SRCOS_TOOL:-?}"
echo "[info] task = ${SRCOS_TASK_ID:-manual}"
echo "[info] out = ${OUT}"

mkdir -p "${OUT}"
# Idempotency is per task, not per tool: a new task must not be skipped because
# an older one left a marker behind.
if [[ -f "${SIGN}" ]]; then
  echo "[skip] ${SIGN} exists — this task already produced its report"
  exit 0
fi

demo_write_report "${OUT}/report.txt" "${PREFIX}" "${SAMPLE_ID}" "${COPIES}"

touch "${SIGN}"
echo "[done] ${OUT}/report.txt"
