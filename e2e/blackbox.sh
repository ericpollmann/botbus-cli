#!/usr/bin/env bash
# botbus TRUE blackbox E2E test — each agent is its OWN process.
#
# Why a shell script and not a Workflow: every agent spawned inside ONE Claude
# session shares ONE MCP connection to the botbus gateway, i.e. ONE subscription.
# The gateway excludes a sender's own subscription from broadcasts, so sibling
# subagents can never hear each other — coordination is impossible in-process.
# (Proven empirically: a sibling's send returns "via":"self-excluded" and never
# arrives.) Separate OS processes each get their own subscription, so their sends
# broadcast normally and peers receive them. This harness therefore launches each
# agent as an independent `claude -p` process with its own botbus connection —
# the same way real coding-agent sessions join a botbus channel in production.
#
# It runs three Haiku-level agents concurrently on a fresh channel:
#   - be-builder : announces its API base URL, writes a counter server
#   - fe-builder : learns the URL from the channel, writes a counter frontend
#   - observer   : passively records the channel transcript (ground truth)
# Then it boots the backend, curls it, checks the frontend points at the right
# port, scores the run deterministically, and appends one JSON line to
# friction_log.jsonl.
#
# Usage:
#   e2e/blackbox.sh                 # one trial
#   for i in $(seq 5); do e2e/blackbox.sh; done   # a batch
#   node e2e/analyze_friction.js    # trend report across trials
#
# Requires: claude CLI on PATH, botbus MCP reachable, node, python3, curl.

set -uo pipefail

MODEL="${BOTBUS_E2E_MODEL:-claude-haiku-4-5-20251001}"
WORK_DIR="${BOTBUS_E2E_WORKDIR:-/tmp/botbus-e2e}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
LOG_FILE="$HERE/friction_log.jsonl"
MCP_CONF="$(mktemp /tmp/botbus-mcp.XXXXXX.json)"
RUN_DIR="$(mktemp -d /tmp/botbus-run.XXXXXX)"
BE_PORT=3001

cleanup() { pkill -f "node ${WORK_DIR}/be/server.js" 2>/dev/null; rm -f "$MCP_CONF"; }
trap cleanup EXIT

echo '{"mcpServers":{"botbus":{"type":"http","url":"https://mcp.botbus.ai/mcp"}}}' > "$MCP_CONF"

# Builder agents need: ToolSearch, the botbus tools, and Write (to create files).
# Observer needs only ToolSearch + botbus. No Bash in the subprocesses — the
# harness itself (this script) does all verification.
BOTBUS_TOOLS="mcp__botbus__set_name mcp__botbus__subscribe mcp__botbus__send mcp__botbus__next mcp__botbus__new_channel"
BUILDER_TOOLS="ToolSearch Write $BOTBUS_TOOLS"
OBSERVER_TOOLS="ToolSearch $BOTBUS_TOOLS"

run_agent() { # name  allowedTools  prompt  [cwd]  -> writes $RUN_DIR/<name>.json
  # Run each agent FROM its target directory: agents don't reliably honor an
  # absolute output path, but a bare filename always lands in cwd. (Observed
  # friction — a builder wrote server.js to the harness's cwd, not the path given.)
  local name="$1"; local tools="$2"; local prompt="$3"; local cwd="${4:-$RUN_DIR}"
  ( cd "$cwd" && claude -p "$prompt" \
      --model "$MODEL" \
      --mcp-config "$MCP_CONF" --strict-mcp-config \
      --allowedTools $tools \
      --output-format json ) \
    > "$RUN_DIR/$name.json" 2> "$RUN_DIR/$name.err"
}

result_text() { # extract .result from a claude -p json envelope
  python3 -c "import json,sys; print(json.load(open('$RUN_DIR/$1.json')).get('result',''))" 2>/dev/null
}

echo "=== botbus blackbox E2E — model=$MODEL ==="
rm -rf "$WORK_DIR"; mkdir -p "$WORK_DIR/fe" "$WORK_DIR/be"

# ── Mint a fresh channel (its own one-shot process) ──────────────────────────
run_agent mint "$OBSERVER_TOOLS" \
  "Call ToolSearch query 'select:mcp__botbus__new_channel'. Then call mcp__botbus__new_channel with no parameters. Print ONLY the channel_id value from the response, nothing else."
CH="$(result_text mint | grep -oE '[a-z0-9]{20,}' | head -1)"
if [ -z "$CH" ]; then echo "FATAL: could not mint channel"; cat "$RUN_DIR/mint.json"; exit 1; fi
echo "Channel: $CH"

# ── Prompts ──────────────────────────────────────────────────────────────────
# Both builders use a re-announce loop so a late-subscribing peer still hears the
# announcement on a later round — the only robust pattern given no history replay.
# DEPLOY-GATED: botbus#97 adds replay-on-subscribe (merged, not yet on
# mcp.botbus.ai). Once it deploys, drop the re-announce loops to a single announce,
# shrink the observer lead, and remove the "does NOT replay" lines below. Confirm
# replay is live first: send → subscribe → next should return the pre-sent message.
# See FINDINGS.md F1.

read -r -d '' BE_PROMPT <<EOF
You are be-builder, joining a botbus channel to build a backend with a peer (fe-builder) live.
1. ToolSearch query 'select:mcp__botbus__set_name,mcp__botbus__subscribe,mcp__botbus__send,mcp__botbus__next'.
2. mcp__botbus__set_name name='be-builder'.
3. mcp__botbus__subscribe channel='$CH'. (botbus does NOT replay older messages, so subscribe before sending.)
4. Write a file named exactly server.js in your CURRENT working directory (use the bare name server.js,
   not an absolute path): a Node.js http-module counter server on port $BE_PORT,
   in-memory counter starting at 0, CORS enabled, runnable with exactly 'node server.js' (NO npm install):
     GET  /api/count      -> {"count":N}
     POST /api/increment  body {"delta":N} -> {"count":N}   (delta may be negative)
5. Coordinate: do at least 2 and up to 4 rounds (do NOT stop before round 2, so a late-joining
   listener still catches an announcement): mcp__botbus__send channel='$CH'
   text='be-ready: base URL http://localhost:$BE_PORT  GET /api/count  POST /api/increment {delta}'
   then mcp__botbus__next channel='$CH' timeout_seconds=8 (record any fe-builder message). After round 2,
   stop once you have heard fe-builder.
6. Finish: mcp__botbus__send channel='$CH' text='be-done'.
End your reply with one line exactly: RESULT_JSON:{"subscribed":true|false,"announced":true|false,"peer_heard":true|false,"file_written":true|false}
EOF

read -r -d '' FE_PROMPT <<EOF
You are fe-builder, joining a botbus channel to build a frontend with a peer (be-builder) live.
The backend's base URL is unknown until be-builder announces it on the channel — you must learn it.
1. ToolSearch query 'select:mcp__botbus__set_name,mcp__botbus__subscribe,mcp__botbus__send,mcp__botbus__next'.
2. mcp__botbus__set_name name='fe-builder'.
3. mcp__botbus__subscribe channel='$CH'. (botbus does NOT replay older messages, so subscribe before listening.)
4. Coordinate: do at least 2 and up to 5 rounds (do NOT stop before round 2): mcp__botbus__send channel='$CH'
   text='fe-ready: need GET /api/count and POST /api/increment {delta}. what base URL?'
   then mcp__botbus__next channel='$CH' timeout_seconds=8. Look for a be-builder message containing a base URL
   like http://localhost:PORT. After round 2, stop as soon as you capture that URL.
5. Write a file named exactly index.html in your CURRENT working directory (bare name, not an absolute
   path): vanilla HTML/CSS/JS counter (starts at 0; buttons +1, -1, Reset).
   Set the JS const API_BASE to the EXACT base URL be-builder announced. ONLY if you never received one,
   use http://localhost:$BE_PORT. GET \\\${API_BASE}/api/count to load; POST \\\${API_BASE}/api/increment {delta}.
6. Finish: mcp__botbus__send channel='$CH' text='fe-done'.
End your reply with one line exactly: RESULT_JSON:{"subscribed":true|false,"announced":true|false,"peer_heard":true|false,"file_written":true|false,"used_default_url":true|false}
EOF

read -r -d '' OBS_PROMPT <<EOF
You are observer, a passive recorder on a botbus channel. Build nothing.
1. ToolSearch query 'select:mcp__botbus__set_name,mcp__botbus__subscribe,mcp__botbus__next'.
2. mcp__botbus__set_name name='observer'.
3. mcp__botbus__subscribe channel='$CH' immediately.
4. Drain: call mcp__botbus__next channel='$CH' timeout_seconds=12 in a loop until you get 3 timeouts in a row or loop 30 times.
   Record every message as "name: body".
End your reply with one line exactly: RESULT_JSON:{"transcript":["name: body", ...],"message_count":N}
EOF

# ── Launch: observer first so it is subscribed before the builders talk ───────
echo "Launching observer…"
run_agent observer "$OBSERVER_TOOLS" "$OBS_PROMPT" "$RUN_DIR" &
OBS_BG=$!
sleep 14   # claude cold start + ToolSearch + subscribe; observer must be live before builders talk
echo "Launching be-builder + fe-builder concurrently…"
run_agent be "$BUILDER_TOOLS" "$BE_PROMPT" "$WORK_DIR/be" &
BE_BG=$!
run_agent fe "$BUILDER_TOOLS" "$FE_PROMPT" "$WORK_DIR/fe" &
FE_BG=$!

wait $BE_BG $FE_BG
echo "Builders done; waiting for observer to drain…"
wait $OBS_BG

# ── Extract structured results ───────────────────────────────────────────────
# Parse the first balanced JSON object after the RESULT_JSON: marker (robust to
# trailing prose or stray braces that a greedy regex would mis-capture).
extract_json() {
  result_text "$1" | python3 -c '
import sys, json
txt = sys.stdin.read()
i = txt.find("RESULT_JSON:")
if i < 0: print("{}"); sys.exit()
try:
    obj, _ = json.JSONDecoder().raw_decode(txt, txt.index("{", i))
    print(json.dumps(obj))
except Exception:
    print("{}")
'
}
# NB: do NOT use ${VAR:-{}} here — bash parses it as default '{' plus a literal
# '}', appending a stray brace to a non-empty value and corrupting the JSON.
FE_R="$(extract_json fe)";        [ -z "$FE_R" ]  && FE_R='{}'
BE_R="$(extract_json be)";        [ -z "$BE_R" ]  && BE_R='{}'
OBS_R="$(extract_json observer)"; [ -z "$OBS_R" ] && OBS_R='{}'

echo "FE:  $FE_R"
echo "BE:  $BE_R"
echo "OBS: $(echo "$OBS_R" | head -c 400)"

# ── Verify the product actually works ────────────────────────────────────────
# Free the port first (a leftover server from a prior run would shadow this one),
# then boot THIS run's server and check the POST delta relative to the current
# count (don't assume the counter starts at 0 — some impls persist or differ).
BE_BOOTS=false; GET_OK=false; POST_OK=false; FE_OK=false; PORT_MATCH=false; FE_PORT=null
num() { echo "$1" | grep -oE '"count"[: ]*-?[0-9]+' | grep -oE '\-?[0-9]+' | head -1; }
if [ -f "$WORK_DIR/be/server.js" ]; then
  pkill -f "node.*/be/server.js" 2>/dev/null; (command -v fuser >/dev/null && fuser -k ${BE_PORT}/tcp 2>/dev/null) || true
  sleep 1
  ( cd "$WORK_DIR/be" && nohup node server.js >/dev/null 2>&1 & ) >/dev/null 2>&1
  sleep 2
  C0="$(curl -s -m 3 http://localhost:$BE_PORT/api/count 2>/dev/null)"
  N0="$(num "$C0")"
  if [ -n "$N0" ]; then BE_BOOTS=true; GET_OK=true; fi
  C1="$(curl -s -m 3 -X POST http://localhost:$BE_PORT/api/increment -H 'Content-Type: application/json' -d '{"delta":5}' 2>/dev/null)"
  N1="$(num "$C1")"
  # POST is correct if the returned count is exactly the prior count + 5
  if [ -n "$N0" ] && [ -n "$N1" ] && [ "$N1" -eq "$((N0+5))" ]; then POST_OK=true; fi
  pkill -f "node.*/be/server.js" 2>/dev/null
fi
if [ -f "$WORK_DIR/fe/index.html" ]; then
  if grep -q '/api/count' "$WORK_DIR/fe/index.html" && grep -q '/api/increment' "$WORK_DIR/fe/index.html"; then FE_OK=true; fi
  FE_PORT="$(grep -oE 'localhost:[0-9]+' "$WORK_DIR/fe/index.html" | head -1 | grep -oE '[0-9]+')"
  [ "${FE_PORT:-}" = "$BE_PORT" ] && PORT_MATCH=true
  FE_PORT="${FE_PORT:-null}"
fi
echo "Verify: boots=$BE_BOOTS GET=$GET_OK POST=$POST_OK fe_ok=$FE_OK port_match=$PORT_MATCH (fe:$FE_PORT be:$BE_PORT)"

# Live coordination evidence. The observer transcript is corroborating proof, but
# the builders' own reports are first-party evidence: if FE reports peer_heard AND
# did NOT fall back to the default URL, it provably learned BE's URL on the channel.
SAW_BE=false; SAW_FE=false
echo "$OBS_R" | grep -qi 'be-ready' && SAW_BE=true
echo "$OBS_R" | grep -qi 'fe-ready' && SAW_FE=true
FE_HEARD="$(echo "$FE_R"  | python3 -c 'import sys,json; d=json.load(sys.stdin); print("y" if d.get("peer_heard") and not d.get("used_default_url") else "n")' 2>/dev/null)"
BE_HEARD="$(echo "$BE_R"  | python3 -c 'import sys,json; d=json.load(sys.stdin); print("y" if d.get("peer_heard") else "n")' 2>/dev/null)"
COORD_LIVE=false
if { [ "$SAW_BE" = true ] && [ "$SAW_FE" = true ]; } || { [ "$FE_HEARD" = y ] && [ "$BE_HEARD" = y ]; }; then COORD_LIVE=true; fi

# ── Deterministic score ──────────────────────────────────────────────────────
SCORE=0
[ "$BE_BOOTS" = true ] && SCORE=$((SCORE+20))
[ "$POST_OK" = true ]  && SCORE=$((SCORE+20))
[ "$FE_OK" = true ]    && SCORE=$((SCORE+20))
[ "$PORT_MATCH" = true ] && SCORE=$((SCORE+25))
[ "$COORD_LIVE" = true ] && SCORE=$((SCORE+15))
PRODUCT_WORKS=false; { [ "$BE_BOOTS" = true ] && [ "$POST_OK" = true ] && [ "$FE_OK" = true ] && [ "$PORT_MATCH" = true ]; } && PRODUCT_WORKS=true
SUCCESS=false; { [ "$SCORE" -ge 90 ] && [ "$PRODUCT_WORKS" = true ]; } && SUCCESS=true
echo "SCORE=$SCORE  product_works=$PRODUCT_WORKS  coordination_live=$COORD_LIVE  success=$SUCCESS"

# ── Append one JSON line to the friction log ─────────────────────────────────
# Build verify/judge as JSON in bash (lowercase true/false is valid JSON), then
# let Python merge everything. Avoids Python-vs-JSON boolean-literal mismatches.
VERIFY_JSON="{\"be_boots\":$BE_BOOTS,\"get_ok\":$GET_OK,\"post_ok\":$POST_OK,\"fe_ok\":$FE_OK,\"fe_port\":${FE_PORT:-null},\"be_port\":$BE_PORT,\"port_match\":$PORT_MATCH}"
JUDGE_JSON="{\"success\":$SUCCESS,\"score\":$SCORE,\"product_works\":$PRODUCT_WORKS,\"coordination_live\":$COORD_LIVE}"

python3 - "$LOG_FILE" "$CH" "$MODEL" "$FE_R" "$BE_R" "$OBS_R" "$VERIFY_JSON" "$JUDGE_JSON" <<'PYEOF'
import json, sys
log_file, ch, model, fe, be, obs, verify, judge = sys.argv[1:9]
def j(s, default):
    try: return json.loads(s)
    except Exception: return default
entry = {
  "version": 3, "harness": "blackbox-multiprocess", "model": model,
  "channel_id": ch,
  "fe": j(fe, {}), "be": j(be, {}),
  "transcript": j(obs, {}).get("transcript", []),
  "verify": j(verify, {}),
  "judge": j(judge, {}),
}
with open(log_file, "a") as f:
    f.write(json.dumps(entry) + "\n")
print("logged run to", log_file)
PYEOF

echo "=== done ($SCORE/100) ==="
