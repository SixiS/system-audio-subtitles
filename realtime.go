package main

import (
	"bytes"
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

	// Direct speech translation (--realtime-translate): one purpose-built
	// interpreter model replaces the transcribe-then-translate pipeline.
	translateURL = "wss://api.openai.com/v1/realtime/translations?model=gpt-realtime-translate"

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

// sentenceEnders is the punctuation that closes a sentence — the same rule
// the window uses to insert line breaks, so segment cuts match what the user
// sees pile up.
const sentenceEnders = ".!?。！？"

// sentenceCount counts sentence-ending punctuation followed by whitespace
// (or end of text).
func sentenceCount(s string) int {
	n := 0
	rs := []rune(s)
	for i, r := range rs {
		if strings.ContainsRune(sentenceEnders, r) {
			if i == len(rs)-1 || unicode.IsSpace(rs[i+1]) {
				n++
			}
		}
	}
	return n
}

// endsSentence reports whether the text's last non-space rune closes a
// sentence — the natural place to cut a subtitle.
func endsSentence(s string) bool {
	t := strings.TrimRightFunc(s, unicode.IsSpace)
	if t == "" {
		return false
	}
	r := []rune(t)
	return strings.ContainsRune(sentenceEnders, r[len(r)-1])
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
// Translation is only set in --realtime-translate mode, where the model
// streams translated text itself and no separate translation call happens.
// An update with IdleGate set carries no segment at all — it reports the
// idle gate opening (false) or closing (true) so the window can show
// "idling…" while nothing is playing.
type SegmentUpdate struct {
	ItemID      string
	Text        string
	Translation string
	Final       bool
	IdleGate    *bool
	Ready       bool // session established — the window can stop showing "connecting…"
}

// dialRealtime opens a realtime WebSocket. Auth failures map to
// errBadAPIKey — the pipeline's signal to park capture and wait for a
// corrected key — so both stream flavors recover from a bad key the same way.
// The dial gets its own deadline: a hung handshake would otherwise block the
// whole pipeline silently, with the window stuck on "listening…" forever.
func dialRealtime(ctx context.Context, key, url string) (*websocket.Conn, error) {
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+key)
	conn, resp, err := websocket.Dial(dialCtx, url, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			return nil, fmt.Errorf("%w (HTTP %d)", errBadAPIKey, resp.StatusCode)
		}
		return nil, err
	}
	conn.SetReadLimit(1 << 20)
	return conn, nil
}

// readFrame is one WebSocket read; readLoop pumps them into a channel so the
// stream loops can select over reads, writer completion, and timers at once.
type readFrame struct {
	data []byte
	err  error
}

func readLoop(ctx context.Context, conn *websocket.Conn) <-chan readFrame {
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
	return reads
}

// newIdleNotifier returns the channel a stream loop drains and the
// best-effort send the audio writer calls on gate transitions. They go
// through a small buffered channel because the writer must never block on
// (or send to) the updates channel directly — it can outlive the stream
// function, whose defer closes updates.
func newIdleNotifier() (chan bool, func(bool)) {
	idleCh := make(chan bool, 4)
	return idleCh, func(idle bool) {
		select {
		case idleCh <- idle:
		default: // best-effort; a full channel just drops the status blink
		}
	}
}

// sendUpdate delivers u without ever blocking past cancellation.
func sendUpdate(ctx context.Context, updates chan<- SegmentUpdate, u SegmentUpdate) {
	select {
	case updates <- u:
	case <-ctx.Done():
	}
}

// realtimeAPIError classifies a server error event: key problems become
// errBadAPIKey (fixable — the pipeline shows the red error and waits),
// anything else is fatal for the session.
func realtimeAPIError(kind string, raw []byte, msg string) error {
	if msg == "" {
		msg = truncate(string(raw), 300)
	}
	if strings.Contains(strings.ToLower(msg), "api key") {
		return fmt.Errorf("%w: %s", errBadAPIKey, msg)
	}
	return fmt.Errorf("%s error: %s", kind, msg)
}

// streamTranscribe pipes PCM from src to the Realtime transcription API and
// emits SegmentUpdates as delta/completed events arrive. Segmentation is
// manual: gpt-realtime-whisper streams deltas continuously, and a segment is
// committed once its words stop — no new delta for --word-gap ms. Cutting on
// transcription activity rather than acoustic silence means background music
// can't hold a segment open; only speech does.
func streamTranscribe(ctx context.Context, cfg Config, key string, src io.Reader, updates chan<- SegmentUpdate) error {
	defer close(updates)

	conn, err := dialRealtime(ctx, key, realtimeURL)
	if err != nil {
		if errors.Is(err, errBadAPIKey) {
			return err
		}
		return fmt.Errorf("connecting to realtime API: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")

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
	idleCh, notifyIdle := newIdleNotifier()
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- streamAudio(ctx, cfg, conn, src, &commits, &act, notifyIdle, "input_audio_buffer.append")
	}()

	reads := readLoop(ctx, conn)

	texts := map[string]string{}
	closed := map[string]bool{} // committed items whose transcription is still finishing
	completed := int32(0)
	writerEnded := false
	var drainTimer <-chan time.Time

	send := func(u SegmentUpdate) { sendUpdate(ctx, updates, u) }

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
			case "session.created":
				send(SegmentUpdate{Ready: true})
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
				var msg string
				if ev.Error != nil {
					msg = ev.Error.Message
				}
				return realtimeAPIError("realtime API", f.data, msg)
			}
		}
	}
}

// streamAudio appends PCM to the input buffer in ~120 ms batches and commits a
// segment once the open segment has words but no new delta has arrived for
// --word-gap ms. Wordless audio (music, silence) never commits on its own — it
// just accumulates into whatever segment the next speech ends up in.
// commits and act may be nil for sessions with no commit protocol
// (translation sessions are continuous, and streamTranslate cuts segments
// from transcript activity instead); appendEvent names the buffer-append
// message, which the two endpoints namespace differently.
func streamAudio(ctx context.Context, cfg Config, conn *websocket.Conn, src io.Reader, commits *atomic.Int32, act *wordActivity, notifyIdle func(bool), appendEvent string) error {
	wordGap := time.Duration(cfg.WordGapMS) * time.Millisecond
	batch := make([]byte, 0, frameBytes*4)
	pending := 0 // audio bytes sent since the last commit
	var lastCommit time.Time

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		ev := map[string]any{
			"type":  appendEvent,
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
		if commits == nil || pending < minCommitBytes {
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
		if act == nil {
			return nil // no commit protocol on this session
		}
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

// streamTranslate pipes PCM to the Realtime translation API
// (gpt-realtime-translate): one purpose-built interpreter model consumes
// speech and streams back translated text — the gpt-4o-mini translation
// pipeline is not involved at all. When show-original is on, the
// input-transcription add-on rides along to supply the source text (billing
// gpt-realtime-whisper on top of the translation rate); with it off the mode
// costs a flat $0.034/min. The stream has no
// segment structure (no item ids, no completed events, translated speech
// audio we ignore), so subtitles are cut client-side: a segment finalizes
// once the translation ends a sentence and no transcript delta has arrived
// for --word-gap ms — or unconditionally after three gaps, at
// --max-sentences, and when the idle gate closes.
func streamTranslate(ctx context.Context, cfg Config, key string, src io.Reader, updates chan<- SegmentUpdate) error {
	defer close(updates)

	conn, err := dialRealtime(ctx, key, translateURL)
	if err != nil {
		if errors.Is(err, errBadAPIKey) {
			return err
		}
		return fmt.Errorf("connecting to realtime translation API: %w", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "done")

	audio := map[string]any{
		"output": map[string]any{"language": cfg.TargetLang},
	}
	if cfg.ShowOriginal {
		audio["input"] = map[string]any{"transcription": map[string]any{"model": realtimeModel}}
	}
	if err := writeJSON(ctx, conn, map[string]any{
		"type":    "session.update",
		"session": map[string]any{"audio": audio},
	}); err != nil {
		return fmt.Errorf("configuring translation session: %w", err)
	}

	idleCh, notifyIdle := newIdleNotifier()
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- streamAudio(ctx, cfg, conn, src, nil, nil, notifyIdle, "session.input_audio_buffer.append")
	}()

	reads := readLoop(ctx, conn)

	wordGap := time.Duration(cfg.WordGapMS) * time.Millisecond
	source, trans := "", ""
	segN, segID := 0, "xl-0"
	var lastDelta time.Time
	var drainTimer <-chan time.Time

	emit := func(final bool) {
		if source == "" && trans == "" {
			return
		}
		sendUpdate(ctx, updates, SegmentUpdate{ItemID: segID, Text: source, Translation: trans, Final: final})
		if final {
			segN++
			segID = fmt.Sprintf("xl-%d", segN)
			source, trans = "", ""
		}
	}

	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()

	for {
		select {
		case werr := <-writerDone:
			writerDone = nil
			if werr != nil && ctx.Err() == nil {
				return werr
			}
			// Input ended (--input mode). session.close flushes the model's
			// pipeline; the timer bounds the wait for its last deltas.
			writeJSON(ctx, conn, map[string]any{"type": "session.close"})
			drainTimer = time.After(10 * time.Second)
		case <-drainTimer:
			emit(true)
			return nil
		case idle := <-idleCh:
			// No cut here: the model's last deltas trail the gate closing by
			// a beat, and the ticker below finalizes once they stop.
			sendUpdate(ctx, updates, SegmentUpdate{IdleGate: &idle})
		case f := <-reads:
			if f.err != nil {
				if ctx.Err() != nil || writerDone == nil {
					emit(true)
					return nil
				}
				return fmt.Errorf("realtime translation read: %w", f.err)
			}
			// The model also streams its spoken translation — ~34 KB of
			// base64 several times a second that we have no use for. Skip
			// those events before the JSON decode; nothing else comes close
			// to containing that literal.
			if bytes.Contains(f.data, []byte(`"session.output_audio.delta"`)) {
				continue
			}
			var ev struct {
				Type  string `json:"type"`
				Delta string `json:"delta"`
				Error *struct {
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
			case "session.created":
				sendUpdate(ctx, updates, SegmentUpdate{Ready: true})
			case "session.input_transcript.delta":
				source += ev.Delta
				lastDelta = time.Now()
				emit(false)
			case "session.output_transcript.delta":
				trans += ev.Delta
				lastDelta = time.Now()
				// Runaway monologue — cut at the sentence that just closed
				// instead of waiting for a pause that may never come.
				emit(cfg.MaxSentences > 0 && sentenceCount(trans) >= cfg.MaxSentences)
			case "session.closed":
				emit(true)
				return nil
			case "error":
				var msg string
				if ev.Error != nil {
					msg = ev.Error.Message
				}
				return realtimeAPIError("realtime translation API", f.data, msg)
			}
		case <-tick.C:
			if source == "" && trans == "" {
				continue
			}
			// A finished sentence plus a word gap of quiet is a subtitle;
			// if closing punctuation never comes, cut anyway after three.
			quiet := time.Since(lastDelta)
			if (quiet >= wordGap && endsSentence(trans)) || quiet >= 3*wordGap {
				emit(true)
			}
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
