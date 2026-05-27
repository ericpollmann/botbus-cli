package main

import (
	"context"
	"time"

	"github.com/coder/websocket"
)

// Per-stream byte caps mirror the server's streamType.readLimit() in
// botbus/main.go. Text frames are tiny by design; audio frames carry
// ~30-60s of Opus in a single 256KB cap.
const (
	textReadLimit  = 16 * 1024
	audioReadLimit = 256 * 1024
)

// runWSText keeps a bidirectional text-stream WebSocket alive. It pumps
// received text frames into recv, drains outgoing user lines from send,
// and reconnects with 2s backoff on drop. Connection state is reported
// on states.
//
// Server excludes the sender's *Conn from broadcasts, so frames we send
// do not echo back on recv — the UI's local "→ text" line is the only
// display of our own messages.
func runWSText(ctx context.Context, target string, recv chan<- []byte, send <-chan []byte, states chan<- connState) {
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
		ws.SetReadLimit(textReadLimit)

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

// runWSAudio keeps a receive-only audio-stream WebSocket alive. The CLI
// doesn't send audio (PTT lives in the web UI), so this is reader-only.
// Frames go to audio; full buffer drops silently rather than backpressuring.
// Audio degradation is soft — no state reporting, no banner — the text
// socket drives the user-visible "connected/down" state.
func runWSAudio(ctx context.Context, target string, audio chan<- []byte) {
	for ctx.Err() == nil {
		ws, _, err := websocket.Dial(ctx, target, nil)
		if err != nil {
			if !sleepCtx(ctx, 2*time.Second) {
				return
			}
			continue
		}
		ws.SetReadLimit(audioReadLimit)

		if typ, m, rerr := ws.Read(ctx); rerr != nil || typ != websocket.MessageBinary || string(m) != "ok" {
			ws.CloseNow()
			if !sleepCtx(ctx, 2*time.Second) {
				return
			}
			continue
		}

		for {
			_, m, err := ws.Read(ctx)
			if err != nil {
				break
			}
			select {
			case audio <- m:
			case <-ctx.Done():
				ws.CloseNow()
				return
			default:
				// audio channel full — drop rather than backpressure
			}
		}
		ws.CloseNow()
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
