package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

type Segment struct {
	ItemID      string
	Source      string
	Translation string
	SrcFinal    bool
	TransFinal  bool
}

// Display renders streaming subtitle state. Finalize is called exactly once
// per segment when both its source and translation are settled; RenderLive is
// called with the current in-progress tail after every change.
type Display interface {
	Finalize(Segment)
	RenderLive([]Segment)
	Close()
}

// onExit is invoked if the display goes away on its own (window closed by the
// user) so the pipeline can shut down instead of running headless.
func newDisplay(cfg Config, onExit func()) (Display, error) {
	if cfg.Window {
		return newWindowDisplay(cfg, onExit)
	}
	return newTermDisplay(cfg), nil
}

// --- terminal ---

type termDisplay struct {
	cfg        Config
	ansi       bool
	width      int
	liveLines  int
	dim, reset string
}

func newTermDisplay(cfg Config) *termDisplay {
	t := &termDisplay{cfg: cfg, width: 100}
	if stdoutIsTTY() {
		t.ansi = true
		t.dim, t.reset = "\x1b[2m", "\x1b[0m"
	}
	if c, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && c > 20 {
		t.width = c
	}
	return t
}

func (t *termDisplay) clearLive() {
	if t.ansi && t.liveLines > 0 {
		fmt.Printf("\x1b[%dF\x1b[J", t.liveLines)
	}
	t.liveLines = 0
}

func (t *termDisplay) Finalize(s Segment) {
	t.clearLive()
	if t.cfg.ShowOriginal && s.Source != "" && s.Source != s.Translation {
		fmt.Println(t.dim + s.Source + t.reset)
	}
	if s.Translation != "" {
		fmt.Println(s.Translation)
	}
}

func (t *termDisplay) RenderLive(segs []Segment) {
	if !t.ansi {
		return // piped output gets finals only
	}
	t.clearLive()
	for _, s := range segs {
		if t.cfg.ShowOriginal && s.Source != "" {
			fmt.Println(t.dim + clipTail(s.Source, t.width-2) + t.reset)
			t.liveLines++
		}
		line := s.Translation
		if line == "" {
			line = "…"
		}
		suffix := ""
		if !s.TransFinal {
			suffix = t.dim + " ⋯" + t.reset
		}
		fmt.Println(clipTail(line, t.width-4) + suffix)
		t.liveLines++
	}
}

func (t *termDisplay) Close() {}

// clipTail keeps live lines to one terminal row, preferring the most recent
// words (wrapping would break the cursor-up redraw math).
func clipTail(s string, max int) string {
	r := []rune(s)
	if max < 1 || len(r) <= max {
		return s
	}
	return "…" + string(r[len(r)-max+1:])
}

// --- native window ---

type windowDisplay struct {
	cfg    Config
	cmd    *exec.Cmd
	pipe   io.WriteCloser
	in     *bufio.Writer
	done   chan struct{}
	finals []Segment
	live   []Segment
}

type windowSeg struct {
	Source      string `json:"source,omitempty"`
	Translation string `json:"translation"`
	Final       bool   `json:"final"`
}

func newWindowDisplay(cfg Config, onExit func()) (*windowDisplay, error) {
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
	w := &windowDisplay{cfg: cfg, cmd: cmd, pipe: pipe, in: bufio.NewWriter(pipe), done: make(chan struct{})}
	go func() {
		cmd.Wait()
		close(w.done)
		if onExit != nil {
			onExit()
		}
	}()
	return w, nil
}

func (w *windowDisplay) send() {
	all := append(append([]Segment{}, w.finals...), w.live...)
	segs := make([]windowSeg, 0, len(all))
	for _, s := range all {
		ws := windowSeg{Translation: s.Translation, Final: s.TransFinal}
		if ws.Translation == "" {
			ws.Translation = "…"
		}
		if w.cfg.ShowOriginal {
			ws.Source = s.Source
		}
		segs = append(segs, ws)
	}
	data, _ := json.Marshal(map[string]any{"segments": segs})
	w.in.Write(data)
	w.in.WriteByte('\n')
	w.in.Flush()
}

func (w *windowDisplay) Finalize(s Segment) {
	w.finals = append(w.finals, s)
	if len(w.finals) > 2 {
		w.finals = w.finals[1:]
	}
	if s.Translation != "" {
		fmt.Println(s.Translation) // keep a plain transcript in the terminal too
	}
	w.send()
}

func (w *windowDisplay) RenderLive(segs []Segment) {
	w.live = segs
	w.send()
}

func (w *windowDisplay) Close() {
	w.pipe.Close() // EOF makes the window app terminate itself
	select {
	case <-w.done:
	case <-time.After(2 * time.Second):
		w.cmd.Process.Kill()
		<-w.done
	}
}
