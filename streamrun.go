package main

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// Streaming mode: realtime transcription deltas drive progressive translation.
// Each segment gets provisional translations as it grows (every couple of new
// words) and one authoritative translation of the full sentence once the
// server finalizes it — which replaces whatever provisional text was shown.

type transResult struct {
	itemID  string
	version int
	text    string
	final   bool // translated from the final source transcript
	err     error
}

type segState struct {
	seg             Segment
	version         int // bumps on every source change
	appliedVersion  int
	inflight        bool
	lastLaunchWords int
	finalRequested  bool
}

func runStream(ctx context.Context, cfg Config, key string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	src, cleanup, err := openPCMSource(ctx, cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	display, err := newDisplay(cfg, cancel) // window closed by user → stop the pipeline
	if err != nil {
		return err
	}
	defer display.Close()

	client := newClient(key)
	updates := make(chan SegmentUpdate, 64)
	streamDone := make(chan error, 1)
	go func() { streamDone <- streamTranscribe(ctx, cfg, key, src, updates) }()

	results := make(chan transResult, 64)
	states := map[string]*segState{}
	var order []string
	var history []string
	inflight := 0
	var streamErr error
	streamEnded := false

	launch := func(st *segState) {
		if cfg.NoTranslate || st.inflight || st.seg.TransFinal {
			return
		}
		words := len(strings.Fields(st.seg.Source))
		if words == 0 {
			return
		}
		if !st.seg.SrcFinal && words-st.lastLaunchWords < 2 {
			return // wait for a couple of new words before re-translating
		}
		if st.seg.SrcFinal && st.finalRequested {
			return
		}
		st.inflight = true
		inflight++
		st.lastLaunchWords = words
		st.finalRequested = st.seg.SrcFinal
		id, ver, text, final := st.seg.ItemID, st.version, st.seg.Source, st.seg.SrcFinal
		hist := append([]string(nil), history...)
		go func() {
			tr, err := client.TranslateText(ctx, text, cfg.TargetLang, hist, !final)
			results <- transResult{itemID: id, version: ver, text: tr, final: final, err: err}
		}()
	}

	handleUpdate := func(u SegmentUpdate) {
		st, ok := states[u.ItemID]
		if !ok {
			st = &segState{seg: Segment{ItemID: u.ItemID}}
			states[u.ItemID] = st
			order = append(order, u.ItemID)
		}
		if u.Text == "" && !u.Final {
			return // registration only (committed event) — no text yet
		}
		st.seg.Source = u.Text
		st.version++
		if u.Final {
			st.seg.SrcFinal = true
		}
		if cfg.NoTranslate {
			st.seg.Translation = u.Text
			st.seg.TransFinal = u.Final
			return
		}
		if u.Final && strings.TrimSpace(u.Text) == "" {
			// Noise/music commits produce empty transcripts. There is nothing
			// to translate — finalize immediately or this segment blocks the
			// finalization queue (and history) forever.
			st.seg.Translation = ""
			st.seg.TransFinal = true
			return
		}
		launch(st)
	}

	handleResult := func(r transResult) {
		inflight--
		st, ok := states[r.itemID]
		if !ok {
			return
		}
		st.inflight = false
		if r.err != nil {
			if ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "sas: translation failed: %v\n", r.err)
			}
			if r.final {
				// Don't stall the pipeline: fall back to the source text.
				if st.seg.Translation == "" {
					st.seg.Translation = st.seg.Source
				}
				st.seg.TransFinal = true
			}
			return
		}
		if r.version >= st.appliedVersion && !st.seg.TransFinal {
			st.appliedVersion = r.version
			st.seg.Translation = r.text
			if r.final && r.version == st.version {
				st.seg.TransFinal = true
				history = append(history, st.seg.Source+" → "+r.text)
				if len(history) > 3 {
					history = history[1:]
				}
			}
		}
		launch(st) // source may have advanced (or finalized) while translating
	}

	sync := func() {
		for len(order) > 0 {
			st := states[order[0]]
			if !st.seg.SrcFinal || !st.seg.TransFinal {
				break
			}
			if strings.TrimSpace(st.seg.Translation) != "" || strings.TrimSpace(st.seg.Source) != "" {
				display.Finalize(st.seg) // empty segments retire silently
			}
			delete(states, order[0])
			order = order[1:]
		}
		start := 0
		if len(order) > 3 {
			start = len(order) - 3
		}
		live := make([]Segment, 0, 3)
		for _, id := range order[start:] {
			live = append(live, states[id].seg)
		}
		display.RenderLive(live)
	}

	for {
		if streamEnded && updates == nil && inflight == 0 {
			for _, id := range order { // flush whatever never finalized
				st := states[id]
				if st.seg.Translation == "" {
					st.seg.Translation = st.seg.Source
				}
				st.seg.SrcFinal, st.seg.TransFinal = true, true
			}
			sync()
			break
		}
		select {
		case <-ctx.Done():
			fmt.Println()
			return streamErr
		case err := <-streamDone:
			streamErr = err
			streamEnded = true
			streamDone = nil
		case u, ok := <-updates:
			if !ok {
				updates = nil
				continue
			}
			handleUpdate(u)
			sync()
		case r := <-results:
			handleResult(r)
			sync()
		}
	}

	minutes := float64(streamedAudioBytes.Load()) / (sampleRate * bytesPerSample) / 60
	fmt.Fprintf(os.Stderr, "sas: %.1f min of audio streamed\n", minutes)
	return streamErr
}
