# Self-Documenting Onboarding Wizard Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace `botbus`' bare name+framing first-run with a guided wizard that names a workspace, prints a paste-into-Claude connect prompt, sets a directive, invites teammates, adds a standing agent, then drops into a live task board — teaching the system by doing.

**Architecture:** All client-side in `botbus-cli` (no hub changes). Steps 1–5 are imperative guided prompts (matching the existing `firstRunOps` readLine style; required because the runtime is rebuilt after the workspace root is created). Step 6, the live board, is a bubbletea model that polls the workspace `/board` JSON on a tick and auto-redraws. Each new piece is a small pure/testable function; the wizard orchestrates existing logic (`hostagent.Create/Update`, `workspaceInvite`, `onboardChildOps`, `setActiveWorkspace`). Operator-identity is **Model A**: the operator's root *is* the workspace org-root.

**Tech Stack:** Go 1.25, `github.com/charmbracelet/bubbletea` (TUI), stdlib `net/http`/`encoding/json`. Existing fabric packages: `fabric/daemon`, `fabric/hostagent`, `fabric/agentstate`, `fabric/profile`.

## Global Constraints

- **No `~/.botbus` writes in tests.** Every test uses `t.TempDir()` paths and `httptest`/fakes — never the real profile/state files or the live hub/router. (HARD RULE.)
- **No legacy/compat code.** `firstRunOps`' plain name+framing prompt is *replaced* by the wizard and deleted in the same change as its callers/tests (Task 7), not kept alongside.
- **Local MCP, not a cloud gateway.** Connect instructions are the operator's local CLI MCP endpoint: `claude mcp add --transport http <name> http://<rt.Addr()>/a/<key>`.
- **Domain constant:** `domain = "botbus.ai"` (package `main`, `cmd/botbus/main.go:43`).
- **Frame format for events:** a channel frame is `"<sender>: <body>"`; an event body is JSON `{"v":1,"type":"task.started","task":"...","title":"...","by":"..."}` (schema in `botbus/events`). Posting = HTTP POST that frame to the channel origin `https://<inbox>.botbus.ai/`.
- **`/board` JSON shape:** `{"in_progress":[…],"blocked":[…],"done":[…]}` where each card is `{"task","title","status","note","ref","by","updated_ts"}`. A non-browser User-Agent (the CLI's `botbus-cli/…`) gets JSON; browsers get HTML.
- **Conventional Commits**, one feature branch (`feat/onboarding-wizard`, already created at `/tmp/botbus-cli-onboard`), PR to `main`, never commit to `main`.
- **Gate before each commit:** `go build ./... && go vet ./... && go test -race ./... && gofmt -l cmd/botbus` (no output from gofmt).

---

### Task 1: Paste-prompt generator

**Files:**
- Create: `cmd/botbus/pasteprompt.go`
- Test: `cmd/botbus/pasteprompt_test.go`

**Interfaces:**
- Consumes: `daemon.ConnectInstructions{MCPCommand, MCPEndpoint, ChannelURL string}` (from `fabric/daemon/ops.go`).
- Produces:
  - `func localPastePrompt(name, role string, inst daemon.ConnectInstructions) string`
  - `func invitePastePrompt(user, joinURL string) string`

- [ ] **Step 1: Write the failing test**

```go
package main

import (
	"strings"
	"testing"

	"github.com/ericpollmann/botbus-cli/fabric/daemon"
)

func TestLocalPastePromptContainsMCPAndChannel(t *testing.T) {
	inst := daemon.ConnectInstructions{
		MCPCommand: "claude mcp add --transport http mythwork http://127.0.0.1:8765/a/k1",
		ChannelURL: "https://chan.botbus.ai/",
	}
	got := localPastePrompt("mythwork", "workspace owner", inst)
	for _, want := range []string{inst.MCPCommand, inst.ChannelURL, "mythwork", "workspace owner"} {
		if !strings.Contains(got, want) {
			t.Fatalf("localPastePrompt missing %q in:\n%s", want, got)
		}
	}
}

func TestLocalPastePromptDefaultsRole(t *testing.T) {
	got := localPastePrompt("agentx", "", daemon.ConnectInstructions{MCPCommand: "x", ChannelURL: "y"})
	if !strings.Contains(got, "agentx") || strings.Contains(got, "()") {
		t.Fatalf("empty role should yield a sensible default, got:\n%s", got)
	}
}

func TestInvitePastePromptContainsJoinURLAndUser(t *testing.T) {
	got := invitePastePrompt("ethan", "https://abc.botbus.ai/?user=ethan")
	for _, want := range []string{"ethan", "https://abc.botbus.ai/?user=ethan"} {
		if !strings.Contains(got, want) {
			t.Fatalf("invitePastePrompt missing %q in:\n%s", want, got)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/botbus/ -run TestLocalPastePrompt -v`
Expected: FAIL — `undefined: localPastePrompt`.

- [ ] **Step 3: Write minimal implementation**

```go
package main

// pasteprompt.go — builds the ready-to-paste Claude Code prompts the onboarding
// wizard prints. localPastePrompt is for identities reachable via the operator's
// LOCAL botbus MCP (the operator's own session, standing agents on this machine);
// invitePastePrompt is the message to send a teammate on another machine (their
// join URL is their credential).

import (
	"fmt"

	"github.com/ericpollmann/botbus-cli/fabric/daemon"
)

func localPastePrompt(name, role string, inst daemon.ConnectInstructions) string {
	if role == "" {
		role = "an agent"
	}
	return fmt.Sprintf(`Paste into a Claude Code session to make it "%s" (%s) on botbus:

1. Connect: %s
2. Read your briefing: %s
3. Follow it. Post status (task.started / task.blocked / task.done) to your
   channel and watch the team board at the workspace root.`,
		name, role, inst.MCPCommand, inst.ChannelURL)
}

func invitePastePrompt(user, joinURL string) string {
	return fmt.Sprintf(`Send this to %s — the URL is their credential to join:

  %s

They install botbus (go install github.com/ericpollmann/botbus-cli/cmd/botbus@latest)
and run:  botbus attach %s`,
		user, joinURL, joinURL)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/botbus/ -run "TestLocalPastePrompt|TestInvitePastePrompt" -v`
Expected: PASS (all three).

- [ ] **Step 5: Commit**

```bash
git add cmd/botbus/pasteprompt.go cmd/botbus/pasteprompt_test.go
git commit -m "feat(cli): paste-into-Claude prompt generators for onboarding"
```

---

### Task 2: Board fetch (`/board` JSON client)

**Files:**
- Create: `cmd/botbus/board_live.go`
- Test: `cmd/botbus/board_live_test.go`

**Interfaces:**
- Consumes: `userAgent()` (from `cmd/botbus/main.go`) — the CLI UA so the hub returns JSON not HTML.
- Produces:
  - `type boardCard struct { Task, Title, Note, By string }` (JSON-tagged)
  - `type boardView struct { InProgress, Blocked, Done []boardCard }` (JSON-tagged)
  - `func fetchBoard(ctx context.Context, channelURL string) (boardView, error)` — GETs `<channelURL>/board`.

- [ ] **Step 1: Write the failing test**

```go
package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchBoardDecodesColumns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/board" {
			t.Errorf("expected /board, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"in_progress":[{"task":"t1","title":"Build it","by":"eric"}],"blocked":[],"done":[{"task":"t0","title":"Done thing","by":"bot"}]}`))
	}))
	defer srv.Close()

	b, err := fetchBoard(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("fetchBoard: %v", err)
	}
	if len(b.InProgress) != 1 || b.InProgress[0].Title != "Build it" || b.InProgress[0].By != "eric" {
		t.Fatalf("in_progress not decoded: %+v", b.InProgress)
	}
	if len(b.Done) != 1 || b.Done[0].Title != "Done thing" {
		t.Fatalf("done not decoded: %+v", b.Done)
	}
}

func TestFetchBoardErrorsOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()
	if _, err := fetchBoard(context.Background(), srv.URL); err == nil {
		t.Fatal("expected error on HTTP 500")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/botbus/ -run TestFetchBoard -v`
Expected: FAIL — `undefined: fetchBoard`.

- [ ] **Step 3: Write minimal implementation**

```go
package main

// board_live.go — the live task board the onboarding wizard ends in. fetchBoard
// GETs a channel's /board JSON (the hub aggregates the whole subtree, so the
// workspace org-root's board is the whole-workspace view); liveBoardModel (Task 3)
// polls it on a tick and bubbletea redraws.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// boardCard mirrors the fields of the hub's /board card the live view renders.
type boardCard struct {
	Task  string `json:"task"`
	Title string `json:"title"`
	Note  string `json:"note"`
	By    string `json:"by"`
}

// boardView mirrors the hub's /board JSON: three status columns of cards.
type boardView struct {
	InProgress []boardCard `json:"in_progress"`
	Blocked    []boardCard `json:"blocked"`
	Done       []boardCard `json:"done"`
}

// fetchBoard GETs <channelURL>/board and decodes the JSON board. The CLI
// User-Agent ensures the hub returns JSON (browsers get HTML).
func fetchBoard(ctx context.Context, channelURL string) (boardView, error) {
	u := strings.TrimRight(channelURL, "/") + "/board"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return boardView{}, err
	}
	req.Header.Set("User-Agent", userAgent())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return boardView{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return boardView{}, fmt.Errorf("board: HTTP %d", resp.StatusCode)
	}
	var b boardView
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		return boardView{}, err
	}
	return b, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/botbus/ -run TestFetchBoard -v`
Expected: PASS (both).

- [ ] **Step 5: Commit**

```bash
git add cmd/botbus/board_live.go cmd/botbus/board_live_test.go
git commit -m "feat(cli): fetchBoard — decode the hub /board JSON"
```

---

### Task 3: Live board bubbletea model

**Files:**
- Modify: `cmd/botbus/board_live.go` (append the model)
- Test: `cmd/botbus/board_live_test.go` (append model tests)

**Interfaces:**
- Consumes: `fetchBoard` (Task 2); `tea` (`github.com/charmbracelet/bubbletea`); `barStyle`/`hintStyle` (existing styles in `cmd/botbus/ui.go`).
- Produces:
  - `func newLiveBoardModel(ctx context.Context, channelURL, title string) liveBoardModel`
  - `liveBoardModel` implementing `tea.Model` (`Init`, `Update`, `View`).
  - message types: `boardMsg boardView`, `boardErrMsg struct{ err error }`, `boardTickMsg struct{}`.

- [ ] **Step 1: Write the failing test**

```go
// (append to board_live_test.go)

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestLiveBoardUpdateAppliesBoardAndErr(t *testing.T) {
	m := newLiveBoardModel(context.Background(), "https://x.botbus.ai/", "mythwork")

	// A board message populates the model and clears any prior error.
	got, _ := m.Update(boardMsg(boardView{InProgress: []boardCard{{Title: "Onboarding"}}}))
	m = got.(liveBoardModel)
	if !m.loaded || len(m.board.InProgress) != 1 {
		t.Fatalf("boardMsg should load the board, got loaded=%v board=%+v", m.loaded, m.board)
	}
	if v := m.View(); !strings.Contains(v, "Onboarding") {
		t.Fatalf("View should render the card title, got:\n%s", v)
	}

	// An error message sets err (and View shows a reconnect line, not a crash).
	got, _ = m.Update(boardErrMsg{err: context.DeadlineExceeded})
	m = got.(liveBoardModel)
	if m.err == nil {
		t.Fatal("boardErrMsg should set err")
	}
	if !strings.Contains(m.View(), "reconnect") {
		t.Fatalf("View should show a reconnect hint on error, got:\n%s", m.View())
	}
}

func TestLiveBoardTickReschedulesFetch(t *testing.T) {
	m := newLiveBoardModel(context.Background(), "https://x.botbus.ai/", "mythwork")
	_, cmd := m.Update(boardTickMsg{})
	if cmd == nil {
		t.Fatal("a tick should schedule the next fetch+tick (non-nil cmd)")
	}
}

func TestLiveBoardQuitKeys(t *testing.T) {
	m := newLiveBoardModel(context.Background(), "https://x.botbus.ai/", "mythwork")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if cmd == nil {
		t.Fatal("esc should return a quit command")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/botbus/ -run TestLiveBoard -v`
Expected: FAIL — `undefined: newLiveBoardModel`.

- [ ] **Step 3: Write minimal implementation**

```go
// (append to board_live.go; add "time" and tea import to the file's import block)

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// boardPollInterval is how often the live board refetches /board. Human cadence;
// a WS-triggered refetch is a later upgrade.
const boardPollInterval = 2 * time.Second

type boardMsg boardView
type boardErrMsg struct{ err error }
type boardTickMsg struct{}

// liveBoardModel polls a channel's /board and redraws on each result. ctx scopes
// the HTTP fetches so program exit cancels in-flight requests.
type liveBoardModel struct {
	ctx    context.Context
	url    string
	title  string
	board  boardView
	loaded bool
	err    error
}

func newLiveBoardModel(ctx context.Context, channelURL, title string) liveBoardModel {
	return liveBoardModel{ctx: ctx, url: channelURL, title: title}
}

func (m liveBoardModel) Init() tea.Cmd { return tea.Batch(m.fetchCmd(), m.tickCmd()) }

func (m liveBoardModel) fetchCmd() tea.Cmd {
	return func() tea.Msg {
		b, err := fetchBoard(m.ctx, m.url)
		if err != nil {
			return boardErrMsg{err: err}
		}
		return boardMsg(b)
	}
}

func (m liveBoardModel) tickCmd() tea.Cmd {
	return tea.Tick(boardPollInterval, func(time.Time) tea.Msg { return boardTickMsg{} })
}

func (m liveBoardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch t := msg.(type) {
	case boardTickMsg:
		return m, tea.Batch(m.fetchCmd(), m.tickCmd())
	case boardMsg:
		m.board = boardView(t)
		m.loaded = true
		m.err = nil
		return m, nil
	case boardErrMsg:
		m.err = t.err
		return m, nil
	case tea.KeyMsg:
		switch t.String() {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m liveBoardModel) View() string {
	var b strings.Builder
	b.WriteString(barStyle.Render("BOTBUS · "+m.title+" · live board") + "\n\n")
	cols := []struct {
		name  string
		cards []boardCard
	}{
		{"In progress", m.board.InProgress},
		{"Blocked", m.board.Blocked},
		{"Done", m.board.Done},
	}
	for _, c := range cols {
		b.WriteString(fmt.Sprintf("%s (%d)\n", c.name, len(c.cards)))
		for _, card := range c.cards {
			title := card.Title
			if title == "" {
				title = card.Task
			}
			line := "  • " + title
			if card.By != "" {
				line += hintStyle.Render("  — " + card.By)
			}
			b.WriteString(line + "\n")
		}
		b.WriteString("\n")
	}
	if m.err != nil {
		b.WriteString(hintStyle.Render("reconnecting…") + "\n")
	} else if !m.loaded {
		b.WriteString(hintStyle.Render("loading…") + "\n")
	}
	b.WriteString(hintStyle.Render("auto-refreshes every 2s · q quit"))
	return b.String()
}
```

> Note: `barStyle` and `hintStyle` are defined in `cmd/botbus/ui.go` (same package). Confirm both exist before implementing; they are used by `rosterModel.View`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/botbus/ -run TestLiveBoard -v`
Expected: PASS (all three).

- [ ] **Step 5: Commit**

```bash
git add cmd/botbus/board_live.go cmd/botbus/board_live_test.go
git commit -m "feat(cli): live board model — tick->fetch->auto-redraw"
```

---

### Task 4: Sample-task seed

**Files:**
- Create: `cmd/botbus/onboard.go`
- Test: `cmd/botbus/onboard_test.go`

**Interfaces:**
- Consumes: `userAgent()` (`cmd/botbus/main.go`).
- Produces: `func seedSampleTask(ctx context.Context, channelURL, byName string) error` — POSTs one `task.started` frame to the channel so the live board shows a card immediately.

- [ ] **Step 1: Write the failing test**

```go
package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSeedSampleTaskPostsStartedFrame(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := seedSampleTask(context.Background(), srv.URL, "mythwork"); err != nil {
		t.Fatalf("seedSampleTask: %v", err)
	}
	if !strings.HasPrefix(gotBody, "mythwork: ") {
		t.Fatalf("frame should be sender-prefixed, got %q", gotBody)
	}
	for _, want := range []string{`"v":1`, `"type":"task.started"`, `"task":"onboarding"`} {
		if !strings.Contains(gotBody, want) {
			t.Fatalf("frame missing %q, got %q", want, gotBody)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/botbus/ -run TestSeedSampleTask -v`
Expected: FAIL — `undefined: seedSampleTask`.

- [ ] **Step 3: Write minimal implementation**

```go
package main

// onboard.go — the self-documenting onboarding wizard: name a workspace, connect
// this session, set a directive, invite teammates, add an agent, then watch the
// live board. Steps 1-5 are imperative prompts (this file); step 6 hands off to
// liveBoardModel (board_live.go). Logic reuses hostagent/workspace/onboardChildOps.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// seedSampleTask posts one task.started event frame to channelURL so the live
// board shows a card immediately. Best-effort: the caller logs any error and
// continues — a failed seed must not abort onboarding.
func seedSampleTask(ctx context.Context, channelURL, byName string) error {
	ev := struct {
		V     int    `json:"v"`
		Type  string `json:"type"`
		Task  string `json:"task"`
		Title string `json:"title"`
		By    string `json:"by"`
	}{1, "task.started", "onboarding", "Onboarding complete — you're live", byName}
	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	frame := byName + ": " + string(body)
	u := strings.TrimRight(channelURL, "/") + "/"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(frame))
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("seed: HTTP %d", resp.StatusCode)
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/botbus/ -run TestSeedSampleTask -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/botbus/onboard.go cmd/botbus/onboard_test.go
git commit -m "feat(cli): seedSampleTask — seed the live board with a task.started"
```

---

### Task 5: Workspace-root setup (Model A)

**Files:**
- Modify: `cmd/botbus/onboard.go` (append)
- Test: `cmd/botbus/onboard_test.go` (append)

**Interfaces:**
- Consumes: `hostagent.Deps`, `hostagent.Create/Update/GetByName`, `hostagent.CreateOpts`, `hostagent.UpdateFields` (`fabric/hostagent`); `profile.Load/Save`, `profile.Profile`, `profile.Root` (`fabric/profile`); `setActiveWorkspace` (`cmd/botbus/workspace.go`); `agentstate.Agent`.
- Produces: `func ensureWorkspaceRoot(ctx context.Context, d hostagent.Deps, profilePath, wsName, user string) (agentstate.Agent, error)` — creates-or-reuses the named org-root (Parent==""), persists it as the operator's profile root (preserving existing `Framing`) + active workspace. Model A: the operator's root *is* the workspace org-root.

> Reuse the existing `fakeDeps(t)` / `stubWorkspaceControl(t)` helpers in `cmd/botbus/workspace_test.go` (same package) — they wire an in-memory hub + a temp state path and never touch `~/.botbus`.

- [ ] **Step 1: Write the failing test**

```go
// (append to onboard_test.go)

import (
	"path/filepath"

	"github.com/ericpollmann/botbus-cli/fabric/hostagent"
	"github.com/ericpollmann/botbus-cli/fabric/profile"
)

func TestEnsureWorkspaceRootCreatesAndPersists(t *testing.T) {
	d := fakeDeps(t) // from workspace_test.go: fakes + temp state path
	profilePath := filepath.Join(t.TempDir(), "profile.json")

	root, err := ensureWorkspaceRoot(context.Background(), d, profilePath, "mythwork", "eric")
	if err != nil {
		t.Fatalf("ensureWorkspaceRoot: %v", err)
	}
	if root.Name != "mythwork" || root.Parent != "" {
		t.Fatalf("org-root should be named mythwork with no parent, got %+v", root)
	}

	p, err := profile.Load(profilePath)
	if err != nil {
		t.Fatalf("profile.Load: %v", err)
	}
	if p.User != "eric" || p.Root.ID != root.ID || p.Root.InboxChannel != root.InboxChannel {
		t.Fatalf("profile not persisted to the org-root: %+v", p)
	}
}

func TestEnsureWorkspaceRootReusesOnRerun(t *testing.T) {
	d := fakeDeps(t)
	profilePath := filepath.Join(t.TempDir(), "profile.json")

	first, err := ensureWorkspaceRoot(context.Background(), d, profilePath, "mythwork", "eric")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := ensureWorkspaceRoot(context.Background(), d, profilePath, "mythwork", "eric")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("re-run should reuse the same org-root id, got %q then %q", first.ID, second.ID)
	}
	agents, _ := hostagent.List(d.StatePath)
	if len(agents) != 1 {
		t.Fatalf("re-run should not mint a second root, got %d agents", len(agents))
	}
}

func TestEnsureWorkspaceRootPreservesFraming(t *testing.T) {
	d := fakeDeps(t)
	profilePath := filepath.Join(t.TempDir(), "profile.json")
	// Pre-seed a profile with an existing directive/framing.
	if err := profile.Save(profilePath, &profile.Profile{User: "eric", Framing: "ship fast"}); err != nil {
		t.Fatalf("seed profile: %v", err)
	}
	if _, err := ensureWorkspaceRoot(context.Background(), d, profilePath, "mythwork", "eric"); err != nil {
		t.Fatalf("ensureWorkspaceRoot: %v", err)
	}
	p, _ := profile.Load(profilePath)
	if p.Framing != "ship fast" {
		t.Fatalf("existing Framing should be preserved, got %q", p.Framing)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/botbus/ -run TestEnsureWorkspaceRoot -v`
Expected: FAIL — `undefined: ensureWorkspaceRoot`.

- [ ] **Step 3: Write minimal implementation**

```go
// (append to onboard.go; add the hostagent/profile/agentstate imports)

import (
	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/hostagent"
	"github.com/ericpollmann/botbus-cli/fabric/profile"
)

func ensureWorkspaceRoot(ctx context.Context, d hostagent.Deps, profilePath, wsName, user string) (agentstate.Agent, error) {
	root, ok, err := hostagent.GetByName(d.StatePath, wsName)
	if err != nil {
		return agentstate.Agent{}, err
	}
	if ok {
		// Reuse: re-register (no field changes) so a prior run that minted locally
		// but failed to reach the router self-heals (mirrors hostagent.EnsureRoot).
		root, err = hostagent.Update(ctx, d, wsName, hostagent.UpdateFields{})
		if err != nil {
			return agentstate.Agent{}, err
		}
	} else {
		root, err = hostagent.Create(ctx, d, hostagent.CreateOpts{Name: wsName}) // Parent="" => org-root
		if err != nil {
			return agentstate.Agent{}, err
		}
	}

	// Persist the org-root as the operator's profile root, preserving any existing
	// Framing (profile.Load returns a zero profile on first run).
	p, err := profile.Load(profilePath)
	if err != nil || p == nil {
		p = &profile.Profile{}
	}
	p.User = user
	p.Root = profile.Root{ID: root.ID, InboxChannel: root.InboxChannel, Key: root.Key}
	if err := profile.Save(profilePath, p); err != nil {
		return agentstate.Agent{}, err
	}
	if err := setActiveWorkspace(d.StatePath, root.ID); err != nil {
		return agentstate.Agent{}, err
	}
	return root, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/botbus/ -run TestEnsureWorkspaceRoot -v`
Expected: PASS (all three).

- [ ] **Step 5: Commit**

```bash
git add cmd/botbus/onboard.go cmd/botbus/onboard_test.go
git commit -m "feat(cli): ensureWorkspaceRoot — Model A workspace org-root setup"
```

---

### Task 6: Wizard orchestration (steps 1–5) + `Addr()` on `Ops`

**Files:**
- Modify: `cmd/botbus/onboard.go` (append `ask`, `rootConnect`, `onboardSteps`)
- Modify: `fabric/daemon/ops.go` (add `Addr() string` to the `Ops` interface)
- Modify: `cmd/botbus/console_test.go` (add `Addr()` to `stubConsoleOps`)
- Test: `cmd/botbus/onboard_test.go` (append orchestration test + a local stub Ops)

**Interfaces:**
- Consumes: `daemon.Ops` (now with `Addr()`), `daemon.ConnectInstructions`; `onboardChildOps(ctx, ops, name, focus)` (`cmd/botbus/console.go`); `workspaceInvite(ctx, d, user, wsName)` (`cmd/botbus/workspace.go`); `hostagent.Update`/`UpdateFields`; `ensureWorkspaceRoot` (Task 5); `localPastePrompt`/`invitePastePrompt` (Task 1); `readLine` (`cmd/botbus/console.go`); `domain` const.
- Produces:
  - `Ops.Addr() string` (interface method; `*daemon.Daemon` already implements it).
  - `func ask(r *bufio.Reader, out io.Writer, prompt string) string` — print prompt, read one trimmed line.
  - `func rootConnect(addr string, root agentstate.Agent) daemon.ConnectInstructions` — local-MCP connect instructions for the root.
  - `func onboardSteps(in io.Reader, out io.Writer, d hostagent.Deps, profilePath string, rebuild func(*profile.Profile) daemon.Ops) (boardURL string, err error)` — runs steps 1–5, returns the workspace root channel URL for the live board.

- [ ] **Step 1: Add `Addr()` to the `Ops` interface and the existing stub**

In `fabric/daemon/ops.go`, add to the `Ops` interface (after `EnsureRoot`):

```go
	// Addr is the local MCP listen address (host:port) used to build connect
	// instructions. *Daemon already implements it.
	Addr() string
```

In `cmd/botbus/console_test.go`, add to `stubConsoleOps`:

```go
func (s *stubConsoleOps) Addr() string { return "127.0.0.1:8765" }
```

- [ ] **Step 2: Run the suite to confirm the interface change compiles**

Run: `go build ./... && go test ./cmd/botbus/ -run TestModeTransitions -v`
Expected: PASS (compiles; `*daemon.Daemon` already satisfies the new method, `stubConsoleOps` now does too).

- [ ] **Step 3: Write the failing orchestration test**

```go
// (append to onboard_test.go)

import (
	"bytes"
	"strings"

	"github.com/ericpollmann/botbus-cli/fabric/daemon"
	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-proto/wire"
)

// stubOnboardOps satisfies daemon.Ops for the orchestration test: CreateChild
// returns canned local-MCP connect instructions; the rest are no-ops.
type stubOnboardOps struct{ createdName, createdFocus string }

func (s *stubOnboardOps) Roster(context.Context) ([]wire.AgentNode, error) { return nil, nil }
func (s *stubOnboardOps) CreateChild(_ context.Context, name, focus string) (agentstate.Agent, daemon.ConnectInstructions, error) {
	s.createdName, s.createdFocus = name, focus
	return agentstate.Agent{Name: name}, daemon.ConnectInstructions{
		MCPCommand: "claude mcp add --transport http " + name + " http://127.0.0.1:8765/a/ck",
		ChannelURL: "https://child.botbus.ai/",
	}, nil
}
func (s *stubOnboardOps) Send(context.Context, string, daemon.SendArgs) error      { return nil }
func (s *stubOnboardOps) ReadInbox(context.Context, string, int) (string, error)   { return "", nil }
func (s *stubOnboardOps) EnsureRoot(context.Context) (agentstate.Agent, error)     { return agentstate.Agent{}, nil }
func (s *stubOnboardOps) Addr() string                                             { return "127.0.0.1:8765" }

func TestOnboardStepsHappyPath(t *testing.T) {
	d := fakeDeps(t)
	profilePath := filepath.Join(t.TempDir(), "profile.json")
	stub := &stubOnboardOps{}
	rebuild := func(*profile.Profile) daemon.Ops { return stub }

	// name / workspace / directive / teammate / (finish invites) / agent / focus
	in := strings.NewReader("eric\nmythwork\nShip v1\nethan\n\nmyth-compiler\nthe compiler\n")
	var out bytes.Buffer

	boardURL, err := onboardSteps(in, &out, d, profilePath, rebuild)
	if err != nil {
		t.Fatalf("onboardSteps: %v", err)
	}
	if !strings.Contains(boardURL, ".botbus.ai") {
		t.Fatalf("boardURL should be the workspace channel, got %q", boardURL)
	}

	s := out.String()
	if !strings.Contains(s, "claude mcp add") {
		t.Fatalf("step 2 should print the local connect command, got:\n%s", s)
	}
	if !strings.Contains(s, "ethan") || !strings.Contains(s, "?user=ethan") {
		t.Fatalf("step 4 should print ethan's join URL, got:\n%s", s)
	}
	if stub.createdName != "myth-compiler" || stub.createdFocus != "the compiler" {
		t.Fatalf("step 5 should create the agent via CreateChild, got name=%q focus=%q", stub.createdName, stub.createdFocus)
	}

	p, _ := profile.Load(profilePath)
	if p.User != "eric" || p.Framing != "Ship v1" || p.Root.ID == "" {
		t.Fatalf("profile not set up by the wizard: %+v", p)
	}
}

func TestOnboardStepsRequiresName(t *testing.T) {
	d := fakeDeps(t)
	profilePath := filepath.Join(t.TempDir(), "profile.json")
	rebuild := func(*profile.Profile) daemon.Ops { return &stubOnboardOps{} }
	if _, err := onboardSteps(strings.NewReader("\n"), &bytes.Buffer{}, d, profilePath, rebuild); err == nil {
		t.Fatal("an empty name should error")
	}
}
```

- [ ] **Step 4: Run test to verify it fails**

Run: `go test ./cmd/botbus/ -run TestOnboardSteps -v`
Expected: FAIL — `undefined: onboardSteps`.

- [ ] **Step 5: Write minimal implementation**

```go
// (append to onboard.go; add "bufio" and "io" to the import block, plus daemon)

import (
	"bufio"
	"io"

	"github.com/ericpollmann/botbus-cli/fabric/daemon"
)

// ask prints prompt to out and returns the next trimmed input line.
func ask(r *bufio.Reader, out io.Writer, prompt string) string {
	fmt.Fprint(out, prompt)
	line, _ := readLine(r) // readLine is defined in console.go; EOF yields ""
	return strings.TrimSpace(line)
}

// rootConnect builds local-MCP connect instructions for the operator's root,
// mirroring the daemon's CreateChild shape (http://<addr>/a/<key>).
func rootConnect(addr string, root agentstate.Agent) daemon.ConnectInstructions {
	endpoint := fmt.Sprintf("http://%s/a/%s", addr, root.Key)
	return daemon.ConnectInstructions{
		MCPCommand:  fmt.Sprintf("claude mcp add --transport http %s %s", root.Name, endpoint),
		MCPEndpoint: endpoint,
		ChannelURL:  fmt.Sprintf("https://%s.%s/", root.InboxChannel, domain),
	}
}

// onboardSteps runs the imperative guided steps 1-5 and returns the workspace
// root channel URL the caller watches in the live board (step 6). rebuild
// produces the runtime Ops bound to the freshly-saved profile so step 5's
// CreateChild parents the new agent under the workspace root.
func onboardSteps(in io.Reader, out io.Writer, d hostagent.Deps, profilePath string, rebuild func(*profile.Profile) daemon.Ops) (string, error) {
	r := bufio.NewReader(in)
	ctx := context.Background()

	fmt.Fprintln(out, "botbus onboarding — let's set up your workspace.\n")

	user := ask(r, out, "Your name: ")
	if user == "" {
		return "", fmt.Errorf("name is required")
	}
	wsName := ask(r, out, "Workspace name: ")
	if wsName == "" {
		return "", fmt.Errorf("workspace name is required")
	}

	// Step 1: create (or reuse) the workspace org-root = the operator's root.
	root, err := ensureWorkspaceRoot(ctx, d, profilePath, wsName, user)
	if err != nil {
		return "", fmt.Errorf("create workspace: %w", err)
	}
	channelURL := fmt.Sprintf("https://%s.%s/", root.InboxChannel, domain)
	fmt.Fprintf(out, "\n✓ workspace %q is live: %s\n", wsName, channelURL)

	// Rebuild Ops against the saved profile so CreateChild sees the new root.
	p, _ := profile.Load(profilePath)
	ops := rebuild(p)

	// Step 2: connect this session (local-MCP paste prompt + terminal fallback).
	inst := rootConnect(ops.Addr(), root)
	fmt.Fprintln(out, "\n── Connect THIS Claude Code session ──")
	fmt.Fprintln(out, localPastePrompt(wsName, "workspace owner", inst))
	fmt.Fprintf(out, "\n(terminal fallback: %s)\n", inst.MCPCommand)

	// Step 3: workspace directive (optional).
	directive := ask(r, out, "\nWorkspace directive (optional, Enter to skip): ")
	if directive != "" {
		if _, uerr := hostagent.Update(ctx, d, wsName, hostagent.UpdateFields{Focus: &directive}); uerr != nil {
			fmt.Fprintln(out, "  (couldn't set directive:", uerr, ")")
		} else {
			if p2, lerr := profile.Load(profilePath); lerr == nil && p2 != nil {
				p2.Framing = directive // injected into child welcomes
				_ = profile.Save(profilePath, p2)
			}
			fmt.Fprintln(out, "✓ directive set")
		}
	}

	// Step 4: invite teammates (loop until a blank name).
	for {
		teammate := ask(r, out, "\nInvite a teammate (name, Enter to finish): ")
		if teammate == "" {
			break
		}
		joinURL, ierr := workspaceInvite(ctx, d, teammate, wsName)
		if ierr != nil {
			fmt.Fprintln(out, "  (invite failed:", ierr, ")")
			continue
		}
		fmt.Fprintln(out, invitePastePrompt(teammate, joinURL))
	}

	// Step 5: add a standing agent (optional).
	agentName := ask(r, out, "\nAdd a standing agent/role (name, Enter to skip): ")
	if agentName != "" {
		focus := ask(r, out, "  Its focus: ")
		msg, aerr := onboardChildOps(ctx, ops, agentName, focus)
		if aerr != nil {
			fmt.Fprintln(out, "  (couldn't create agent:", aerr, ")")
		} else {
			fmt.Fprintf(out, "\nPaste into a NEW Claude Code session to run %s:\n%s\n", agentName, msg)
		}
	}

	return channelURL, nil
}
```

> Note: `fmt.Fprintln(out, "…\n")` adds an extra blank line intentionally; if `gofmt`/`vet` flags the literal `\n` in `Fprintln`, switch that one call to `fmt.Fprint(out, "…\n\n")`.

- [ ] **Step 6: Run test to verify it passes**

Run: `go test ./cmd/botbus/ -run "TestOnboardSteps|TestEnsureWorkspaceRoot|TestSeedSampleTask" -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add cmd/botbus/onboard.go cmd/botbus/onboard_test.go fabric/daemon/ops.go cmd/botbus/console_test.go
git commit -m "feat(cli): onboardSteps wizard orchestration + Ops.Addr()"
```

---

### Task 7: Entry wiring (`botbus onboard` + first-run) + README

**Files:**
- Modify: `cmd/botbus/onboard.go` (append `runOnboard`)
- Modify: `cmd/botbus/main.go` (dispatch `onboard`)
- Modify: `cmd/botbus/console.go` (first-run path → `runOnboard`; delete `firstRunOps`)
- Modify: `cmd/botbus/console_run_test.go` (remove `firstRunOps`/onboard-prompt tests that no longer apply)
- Modify: `README.md` (document the wizard)

**Interfaces:**
- Consumes: `onboardSteps` (Task 6), `seedSampleTask` (Task 4), `newLiveBoardModel` (Task 3), `buildRuntime` (`cmd/botbus/runtime.go`), `profile.DefaultPath`/`Load`, `agentstate.DefaultPath`, `realDeps()` (`cmd/botbus/agent.go`), `tea`.
- Produces: `func runOnboard()` — the full wizard entrypoint (steps 1–5 then the live board).

- [ ] **Step 1: Implement `runOnboard`**

```go
// (append to onboard.go; add os, signal, syscall, tea imports as needed)

import (
	"os"
	"os/signal"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"
)

// runOnboard is the no-args wizard entrypoint: guided setup (steps 1-5) then the
// live board (step 6). Used for bare-botbus first-run and `botbus onboard`.
func runOnboard() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	profilePath := profile.DefaultPath()
	deps := realDeps()
	rebuild := func(p *profile.Profile) daemon.Ops { return buildRuntime(p) }

	channelURL, err := onboardSteps(os.Stdin, os.Stdout, deps, profilePath, rebuild)
	if err != nil {
		fmt.Fprintln(os.Stderr, "onboard:", err)
		os.Exit(1)
	}

	// Seed one card so the board isn't empty on first paint (best-effort).
	if p, lerr := profile.Load(profilePath); lerr == nil && p != nil {
		if serr := seedSampleTask(ctx, channelURL, "you"); serr != nil {
			fmt.Fprintln(os.Stderr, "(seed skipped:", serr, ")")
		}
	}

	fmt.Fprintln(os.Stdout, "\nOpening your live board — watch tasks appear. (q to quit)")
	m := newLiveBoardModel(ctx, channelURL, "your workspace")
	if _, rerr := tea.NewProgram(m, tea.WithAltScreen()).Run(); rerr != nil {
		fmt.Fprintln(os.Stderr, rerr)
	}
	fmt.Fprintln(os.Stdout, "Done. Run `botbus` anytime to open your console (it serves the local MCP your agents use).")
}
```

- [ ] **Step 2: Dispatch `onboard` in `main.go`**

In `cmd/botbus/main.go`, in `main()`, add alongside the other subcommand checks (e.g. after the `workspace` block):

```go
	// Guided self-documenting onboarding (re-runnable).
	if len(os.Args) > 1 && os.Args[1] == "onboard" {
		runOnboard()
		return
	}
```

- [ ] **Step 3: Route first-run to the wizard and delete `firstRunOps`**

In `cmd/botbus/console.go`, replace the first-run block in `runConsole`:

```go
	// existing:
	rt := buildRuntime(p)
	if !p.Configured() {
		p, err = firstRunOps(os.Stdin, os.Stdout, rt, profilePath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "setup:", err)
			os.Exit(1)
		}
		rt = buildRuntime(p) // rebuild with the now-populated profile
	}
```

with:

```go
	if !p.Configured() {
		// First run: hand off to the guided wizard (it sets up the profile +
		// workspace and ends in the live board). The operator re-runs `botbus`
		// afterward to open the console.
		runOnboard()
		return
	}
	rt := buildRuntime(p)
```

Then **delete** `firstRunOps` (and `onboardChildOps`'s caller is unaffected). Remove now-dead tests in `cmd/botbus/console_run_test.go` that exercise `firstRunOps` and the inline name/framing prompt. (Keep `onboardChildOps`, `updateOnboard`, and the roster `o`-onboard shortcut — they remain in use.)

- [ ] **Step 4: Verify the build + full suite**

Run: `go build ./... && go vet ./... && go test -race ./... && gofmt -l cmd/botbus`
Expected: build OK, vet clean, all tests PASS, `gofmt -l` prints nothing.

- [ ] **Step 5: Update the README**

In `README.md`, add a short section near the getting-started/usage area:

```markdown
## First run

Run `botbus` on a fresh machine and it walks you through everything:

1. **Name your workspace** — creates your coordination root.
2. **Connect this session** — paste the printed prompt into Claude Code (adds the
   local botbus MCP); a terminal `claude mcp add …` fallback is shown too.
3. **Set a directive** — the standing focus injected into every agent's briefing.
4. **Invite teammates** — each gets a join URL (their credential) to paste/send.
5. **Add a standing agent** — get a paste-prompt for a new Claude Code session.
6. **Watch the live board** — tasks appear as agents post status.

Re-run the wizard anytime with `botbus onboard`. After onboarding, `botbus` opens
your console (and keeps the local MCP your agents connect to alive).
```

- [ ] **Step 6: Final gate + commit**

Run: `go build ./... && go vet ./... && go test -race ./... && gofmt -l cmd/botbus`
Expected: all green.

```bash
git add cmd/botbus/onboard.go cmd/botbus/main.go cmd/botbus/console.go cmd/botbus/console_run_test.go README.md
git commit -m "feat(cli): wire botbus onboard + first-run wizard; drop firstRunOps"
```

---

## Self-Review

**1. Spec coverage:**
- §2 entry point & re-runnability → Task 7 (bare-botbus first-run → `runOnboard`; `botbus onboard` dispatch). ✓
- §3 flow steps 1–6 → Tasks 5 (step 1), 6 (steps 2–5), 4+3 (step 6 seed + live board), 7 (wiring). ✓
- §4 Model A → Task 5 `ensureWorkspaceRoot` (operator root == org-root). ✓
- §5 paste-prompt generator (local + invite shapes) → Task 1. ✓
- §6 live board (poll `/board`, tick→redraw) → Tasks 2+3. ✓
- §7 wizard orchestrator → Task 6. ✓
- §8 data flow → exercised across Tasks 4–7. ✓
- §9 error handling (router unreachable ret[r]y; board fetch fail → reconnect; skip; re-run reuse) → Task 3 (reconnect line), Task 5 (reuse), Task 6 (per-step error prints). ✓
- §10 testing (pure pastePrompt; httptest fetchBoard/seed; stub-ops orchestration; fakeDeps; no `~/.botbus`/network) → every task's tests. ✓
- §11 non-goals (no hub change, no WS board, no cloud gateway) → respected. ✓

**2. Placeholder scan:** No TBD/TODO/"handle errors"; every code step shows full code and exact commands. ✓

**3. Type consistency:**
- `daemon.ConnectInstructions{MCPCommand, MCPEndpoint, ChannelURL}` used consistently (Tasks 1, 6).
- `boardView`/`boardCard` defined Task 2, consumed Task 3 (`boardMsg boardView`). ✓
- `Ops.Addr() string` added Task 6 step 1 before first use in `rootConnect`/`onboardSteps`; `*daemon.Daemon` already implements it; `stubConsoleOps` + `stubOnboardOps` updated. ✓
- `ensureWorkspaceRoot`/`onboardSteps`/`seedSampleTask`/`fetchBoard`/`newLiveBoardModel` signatures match across producer/consumer blocks. ✓
- `onboardChildOps(ctx, ops, name, focus) (string, error)` (existing) used as-is in Task 6. ✓
