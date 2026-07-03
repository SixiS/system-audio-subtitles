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

// display drives the native subtitle window (helper/subtitlewindow.swift) over
// a JSON-lines stdin protocol. Finalize is called exactly once per segment when
// both its source and translation are settled; RenderLive replaces the pinned
// live area after every change.
type display struct {
	cfg  Config
	cmd  *exec.Cmd
	pipe io.WriteCloser
	in   *bufio.Writer
	done chan struct{}
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
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w (run `make` first?)", path, err)
	}
	w := &display{cfg: cfg, cmd: cmd, pipe: pipe, in: bufio.NewWriter(pipe), done: make(chan struct{})}
	go func() {
		cmd.Wait()
		close(w.done)
		if onExit != nil {
			onExit()
		}
	}()
	return w, nil
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
