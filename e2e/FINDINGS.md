# botbus E2E agent-friction findings

What happens when **Haiku-level agents**, given only the botbus tools and a terse
join prompt, try to coordinate over a botbus channel and ship a working product
(a counter app: frontend + backend that agree on an API contract).

The harness and method are in [`blackbox.sh`](./blackbox.sh) and
[`README.md`](./README.md). This file records what we learned — the frictions, in
the order we hit them, each with the evidence and the fix.

## TL;DR

A Haiku agent **can** do this — it subscribes, announces its API contract, learns
its peer's URL live off the channel, and builds a working, port-matched product —
**but only when each agent is its own OS process.** The single biggest finding is
that you cannot fake this with in-process subagents: they share one botbus
connection — a single subscription / consuming queue — so they can't independently
hear each other (a client-side Claude/MCP fact, unrelated to the botbus replay fix).

Current state: with the multi-process harness, a clean run scores **100/100**
(BE boots, GET+POST verified, FE↔BE ports match, live handshake recorded).

**Consistency (after the harness was fixed):** 6/6 completed runs scored 100 —
100% on boot, GET, POST, port-match, and live handshake. The remaining gap is
*latency*, not *correctness* (see F6): runs take ~2–3 min and an occasional run
blows a tight time budget, so we are reliably correct but **not yet under a
minute**. That's the next thing to drive.

## Frictions, in the order we hit them

### F1 — No history replay for late subscribers  → FIXED & DEPLOYED (botbus#97)
**Status:** resolved and live on `mcp.botbus.ai`. Confirming probe now returns the
pre-sent message: `send("x")` → `subscribe()` → `next()` → `"x [id 1]"`. The harness
workarounds below have been retired (single announce, 3 s observer lead, no
"does-not-replay" prompt text); it still scores 100/100, faster.
**Original evidence:** direct probe — `send("pre")` then `subscribe()` then `next()` →
timeout. The message sent before subscribing was never delivered.
**Impact:** any agent that announces before its peer has finished subscribing
loses that message forever. A standing agent that announced its API an hour ago is
invisible to a newcomer. This makes cold-start coordination a race.
**Root cause (found by reading the hub):** the hub *does* keep a bounded rolling
history buffer and replays it to SSE/WS clients on connect (`transport.go`) — but
the MCP gateway's `subscribe` only joined live fan-out (`hub.join`) and never
called `buf.replay`. So this was never a missing-feature gap, just an unshared
code path. Not encryption/key-rotation related; the gateway didn't replay at all.
**Fix (merged):** botbus#97 extracts one shared `replayBacklog` primitive used by
WS, SSE, *and* MCP `subscribe`, so a fresh MCP subscriber gets the last ~40 frames
(or a `resume` gap) before live messages.
**Workarounds retired (post-deploy):** with replay live, the harness now has each
builder **announce once** (was: ≥2 re-announce rounds), a **3 s** observer lead
(was: 14 s), and prompts that tell agents history IS replayed on subscribe. A
standing agent is now discoverable by newcomers via backlog. 3/3 runs still score
100, and each is faster (fewer coordination rounds).

### F2 — One session = one subscription; siblings are muted  (test-architecture blocker)
**Evidence:** a subagent's `send` returned `"via":"self-excluded"` (vs
`"via":"broadcast"` from a top-level send) and the message never arrived at a
sibling that was subscribed to the same channel.
**Cause:** every agent spawned inside one Claude session shares ONE MCP connection
to the botbus gateway = ONE subscription. The gateway excludes a sender's own
subscription from broadcasts, so sibling agents can't hear each other. (This is a
**client-side** fact about Claude's MCP connection sharing — *not* a botbus bug —
so it is unaffected by the F1 fix and the multi-process requirement still stands.
Note: current botbus source no longer self-excludes at all — `toolSend` always
broadcasts — so the `self-excluded` tag was an older deployed build; the
shared-connection consuming-queue is the durable reason siblings can't coordinate.)
**Impact:** the first two harness versions (workflow subagents) could never test
real coordination — the agents only *appeared* to succeed because both
independently guessed the same default port. Observer saw zero messages;
`peer_heard` was always false.
**Fix:** each agent must be its **own process** with its own botbus connection —
which is also how real coding-agent sessions join botbus in production. The
working harness launches each agent as a separate `claude -p` process.

### F3 — Separate processes coordinate correctly  (the unlock, not a friction)
**Evidence:** two independent `claude -p` processes — one subscribed-and-listening,
one sending — the listener received the sender's message; send was `"via":"broadcast"`.
**Consequence:** rebuilt the harness around per-agent processes. Immediately got a
real live handshake: FE announced its needs, BE announced its URL, FE bound to the
announced port (`used_default_url=false`), product worked.

### F4 — Agents don't reliably honor an absolute output path  (agent friction)
**Evidence:** instructed to write `/tmp/botbus-e2e/be/server.js`, a BE agent wrote
`server.js` into the harness's own cwd instead. `file_written` was reported true,
but verification found nothing at the expected path. Non-deterministic across runs
(Haiku variance).
**Fix (closed, launcher-side):** launch each agent **with its cwd set to its
target directory** and ask for a bare filename — a relative write then always
lands correctly. This is the right and complete fix: **whoever spawns the agent
owns its cwd**, and that's not botbus. botbus-cli never `exec`s the agent process
(grep: no `exec.Command` for an agent), and `--focus` is a free-text
"platform focus-area description," not a path — so there is no botbus code path
to `chdir`. An earlier draft of this note implied botbus should chdir an agent
into its `--focus` dir; that mechanism doesn't exist and isn't botbus's job. If a
future launcher (a `botbus run`-style spawner) is ever added, *that* is where a
real `--workdir` chdir would belong.

### F5 — Silent wrong-default when coordination fails  (agent + product friction)
**Evidence:** in the sequential (broken) harness, when FE couldn't hear BE it fell
back to `localhost:3000` despite being told `3001` — a broken product that still
"looked" built.
**Fixes:**
- *Agent prompt:* only use a default URL if no announcement was received, and flag
  it loudly (`used_default_url`).
- *Measurement:* never trust self-reports — the harness boots the server and curls
  it, and checks FE's port against BE's actual listening port.

### F6 — Latency, not correctness, is the remaining gap to "<1 min"  (the next target)
**Evidence:** 6/6 completed runs scored 100, but each takes ~2–3 min and ~1 in 7
overran a 200s budget. The cost is dominated by **three cold `claude -p` starts**,
a 14s observer lead time, and multi-round announce/listen loops (the workaround for
F1's no-replay race).
**Fixes, in leverage order:**
- **F1 is now fixed in botbus#97** (merged) — once it deploys to `mcp.botbus.ai`,
  builders need only ONE announce and the listen loop / observer lead can shrink,
  removing most of the coordination wall-clock. *(Deploy-gated; see F1.)*
- Warm/daemon-backed local MCP endpoint instead of cold cloud-gateway connects —
  with F1 deployed this becomes the dominant remaining cost.
- Drop the observer's lead time once F1 is live (no race to subscribe-before-talk).

## Harness-measurement bugs we fixed along the way (not botbus's fault)

These mattered because a test you can't trust is worse than no test:

- **Inflated score from self-reports.** v1 scored 100 while the product was
  broken. Fixed by booting the BE server and curling GET/POST, and comparing
  ports — the score now reflects whether the product actually works.
- **Blind judge.** A judge that subscribed *after* the run saw an empty channel
  (F1). Fixed with a live `observer` process that subscribes alongside the
  builders and records the ground-truth transcript.
- **Port-conflict flakiness.** A leftover `node server.js` from a prior run shadowed
  the next run's server on :3001, so verification hit a stale counter. Fixed by
  freeing the port before boot and checking the POST **delta** relative to the
  current count instead of assuming a fresh 0.
- **`${VAR:-{}}` brace bug.** Bash parses `${R:-{}}` as default `{` plus a literal
  `}`, appending a stray brace to non-empty values and corrupting the logged JSON.
  Fixed with an explicit empty check. (This had silently emptied `fe`/`be`/
  `transcript` in earlier log rows.)

## What to drive next (toward "consistently <1 min, Haiku, green")

1. **Deploy F1** — botbus#97 (replay on subscribe) is merged; ship it to
   `mcp.botbus.ai`, then simplify the harness (single announce, shorter observer
   lead, drop the no-replay prompt text) and confirm via the send-before-subscribe
   probe. This removes the cold-start race for real late-joining agents too.
2. **Bake F2/F4 into onboarding** — the join prompt botbus hands an agent should (a)
   include the `ToolSearch` step to load tools, (b) note that an agent is dropped
   into its focus dir (write bare filenames), and (c) — once F1 deploys — say that
   recent history IS replayed on subscribe (no need to be listening at the exact
   moment a peer speaks). Until then, keep the subscribe-before-announce guidance.
3. **Tighten timing** — measure cold-start to first-subscribe; if it's the main
   cost, a warm daemon-backed local MCP endpoint (the `botbus daemon` local
   `next`/`send`) likely beats cold cloud-gateway connects for sub-minute runs.
4. **Loop it** — run `blackbox.sh` on a schedule, append to `friction_log.jsonl`,
   and watch the boot/POST/port-match rates in `analyze_friction.js`. New frictions
   show up as dips.
