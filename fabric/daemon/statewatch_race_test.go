package daemon

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
	"github.com/ericpollmann/botbus-cli/fabric/control"
)

// TestStateWatchNoRaceUnderTraffic runs the watcher at a 1ms poll while an
// external writer rewrites state.json with monotonically increasing epochs and
// reader goroutines hammer the locked key-read paths. Run with -race: any
// unsynchronised access to d.state / ws.Key fails the test.
func TestStateWatchNoRaceUnderTraffic(t *testing.T) {
	srv := stubAcceptAll(t)
	defer srv.Close()
	setStatePollEvery(time.Millisecond)
	defer setStatePollEvery(2 * time.Second)

	p := filepath.Join(t.TempDir(), "state.json")
	st := &agentstate.State{
		Daemon:     agentstate.Daemon{RouterURL: srv.URL, MCPAddr: freeAddr(t)},
		Agents:     []agentstate.Agent{{ID: "root", Key: "rootkey", InboxChannel: "root-inbox"}},
		Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: 1, Key: k(1), Roster: "roster"}},
	}
	if err := agentstate.Save(p, st); err != nil {
		t.Fatal(err)
	}

	d := NewRuntime(Config{State: st, StatePath: p, Hub: newCountingHub(), Control: control.NewClient(srv.URL), Domain: "botbus.ai"})
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = d.Run(ctx) }()

	stop := make(chan struct{})
	var wg sync.WaitGroup

	// External writer: rewrite state.json with rising epochs.
	wg.Add(1)
	go func() {
		defer wg.Done()
		epoch := uint32(2)
		for {
			select {
			case <-stop:
				return
			default:
			}
			next := &agentstate.State{
				Daemon:     st.Daemon,
				Agents:     st.Agents,
				Workspaces: []agentstate.Workspace{{RootID: "root", E2E: true, Epoch: epoch, Key: k(byte(epoch)), Roster: "roster"}},
			}
			_ = agentstate.Save(p, next)
			epoch++
			time.Sleep(time.Millisecond)
		}
	}()

	// Readers: hammer the locked key paths concurrently with the watcher.
	ws := &d.state.Workspaces[0]
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = d.currentKey(ws)
				_, _, _ = d.e2eContextFor("root")
			}
		}()
	}

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
	cancel()
}
