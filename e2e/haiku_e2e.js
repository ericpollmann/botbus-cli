// botbus end-to-end agent friction test
//
// Tests whether a Haiku-level agent can use the botbus MCP tools to:
//   1. Subscribe to a channel
//   2. Receive a task
//   3. Coordinate via the channel
//   4. Produce a working software artifact
//
// Architecture: orchestrator-mediated pipeline to avoid message-consumption
// races (parallel agents on a single MCP session share one next() buffer).
// Once friction is low enough, upgrade to true-parallel with separate sessions.
//
// Run once:
//   Workflow({ scriptPath: 'e2e/haiku_e2e.js' })
//
// Run in a loop (every 30 min):
//   /loop 30m Workflow({ scriptPath: 'e2e/haiku_e2e.js' }) then commit e2e/friction_log.jsonl
//
// Friction log: e2e/friction_log.jsonl — one JSON line per run.
// Over time, watch the score column trend upward and friction_points shrink.

export const meta = {
  name: 'botbus-e2e-agent-test',
  description: 'E2E: Haiku agents coordinate live via botbus to build a counter app (FE + BE)',
  phases: [
    { title: 'Setup', detail: 'Mint a fresh channel' },
    { title: 'Coordinate', detail: 'FE + BE build live; an observer records the channel transcript' },
    { title: 'Verify', detail: 'Boot the BE server, curl endpoints, check FE binds the right port' },
    { title: 'Judge', detail: 'Score against the real transcript + integration result, log friction' },
  ],
}

// ── v2 design notes ──────────────────────────────────────────────────────────
// Run 1 (sequential) exposed two harness flaws that masked real behavior:
//   1. FE ran before BE, so FE always timed out waiting for BE and fell back to
//      a wrong default port — the product was broken but scored 100.
//   2. The judge subscribed AFTER all traffic; botbus does not replay history to
//      a fresh subscription, so the judge saw an empty channel and scored
//      coordination purely from agent self-reports (untrustworthy).
// v2 fixes both:
//   - FE, BE, and a passive OBSERVER all start in one parallel() barrier, so the
//     observer subscribes at the same instant and captures the LIVE transcript.
//   - A Verify step actually boots the BE server and curls it, and checks that
//     FE's configured API base matches BE's listening port — the score now
//     reflects whether the product actually works, not just whether files exist.

// ── Constants ──────────────────────────────────────────────────────────────

const WORK_DIR = '/tmp/botbus-e2e'
const LOG_FILE = '/home/user/botbus-cli/e2e/friction_log.jsonl'

// The minimal "join" prompt that botbus should ideally be able to give any agent.
// Intentionally terse — friction comes from what's missing or ambiguous here.
// This is the closest thing to what a real botbus user pastes into their agent.
const JOIN_PREAMBLE = `You are a coding agent joining a botbus coordination channel to build software with a peer agent.

STEP 0 — load tools (REQUIRED before any botbus call):
Call ToolSearch with query "select:mcp__botbus__set_name,mcp__botbus__subscribe,mcp__botbus__send,mcp__botbus__next"

STEP 1 — identify yourself:
Call mcp__botbus__set_name with name "AGENT_NAME"

STEP 2 — join the channel:
Call mcp__botbus__subscribe with channel "CHANNEL_ID"

The rest (announce / listen / build / report) is in YOUR ROLE below.
IMPORTANT about coordination: botbus does NOT replay messages sent before you
subscribed. Subscribe FIRST, then announce, then listen. Every record you keep of
what a peer said must come from a next() call you made AFTER subscribing.

Return a JSON object with keys:
  subscribed (bool), announced (bool), peer_heard (bool),
  peer_messages ([string]  — raw bodies of every message you received from the peer),
  file_written (bool), file_path (string or null), friction ([string])
`

// ── Helpers ────────────────────────────────────────────────────────────────

function fePrompt(channelId) {
  return JOIN_PREAMBLE
    .replace('AGENT_NAME', 'fe-builder')
    .replace(/CHANNEL_ID/g, channelId) +
`
YOUR ROLE: Frontend Builder. A peer "be-builder" is building the backend on this
channel RIGHT NOW, at the same time as you. You must learn its API base URL from it.

STEP 3 — announce (immediately after subscribing):
  mcp__botbus__send text="fe-ready: I need GET /api/count -> {count} and POST /api/increment {delta} -> {count}. What is your base URL?"

STEP 4 — listen for the backend's URL:
  Call mcp__botbus__next (channel "${channelId}", timeout_seconds 30) in a loop, up to 6 times.
  You are looking for a message from be-builder containing a base URL like http://localhost:PORT
  (it will start with "be-ready:"). Record every message body you receive in peer_messages.
  Stop looping once you have the base URL, or after 6 tries.

STEP 5 — build ${WORK_DIR}/fe/index.html:
  - Vanilla HTML/CSS/JS, no bundler, no npm. Counter starts at 0.
  - Three buttons: +1, -1, Reset.
  - Set the JS constant API_BASE to the EXACT base URL the backend announced.
    If and ONLY if you never received one, use http://localhost:3001 and add a
    friction note saying you had to guess.
  - GET  \${API_BASE}/api/count to load the initial count.
  - POST \${API_BASE}/api/increment with body {delta: 1} / {delta: -1}. Reset sets count to 0.

STEP 6: mcp__botbus__send text="fe-done: ${WORK_DIR}/fe/index.html". Set peer_heard=true iff
you received at least one be-builder message. file_path is the index.html path.
`
}

function bePrompt(channelId) {
  return JOIN_PREAMBLE
    .replace('AGENT_NAME', 'be-builder')
    .replace(/CHANNEL_ID/g, channelId) +
`
YOUR ROLE: Backend Builder. A peer "fe-builder" is building the frontend on this
channel RIGHT NOW. It needs your base URL, so announce it EARLY and clearly.

STEP 3 — announce (immediately after subscribing, before you build):
  mcp__botbus__send text="be-ready: base URL http://localhost:3001 serving GET /api/count -> {count} and POST /api/increment {delta} -> {count}"

STEP 4 — build ${WORK_DIR}/be/server.js:
  - A counter server on port 3001, in-memory counter starting at 0.
  - GET  /api/count      -> {count: N}
  - POST /api/increment  body {delta: N} -> {count: N}   (delta may be negative)
  - CORS headers so a browser frontend can call it.
  - MUST run with exactly: node server.js  (no npm install). Prefer Node's built-in
    http module over Express so there are zero dependencies.

STEP 5 — listen briefly for the frontend:
  Call mcp__botbus__next (timeout_seconds 20) up to 3 times. Record bodies in peer_messages.

STEP 6: mcp__botbus__send text="be-done: ${WORK_DIR}/be/server.js". Set peer_heard=true iff
you received at least one fe-builder message. file_path is the server.js path.
`
}

function observerPrompt(channelId) {
  return `You are a passive OBSERVER recording a botbus channel transcript. Do not build anything.

1. ToolSearch query "select:mcp__botbus__set_name,mcp__botbus__subscribe,mcp__botbus__next"
2. mcp__botbus__set_name name="observer"
3. mcp__botbus__subscribe channel="${channelId}"   (do this IMMEDIATELY — you must be subscribed before the builders talk)
4. Drain the channel: call mcp__botbus__next (channel "${channelId}", timeout_seconds 20) in a loop.
   Keep going until you have received 3 consecutive timeouts in a row, or you have looped 25 times.
   Record EVERY message as "name: body" using the name and body fields from each result.

Return JSON: { transcript: [string], message_count: number }
The transcript is the ground-truth record of what the agents actually said to each other.`
}

function judgePrompt(channelId, feResult, beResult, observerResult, verify) {
  return `You are the judge of a botbus E2E agent test run. Score what ACTUALLY happened,
using the ground-truth transcript and integration result below — not the agents' own claims.

Channel: ${channelId}

GROUND-TRUTH CHANNEL TRANSCRIPT (recorded live by a passive observer):
${JSON.stringify(observerResult?.transcript ?? [], null, 2)}

INTEGRATION VERIFICATION (the harness actually booted the BE server and curled it):
${JSON.stringify(verify, null, 2)}

FE agent self-report: ${JSON.stringify(feResult)}
BE agent self-report: ${JSON.stringify(beResult)}

SCORING (0–100), based on the transcript and verification, NOT self-reports:
  +20  BE server booted and GET /api/count returned {count:...}        (see verify.be_boots, verify.get_ok)
  +20  POST /api/increment changed the count correctly                 (see verify.post_ok)
  +20  FE file exists, is a counter UI, and calls /api/count + /api/increment  (see verify.fe_ok)
  +25  FE's configured API base port MATCHES BE's listening port       (see verify.port_match)
       — this is the real coordination payoff; a mismatch means a broken product
  +15  The transcript shows BOTH a be-ready (URL announcement) AND an fe-ready message
       (i.e. the agents actually talked live, not just built in isolation)

Also set:
  product_works = (verify.be_boots AND verify.get_ok AND verify.post_ok AND verify.fe_ok AND verify.port_match)
  coordination_live = (transcript contains BOTH an fe-ready and a be-ready message)
  success = (score >= 90 AND product_works)

FRICTION — list every friction point. Draw especially on the transcript: did the FE hear the
BE's URL before building? Did anyone fail to subscribe before talking? Did an agent skip ToolSearch?
Did the FE fall back to a guessed port? Was the announcement format ambiguous? Each friction point
needs {phase, agent, issue, severity, fix} where fix is a concrete change to botbus, its onboarding
copy, or the agent prompt that would remove the friction.

SUGGESTIONS — 3-6 prioritized, concrete improvements to make a Haiku agent succeed here in under a minute.

Return JSON matching the schema.`
}

// ── Result schema ──────────────────────────────────────────────────────────

const AGENT_SCHEMA = {
  type: 'object',
  properties: {
    subscribed: { type: 'boolean' },
    announced: { type: 'boolean' },
    peer_heard: { type: 'boolean' },
    peer_messages: { type: 'array', items: { type: 'string' } },
    file_written: { type: 'boolean' },
    file_path: { type: ['string', 'null'] },
    friction: { type: 'array', items: { type: 'string' } },
  },
  required: ['subscribed', 'announced', 'peer_heard', 'peer_messages', 'file_written', 'file_path', 'friction'],
}

const OBSERVER_SCHEMA = {
  type: 'object',
  required: ['transcript', 'message_count'],
  properties: {
    transcript: { type: 'array', items: { type: 'string' } },
    message_count: { type: 'number' },
  },
}

const VERIFY_SCHEMA = {
  type: 'object',
  required: ['be_boots', 'get_ok', 'post_ok', 'fe_ok', 'fe_port', 'be_port', 'port_match', 'notes'],
  properties: {
    be_boots: { type: 'boolean' },
    get_ok: { type: 'boolean' },
    post_ok: { type: 'boolean' },
    fe_ok: { type: 'boolean' },
    fe_port: { type: ['number', 'null'] },
    be_port: { type: ['number', 'null'] },
    port_match: { type: 'boolean' },
    notes: { type: 'string' },
  },
}

const JUDGE_SCHEMA = {
  type: 'object',
  required: ['success', 'score', 'product_works', 'coordination_live', 'friction_points', 'suggestions'],
  properties: {
    success: { type: 'boolean' },
    score: { type: 'number' },
    product_works: { type: 'boolean' },     // BE boots, GET+POST work, ports match
    coordination_live: { type: 'boolean' }, // transcript shows both fe-ready and be-ready
    friction_points: {
      type: 'array',
      items: {
        type: 'object',
        required: ['phase', 'agent', 'issue', 'severity', 'fix'],
        properties: {
          phase: { type: 'string' },
          agent: { type: 'string' },
          issue: { type: 'string' },
          severity: { type: 'string', enum: ['blocking', 'high', 'medium', 'low'] },
          fix: { type: 'string' },
        },
      },
    },
    suggestions: { type: 'array', items: { type: 'string' } },
  },
}

// ── Main ───────────────────────────────────────────────────────────────────

// Phase 1: fresh workspace + fresh channel
phase('Setup')

await agent(
  `Run bash: rm -rf ${WORK_DIR} && mkdir -p ${WORK_DIR}/fe ${WORK_DIR}/be. Return "ok".`,
  { label: 'mkdir', model: 'haiku' }
)

const setupResult = await agent(`
Mint a botbus channel for a coordination test.
1. ToolSearch query "select:mcp__botbus__new_channel"
2. Call mcp__botbus__new_channel (no parameters).
3. Return ONLY the channel_id from the response — the bare id, nothing else.`,
  { label: 'mint-channel', model: 'haiku' }
)

const channelId = setupResult.trim().split(/\s+/).pop().replace(/['"]/g, '').trim()
log(`Channel: ${channelId}`)

if (!channelId || channelId.length < 8) {
  log('ERROR: could not extract channel ID. Aborting.')
  return { success: false, score: 0, error: 'channel_id_extraction_failed', raw: setupResult }
}

// Phase 2: FE, BE, and observer run concurrently so the agents coordinate LIVE
// and the observer captures the real transcript. parallel() is the barrier we
// actually want here: all three must start together.
phase('Coordinate')
log('Launching fe-builder, be-builder, and observer concurrently…')

const [feResult, beResult, observerResult] = await parallel([
  () => agent(fePrompt(channelId), { label: 'fe-builder', phase: 'Coordinate', model: 'haiku', schema: AGENT_SCHEMA }),
  () => agent(bePrompt(channelId), { label: 'be-builder', phase: 'Coordinate', model: 'haiku', schema: AGENT_SCHEMA }),
  () => agent(observerPrompt(channelId), { label: 'observer', phase: 'Coordinate', model: 'haiku', schema: OBSERVER_SCHEMA }),
])

log(`FE: subscribed=${feResult?.subscribed} announced=${feResult?.announced} peer_heard=${feResult?.peer_heard} file=${feResult?.file_written}`)
log(`BE: subscribed=${beResult?.subscribed} announced=${beResult?.announced} peer_heard=${beResult?.peer_heard} file=${beResult?.file_written}`)
log(`Observer captured ${observerResult?.message_count ?? 0} message(s)`)
;(observerResult?.transcript ?? []).forEach(m => log(`  «${m}»`))

// Phase 3: real integration check — boot the BE server and curl it; inspect FE.
phase('Verify')

const verify = await agent(`
You are verifying whether the counter product actually works end to end. Use Bash only.

1. BE server boots:
   - Run: (cd ${WORK_DIR}/be && timeout 8 node server.js & sleep 2)  — start it in the background.
     If node server.js needs deps it will fail — note that as be_boots=false.
2. GET works:
   - curl -s -m 3 http://localhost:3001/api/count   → expect JSON like {"count":0}. Set get_ok + initial count.
3. POST works:
   - curl -s -m 3 -X POST http://localhost:3001/api/increment -H 'Content-Type: application/json' -d '{"delta":5}'
     → expect {"count":5}. Then curl GET again → expect {"count":5}. Set post_ok if the count changed correctly.
4. Kill the server: pkill -f "node server.js" || true
5. Inspect FE:
   - cat ${WORK_DIR}/fe/index.html
   - fe_ok = file exists AND has counter buttons AND references /api/count AND /api/increment.
   - Extract the port the FE points at: grep -oE 'localhost:[0-9]+' ${WORK_DIR}/fe/index.html | head -1
   - fe_port = that number (or null). be_port = 3001 (the port BE actually listens on — confirm via grep on server.js).
   - port_match = (fe_port == be_port).

Return JSON with EXACTLY these keys:
{ be_boots: bool, get_ok: bool, post_ok: bool, fe_ok: bool,
  fe_port: number|null, be_port: number|null, port_match: bool,
  notes: string }`,
  { label: 'verify-integration', model: 'haiku', schema: VERIFY_SCHEMA }
)

log(`Verify: boots=${verify?.be_boots} GET=${verify?.get_ok} POST=${verify?.post_ok} fe_ok=${verify?.fe_ok} port_match=${verify?.port_match} (fe:${verify?.fe_port} be:${verify?.be_port})`)

// Phase 4: judge against ground truth
phase('Judge')

const judgeResult = await agent(
  judgePrompt(channelId, feResult, beResult, observerResult, verify),
  { label: 'judge', model: 'haiku', schema: JUDGE_SCHEMA }
)

log(`Score: ${judgeResult?.score}/100  Success: ${judgeResult?.success}`)
log(`Friction points: ${judgeResult?.friction_points?.length ?? 0}`)
judgeResult?.friction_points?.forEach(fp =>
  log(`  [${fp.severity}] ${fp.agent}/${fp.phase}: ${fp.issue}`)
)

// Append the run to the friction log (one JSON line)
const logEntry = {
  version: 2,
  channel_id: channelId,
  fe: feResult,
  be: beResult,
  transcript: observerResult?.transcript ?? [],
  verify,
  judge: judgeResult,
}

await agent(
  `Append exactly this one JSON line (then a newline) to ${LOG_FILE}; create the file if absent.
Run this bash, with the JSON passed via stdin to avoid quoting issues:
  cat > /tmp/botbus-e2e-entry.json <<'BOTBUS_EOF'
${JSON.stringify(logEntry)}
BOTBUS_EOF
  python3 -c "import json; d=json.load(open('/tmp/botbus-e2e-entry.json')); open('${LOG_FILE}','a').write(json.dumps(d)+'\\n'); print('ok')"
Return python's output ("ok") or the error.`,
  { label: 'write-log', model: 'haiku' }
)

return judgeResult
