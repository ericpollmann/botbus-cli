package main

import (
	"context"
	"time"

	"github.com/coder/websocket"
)

// runWS keeps a single WebSocket connection alive. It pumps received frames
// into recv, drains outgoing user lines from send, and reconnects with 2s
// backoff on drop. Connection state is reported on states.
//
// Server excludes the sender's *Conn from broadcasts, so frames we send do
// not echo back on recv — the UI's local "→ text" line is the only display
// of our own messages.
func runWS(ctx context.Context, target string, recv chan<- []byte, send <-chan []byte, states chan<- connState) {
	defer close(recv)
	for ctx.Err() == nil {
		states <- stConnecting
		ws, _, err := websocket.Dial(ctx, target, nil)
		if err != nil {
			states <- stDown
			if !sleepCtx(ctx, 2*time.Second) {
				return
			}
			continue
		}
		ws.SetReadLimit(64 * 1024)

		// First frame is the server's binary "ok" handshake.
		if typ, m, rerr := ws.Read(ctx); rerr != nil || typ != websocket.MessageBinary || string(m) != "ok" {
			ws.CloseNow()
			states <- stDown
			if !sleepCtx(ctx, 2*time.Second) {
				return
			}
			continue
		}
		states <- stConnected

		readerDone := make(chan struct{})
		go func() {
			defer close(readerDone)
			for {
				_, m, err := ws.Read(ctx)
				if err != nil {
					return
				}
				select {
				case recv <- m:
				case <-ctx.Done():
					return
				}
			}
		}()

	writeLoop:
		for {
			select {
			case msg := <-send:
				wctx, wc := context.WithTimeout(ctx, 5*time.Second)
				err := ws.Write(wctx, websocket.MessageBinary, msg)
				wc()
				if err != nil {
					break writeLoop
				}
			case <-readerDone:
				break writeLoop
			case <-ctx.Done():
				ws.CloseNow()
				return
			}
		}
		ws.CloseNow()
		states <- stDown
		if !sleepCtx(ctx, 2*time.Second) {
			return
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}
