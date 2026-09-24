#!/bin/sh
# Start a throwaway gateway, mint a read token, run the live test against it.
# Used during development; it is not part of `npm test`.
set -e
BASE="${1:-http://127.0.0.1:30199}"
DIR="${2:-/tmp/srcos-live}"
cd "$(dirname "$0")/../../.."        # repository root
# SRCOS keeps runtime state in <parent-of-config>/data (config/paths.go), not
# inside config/: the fixture must match or every read is "not found".
rm -rf "$DIR" && mkdir -p "$DIR/config" "$DIR/data/homes/alice"
printf '# Notes\n\nhello from the live test\n' > "$DIR/data/homes/alice/notes.md"
printf 'gene\tvalue\nA\t1\n' > "$DIR/data/homes/alice/rows.tsv"
printf 'testpass123\ntestpass123\n' | ./srcos user alice -d "$DIR/config" >/dev/null
TOKEN=$(./srcos token create -d "$DIR/config" --user alice --label live --scope read | grep -o 'srcos_[a-z2-7]*\.[A-Za-z0-9_-]*' | head -1)
# A gateway from an earlier run would keep the port and serve the *old* binary,
# which makes the test lie (it passes against code one commit behind).
lsof -ti:"${BASE##*:}" | xargs -r kill
sleep 0.3
(nohup ./srcos serve -d "$DIR/config" --host 127.0.0.1 --port "${BASE##*:}" > "$DIR/serve.log" 2>&1 &)
sleep 1.5
SRCOS_BASE_URL="$BASE" SRCOS_TOKEN="$TOKEN" node --test integrations/dsh-plugin/test/live.test.mjs
lsof -ti:"${BASE##*:}" | xargs -r kill
