package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

const (
	realtimeURL   = "wss://api.openai.com/v1/realtime?intent=transcription"
	realtimeModel = "gpt-realtime-whisper"
)

var streamedAudioBytes atomic.Int64

// SegmentUpdate carries the full accumulated source text for one speech
// segment (realtime item). Final marks the server's completed transcript.
type SegmentUpdate struct {
	ItemID string
	Text   string
	Final  bool
}

// streamTranscribe pipes PCM from src to the Realtime transcription API and
// emits SegmentUpdates as delta/completed events arrive. Segmentation is
// manual: gpt-realtime-whisper streams deltas continuously, and our energy VAD
// decides when to commit (ending a segment) after --silence-cut of silence.
func streamTranscribe(ctx context.Context, cfg Config, key string, src io.Reader, updates chan<- SegmentUpdate) error {
	defer close(updates)

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+key)
	conn, _, err := websocket.Dial(ctx, realtimeURL, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
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
	writerDone := make(chan error, 1)
	go func() { writerDone <- streamAudio(ctx, cfg, conn, src, &commits) }()

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
				send(SegmentUpdate{ItemID: ev.ItemID})
			case "conversation.item.input_audio_transcription.delta":
				texts[ev.ItemID] += ev.Delta
				send(SegmentUpdate{ItemID: ev.ItemID, Text: texts[ev.ItemID]})
			case "conversation.item.input_audio_transcription.completed":
				delete(texts, ev.ItemID)
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
				return fmt.Errorf("realtime API error: %s", msg)
			}
		}
	}
}

// streamAudio appends PCM to the input buffer in ~120 ms batches and commits
// a segment once the energy VAD sees --silence-cut of trailing silence (only
// if the segment actually contained speech).
func streamAudio(ctx context.Context, cfg Config, conn *websocket.Conn, src io.Reader, commits *atomic.Int32) error {
	silenceCutFrames := cfg.SilenceCutMS / frameMS
	frame := make([]byte, frameBytes)
	batch := make([]byte, 0, frameBytes*4)
	var silenceRun, speechFrames int

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		ev := map[string]any{
			"type":  "input_audio_buffer.append",
			"audio": base64.StdEncoding.EncodeToString(batch),
		}
		streamedAudioBytes.Add(int64(len(batch)))
		batch = batch[:0]
		return writeJSON(ctx, conn, ev)
	}
	commit := func() error {
		if speechFrames < minSpeechFrames {
			return nil // nothing worth transcribing since the last commit
		}
		if err := flush(); err != nil {
			return err
		}
		speechFrames = 0
		commits.Add(1)
		return writeJSON(ctx, conn, map[string]any{"type": "input_audio_buffer.commit"})
	}

	for ctx.Err() == nil {
		if _, err := io.ReadFull(src, frame); err != nil {
			flush()
			commit()
			return nil // input ended
		}
		batch = append(batch, frame...)
		if frameRMS(frame) >= cfg.VADThreshold {
			speechFrames++
			silenceRun = 0
		} else {
			silenceRun++
		}
		if len(batch) >= frameBytes*4 {
			if err := flush(); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
		if silenceRun == silenceCutFrames {
			if err := commit(); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
	}
	return nil
}

func writeJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, data)
}
