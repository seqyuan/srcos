#!/usr/bin/env bash
#
# Path-input half of the shared package: the value comes from
# <srcos-path-picker> / GET /api/paths, so it is a sandbox path under a
# declared storage (here: /data). Everything else is the same contract as
# task/work.sh.

set -euo pipefail

: "${SRCOS_TOOL_DIR:?SRCOS_TOOL_DIR is not set — root: .. should expose the package root}"
: "${SRCOS_WORKSPACE:?SRCOS_WORKSPACE is not set}"
# shellcheck source=/dev/null
source "${SRCOS_TOOL_DIR}/shared/lib.sh"

INPUT_DIR="${SRCOS_PARAM_INPUT_DIR:?input_dir is required}"
PREFIX="${SRCOS_PARAM_PREFIX:-path}"

if [[ ! -d "${INPUT_DIR}" ]]; then
  echo "[error] ${INPUT_DIR} is not a directory (the path picker should only offer directories)" >&2
  exit 2
fi

OUT="${SRCOS_WORKSPACE}/out/${SRCOS_TASK_ID:-manual}"
SIGN="${OUT}/.sign"

echo "[info] tool dir = ${SRCOS_TOOL_DIR}"
echo "[info] user/tool = ${SRCOS_USER:-?}/${SRCOS_TOOL:-?}"
echo "[info] input_dir = ${INPUT_DIR}"
echo "[info] out = ${OUT}"

mkdir -p "${OUT}"
if [[ -f "${SIGN}" ]]; then
  echo "[skip] ${SIGN} exists — this task already produced its report"
  exit 0
fi

demo_write_listing "${OUT}/report.txt" "${PREFIX}" "${INPUT_DIR}"
echo "[list] first entries under ${INPUT_DIR}:"
ls -1A "${INPUT_DIR}" | head -5 || true

touch "${SIGN}"
echo "[done] ${OUT}/report.txt"
