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
  description: 'E2E: Haiku agents coordinate via botbus to build a counter app (FE + BE)',
  phases: [
    { title: 'Setup', detail: 'Mint channel, post tasks via orchestrator' },
    { title: 'FE', detail: 'FE agent joins, announces API contract, builds frontend' },
    { title: 'BE', detail: 'BE agent joins, confirms API contract, builds backend' },
    { title: 'Judge', detail: 'Score success, measure friction, write log' },
  ],
}

// ── Constants ──────────────────────────────────────────────────────────────

const WORK_DIR = '/tmp/botbus-e2e'
const LOG_FILE = '/home/user/botbus-cli/e2e/friction_log.jsonl'

// The minimal "join" prompt that botbus should ideally be able to give any agent.
// Intentionally terse — friction comes from what's missing or ambiguous here.
const JOIN_PREAMBLE = `You are a coding agent joining a botbus coordination channel.

STEP 0 — load tools:
Call ToolSearch with query "select:mcp__botbus__set_name,mcp__botbus__subscribe,mcp__botbus__send,mcp__botbus__next"
You must do this before calling any botbus tool.

STEP 1 — identify yourself:
Call mcp__botbus__set_name with name "AGENT_NAME"

STEP 2 — join the channel:
Call mcp__botbus__subscribe with channel "CHANNEL_ID"

STEP 3 — receive your task:
Call mcp__botbus__next (channel: "CHANNEL_ID", timeout_seconds: 60).
Your task message starts with the prefix "task:AGENT_ROLE ".
If the first message has a different prefix, call next() again (up to 5 times total).

STEP 4 — coordinate:
Read the task. Then call mcp__botbus__send to announce what you need from the other agent
(e.g. what API endpoints you require, or what API you will expose).

STEP 5 — build:
Write your code artifact to disk using Bash.

STEP 6 — announce done:
Call mcp__botbus__send with text "DONE: <absolute path to your file>"

Return a JSON object with keys:
  subscribed (bool), task_received (bool), announced (bool),
  file_written (bool), file_path (string or null), friction ([string])
`

// ── Helpers ────────────────────────────────────────────────────────────────

function fePrompt(channelId) {
  return JOIN_PREAMBLE
    .replace('AGENT_NAME', 'fe-builder')
    .replace(/CHANNEL_ID/g, channelId)
    .replace('AGENT_ROLE', 'fe') +
`
YOUR ROLE: Frontend Builder

Your task (it will arrive labelled "task:fe"):
  Build a self-contained counter app at ${WORK_DIR}/fe/index.html
  - Vanilla HTML/CSS/JS, no bundler, no npm
  - Shows a counter starting at 0
  - Three buttons: +1, -1, Reset
  - Reads initial count from GET /api/count → {count: N}
  - Sends updates via POST /api/increment with body {delta: 1 or -1}
  - Replace API_BASE_URL placeholder with the URL the BE agent announces

In STEP 4, announce: "api-needs: GET /api/count → {count}, POST /api/increment {delta} → {count}"
After you announce, call mcp__botbus__next once more (timeout_seconds: 30) to see if the BE agent
confirmed. Include whatever they said in your friction log.

Build the file even if you don't hear back from BE within the timeout.
`
}

function bePrompt(channelId, feApiAnnouncement) {
  return JOIN_PREAMBLE
    .replace('AGENT_NAME', 'be-builder')
    .replace(/CHANNEL_ID/g, channelId)
    .replace('AGENT_ROLE', 'be') +
`
YOUR ROLE: Backend Builder

${feApiAnnouncement ? `The FE agent has already announced their API needs:
"${feApiAnnouncement}"
` : 'The FE agent may have already announced their API needs — check the channel.'}

Your task (it will arrive labelled "task:be"):
  Build an Express.js counter server at ${WORK_DIR}/be/server.js
  - Port 3001
  - In-memory counter starting at 0
  - GET  /api/count       → { count: N }
  - POST /api/increment   body { delta: N } → { count: N }  (delta can be negative)
  - Add CORS headers so the frontend can call it
  - Do NOT require npm install — write the file so it works with: node server.js
    (assume express is available globally or write it so it can self-install)

In STEP 4, confirm: "api-confirmed: I will implement GET /api/count and POST /api/increment on port 3001"

Build the file. Server URL will be: http://localhost:3001
`
}

function judgePrompt(channelId, feResult, beResult) {
  return `You are evaluating a botbus E2E agent test run.

Channel: ${channelId}

FE agent reported: ${JSON.stringify(feResult)}
BE agent reported: ${JSON.stringify(beResult)}

YOUR TASKS:

1. Load tools: ToolSearch "select:mcp__botbus__subscribe,mcp__botbus__next"
2. Subscribe to channel "${channelId}" and drain remaining messages:
   call mcp__botbus__next up to 30 times (timeout_seconds: 5 each), stop when status is "timeout"
   Collect all message bodies.
3. Check files with Bash:
   - cat ${WORK_DIR}/fe/index.html  (note: does it exist? does it have the counter UI? does it reference /api/count?)
   - cat ${WORK_DIR}/be/server.js   (note: does it exist? does it have GET /api/count and POST /api/increment?)
4. Score the run 0–100:
   - +20 if FE file exists and is valid HTML with a counter
   - +20 if FE calls /api/count and /api/increment
   - +20 if BE file exists and has both endpoints
   - +20 if the agents exchanged at least one coordination message on the channel
   - +10 if both agents reported task_received=true
   - +10 if both agents reported announced=true

5. Identify every friction point. Examples of what counts:
   - Agent failed to load tools (ToolSearch not called or called wrong)
   - Agent called subscribe but never called next
   - Agent's next() returned timeout immediately (not subscribed before messages arrived)
   - Task message not found (consumed by wrong agent)
   - File not written to expected path
   - Agent wrote file but path wrong
   - Coordination messages missing or garbled
   - Agent did not announce DONE

Return JSON matching the schema below. Be specific in friction_points — include the exact issue
and a one-sentence fix suggestion.
`
}

// ── Result schema ──────────────────────────────────────────────────────────

const AGENT_SCHEMA = {
  type: 'object',
  properties: {
    subscribed: { type: 'boolean' },
    task_received: { type: 'boolean' },
    announced: { type: 'boolean' },
    file_written: { type: 'boolean' },
    file_path: { type: ['string', 'null'] },
    friction: { type: 'array', items: { type: 'string' } },
  },
  required: ['subscribed', 'task_received', 'announced', 'file_written', 'file_path', 'friction'],
}

const JUDGE_SCHEMA = {
  type: 'object',
  required: ['success', 'score', 'fe_file_ok', 'be_file_ok', 'coordination_ok', 'friction_points', 'suggestions'],
  properties: {
    success: { type: 'boolean' },
    score: { type: 'number' },
    fe_file_ok: { type: 'boolean' },
    be_file_ok: { type: 'boolean' },
    coordination_ok: { type: 'boolean' },
    channel_messages: { type: 'array', items: { type: 'string' } },
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

// Phase 1: Create channel and post tasks
phase('Setup')

await agent(
  `Run bash command: mkdir -p ${WORK_DIR}/fe ${WORK_DIR}/be`,
  { label: 'mkdir', model: 'haiku' }
)

const setupResult = await agent(`
You are the test orchestrator. Do this in order:

1. Call ToolSearch with query "select:mcp__botbus__new_channel,mcp__botbus__set_name,mcp__botbus__subscribe,mcp__botbus__send"

2. Call mcp__botbus__set_name with name "orchestrator"

3. Call mcp__botbus__new_channel (no parameters). Extract the channel_id from the response.

4. Call mcp__botbus__subscribe with channel = that channel_id.

5. Call mcp__botbus__send twice, in order:
   First:  channel=<channel_id>, text="task:fe Build a vanilla JS counter app at ${WORK_DIR}/fe/index.html. Use API base http://localhost:3001. Call GET /api/count to load initial count. Call POST /api/increment with body {delta:1} or {delta:-1} to change it. Three buttons: +1, -1, Reset. Announce your api-needs on this channel before building."
   Second: channel=<channel_id>, text="task:be Build an Express.js counter server at ${WORK_DIR}/be/server.js on port 3001. GET /api/count returns {count:N}. POST /api/increment with body {delta:N} updates and returns {count:N}. Add CORS. Confirm the FE agent api-needs after you see them."

6. Return ONLY the channel_id string. Nothing else. No explanation.
`,
  { label: 'setup-orchestrator', model: 'haiku' }
)

const channelId = setupResult.trim().split('\n').pop().trim()
log(`Channel: ${channelId}`)

if (!channelId || channelId.length < 8) {
  log('ERROR: could not extract channel ID from setup. Aborting.')
  return { success: false, score: 0, error: 'channel_id_extraction_failed', raw: setupResult }
}

// Phase 2: FE agent
phase('FE')

const feResult = await agent(
  fePrompt(channelId),
  { label: 'fe-builder', model: 'haiku', schema: AGENT_SCHEMA }
)

log(`FE: subscribed=${feResult?.subscribed} task=${feResult?.task_received} file=${feResult?.file_written} path=${feResult?.file_path}`)
if (feResult?.friction?.length) log(`FE friction: ${feResult.friction.join(' | ')}`)

// Phase 3: BE agent — pass FE's API announcement as context to reduce ambiguity
phase('BE')

const feAnnouncement = feResult?.announced
  ? `FE agent (fe-builder) announced their API needs on the channel.`
  : null

const beResult = await agent(
  bePrompt(channelId, feAnnouncement),
  { label: 'be-builder', model: 'haiku', schema: AGENT_SCHEMA }
)

log(`BE: subscribed=${beResult?.subscribed} task=${beResult?.task_received} file=${beResult?.file_written} path=${beResult?.file_path}`)
if (beResult?.friction?.length) log(`BE friction: ${beResult.friction.join(' | ')}`)

// Phase 4: Judge
phase('Judge')

const judgeResult = await agent(
  judgePrompt(channelId, feResult, beResult),
  { label: 'judge', model: 'haiku', schema: JUDGE_SCHEMA }
)

log(`Score: ${judgeResult?.score}/100  Success: ${judgeResult?.success}`)
log(`Friction points: ${judgeResult?.friction_points?.length ?? 0}`)
judgeResult?.friction_points?.forEach(fp =>
  log(`  [${fp.severity}] ${fp.agent}/${fp.phase}: ${fp.issue}`)
)

// Write friction log entry
const logEntry = {
  channel_id: channelId,
  fe: feResult,
  be: beResult,
  judge: judgeResult,
}

await agent(
  `Append exactly this JSON line (no newlines inside, one trailing newline) to ${LOG_FILE}.
Create the file if it doesn't exist.
Use this bash command:
  printf '%s\\n' '${JSON.stringify(logEntry).replace(/\\/g, '\\\\').replace(/'/g, "'\\''")}' >> ${LOG_FILE}

Then run: tail -1 ${LOG_FILE} | python3 -c "import json,sys; json.load(sys.stdin); print('log entry valid')"
Return "ok" if it worked, or the error message if it failed.`,
  { label: 'write-log', model: 'haiku' }
)

return judgeResult
