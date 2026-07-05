package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/coder/websocket"
)

// errBadAPIKey marks failures the user can fix by entering a working key in
// the settings menu; the pipeline waits and retries instead of exiting.
var errBadAPIKey = errors.New("the OpenAI API rejected the key")

const (
	realtimeURL   = "wss://api.openai.com/v1/realtime?intent=transcription"
	realtimeModel = "gpt-realtime-whisper"

	frameMS    = 30
	frameBytes = sampleRate * frameMS / 1000 * bytesPerSample

	// The API rejects commits of very short buffers (<100 ms of audio).
	minCommitBytes = sampleRate * bytesPerSample / 5 // 200 ms

	// Idle gate: the API bills per audio-minute streamed, and a silent Mac
	// reaches us as either near-zero frames (some process rendering digital
	// silence) or no frames at all (nothing rendering — the tap goes fully
	// quiet). After silenceHold without an audible frame the writer stops
	// sending audio. silencePeak (~-66 dBFS) is far below any audible audio
	// but above zero to tolerate dither.
	silencePeak    = 16
	silenceHold    = 2 * time.Second
	gatedKeepalive = 15 * time.Second // one frame this often holds the session open
)

// wordActivity shares transcription-delta state between the event reader
// (which sees deltas arrive) and the audio writer (which decides commits).
// hasWords is only ever true when lastDeltaNano is set: the reader stores the
// timestamp first.
type wordActivity struct {
	hasWords      atomic.Bool  // the open (uncommitted) segment has transcribed words
	lastDeltaNano atomic.Int64 // arrival time of its most recent delta
	forceCommit   atomic.Bool  // segment hit --max-sentences; cut at the next opportunity
}

// sentenceCount counts sentence-ending punctuation followed by whitespace (or
// end of text) — the same rule the window uses to insert line breaks, so the
// --max-sentences cut matches what the user sees pile up.
func sentenceCount(s string) int {
	n := 0
	rs := []rune(s)
	for i, r := range rs {
		if strings.ContainsRune(".!?。！？", r) {
			if i == len(rs)-1 || unicode.IsSpace(rs[i+1]) {
				n++
			}
		}
	}
	return n
}

var streamedAudioBytes atomic.Int64
var gatedAudioBytes atomic.Int64 // silent audio dropped by the idle gate (never billed)

// framePeak returns the largest absolute s16le sample in the frame.
func framePeak(b []byte) int {
	peak := 0
	for i := 0; i+1 < len(b); i += 2 {
		v := int(int16(uint16(b[i]) | uint16(b[i+1])<<8))
		if v < 0 {
			v = -v
		}
		if v > peak {
			peak = v
		}
	}
	return peak
}

// SegmentUpdate carries the full accumulated source text for one speech
// segment (realtime item). Final marks the server's completed transcript.
// An update with IdleGate set carries no segment at all — it reports the
// idle gate opening (false) or closing (true) so the window can show
// "idling…" while nothing is playing.
type SegmentUpdate struct {
	ItemID   string
	Text     string
	Final    bool
	IdleGate *bool
}

// streamTranscribe pipes PCM from src to the Realtime transcription API and
// emits SegmentUpdates as delta/completed events arrive. Segmentation is
// manual: gpt-realtime-whisper streams deltas continuously, and a segment is
// committed once its words stop — no new delta for --word-gap ms. Cutting on
// transcription activity rather than acoustic silence means background music
// can't hold a segment open; only speech does.
func streamTranscribe(ctx context.Context, cfg Config, key string, src io.Reader, updates chan<- SegmentUpdate) error {
	defer close(updates)

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+key)
	conn, resp, err := websocket.Dial(ctx, realtimeURL, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			return fmt.Errorf("%w (HTTP %d)", errBadAPIKey, resp.StatusCode)
		}
		return fmt.Errorf("connecting to realtime API: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")
	conn.SetReadLimit(1 << 20)

	transcription := map[string]any{"model": realtimeModel}
	if cfg.SourceLang != "" {
		transcription["language"] = cfg.SourceLang
	}
	if cfg.StreamDelay != "" {
		transcription["delay"] = cfg.StreamDelay
	}
	if err := writeJSON(ctx, conn, map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type": "transcription",
			"audio": map[string]any{
				"input": map[string]any{
					"format":         map[string]any{"type": "audio/pcm", "rate": sampleRate},
					"transcription":  transcription,
					"turn_detection": nil, // manual commits (required for gpt-realtime-whisper)
				},
			},
		},
	}); err != nil {
		return fmt.Errorf("configuring session: %w", err)
	}

	var commits atomic.Int32
	var act wordActivity
	// Gate transitions are reported through a small buffered channel: the
	// writer must never block on (or send to) the updates channel directly —
	// it can outlive this function, whose defer closes updates.
	idleCh := make(chan bool, 4)
	notifyIdle := func(idle bool) {
		select {
		case idleCh <- idle:
		default: // best-effort; a full channel just drops the status blink
		}
	}
	writerDone := make(chan error, 1)
	go func() { writerDone <- streamAudio(ctx, cfg, conn, src, &commits, &act, notifyIdle) }()

	type readFrame struct {
		data []byte
		err  error
	}
	reads := make(chan readFrame)
	go func() {
		for {
			_, data, err := conn.Read(ctx)
			reads <- readFrame{data, err}
			if err != nil {
				return
			}
		}
	}()

	texts := map[string]string{}
	closed := map[string]bool{} // committed items whose transcription is still finishing
	completed := int32(0)
	writerEnded := false
	var drainTimer <-chan time.Time

	send := func(u SegmentUpdate) {
		select {
		case updates <- u:
		case <-ctx.Done():
		}
	}

	for {
		select {
		case werr := <-writerDone:
			writerDone = nil
			writerEnded = true
			if werr != nil && ctx.Err() == nil {
				return werr
			}
			if completed >= commits.Load() {
				return nil
			}
			drainTimer = time.After(8 * time.Second) // input ended; wait for stragglers
		case <-drainTimer:
			return nil
		case idle := <-idleCh:
			send(SegmentUpdate{IdleGate: &idle})
		case f := <-reads:
			if f.err != nil {
				if ctx.Err() != nil || writerEnded {
					return nil
				}
				return fmt.Errorf("realtime read: %w", f.err)
			}
			var ev struct {
				Type       string `json:"type"`
				ItemID     string `json:"item_id"`
				Delta      string `json:"delta"`
				Transcript string `json:"transcript"`
				Error      *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(f.data, &ev); err != nil {
				continue
			}
			if cfg.DebugEvents {
				fmt.Fprintf(os.Stderr, "sas: event %s\n", truncate(string(f.data), 300))
			}
			switch ev.Type {
			case "input_audio_buffer.committed":
				// Commits arrive in strict audio order (deltas don't — the
				// server transcribes segments in parallel), so this is where
				// segment order is established downstream.
				closed[ev.ItemID] = true
				act.hasWords.Store(false) // the open segment starts fresh
				act.forceCommit.Store(false)
				send(SegmentUpdate{ItemID: ev.ItemID})
			case "conversation.item.input_audio_transcription.delta":
				texts[ev.ItemID] += ev.Delta
				if !closed[ev.ItemID] {
					// Words for the open segment; late deltas for committed
					// segments mustn't count or they'd trigger spurious commits.
					act.lastDeltaNano.Store(time.Now().UnixNano())
					act.hasWords.Store(true)
					if cfg.MaxSentences > 0 && sentenceCount(texts[ev.ItemID]) >= cfg.MaxSentences {
						act.forceCommit.Store(true) // runaway monologue — cut without waiting for a pause
					}
				}
				send(SegmentUpdate{ItemID: ev.ItemID, Text: texts[ev.ItemID]})
			case "conversation.item.input_audio_transcription.completed":
				delete(texts, ev.ItemID)
				delete(closed, ev.ItemID)
				completed++
				send(SegmentUpdate{ItemID: ev.ItemID, Text: ev.Transcript, Final: true})
				if writerEnded && completed >= commits.Load() {
					return nil
				}
			case "error":
				msg := truncate(string(f.data), 300)
				if ev.Error != nil {
					msg = ev.Error.Message
				}
				if strings.Contains(strings.ToLower(msg), "api key") {
					return fmt.Errorf("%w: %s", errBadAPIKey, msg)
				}
				return fmt.Errorf("realtime API error: %s", msg)
			}
		}
	}
}

// streamAudio appends PCM to the input buffer in ~120 ms batches and commits a
// segment once the open segment has words but no new delta has arrived for
// --word-gap ms. Wordless audio (music, silence) never commits on its own — it
// just accumulates into whatever segment the next speech ends up in.
func streamAudio(ctx context.Context, cfg Config, conn *websocket.Conn, src io.Reader, commits *atomic.Int32, act *wordActivity, notifyIdle func(bool)) error {
	wordGap := time.Duration(cfg.WordGapMS) * time.Millisecond
	batch := make([]byte, 0, frameBytes*4)
	pending := 0 // audio bytes sent since the last commit
	var lastCommit time.Time

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		ev := map[string]any{
			"type":  "input_audio_buffer.append",
			"audio": base64.StdEncoding.EncodeToString(batch),
		}
		streamedAudioBytes.Add(int64(len(batch)))
		pending += len(batch)
		batch = batch[:0]
		return writeJSON(ctx, conn, ev)
	}
	commit := func() error {
		if err := flush(); err != nil {
			return err
		}
		if pending < minCommitBytes {
			return nil
		}
		pending = 0
		lastCommit = time.Now()
		commits.Add(1)
		return writeJSON(ctx, conn, map[string]any{"type": "input_audio_buffer.commit"})
	}

	// A silent Mac reaches us in two different ways: near-zero frames (some
	// process rendering digital silence) or no frames at all (nothing
	// rendering — the tap delivers nothing). The gate therefore closes on
	// wall-clock time since the last audible frame, and a reader goroutine
	// feeds frames so this loop can also wake on a timer when the tap is
	// fully quiet — to close the gate, send keepalives, and still commit a
	// trailing segment on its word gap.
	type readResult struct {
		data []byte
		err  error
	}
	frames := make(chan readResult, 4)
	go func() {
		for {
			f := make([]byte, frameBytes)
			if _, err := io.ReadFull(src, f); err != nil {
				select {
				case frames <- readResult{nil, err}:
				case <-ctx.Done():
				}
				return
			}
			select {
			case frames <- readResult{f, nil}:
			case <-ctx.Done():
				return
			}
		}
	}()

	commitCheck := func() error {
		// The lastCommit guard covers the gap before the server's committed
		// event resets hasWords — without it we'd re-commit immediately.
		if act.hasWords.Load() && time.Since(lastCommit) >= wordGap {
			gapElapsed := time.Since(time.Unix(0, act.lastDeltaNano.Load())) >= wordGap
			if gapElapsed || act.forceCommit.Load() {
				act.forceCommit.Store(false)
				return commit()
			}
		}
		return nil
	}

	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	lastAudible := time.Now()
	var lastKeepalive time.Time
	gateClosed := false
	closeGate := func() error {
		gateClosed = true
		lastKeepalive = time.Now()
		notifyIdle(true)
		// Don't leave real audio parked in the batch for the whole idle
		// stretch — it belongs to the audible period that just ended.
		return flush()
	}

	for {
		var err error
		select {
		case <-ctx.Done():
			return nil
		case r := <-frames:
			if r.err != nil {
				// Input ended. Commit whatever is buffered unconditionally:
				// with --input the whole file may upload before any delta
				// arrives, so hasWords can't be trusted here.
				commit()
				return nil
			}
			if framePeak(r.data) >= silencePeak {
				lastAudible = time.Now()
				if gateClosed {
					gateClosed = false
					notifyIdle(false)
				}
			}
			switch {
			case !gateClosed && time.Since(lastAudible) >= silenceHold:
				err = closeGate()
				gatedAudioBytes.Add(int64(len(r.data)))
			case gateClosed:
				// Idle: don't pay to stream silence. One frame every
				// gatedKeepalive holds the session open.
				if time.Since(lastKeepalive) >= gatedKeepalive {
					lastKeepalive = time.Now()
					batch = append(batch, r.data...)
					err = flush()
				} else {
					gatedAudioBytes.Add(int64(len(r.data)))
				}
			default:
				batch = append(batch, r.data...)
				if len(batch) >= frameBytes*4 {
					err = flush()
				}
			}
		case <-tick.C:
			if !gateClosed && time.Since(lastAudible) >= silenceHold {
				err = closeGate()
			} else if gateClosed && time.Since(lastKeepalive) >= gatedKeepalive {
				// No source frames to forward — synthesize a silent one.
				lastKeepalive = time.Now()
				batch = append(batch, make([]byte, frameBytes)...)
				err = flush()
			}
		}
		if err == nil {
			err = commitCheck()
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}

func writeJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, data)
}
