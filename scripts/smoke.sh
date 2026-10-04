#!/usr/bin/env bash
# Smoke-test the chat path with one real turn.
#
# Why this exists: a rebuild broke every turn on this stack and nothing noticed. pi recorded
# `stopReason: "error"` in its session files while pi-gateway answered HTTP 200 with an
# empty body, so from the outside the stack looked healthy and was returning blanks. The
# first signal was a person noticing missing replies.
#
# This is the check that would have caught it: one turn through the same public path the app
# uses, asserting that words come back. It is deliberately the cheapest possible turn,
# because it runs on every start and restart.
#
# Exit 0 = the stack answers. Non-zero = it does not, and the deploy should be considered
# failed.
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE="$REPO/.env"

if [[ ! -f "$ENV_FILE" ]]; then
  echo "smoke: no .env at $ENV_FILE" >&2
  exit 1
fi
# shellcheck disable=SC1090
set -a && . "$ENV_FILE" && set +a

: "${PASSWORD:?smoke: PASSWORD is not set in .env}"
: "${MONGODB_URI:?smoke: MONGODB_URI is not set in .env}"

PROFILE="${SMOKE_PROFILE:-god}"
# Overridable because the app lets a tab pick any model, and a model the provider has
# retired would otherwise fail this check for the wrong reason.
MODEL="${SMOKE_MODEL:-mimo-v2.6-flash}"
BASE="${SMOKE_BASE:-http://127.0.0.1:8080}"
MARKER="SMOKE-OK"
TIMEOUT="${SMOKE_TIMEOUT:-180}"
# pi needs time to boot: the entrypoint proves every MCP server connects before the gateway
# binds, and profile discovery adds several more seconds. Without this wait the check fires
# against a container that is still starting and reports a 502 as a broken stack.
READY_TIMEOUT="${SMOKE_READY_TIMEOUT:-120}"

auth=(-H "Authorization: Bearer $PASSWORD")

# wait_ready polls an endpoint that only answers once pi-gateway is listening with its
# profiles up. /v1/skills is used rather than a bare TCP check because the gateway binds
# :8643 only after profile discovery, and a port that is open is not the same as an agent
# that can take a turn.
wait_ready() {
  local deadline=$((SECONDS + READY_TIMEOUT))
  while ((SECONDS < deadline)); do
    if [[ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "${auth[@]}" \
        "$BASE/p/$PROFILE/v1/skills" 2>/dev/null)" == "200" ]]; then
      return 0
    fi
    sleep 3
  done
  return 1
}

if ! wait_ready; then
  echo "smoke: FAIL — $BASE/p/$PROFILE/v1/skills did not answer 200 within ${READY_TIMEOUT}s" >&2
  echo "smoke: the gateway binds its port only after profile discovery, so this is a" >&2
  echo "smoke: boot failure rather than a bad turn. Try: podman logs docker_pi_1 | tail -40" >&2
  exit 1
fi

body=$(printf '{"provider":"opencode-go","model":"%s","model_options":{"reasoning_effort":"off"},"messages":[{"role":"user","content":"Reply with exactly: %s"}],"stream":true}' \
  "$MODEL" "$MARKER")

out=$(curl -sN --max-time "$TIMEOUT" "$BASE/p/$PROFILE/v1/chat/completions" \
  "${auth[@]}" \
  -H 'Content-Type: application/json' \
  -d "$body" 2>&1)
curl_status=$?

if [[ $curl_status -ne 0 ]]; then
  echo "smoke: FAIL — curl exited $curl_status talking to $BASE/p/$PROFILE" >&2
  echo "smoke: is the stack up? ./scripts/hermes.sh status" >&2
  exit 1
fi

# The marker is checked against the reassembled reply, NOT against the raw stream. The
# gateway emits one SSE frame per delta and the model splits the marker wherever its token
# boundaries fall, so `"content":"SM"` and `"content":"OKE-OK"` arrive as two frames with
# JSON syntax between them. A substring search over the raw bytes can therefore never
# match a reply that arrived correctly — this exact bug made the check report a healthy
# stack as broken.
reply=$(printf '%s' "$out" | python3 -c '
import json, sys
chunks = []
for line in sys.stdin:
    line = line.strip()
    if not line.startswith("data:"):
        continue
    payload = line[len("data:"):].strip()
    if payload == "[DONE]":
        continue
    try:
        obj = json.loads(payload)
    except Exception:
        continue
    for choice in obj.get("choices") or []:
        for key in ("delta", "message"):
            part = choice.get(key)
            if isinstance(part, dict) and isinstance(part.get("content"), str):
                chunks.append(part["content"])
sys.stdout.write("".join(chunks))
' 2>/dev/null)

if [[ -z "$reply" ]]; then
  # No python3, or nothing parseable. Fall back to the raw stream so a missing tool
  # degrades the check rather than removing it.
  reply=$(printf '%s' "$out" | tr -d '\n')
fi

if [[ "$reply" != *"$MARKER"* ]]; then
  echo "smoke: FAIL — no reply containing $MARKER from $BASE/p/$PROFILE (model $MODEL)" >&2
  echo "smoke: reassembled reply was: ${reply:0:200}" >&2
  echo "smoke: --- last 400 bytes of the response ---" >&2
  printf '%s' "$out" | tail -c 400 >&2
  echo >&2
  echo "smoke: an empty or contentless 200 is the signature of a turn pi could not" >&2
  echo "smoke: complete, so start with the agent's own log:" >&2
  echo "smoke:   podman logs docker_pi_1 | tail -40" >&2
  echo "smoke: then the session file, which records stopReason and errorMessage:" >&2
  echo "smoke:   podman exec docker_pi_1 sh -c 'tail -3 \$(ls -t /opt/pi/sessions/*.jsonl | head -1)'" >&2
  exit 1
fi

echo "smoke: OK — $BASE/p/$PROFILE answered ($MODEL): $reply"
exit 0