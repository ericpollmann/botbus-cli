package main

import (
	"context"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

// dialOpts returns the WebSocket dial options used by both runWSText and
// runWSAudio. Setting User-Agent here lets the server's classifyUA bucket
// our WS upgrades as classCLI alongside the new.botbus.ai mint and proxy
// lookup in updater.go.
func dialOpts() *websocket.DialOptions {
	return &websocket.DialOptions{
		HTTPHeader: http.Header{"User-Agent": []string{userAgent()}},
	}
}

// Per-stream byte caps mirror the server's streamType.readLimit() in
// botbus/main.go. Text frames are tiny by design; audio frames carry
// ~30-60s of Opus in a single 256KB cap.
const (
	textReadLimit  = 16 * 1024
	audioReadLimit = 256 * 1024
)

// reconnectBackoff is the pause between a dropped connection and the next
// dial. A var (not const) so tests can shrink it; production stays at 2s.
var reconnectBackoff = 2 * time.Second

// runWSText keeps a bidirectional text-stream WebSocket alive. It pumps
// received text frames into recv, drains outgoing user lines from send,
// and reconnects with backoff on drop. Connection state is reported on
// states.
//
// A resume ring persists across reconnects: every received frame is recorded
// into it, and each (re)dial carries a ?resume=<count>.<hex-fp> token derived
// from the last K frames seen. The server replays only the gap the client
// missed instead of dumping the last 40 on every reconnect (see resume.go).
// The first connect has an empty ring → no token → the server sends recent
// context, the intended "history on join" behavior.
//
// Server excludes the sender's *Conn from broadcasts, so frames we send
// do not echo back on recv — the UI's local "→ text" line is the only
// display of our own messages.
func runWSText(ctx context.Context, target, histBase string, recv chan<- []byte, send <-chan []byte, states chan<- connState, seedCh chan<- seedMsg) {
	defer close(recv)
	ring := newFPRing(resumeDefaultK)
	// Seed the scrollback + resume ring from /history BEFORE the first connect.
	// The model renders the seed as initial history (with a pagination cursor),
	// and the seeded ring makes the first dial carry a resume token so the
	// server replays only the gap rather than re-pushing these same frames over
	// the socket. In --monitor mode nothing reads seedCh (it's buffered), but
	// the ring seed still suppresses the last-40 replay so an agent wrapper
	// isn't flooded with stale messages on startup. Best-effort: on failure the
	// ring stays empty and the server replays the last 40 over the WS as usual.
	if seedCh != nil {
		if page, err := fetchHistory(ctx, histBase, "/history", "", 40); err == nil {
			frames := histFramesChrono(page)
			for _, f := range frames {
				ring.add(f)
			}
			seedCh <- seedMsg{frames: frames, next: page.Next}
		} else {
			seedCh <- seedMsg{}
		}
	}
	for ctx.Err() == nil {
		states <- stConnecting
		ws, _, err := websocket.Dial(ctx, withResume(target, ring.token()), dialOpts())
		if err != nil {
			states <- stDown
			if !sleepCtx(ctx, reconnectBackoff) {
				return
			}
			continue
		}
		ws.SetReadLimit(textReadLimit)

		// First frame is the server's binary "ok" handshake.
		if typ, m, rerr := ws.Read(ctx); rerr != nil || typ != websocket.MessageBinary || string(m) != "ok" {
			ws.CloseNow()
			states <- stDown
			if !sleepCtx(ctx, reconnectBackoff) {
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
				ring.add(m) // record before forwarding so the next resume token reflects it
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
		if !sleepCtx(ctx, reconnectBackoff) {
			return
		}
	}
}

// runWSAudio keeps a receive-only audio-stream WebSocket alive. The CLI
// doesn't send audio (PTT lives in the web UI), so this is reader-only.
// Frames go to audio; full buffer drops silently rather than backpressuring.
// Audio degradation is soft — no state reporting, no banner — the text
// socket drives the user-visible "connected/down" state.
//
// Like the text stream it carries a resume token so a reconnecting listener
// gets only the audio it missed, not a last-40 burst of stale Opus on every
// drop. The ring records every wire-received frame (even ones later dropped
// from the playback channel), so the token tracks the buffer position, not
// what actually played.
func runWSAudio(ctx context.Context, target, histBase string, audio chan<- []byte) {
	ring := newFPRing(resumeDefaultK)
	// Seed the audio ring from /audio/history before the first connect so the
	// first dial carries a resume token and the server doesn't replay the last
	// ~40 audio frames as a burst of stale Opus on startup. Best-effort; no
	// rendering — audio history is for the resume token only, not playback.
	if page, err := fetchHistory(ctx, histBase, "/audio/history", "", resumeDefaultK); err == nil {
		for _, f := range histFramesChrono(page) {
			ring.add(f)
		}
	}
	for ctx.Err() == nil {
		ws, _, err := websocket.Dial(ctx, withResume(target, ring.token()), dialOpts())
		if err != nil {
			if !sleepCtx(ctx, reconnectBackoff) {
				return
			}
			continue
		}
		ws.SetReadLimit(audioReadLimit)

		if typ, m, rerr := ws.Read(ctx); rerr != nil || typ != websocket.MessageBinary || string(m) != "ok" {
			ws.CloseNow()
			if !sleepCtx(ctx, reconnectBackoff) {
				return
			}
			continue
		}

		for {
			_, m, err := ws.Read(ctx)
			if err != nil {
				break
			}
			ring.add(m) // record every received frame so the resume token tracks the buffer
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
		if !sleepCtx(ctx, reconnectBackoff) {
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
