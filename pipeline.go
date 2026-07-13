package main

import (
	"context"
	"errors"
	"fmt"
	"io"
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

// ensureKey returns the stored API key. On first boot (nothing stored) it asks
// the window to prompt — a text field, plus a "use $OPENAI_API_KEY" button when
// the environment has one — and stores the answer. Returns "" if the user
// dismissed the prompt (quit) instead of providing a key.
func ensureKey(ctx context.Context, display *display, cfg *Config) (string, error) {
	if key := loadKey(); key != "" {
		return key, nil
	}
	display.RequestKey()
	for {
		select {
		case <-ctx.Done():
			return "", nil // window closed at the prompt
		case ev := <-display.Events:
			switch ev.Type {
			case "key":
				if ev.Key == "" {
					continue
				}
				if err := saveKey(ev.Key); err != nil {
					fmt.Fprintf(os.Stderr, "sas: storing API key: %v\n", err)
				}
				return ev.Key, nil
			case "clear_key":
				clearKey()
				return "", nil
			case "set_prefs":
				if ev.Prefs != nil { // preferences edited while the key prompt is up
					applyPrefs(cfg, *ev.Prefs)
					savePrefs(prefsFromConfig(*cfg))
					display.setConfig(*cfg)
					display.SendPrefs(*cfg)
				}
			}
		}
	}
}

func runStream(ctx context.Context, cfg Config) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	display, err := newDisplay(cfg, cancel) // window closed by user → stop the pipeline
	if err != nil {
		return err
	}
	defer display.Close()
	display.SendPrefs(cfg) // the Preferences dialog needs the live values

	key, err := ensureKey(ctx, display, &cfg)
	if err != nil {
		return err
	}
	if key == "" {
		return nil // prompt dismissed — nothing to do
	}

	src, cleanup, err := openPCMSource(ctx, cfg)
	if err != nil {
		return err
	}
	// cleanup is reassigned when capture restarts after a bad-key wait, so the
	// deferred call must resolve it late.
	defer func() { cleanup() }()

	client := newClient(key)
	var updates chan SegmentUpdate
	var streamDone chan error
	sessionCancel := func() {} // cancels just the realtime session, not the pipeline
	startStream := func(c Config, k string, s io.Reader) {
		sctx, cancel := context.WithCancel(ctx)
		sessionCancel = cancel
		u := make(chan SegmentUpdate, 64)
		d := make(chan error, 1)
		updates, streamDone = u, d
		display.SetIdle(false) // a fresh session starts with the gate open
		stream := streamTranscribe
		if c.sessionTranslates() {
			stream = streamTranslate
		}
		go func() { d <- stream(sctx, c, k, s, u) }()
	}
	startStream(cfg, key, src)

	results := make(chan transResult, 64)
	states := map[string]*segState{}
	var order []string
	var history []string
	inflight := 0
	var streamErr error
	streamEnded := false
	waitingForKey := false
	pendingRestart := false

	// A new realtime session assigns unrelated item IDs, so segments left open
	// by the old one would block the finalization queue forever.
	retireOpen := func() {
		for _, id := range order {
			st := states[id]
			if st.seg.Translation == "" {
				st.seg.Translation = st.seg.Source
			}
			st.seg.SrcFinal, st.seg.TransFinal = true, true
		}
	}

	launch := func(st *segState) {
		if cfg.NoTranslate || cfg.sessionTranslates() || st.inflight || st.seg.TransFinal {
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
		lang := cfg.TargetLang // snapshot: cfg is mutable via Preferences
		hist := append([]string(nil), history...)
		go func() {
			tr, err := client.TranslateText(ctx, text, lang, hist, !final)
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
		if u.Text == "" && u.Translation == "" && !u.Final {
			return // registration only (committed event) — no text yet
		}
		st.seg.Source = u.Text
		st.version++
		if u.Final {
			st.seg.SrcFinal = true
		}
		if cfg.sessionTranslates() {
			// The realtime session translates as it goes — its updates carry
			// both transcripts and there is nothing to launch.
			st.seg.Translation = u.Translation
			st.seg.TransFinal = u.Final
			return
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
		case ev := <-display.Events:
			switch ev.Type {
			case "key":
				if ev.Key != "" {
					key = ev.Key
					client.setKey(key)
					if err := saveKey(key); err != nil {
						fmt.Fprintf(os.Stderr, "sas: storing API key: %v\n", err)
					} else {
						fmt.Fprintln(os.Stderr, "sas: API key updated")
					}
					if waitingForKey {
						src, cleanup, err = openPCMSource(ctx, cfg)
						if err != nil {
							return err
						}
						waitingForKey = false
						startStream(cfg, key, src)
						sync() // clears the error back to "listening…"
					} else if !streamEnded {
						// Restart the realtime session so the new key is
						// actually exercised — a broken one must surface as
						// the red error, not silently ride the old auth.
						pendingRestart = true
						sessionCancel()
					}
				}
			case "set_prefs":
				if ev.Prefs != nil {
					before := cfg
					applyPrefs(&cfg, *ev.Prefs)
					display.setConfig(cfg)
					display.SendPrefs(cfg) // echo back the clamped values
					if err := savePrefs(prefsFromConfig(cfg)); err != nil {
						fmt.Fprintf(os.Stderr, "sas: storing preferences: %v\n", err)
					} else {
						fmt.Fprintln(os.Stderr, "sas: preferences updated")
					}
					// Source language, latency, word gap, and max sentences
					// live in the realtime session — restart it to apply
					// them. Everything else (target language, show-original,
					// Dock icon, …) takes effect without one.
					sessionChanged := cfg.SourceLang != before.SourceLang ||
						cfg.StreamDelay != before.StreamDelay ||
						cfg.WordGapMS != before.WordGapMS ||
						cfg.MaxSentences != before.MaxSentences ||
						cfg.sessionTranslates() != before.sessionTranslates() ||
						// In translate mode the target language lives in the
						// realtime session too, not just in REST calls.
						(cfg.sessionTranslates() && cfg.TargetLang != before.TargetLang)
					if sessionChanged && !streamEnded && !waitingForKey {
						pendingRestart = true
						sessionCancel()
					}
				}
			case "clear_key":
				if err := clearKey(); err != nil {
					fmt.Fprintf(os.Stderr, "sas: clearing API key: %v\n", err)
				} else {
					fmt.Fprintln(os.Stderr, "sas: API key cleared")
				}
				cancel()
			}
		case err := <-streamDone:
			if pendingRestart && ctx.Err() == nil {
				// The session was torn down to apply an edited key or new
				// preferences; bring it back up. If the key is broken, this
				// session fails with errBadAPIKey and parks below.
				streamDone = nil
				retireOpen()
				sync()
				startStream(cfg, key, src)
				pendingRestart = false
				continue
			}
			if errors.Is(err, errBadAPIKey) && ctx.Err() == nil {
				// Fixable: park capture, show the problem, and wait for a
				// corrected key from the settings menu.
				cleanup()
				streamDone = nil
				waitingForKey = true
				retireOpen()
				sync()
				fmt.Fprintf(os.Stderr, "sas: %v\n", err)
				display.ShowError("Invalid OpenAI API key — fix it via ⚙ → Edit API Key…")
				continue
			}
			streamErr = err
			streamEnded = true
			streamDone = nil
		case u, ok := <-updates:
			if !ok {
				updates = nil
				continue
			}
			if u.IdleGate != nil {
				if *u.IdleGate {
					fmt.Fprintln(os.Stderr, "sas: idle — silence, audio streaming paused")
				} else {
					fmt.Fprintln(os.Stderr, "sas: audio resumed")
				}
				display.SetIdle(*u.IdleGate)
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
	skipped := float64(gatedAudioBytes.Load()) / (sampleRate * bytesPerSample) / 60
	if skipped >= 0.05 {
		fmt.Fprintf(os.Stderr, "sas: %.1f min of audio streamed (%.1f min of silence not sent)\n", minutes, skipped)
	} else {
		fmt.Fprintf(os.Stderr, "sas: %.1f min of audio streamed\n", minutes)
	}
	return streamErr
}
