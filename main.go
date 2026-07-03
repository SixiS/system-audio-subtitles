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
	VADThreshold float64
	SilenceCutMS int
	StreamDelay  string
	DebugEvents  bool
}

func main() {
	var cfg Config
	flag.StringVar(&cfg.TargetLang, "target-lang", "en", "language to translate subtitles into")
	flag.StringVar(&cfg.SourceLang, "source-lang", "", "optional source language hint for transcription (ISO 639-1)")
	flag.StringVar(&cfg.Helper, "helper", "bin/audiotap", "path to the audiotap capture helper")
	flag.StringVar(&cfg.Input, "input", "", "dev mode: read a 24 kHz mono s16 WAV file instead of live capture")
	flag.StringVar(&cfg.Record, "record", "", "capture system audio to this WAV file and exit")
	flag.IntVar(&cfg.Seconds, "seconds", 5, "duration for --record")
	flag.BoolVar(&cfg.ShowOriginal, "show-original", false, "also show the untranslated text")
	flag.BoolVar(&cfg.NoTranslate, "no-translate", false, "transcription only")
	flag.Float64Var(&cfg.VADThreshold, "vad-threshold", 0.01, "RMS level above which a frame counts as speech")
	flag.IntVar(&cfg.SilenceCutMS, "silence-cut", 510, "trailing silence that ends a segment, in milliseconds")
	flag.StringVar(&cfg.StreamDelay, "stream-delay", "low", "realtime delay/accuracy setting: minimal|low|medium|high|xhigh")
	flag.BoolVar(&cfg.DebugEvents, "debug-events", false, "log raw realtime API events to stderr")
	flag.Parse()

	if cfg.SilenceCutMS < 90 {
		cfg.SilenceCutMS = 90
	}

	if cfg.Record != "" {
		if err := recordWAV(cfg.Helper, cfg.Record, cfg.Seconds); err != nil {
			fatal(err.Error())
		}
		return
	}

	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		fatal("OPENAI_API_KEY is not set")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := runStream(ctx, cfg, key); err != nil {
		fatal(err.Error())
	}
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "sas: "+msg)
	os.Exit(1)
}
