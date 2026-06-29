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
# It runs two Haiku-level builder agents concurrently on a fresh channel, with the
# harness itself tailing the channel over SSE as a live, zero-cost observer:
#   - be-builder : picks a RANDOM port (this run only), announces it, writes a server
#   - fe-builder : learns that port from the channel, writes a counter frontend
#   - (observer) : a curl SSE tail in THIS script — streams the convo live to your
#                  terminal and records the ground-truth transcript (no LLM, no
#                  cold start, no end-of-run dead air)
# Then it boots the backend, curls it, checks the frontend points at the right
# port, scores the run deterministically, and appends one JSON line to
# friction_log.jsonl.
#
# Why a RANDOM port is the crux of the test: if the agreed port were a constant
# (it used to be 3001), the frontend could "match" it by defaulting to that same
# constant WITHOUT ever reading the backend's message — a 100 score that proves
# nothing. By giving the random port ONLY to be-builder and removing fe-builder's
# fallback, port_match=true is achievable ONLY if fe-builder actually read the
# announcement off the channel. It cannot guess a random 20000–39999 port. That
# makes coordination FALSIFIABLE: no real read → no port → product fails.
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
# Random port per run, known ONLY to be-builder. fe-builder must learn it off the
# channel — it is never written into fe-builder's prompt. This is what makes a
# port match real evidence of coordination rather than a shared hardcoded default.
BE_PORT=$(( 20000 + (RANDOM % 20000) ))

cleanup() { kill "${SSE_BG:-}" 2>/dev/null; pkill -f "node ${WORK_DIR}/be/server.js" 2>/dev/null; rm -f "$MCP_CONF"; }
trap cleanup EXIT

echo '{"mcpServers":{"botbus":{"type":"http","url":"https://mcp.botbus.ai/mcp"}}}' > "$MCP_CONF"

# Builder agents need: ToolSearch, the botbus tools, and Write (to create files).
# Observer needs only ToolSearch + botbus. No Bash in the subprocesses — the
# harness itself (this script) does all verification.
BOTBUS_TOOLS="mcp__botbus__set_name mcp__botbus__subscribe mcp__botbus__send mcp__botbus__next mcp__botbus__new_channel"
BUILDER_TOOLS="ToolSearch Write $BOTBUS_TOOLS"
MINT_TOOLS="ToolSearch $BOTBUS_TOOLS"

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
run_agent mint "$MINT_TOOLS" \
  "Call ToolSearch query 'select:mcp__botbus__new_channel'. Then call mcp__botbus__new_channel with no parameters. Print ONLY the channel_id value from the response, nothing else."
CH="$(result_text mint | grep -oE '[a-z0-9]{20,}' | head -1)"
if [ -z "$CH" ]; then echo "FATAL: could not mint channel"; cat "$RUN_DIR/mint.json"; exit 1; fi
echo "Channel: $CH"

# ── Prompts ──────────────────────────────────────────────────────────────────
# botbus replays recent channel history on subscribe (botbus#97, live on
# mcp.botbus.ai), so a peer's announcement is delivered whether it was sent before
# OR after this agent subscribes. That removes the cold-start race: each builder
# announces ONCE and listens, instead of the old re-announce loop. (History: this
# used to need ≥2 re-announce rounds + a long observer lead; see FINDINGS.md F1.)

read -r -d '' BE_PROMPT <<EOF
You are be-builder, joining a botbus channel to build a backend with a peer (fe-builder) live.
1. ToolSearch query 'select:mcp__botbus__set_name,mcp__botbus__subscribe,mcp__botbus__send,mcp__botbus__next'.
2. mcp__botbus__set_name name='be-builder'.
3. mcp__botbus__subscribe channel='$CH'. (On subscribe you receive recent channel history, so you won't miss anything fe-builder already said.)
4. Write a file named exactly server.js in your CURRENT working directory (use the bare name server.js,
   not an absolute path): a Node.js http-module counter server on port $BE_PORT,
   in-memory counter starting at 0, CORS enabled, runnable with exactly 'node server.js' (NO npm install):
     GET  /api/count      -> {"count":N}
     POST /api/increment  body {"delta":N} -> {"count":N}   (delta may be negative)
5. Announce ONCE: mcp__botbus__send channel='$CH'
   text='be-ready: base URL http://localhost:$BE_PORT  GET /api/count  POST /api/increment {delta}'
6. Listen for fe-builder: call mcp__botbus__next channel='$CH' timeout_seconds=10 up to 3 times; record any
   fe-builder message (history replay means you'll see its fe-ready even if it was sent before you subscribed).
7. Finish: mcp__botbus__send channel='$CH' text='be-done'.
End your reply with one line exactly: RESULT_JSON:{"subscribed":true|false,"announced":true|false,"peer_heard":true|false,"file_written":true|false}
EOF

read -r -d '' FE_PROMPT <<EOF
You are fe-builder, joining a botbus channel to build a frontend with a peer (be-builder) live.
The backend's base URL is unknown until be-builder announces it on the channel — you must learn it.
1. ToolSearch query 'select:mcp__botbus__set_name,mcp__botbus__subscribe,mcp__botbus__send,mcp__botbus__next'.
2. mcp__botbus__set_name name='fe-builder'.
3. mcp__botbus__subscribe channel='$CH'. (On subscribe you receive recent channel history, so be-builder's URL announcement reaches you whether it was sent before or after you subscribed.)
4. Announce ONCE: mcp__botbus__send channel='$CH'
   text='fe-ready: need GET /api/count and POST /api/increment {delta}. what base URL?'
5. Listen for the backend URL: call mcp__botbus__next channel='$CH' timeout_seconds=10 in a loop up to 4 times.
   Look for a be-builder message containing a base URL like http://localhost:PORT; stop as soon as you capture it.
6. Write a file named exactly index.html in your CURRENT working directory (bare name, not an absolute
   path): vanilla HTML/CSS/JS counter (starts at 0; buttons +1, -1, Reset).
   Set the JS const API_BASE to the EXACT base URL be-builder announced on the channel.
   You are NOT told the backend port anywhere in these instructions — the ONLY way to know it
   is to read be-builder's message. If you never received a base URL, do NOT guess a port:
   set API_BASE='http://localhost:0' and report used_default_url:true.
   GET \\\${API_BASE}/api/count to load; POST \\\${API_BASE}/api/increment {delta}.
7. Finish: mcp__botbus__send channel='$CH' text='fe-done'.
End your reply with one line exactly: RESULT_JSON:{"subscribed":true|false,"announced":true|false,"peer_heard":true|false,"file_written":true|false,"used_default_url":true|false}
EOF

# ── Observer = a live curl SSE tail, NOT an LLM ──────────────────────────────
# The observer used to be a third cold `claude -p` agent that drained the channel
# with timeout loops and only printed its transcript at the very end (a minute of
# dead air). It needs no intelligence — it just records. So the harness tails the
# botbus SSE stream itself and prints each message the instant it arrives: the
# conversation streams live to your terminal, it costs zero agent startup, and the
# transcript is now harness ground truth (curl) rather than a Haiku self-report.
# `?ids=0` strips the ` [id N]` suffix for clean display; the awk reassembles
# multi-line `data:` frames and dispatches on the blank line.
TRANSCRIPT="$RUN_DIR/transcript.txt"
: > "$TRANSCRIPT"
sse_tail() {
  curl -sN --max-time 180 -H "Accept: text/event-stream" "https://$CH.botbus.ai/?ids=0" 2>/dev/null \
  | awk '
      /^data:/ { v=$0; sub(/^data: ?/,"",v); m=(n++ ? m "\n" v : v); next }
      /^$/     { if (n) { print m; fflush() } n=0; m="" }
    '
}

# ── Launch ───────────────────────────────────────────────────────────────────
# Start the live tail first (replay on subscribe means it still catches anything
# said in the ~1s before it connects), then the two builders concurrently. Each
# channel message prints as "  💬 <name>: <body>" the moment it lands.
echo "Streaming channel live (observer = curl SSE tail)…"
sse_tail | while IFS= read -r line; do printf '  💬 %s\n' "$line"; echo "$line" >> "$TRANSCRIPT"; done &
SSE_BG=$!
sleep 1
echo "Launching be-builder + fe-builder concurrently…"
run_agent be "$BUILDER_TOOLS" "$BE_PROMPT" "$WORK_DIR/be" &
BE_BG=$!
run_agent fe "$BUILDER_TOOLS" "$FE_PROMPT" "$WORK_DIR/fe" &
FE_BG=$!

wait $BE_BG $FE_BG
echo "Builders done; draining final channel messages…"
sleep 2                       # let fe-done/be-done land on the stream
kill "$SSE_BG" 2>/dev/null; wait "$SSE_BG" 2>/dev/null

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
# OBS_R is built from the harness's own SSE transcript (ground truth), not an agent.
OBS_R="$(python3 -c '
import json
lines = [l.rstrip("\n") for l in open("'"$TRANSCRIPT"'")] if __import__("os").path.exists("'"$TRANSCRIPT"'") else []
lines = [l for l in lines if l.strip()]
print(json.dumps({"transcript": lines, "message_count": len(lines)}))
' 2>/dev/null)"; [ -z "$OBS_R" ] && OBS_R='{}'

echo "FE:  $FE_R"
echo "BE:  $BE_R"
echo "OBS: $(echo "$OBS_R" | head -c 400)"

# ── Verify the product actually works ────────────────────────────────────────
# Free the port first (a leftover server from a prior run would shadow this one),
# then boot THIS run's server and check the POST delta relative to the current
# count (don't assume the counter starts at 0 — some impls persist or differ).
BE_BOOTS=false; GET_OK=false; POST_OK=false; FE_OK=false; PORT_MATCH=false; FE_PORT=null
num() { echo "$1" | grep -oE '"count"[: ]*-?[0-9]+' | grep -oE '\-?[0-9]+' | head -1; }
free_port() { # cross-platform (macOS + Linux): kill whatever holds TCP port $1
  local pids
  if command -v lsof >/dev/null 2>&1; then
    pids="$(lsof -ti "tcp:$1" 2>/dev/null)"; [ -n "$pids" ] && kill $pids 2>/dev/null
  elif command -v fuser >/dev/null 2>&1; then
    fuser -k "$1/tcp" 2>/dev/null || true
  fi
}
if [ -f "$WORK_DIR/be/server.js" ]; then
  pkill -f "node.*/be/server.js" 2>/dev/null; free_port "$BE_PORT"
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

# Live coordination evidence. PORT_MATCH is the hard proof: BE's port is random and
# never given to FE, so FE pointing at it means FE read BE's announcement off the
# channel — unguessable, unfakeable. We additionally require the observer to have
# independently recorded BE's announcement (SAW_BE), so a recorded transcript backs
# the claim. The builders' self-reports are logged but NOT trusted for the score.
SAW_BE=false; SAW_FE=false
echo "$OBS_R" | grep -qi 'be-ready' && SAW_BE=true
echo "$OBS_R" | grep -qi 'fe-ready' && SAW_FE=true
COORD_LIVE=false
{ [ "$PORT_MATCH" = true ] && [ "$SAW_BE" = true ]; } && COORD_LIVE=true

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
