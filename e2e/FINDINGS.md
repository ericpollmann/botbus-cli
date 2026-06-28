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
connection and the gateway mutes their own line, so they never hear each other.

Current state: with the multi-process harness, a clean run scores **100/100**
(BE boots, GET+POST verified, FE↔BE ports match, live handshake recorded).

## Frictions, in the order we hit them

### F1 — No history replay for late subscribers  (botbus product friction)
**Evidence:** direct probe — `send("pre")` then `subscribe()` then `next()` →
timeout. The message sent before subscribing is never delivered.
**Impact:** any agent that announces before its peer has finished subscribing
loses that message forever. A standing agent that announced its API an hour ago is
invisible to a newcomer. This makes cold-start coordination a race.
**Fixes:**
- *Product:* replay a small bounded history to a fresh subscriber (the hub already
  keeps a rolling buffer per the README — surface it on the MCP `subscribe`/`next`
  path, e.g. an optional `replay_last: N`).
- *Workaround used in harness:* agents **re-announce** for ≥2 rounds so a
  late-joining peer still catches a later announcement.

### F2 — One session = one subscription; siblings are muted  (test-architecture blocker)
**Evidence:** a subagent's `send` returned `"via":"self-excluded"` (vs
`"via":"broadcast"` from a top-level send) and the message never arrived at a
sibling that was subscribed to the same channel.
**Cause:** every agent spawned inside one Claude session shares ONE MCP connection
to the botbus gateway = ONE subscription. The gateway excludes a sender's own
subscription from broadcasts, so sibling agents can't hear each other.
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
**Fixes:**
- *Harness:* launch each agent **with its cwd set to its target directory**, and
  ask for a bare filename. A relative write then always lands correctly. This
  mirrors what botbus itself should do: `chdir` an agent into its `--focus`
  directory rather than trusting it to write to the right place.

### F5 — Silent wrong-default when coordination fails  (agent + product friction)
**Evidence:** in the sequential (broken) harness, when FE couldn't hear BE it fell
back to `localhost:3000` despite being told `3001` — a broken product that still
"looked" built.
**Fixes:**
- *Agent prompt:* only use a default URL if no announcement was received, and flag
  it loudly (`used_default_url`).
- *Measurement:* never trust self-reports — the harness boots the server and curls
  it, and checks FE's port against BE's actual listening port.

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

1. **Land F1 in botbus** — bounded replay on subscribe. This is the highest-leverage
   product change: it removes the cold-start race that the harness currently works
   around with re-announce loops, and it directly helps real late-joining agents.
2. **Bake F2/F4 into onboarding** — the join prompt botbus hands an agent should (a)
   include the `ToolSearch` step to load tools, (b) state that history isn't
   replayed so subscribe-before-announce, and (c) be delivered with the agent
   already chdir'd into its focus dir.
3. **Tighten timing** — measure cold-start to first-subscribe; if it's the main
   cost, a warm daemon-backed local MCP endpoint (the `botbus daemon` local
   `next`/`send`) likely beats cold cloud-gateway connects for sub-minute runs.
4. **Loop it** — run `blackbox.sh` on a schedule, append to `friction_log.jsonl`,
   and watch the boot/POST/port-match rates in `analyze_friction.js`. New frictions
   show up as dips.
