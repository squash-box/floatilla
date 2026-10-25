#!/usr/bin/env bash
# End-to-end check against a running stack (make up): submits an impl task
# on the scripted model, approves every prompt, resumes it from the review
# park, and expects it to finish. Exits non-zero on failure or timeout.
set -euo pipefail

cd "$(dirname "$0")/.."
F=${FLOATILLA_BIN:-bin/floatilla}
TIMEOUT=${E2E_TIMEOUT:-180}

if [[ ! -x "$F" ]]; then
  go build -o "$F" ./cmd/floatilla
fi

# Capture output before parsing it: piping into head/grep -q can SIGPIPE
# the CLI, which pipefail turns into a failure.
status() {
  local out
  out=$("$F" get "$1")
  awk 'NR == 1 { print $5 }' <<<"$out"
}

echo "waiting for the control plane…"
for _ in $(seq 1 60); do
  "$F" types >/dev/null 2>&1 && break
  sleep 1
done
"$F" types

name="e2e-$(date +%s)"
id=$("$F" submit -type impl -name "$name" -title "End-to-end check" -input demo.ask=true)
echo "submitted $name ($id)"

answered=0
parked=0
deadline=$((SECONDS + TIMEOUT))
while (( SECONDS < deadline )); do
  s=$(status "$id")
  case "$s" in
    waiting)
      detail=$("$F" get "$id")
      if [[ $detail == *"Scripted question"* ]]; then
        "$F" answer "$id" -text "use the boring approach"
      else
        "$F" answer "$id" -yes
      fi
      answered=$((answered + 1))
      ;;
    parked)
      "$F" event "$id" pr.review -payload '{"state":"changes_requested"}'
      parked=$((parked + 1))
      ;;
    done)
      "$F" get "$id"
      if (( answered == 0 || parked == 0 )); then
        echo "FAIL: finished without exercising prompts ($answered) or parking ($parked)"
        exit 1
      fi
      echo "PASS: done after $answered answers and $parked resume(s)"
      exit 0
      ;;
    failed)
      "$F" get "$id"
      echo "FAIL: task failed"
      exit 1
      ;;
  esac
  sleep 1
done

"$F" get "$id" || true
echo "FAIL: timed out after ${TIMEOUT}s (status: $(status "$id"))"
exit 1
