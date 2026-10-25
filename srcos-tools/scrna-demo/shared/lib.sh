#!/usr/bin/env bash
#
# Shared helpers for the scrna-demo package. `work.sh` sources this through
# ${SRCOS_TOOL_DIR}/shared/lib.sh, which is the relative-path proof the example
# exists to show: the task manifest lives in task/, but the code it uses lives
# one level up, in the package root.
#
# Keep this POSIX-bash-only and side-effect free at source time.

demo_write_report() {
  local file="$1" prefix="$2" sample="$3" copies="$4"
  local i
  {
    printf '# %s / %s\n' "${prefix}" "${sample}"
    printf 'workspace=%s\n' "${SRCOS_WORKSPACE:-}"
    printf 'task=%s\n' "${SRCOS_TASK_ID:-manual}"
    printf 'tool_dir=%s\n' "${SRCOS_TOOL_DIR:-}"
    printf '\n'
    for ((i = 1; i <= copies; i++)); do
      printf '%s %s copy %d/%d\n' "${prefix}" "${sample}" "${i}" "${copies}"
    done
  } > "${file}"
  echo "[write] ${file}"
}
