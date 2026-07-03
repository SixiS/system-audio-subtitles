package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

type Segment struct {
	ItemID      string
	Source      string
	Translation string
	SrcFinal    bool
	TransFinal  bool
}

type windowSeg struct {
	Source      string `json:"source,omitempty"`
	Translation string `json:"translation"`
	Final       bool   `json:"final"`
}

// winEvent is a message from the window app on its stdout: user actions from
// the API-key prompt and the menu-bar settings menu.
type winEvent struct {
	Type string `json:"type"` // "key" (set/update the API key) or "clear_key"
	Key  string `json:"key"`
}

// display drives the native subtitle window (helper/subtitlewindow.swift) over
// a JSON-lines stdin protocol. Finalize is called exactly once per segment when
// both its source and translation are settled; RenderLive replaces the pinned
// live area after every change. User actions in the window arrive on Events.
type display struct {
	cfg    Config
	cmd    *exec.Cmd
	pipe   io.WriteCloser
	in     *bufio.Writer
	done   chan struct{}
	Events chan winEvent
}

// onExit is invoked if the window goes away on its own (closed by the user) so
// the pipeline can shut down instead of running headless.
func newDisplay(cfg Config, onExit func()) (*display, error) {
	path := filepath.Join(filepath.Dir(cfg.Helper), "subtitle-window")
	cmd := exec.Command(path)
	cmd.Stderr = os.Stderr
	pipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w (run `make` first?)", path, err)
	}
	w := &display{
		cfg: cfg, cmd: cmd, pipe: pipe, in: bufio.NewWriter(pipe),
		done: make(chan struct{}), Events: make(chan winEvent, 8),
	}
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			var ev winEvent
			if json.Unmarshal(sc.Bytes(), &ev) != nil {
				continue
			}
			select {
			case w.Events <- ev:
			default: // never block the window on a full channel
			}
		}
	}()
	go func() {
		cmd.Wait()
		close(w.done)
		if onExit != nil {
			onExit()
		}
	}()
	return w, nil
}

// RequestKey makes the window show the first-boot API-key prompt; the answer
// comes back as a "key" event.
func (w *display) RequestKey() {
	w.sendJSON(map[string]any{"type": "need_key"})
}

// ShowError replaces the live area with a red error message. It stays until
// the next live render.
func (w *display) ShowError(msg string) {
	w.sendJSON(map[string]any{"type": "error", "message": msg})
}

func (w *display) sendJSON(v any) {
	data, _ := json.Marshal(v)
	w.in.Write(data)
	w.in.WriteByte('\n')
	w.in.Flush()
}

// Finalize appends to the window's scrollable history.
func (w *display) Finalize(s Segment) {
	msg := map[string]any{
		"type":        "append",
		"translation": s.Translation,
		"time":        time.Now().Format("15:04:05"),
	}
	if w.cfg.ShowOriginal && s.Source != "" && s.Source != s.Translation {
		msg["source"] = s.Source
	}
	if s.Translation != "" {
		fmt.Println(s.Translation) // keep a plain transcript in the terminal too
	}
	w.sendJSON(msg)
}

// RenderLive replaces the window's pinned live area.
func (w *display) RenderLive(segs []Segment) {
	out := make([]windowSeg, 0, len(segs))
	for _, s := range segs {
		if s.Source == "" && s.Translation == "" {
			continue // committed but no transcript yet — nothing to show
		}
		ws := windowSeg{Translation: s.Translation, Final: s.TransFinal}
		if ws.Translation == "" {
			ws.Translation = "…"
		}
		if w.cfg.ShowOriginal {
			ws.Source = s.Source
		}
		out = append(out, ws)
	}
	w.sendJSON(map[string]any{"type": "live", "segments": out})
}

func (w *display) Close() {
	w.pipe.Close() // EOF makes the window app terminate itself
	select {
	case <-w.done:
	case <-time.After(2 * time.Second):
		w.cmd.Process.Kill()
		<-w.done
	}
}
