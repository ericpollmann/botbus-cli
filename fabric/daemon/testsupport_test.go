package daemon

import "github.com/ericpollmann/botbus-cli/fabric/agentstate"

// pendingLen returns the current length of ws.Pending, read under d.mu.
// Used by tests to poll pending without racing the loop.
func (d *Daemon) pendingLen(ws *agentstate.Workspace) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(ws.Pending)
}

// pendingReqID returns the ReqID of the i-th pending entry, read under d.mu.
func (d *Daemon) pendingReqID(ws *agentstate.Workspace, i int) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if i >= len(ws.Pending) {
		return ""
	}
	return ws.Pending[i].ReqID
}
