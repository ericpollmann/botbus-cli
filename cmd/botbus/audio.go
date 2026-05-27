package main

// audio.go — playback of incoming audio frames from the /audio stream.
//
// The CLI is text-first, but voice listeners on the web UI broadcast audio
// alongside transcripts. To keep the CLI useful in a voice-heavy channel,
// we play received audio through a native command-line player. macOS's
// built-in afplay handles WebM/Opus on modern versions; ffplay/mpv/mplayer
// are tried in turn as portable fallbacks.
//
// Audio frames arrive on a separate WebSocket (/audio path) — there is no
// type-byte multiplexing; the URL is what makes them audio frames.
//
// Playback is serial: one frame plays to completion before the next starts.
// A 64KB cap on frame size puts an upper bound on each clip (~5-10 seconds
// of Opus typically), and the audio channel is buffered so a slow player
// can't backpressure the WS reader — overflow drops silently.

import (
	"context"
	"os"
	"os/exec"
)

// playerCmd is the chosen audio player binary plus any baseline args.
// Nil/empty means no player was found; we still drain the audio channel
// to avoid blocking the WS reader, just without sound.
var playerCmd []string

// playerHint is a one-line message printed to stderr before the TUI
// starts if the only available player is afplay (which doesn't decode
// the webm/opus that Chrome and Firefox MediaRecorder produce). Empty
// when a webm-capable player was found or none at all.
var playerHint string

func init() {
	// Preference order matters: web clients on Chrome/Firefox send
	// audio/webm (Opus), which afplay can't decode — CoreAudio's
	// AudioFile decoders don't include the webm container. ffplay, mpv,
	// and mplayer all handle webm natively, so they go first. afplay
	// stays as a last-ditch fallback for Safari users sending audio/mp4
	// (which afplay DOES decode).
	for _, candidate := range [][]string{
		{"ffplay", "-nodisp", "-autoexit", "-loglevel", "quiet"},
		{"mpv", "--no-video", "--really-quiet"},
		{"mplayer", "-really-quiet"},
		{"afplay"},
	} {
		if _, err := exec.LookPath(candidate[0]); err == nil {
			playerCmd = candidate
			if candidate[0] == "afplay" {
				playerHint = "note: only afplay available — webm/opus voice messages won't play.\n" +
					"      install ffmpeg (`brew install ffmpeg`) or mpv for reliable playback.\n"
			}
			return
		}
	}
}

// parseAudioFrame extracts the audio payload from a wire frame on the
// /audio stream:
//
//	[name UTF-8][": "][audio bytes]
//
// Returns ("", nil, false) for any malformed frame.
func parseAudioFrame(m []byte) (name string, audio []byte, ok bool) {
	if len(m) < 3 {
		return "", nil, false
	}
	for i := 0; i < len(m)-1; i++ {
		if m[i] == ':' && m[i+1] == ' ' {
			if i == 0 {
				return "", nil, false // empty name
			}
			return string(m[:i]), m[i+2:], true
		}
	}
	return "", nil, false
}

// runAudio consumes audio frames and plays each via the detected player.
// Exits when ctx is cancelled. Frames continue to be drained (and dropped)
// even when no player is available so the WS reader never blocks on send.
func runAudio(ctx context.Context, audio <-chan []byte) {
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-audio:
			if len(playerCmd) == 0 {
				continue
			}
			playFrame(ctx, m)
		}
	}
}

func playFrame(ctx context.Context, m []byte) {
	_, bytes, ok := parseAudioFrame(m)
	if !ok || len(bytes) == 0 {
		return
	}
	f, err := os.CreateTemp("", "botbus-*.webm")
	if err != nil {
		return
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err := f.Write(bytes); err != nil {
		f.Close()
		return
	}
	f.Close()

	args := append([]string{}, playerCmd[1:]...)
	args = append(args, name)
	cmd := exec.CommandContext(ctx, playerCmd[0], args...)
	_ = cmd.Run()
}
