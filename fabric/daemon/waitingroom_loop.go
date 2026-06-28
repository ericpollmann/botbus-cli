package daemon

import (
	"context"
	"log"

	"github.com/ericpollmann/botbus-cli/fabric/agentstate"
)

func runWaitingRoom(ctx context.Context, d *Daemon, ws *agentstate.Workspace) {
	cursor := ""
	for {
		if ctx.Err() != nil {
			return
		}
		frames, err := d.hub.Subscribe(ctx, ws.WaitingRoom, cursor)
		if err != nil {
			log.Printf("daemon: subscribe waiting-room %s: %v", ws.WaitingRoom, err)
			if !sleepCtx(ctx, reconnectBackoff) {
				return
			}
			continue
		}
		disconnected := false
		for !disconnected {
			select {
			case <-ctx.Done():
				return
			case fr, ok := <-frames:
				if !ok {
					disconnected = true
					break
				}
				req, perr := parseJoinRequest([]byte(fr.Body))
				if perr == nil && req.ReqID != "" && len(req.SignPub) > 0 && len(req.EncPub) > 0 {
					d.recordPending(ws, req)
				}
				if fr.Resume != "" {
					cursor = fr.Resume
				}
			}
		}
		if !sleepCtx(ctx, reconnectBackoff) {
			return
		}
	}
}

func (d *Daemon) recordPending(ws *agentstate.Workspace, req JoinRequest) {
	d.mu.Lock()
	for _, p := range ws.Pending {
		if p.ReqID == req.ReqID {
			d.mu.Unlock()
			return
		}
	}
	ws.Pending = append(ws.Pending, agentstate.PendingJoin{
		ReqID: req.ReqID, Name: req.Name, ParentIntent: req.ParentIntent,
		SignPub: append([]byte(nil), req.SignPub...),
		EncPub:  append([]byte(nil), req.EncPub...),
	})
	d.mu.Unlock()
	d.persistWorkspaceKey(ws) // reuse the best-effort state.json save
}
