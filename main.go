package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

type Config struct {
	TargetLang   string
	SourceLang   string
	Helper       string
	Input        string
	Record       string
	Seconds      int
	ShowOriginal bool
	NoTranslate  bool
	WordGapMS    int
	MaxSentences int
	StreamDelay  string
	DockIcon     bool
	DebugEvents  bool
}

func main() {
	var cfg Config
	flag.StringVar(&cfg.TargetLang, "target-lang", "en", "language to translate subtitles into")
	flag.StringVar(&cfg.SourceLang, "source-lang", "", "optional source language hint for transcription (ISO 639-1)")
	flag.StringVar(&cfg.Helper, "helper", "", "path to the audiotap capture helper (default: next to the sas binary, else bin/audiotap)")
	flag.StringVar(&cfg.Input, "input", "", "dev mode: read a 24 kHz mono s16 WAV file instead of live capture")
	flag.StringVar(&cfg.Record, "record", "", "capture system audio to this WAV file and exit")
	flag.IntVar(&cfg.Seconds, "seconds", 5, "duration for --record")
	flag.BoolVar(&cfg.ShowOriginal, "show-original", true, "also show the untranslated text")
	flag.BoolVar(&cfg.NoTranslate, "no-translate", false, "transcription only")
	flag.IntVar(&cfg.WordGapMS, "word-gap", 1000, "milliseconds without new transcribed words that ends a segment")
	flag.IntVar(&cfg.MaxSentences, "max-sentences", 3, "sentences in one live segment before it is force-cut into history (0 = no limit)")
	flag.StringVar(&cfg.StreamDelay, "stream-delay", "low", "realtime delay/accuracy setting: minimal|low|medium|high|xhigh")
	flag.BoolVar(&cfg.DockIcon, "dock-icon", true, "show a Dock icon while running")
	flag.BoolVar(&cfg.DebugEvents, "debug-events", false, "log raw realtime API events to stderr")
	flag.Parse()

	if cfg.Helper == "" {
		cfg.Helper = defaultHelper()
	}

	// Stored preferences (edited via the window's Preferences dialog) fill in
	// everything the user didn't set explicitly on the command line.
	if p := loadPrefs(); p != nil {
		set := map[string]bool{}
		flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
		if !set["target-lang"] && p.TargetLang != "" {
			cfg.TargetLang = p.TargetLang
		}
		if !set["source-lang"] {
			cfg.SourceLang = p.SourceLang
		}
		if !set["show-original"] {
			cfg.ShowOriginal = p.ShowOriginal
		}
		if !set["no-translate"] {
			cfg.NoTranslate = p.NoTranslate
		}
		if !set["stream-delay"] && p.StreamDelay != "" {
			cfg.StreamDelay = p.StreamDelay
		}
		if !set["dock-icon"] {
			cfg.DockIcon = p.DockIcon
		}
		if !set["word-gap"] && p.WordGapMS > 0 {
			cfg.WordGapMS = p.WordGapMS
		}
		if !set["max-sentences"] && p.MaxSentences > 0 {
			cfg.MaxSentences = p.MaxSentences
		}
	}

	if cfg.WordGapMS < 150 {
		cfg.WordGapMS = 150 // below this, normal gaps between delta batches cause spurious cuts
	}

	if cfg.Record != "" {
		if err := recordWAV(cfg.Helper, cfg.Record, cfg.Seconds); err != nil {
			fatal(err.Error())
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := runStream(ctx, cfg); err != nil {
		fatal(err.Error())
	}
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "sas: "+msg)
	os.Exit(1)
}
