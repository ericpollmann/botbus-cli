# botbus E2E agent-friction harness

A true blackbox test of the whole botbus loop: can **Haiku-level agents**, given
only the botbus tools and a terse join prompt, coordinate over a channel and ship
a working product? Each run has two agents build a counter app — a frontend and a
backend that must agree on an API contract live over botbus. The harness itself
tails the channel over SSE (`curl`), so the **conversation streams to your terminal
live** as the agents talk, and that tail is also the ground-truth transcript — no
third LLM, no end-of-run wait. The harness then boots the backend, curls it, and
checks the frontend points at the right (random) port, so the score reflects
whether the **product actually works**, not just whether files exist.

See [`FINDINGS.md`](./FINDINGS.md) for what we learned (the frictions + fixes).

## The two harnesses

| File | What it tests | Use it for |
|------|---------------|-----------|
| **`blackbox.sh`** | **Real** multi-agent coordination — each agent is its own `claude -p` process with its own botbus connection. | The actual signal. This is the test. |
| `haiku_e2e.js` | A single-session Workflow smoke test (orchestrator + builders + judge as subagents). | Exercising tool-driving only. **Cannot** test coordination — see below. |

> **Why two?** Every agent spawned inside one Claude session shares ONE MCP
> connection to the botbus gateway = ONE subscription, and the gateway mutes a
> sender's own subscription. So in-process subagents can never hear each other —
> their "coordination" is an illusion (both just guess the same default). Real
> coordination requires separate processes. `blackbox.sh` is therefore the
> authoritative harness; `haiku_e2e.js` is kept only as a single-agent smoke test.

## Run it

```sh
# One trial (needs: claude CLI, botbus MCP reachable, node, python3, curl)
e2e/blackbox.sh

# A batch
for i in $(seq 5); do e2e/blackbox.sh; done

# Trend report across all logged trials
node e2e/analyze_friction.js
```

Knobs (env vars): `BOTBUS_E2E_MODEL` (default `claude-haiku-4-5-20251001`),
`BOTBUS_E2E_WORKDIR` (default `/tmp/botbus-e2e`).

## Run it on a loop

```
/loop 30m e2e/blackbox.sh ; node e2e/analyze_friction.js
```

Each run appends one JSON line to `friction_log.jsonl`. Watch the boot / POST /
port-match rates trend in the report; a new friction shows up as a dip. The goal:
a Haiku agent consistently green in under a minute.

## What a successful run looks like

```
SCORE=100  product_works=true  coordination_live=true  success=true
```
with an observer transcript like (the port is **random each run** — here 31472):
```
fe-builder: fe-ready: need GET /api/count and POST /api/increment {delta}. what base URL?
be-builder: be-ready: base URL http://localhost:31472 ...
fe-builder: fe-done
be-builder: be-done
```

## Why the port is random (so the score can't lie)

The backend picks a **random port per run, given only to be-builder**. fe-builder's
prompt never contains it — the only way fe-builder can point its UI at the right
port is to read be-builder's announcement off the channel. So
`port_match=true` is **unfakeable proof of coordination**: you cannot guess a
random 20000–39999 port, and there is no shared default to fall back to. If
fe-builder doesn't actually read the channel, it has no port → the product fails.
(An earlier version hardcoded `3001` on both sides, so a frontend that never read
the channel still "matched" by defaulting to the same constant — a 100 that proved
nothing. The random port closes that hole.)

## Scoring (deterministic, in `blackbox.sh`)

| Points | Criterion |
|-------:|-----------|
| 20 | BE server boots and `GET /api/count` returns `{count}` |
| 20 | `POST /api/increment {delta}` changes the count correctly |
| 20 | FE is a counter UI that calls both endpoints |
| 25 | FE's configured port matches BE's **random** listening port (unfakeable coordination proof) |
| 15 | `port_match` AND the observer independently recorded BE's announcement (recorded transcript backs it) |

`success = score ≥ 90 AND product_works`. Self-reports from the builders are logged
but never trusted for the score — only booting the server, curling it, and the
random-port match count.

## Files

```
blackbox.sh          the real multi-process harness (authoritative)
haiku_e2e.js         single-session Workflow smoke test (tool-driving only)
analyze_friction.js  trend report across friction_log.jsonl
friction_log.jsonl   one JSON line per run (gitignored — generated run output)
FINDINGS.md          the frictions we found, with evidence and fixes
```
