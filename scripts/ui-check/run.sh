#!/usr/bin/env bash
# Runs the admin UI checks against a throwaway local instance and removes it
# afterwards. Never point this at a production instance or account.
#
# Usage: scripts/ui-check/run.sh [check.js ...]
#   Default checks: settings-ui-check.js, model-ui-check.js, ownership-ui-check.js
#   PUPPETEER    puppeteer or puppeteer-core module path (default: puppeteer-core
#                resolved by node, e.g. after `npm i puppeteer-core` in NODE_PATH)
#   CHROME_PATH  browser executable (default: installed Chrome found by puppeteer)
# Exit status is 0 only when every check passes at 1280, 375 and 390 px widths.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
checks=("$@")
if [ ${#checks[@]} -eq 0 ]; then
  checks=("$root"/scripts/ui-check/{settings,model,ownership}-ui-check.js)
fi
pptr="${PUPPETEER:-puppeteer-core}"

work="$(mktemp -d "${TMPDIR:-/tmp}/mail-auto-ui-check.XXXXXX")"
pid=""
cleanup() {
  if [ -n "$pid" ]; then
    kill "$pid" 2>/dev/null || true
    wait "$pid" 2>/dev/null || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 130' INT TERM

(cd "$root" && CGO_ENABLED=0 go build -trimpath -o "$work/mail-auto" ./cmd/mail-auto)
port="$(node -e 'const s=require("net").createServer().listen(0,"127.0.0.1",()=>{console.log(s.address().port);s.close()})')"
base="http://127.0.0.1:$port"
mkdir -m 700 "$work/data" "$work/keys" "$work/secrets"
flags=(-data-dir "$work/data" -key-dir "$work/keys" -secret-dir "$work/secrets" -listen "127.0.0.1:$port")
"$work/mail-auto" "${flags[@]}" -automatic-sync=false serve >"$work/server.log" 2>&1 &
pid=$!
for _ in $(seq 1 60); do
  curl -fsS "$base/readyz" >/dev/null 2>&1 && break
  kill -0 "$pid" 2>/dev/null || { echo "UI check aborted: instance did not start" >&2; tail -n 5 "$work/server.log" >&2; exit 1; }
  sleep 0.5
done
curl -fsS "$base/readyz" >/dev/null || { echo "UI check aborted: instance not ready" >&2; exit 1; }

# Temporary bootstrap account -> fresh random credentials, stored only in $work.
boot="$("$work/mail-auto" "${flags[@]}" bootstrap show)"
u="$(printf '%s\n' "$boot" | sed -n 's/^Username: //p')"
p="$(printf '%s\n' "$boot" | sed -n 's/^Temporary password: //p')"
(umask 077 && U="$u" P="$p" node -e '
const c=require("crypto"),r=()=>c.randomBytes(12).toString("hex");
require("fs").writeFileSync(process.argv[1],JSON.stringify({u:process.env.U,p:process.env.P,nu:"ui-"+r().slice(0,8),np:r()}));' "$work/creds.json")

node "$root/scripts/ui-check/run-checks.cjs" "$pptr" "$base" "$work/creds.json" "${checks[@]}"
