# Self-documenting onboarding wizard — design

- **Date:** 2026-06-23
- **Status:** Approved. Operator-identity decision resolved → **Model A** (operator's
  root *is* the workspace org-root); revisit symmetric Model B only if issues arise.
- **Repo:** `botbus-cli` (the wizard is entirely client-side; the hub needs no new endpoints).
- **Parent designs:** `botbus/docs/superpowers/specs/2026-06-23-fleet-platform-design.md`, `…/2026-06-23-hierarchical-aggregation.md`.

## 1. Goal

Make `botbus` self-documenting. Today a new user faces a bare name+framing prompt
(`firstRunOps`) and is then on their own to discover `workspace create`, `invite`,
agent creation, directives, and the docs URLs. Replace that with a **guided wizard**
that walks the operator through the whole loop — name a workspace, connect their
Claude Code session, set a directive, invite teammates, add a standing agent — and
then drops them into a **live board** so they *watch* coordination docs appear.

The wizard teaches by doing: every step runs the real command and shows the real
artifact (a paste-into-Claude prompt, a join URL, a live task card), so the user
finishes onboarded *and* knowing how the system works.

## 2. Entry point & re-runnability

- **Bare `botbus` on an unconfigured machine** runs the wizard. This **replaces**
  `firstRunOps`' plain name+framing prompt (per "No Legacy / Compat Code" — the old
  prompt is deleted, not kept alongside).
- **`botbus onboard`** re-runs the wizard anytime — for re-onboarding, demoing, or
  adding a workspace later. Re-running is **idempotent**: an existing workspace/root
  is reused (offered as the default), not duplicated.
- After configuration, bare `botbus` continues to open the normal roster console
  (unchanged). The inline `o`-onboard shortcut on the roster stays, and now shares
  the wizard's "add agent" step logic.

## 3. The flow

Steps 1–5 are imperative guided prompts (matching the existing `firstRunOps`
readLine style — and required because the runtime is rebuilt after the root is
created in step 1); **step 6, the live board, is bubbletea** (where model-update→
auto-refresh matters). Each step runs the real action and shows its real output
before advancing. `esc`/empty skips an optional step; `ctrl+c` quits. The steps:

1. **Name your workspace** → `workspaceCreate(name)` mints the org-root anchor and
   sets it active (`setActiveWorkspace`). Shows the workspace channel URL.
2. **Connect this session** → prints the **paste-into-Claude prompt** for the
   operator's root (primary) *and* the raw terminal `claude mcp add …` command +
   channel URL (fallback). This adds the **local** botbus MCP to the operator's
   Claude Code session so Claude can drive the bus. (§5.)
3. **Set a directive** (optional, skippable) → sets the root's `Focus` (the
   workspace directive, surfaced in every agent's briefing).
4. **Invite teammates** (repeatable loop, skippable) → each `workspaceInvite(user)`
   prints that teammate's **join URL** + a short teammate-setup paste-prompt to send
   them. "Add another / done."
5. **Add a standing agent** (optional, skippable) → `onboardChildOps(name, focus)`
   creates a child role and prints *its* local-MCP paste-prompt (for a Claude Code
   session that will *be* that agent on this machine).
6. **Watch it live** → drops into the **live board** (§6): polls the workspace
   `/board` JSON on a tick and redraws. The wizard posts one sample
   `task.started` event so the user literally watches a card appear, then leaves
   them in the live console.

Every step is skippable except naming the workspace; the minimum path is "name it →
connect → live board." The wizard never blocks on a teammate or agent being added.

## 4. Operator-identity model (open decision)

How does the human operator relate to the workspace they create?

- **Model A (recommended) — operator's root *is* the workspace org-root.** "Name your
  workspace" names the operator's own root (`Parent==""`); it doubles as the
  workspace anchor. Teammates (`invite`) and agents become descendants. The operator's
  `/board` = the whole-workspace board. This is the minimal realization: it reuses
  `firstRunOps`' single-root model almost unchanged (the root just gets a name = the
  workspace name, plus a directive), and matches Eric's earlier "Hybrid — root takes
  both" / "single root that handles everything including spinning up child agents."
  Trade-off: there's no separate "operator-only" docs view distinct from the whole
  workspace — acceptable, since the operator *is* the coordinator.

- **Model B — workspace org-root is a pure anchor; operator is the first user under
  it.** Every human is uniformly a user-root under the anchor; the anchor aggregates
  all users. Gives a clean three-level view (anchor=workspace, user=one human,
  agent=leaf) matching `hierarchical-aggregation.md`. Trade-off: the anchor is a node
  nobody sessions as, and the wizard does two creates (anchor + operator-user) on
  first run.

**Recommendation: Model A.** It's the lazy-senior-dev minimum that's still correct,
and the three-level aggregation still holds for teammates (workspace board includes
the operator's items; teammate-root = that teammate; agent = leaf). If per-creator
separation is ever wanted, Model B is a later refactor, not a rewrite. **This is the
one decision to confirm at spec review.**

## 5. Component: paste-prompt generator

A pure function, unit-tested independently:

```go
// pastePrompt builds the ready-to-paste Claude Code prompt for an identity.
// inst carries the local MCP command + channel URL from ops.CreateChild /
// the operator's root connect instructions.
func pastePrompt(name, role string, inst daemon.ConnectInstructions) string
```

Two shapes, because the MCP is **local per-CLI** (the operator's `botbus` hosts the
MCP on `127.0.0.1:<port>/a/<key>`; that URL only works on this machine):

- **Local identity** (operator's own session in step 2; standing agents in step 5):
  the prompt tells Claude to run `inst.MCPCommand` (adds the local botbus MCP), then
  read its briefing at `inst.ChannelURL` and coordinate — post `task.started/blocked/
  done` to its channel, watch the team board at the workspace root.
- **Teammate invite** (step 4, a human on another machine): no local MCP URL applies,
  so the output is the **join URL** (`https://<inbox>.botbus.ai/?user=<user>` — their
  credential) plus a one-line setup prompt: install botbus, run it, connect with this
  URL. Built by `workspaceInvite` (already returns the join URL).

The prompt text is deliberately short and copy-pasteable; the briefing (served by the
hub on connect, per HA-2) carries the full role context, so the paste-prompt doesn't
duplicate it.

## 6. Component: live board

A bubbletea sub-model embedded as the wizard's final step (and reusable on its own):

```go
type liveBoardModel struct { url string; board BoardView; err error }
// tick (tea.Tick ~2s) → fetchBoard(ctx, url) → bubbletea redraws the new model.
func fetchBoard(ctx context.Context, boardURL string) (BoardView, error) // GET <url>, decode JSON
```

- **Data source:** the workspace org-root's `/board` JSON
  (`https://<org-root-inbox>.botbus.ai/board`). `/board` carries the actual task
  cards (InProgress / Blocked / Done lists) — the richer "cards popping in" view —
  whereas `/docs` is only counts. The hub already aggregates across the subtree
  (`eventsForSubtree`), so hitting the org-root's `/board` shows the **whole
  workspace**.
- **Refresh:** a `tea.Tick` (~2s) issues `fetchBoard` as a `tea.Cmd`; its result
  message updates the model; `View` re-renders automatically. No manual redraw —
  exactly the bubbletea model-update→repaint Eric described. A failed fetch sets
  `err` and shows a muted "reconnecting…" line; the next tick retries.
- **Demo seed:** the wizard posts one `task.started` to the operator's channel before
  entering this step, so the first paint already shows a card (proof the loop works),
  then live updates flow as agents post.
- **Upgrade path (non-goal now):** replace the poll with a WS subscription that
  triggers a refetch on each event for instant updates. The ~2s poll is fine at human
  cadence; revisit only if it ever feels laggy.

The `BoardView` shape mirrors the hub's `projectBoard` output (InProgress/Blocked/
Done item lists). If a shared `wire`/proto type for it exists it's reused; otherwise a
small local struct matching the JSON is defined in the CLI (the hub's `/board` JSON is
the contract).

## 7. Component: wizard orchestrator

`wizard.go` — the bubbletea step state machine. It owns no business logic; it calls
the existing functions and renders their results:

- step 1 → `workspaceCreate` + `setActiveWorkspace` (from `workspace.go` / agentstate)
- step 2 → operator root connect instructions + `pastePrompt`
- step 3 → set root `Focus` (reuse the `agent update --focus` logic / hostagent.Update)
- step 4 → `workspaceInvite` (loop) + `pastePrompt` (teammate shape)
- step 5 → `onboardChildOps` + `pastePrompt` (local shape)
- step 6 → post sample `task.started`, hand off to `liveBoardModel`

Logic functions take a `hostagent.Deps` / `daemon.Ops` so the wizard is testable with
fakes and a temp state path (the established pattern; **never touches `~/.botbus`**).

## 8. Data flow

```
botbus (unconfigured)  ─▶ wizard model
  step1 ─▶ workspaceCreate ─▶ org-root minted (hostagent.Create) ─▶ agentstate + profile + active ws
  step2 ─▶ connect instructions (local MCP) ─▶ pastePrompt ─▶ shown
  step3 ─▶ hostagent.Update(root, focus)
  step4 ─▶ workspaceInvite(user) ─▶ join URL ─▶ pastePrompt(teammate) ─▶ shown   (loop)
  step5 ─▶ onboardChildOps(name, focus) ─▶ child + local MCP ─▶ pastePrompt ─▶ shown
  step6 ─▶ POST task.started ─▶ liveBoardModel ─▶ tick→GET /board JSON→redraw
```

All hub calls are existing endpoints (mint/register via control, post via the channel,
`GET /board` JSON). No hub changes.

## 9. Error handling

- **Router/hub unreachable** at step 1 (mint/register fails): show the error, let the
  operator retry the step; don't half-persist (mirror `workspaceCreate`'s behavior).
- **Live board fetch fails:** muted "reconnecting…" line, retry next tick; never crash
  the TUI.
- **`esc` mid-step:** abort that step (back a level / skip), not the whole wizard;
  `ctrl+c` quits (matches the existing `updateOnboard` contract).
- **Re-run with existing workspace:** detect via agentstate/profile; offer the existing
  workspace as the default and skip re-minting.

## 10. Testing

- `pastePrompt` — pure, table-tested: local shape contains the MCP command + channel
  URL + role; teammate shape contains the join URL + `?user=`; url-escaping preserved.
- `fetchBoard` — against an `httptest` server returning canned `/board` JSON; asserts
  decode + error path (non-200, bad JSON). Never hits the live hub.
- Wizard step machine — drive `Update` with `tea.KeyMsg` through the steps with stubbed
  ops (mirroring `console_test.go`'s `stubConsoleOps`): asserts each step calls the
  right logic fn with the right args and advances; `esc`/`ctrl+c` contracts hold.
- `liveBoardModel` — a tick message triggers a fetch cmd; a board-result message
  updates the model; `View` renders cards; an error message shows the reconnect line.
- All tests use fakes + `t.TempDir()` state paths; **no `~/.botbus` writes, no live
  network** (HARD RULE).

## 11. Non-goals

- No hub changes (no new endpoints; reuse `/board`, briefing, control mint/register).
- No WS-driven live board yet (poll `/board`; WS refetch is a later upgrade).
- No cloud MCP gateway (the MCP is local per-CLI; that's the actual architecture).
- No caching of the aggregation (the hub already handles `/board`; revisit on a real
  profile, per the Phase-3 lesson).
- No multi-workspace switching UI beyond what `workspace use` already provides.

## 12. Build order

1. **`pastePrompt`** (pure + tests) — the smallest reusable piece; both shapes.
2. **`fetchBoard` + `liveBoardModel`** (tick→fetch→redraw + tests) — the live board in
   isolation, runnable standalone.
3. **`wizard.go` step machine** wiring steps 1–5 to existing logic fns (+ stubbed-ops
   tests).
4. **Step 6 integration** — sample `task.started` seed + hand off to `liveBoardModel`.
5. **Entry wiring** — bare-`botbus` first-run → wizard (delete `firstRunOps`' plain
   prompt path); add `botbus onboard` subcommand + `main.go` dispatch.
6. **Docs** — README: "first run walks you through everything," and the
   `botbus onboard` re-run.
