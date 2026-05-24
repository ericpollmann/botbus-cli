package main

import (
	"context"
	"time"

	"github.com/coder/websocket"
)

// runWS keeps a single WebSocket connection alive. It pumps received text
// frames into recv, routes 0x01 audio frames to audio (best-effort — full
// buffer drops silently rather than backpressure the reader), drains
// outgoing user lines from send, and reconnects with 2s backoff on drop.
// Connection state is reported on states.
//
// Server excludes the sender's *Conn from broadcasts, so frames we send do
// not echo back on recv — the UI's local "→ text" line is the only display
// of our own messages.
func runWS(ctx context.Context, target string, recv, audio chan<- []byte, send <-chan []byte, states chan<- connState) {
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
		// Match server-side readLimit (and web MAX_FRAME) — 256KB covers
		// ~30-60s of audio in a single 0x01 frame.
		ws.SetReadLimit(256 * 1024)

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
				// Route 0x01 audio frames to the audio player; drop other
				// typed frames (reserved type bytes the CLI doesn't know).
				if len(m) > 0 && m[0] == 0x01 {
					select {
					case audio <- m:
					case <-ctx.Done():
						return
					default:
						// audio channel full — drop rather than backpressure
					}
					continue
				}
				if isTypedFrame(m) {
					continue
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
