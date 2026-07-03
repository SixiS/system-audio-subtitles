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
	Model        string
	Helper       string
	Input        string
	Record       string
	Seconds      int
	Fast         bool
	ShowOriginal bool
	NoTranslate  bool
	ChunkDebug   bool
	Timing       bool
	VADThreshold float64
	MaxChunkSec  float64
	SilenceCutMS int
	Stream       bool
	Window       bool
	StreamDelay  string
	DebugEvents  bool
}

func main() {
	var cfg Config
	flag.StringVar(&cfg.TargetLang, "target-lang", "en", "language to translate subtitles into")
	flag.StringVar(&cfg.SourceLang, "source-lang", "", "optional source language hint for transcription (ISO 639-1)")
	flag.StringVar(&cfg.Model, "model", "gpt-4o-mini-transcribe", "transcription model")
	flag.StringVar(&cfg.Helper, "helper", "bin/audiotap", "path to the audiotap capture helper")
	flag.StringVar(&cfg.Input, "input", "", "dev mode: read a 16 kHz mono s16 WAV file instead of live capture")
	flag.StringVar(&cfg.Record, "record", "", "capture system audio to this WAV file and exit")
	flag.IntVar(&cfg.Seconds, "seconds", 5, "duration for --record")
	flag.BoolVar(&cfg.Fast, "fast", false, "single-call whisper-1 mode (transcribe+translate, English output only)")
	flag.BoolVar(&cfg.ShowOriginal, "show-original", false, "also print the untranslated text (dimmed)")
	flag.BoolVar(&cfg.NoTranslate, "no-translate", false, "transcription only")
	flag.BoolVar(&cfg.ChunkDebug, "chunk-debug", false, "dev mode: log VAD chunk boundaries instead of calling the API")
	flag.Float64Var(&cfg.VADThreshold, "vad-threshold", 0.01, "RMS level above which a frame counts as speech")
	flag.Float64Var(&cfg.MaxChunkSec, "max-chunk", 8, "hard cut for continuous speech, in seconds (smaller = lower latency, choppier context)")
	flag.IntVar(&cfg.SilenceCutMS, "silence-cut", 510, "trailing silence that ends a chunk, in milliseconds")
	flag.BoolVar(&cfg.Timing, "timing", false, "log per-chunk audio length and API latency to stderr")
	flag.BoolVar(&cfg.Stream, "stream", false, "use the Realtime API: live deltas + progressive translations")
	flag.BoolVar(&cfg.Window, "window", false, "render subtitles in a floating native window (requires --stream)")
	flag.StringVar(&cfg.StreamDelay, "stream-delay", "low", "realtime delay/accuracy setting: minimal|low|medium|high|xhigh")
	flag.BoolVar(&cfg.DebugEvents, "debug-events", false, "log raw realtime API events to stderr")
	flag.Parse()

	if cfg.MaxChunkSec < 0.5 {
		cfg.MaxChunkSec = 0.5
	}
	if cfg.SilenceCutMS < 90 {
		cfg.SilenceCutMS = 90
	}

	targetLangSet := false
	modelSet := false
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "target-lang":
			targetLangSet = true
		case "model":
			modelSet = true
		}
	})
	if cfg.Fast && targetLangSet {
		fatal("--fast uses whisper-1's translations endpoint, which only outputs English; --target-lang cannot be combined with it")
	}
	if cfg.Fast && cfg.NoTranslate {
		fatal("--fast and --no-translate are mutually exclusive")
	}
	if cfg.Fast && modelSet {
		fmt.Fprintln(os.Stderr, "sas: note: --model is ignored with --fast (whisper-1 is used)")
	}
	if cfg.Stream && cfg.Fast {
		fatal("--fast is a chunked-mode flag; --stream already translates progressively")
	}
	if cfg.Window && !cfg.Stream {
		fatal("--window requires --stream")
	}
	if cfg.Stream && modelSet {
		fmt.Fprintf(os.Stderr, "sas: note: --model is ignored with --stream (%s is used)\n", realtimeModel)
	}

	if cfg.Record != "" {
		if err := recordWAV(cfg.Helper, cfg.Record, cfg.Seconds); err != nil {
			fatal(err.Error())
		}
		return
	}

	key := os.Getenv("OPENAI_API_KEY")
	if key == "" && !cfg.ChunkDebug {
		fatal("OPENAI_API_KEY is not set")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.Stream {
		if err := runStream(ctx, cfg, key); err != nil {
			fatal(err.Error())
		}
		return
	}
	if err := run(ctx, cfg, key); err != nil {
		fatal(err.Error())
	}
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "sas: "+msg)
	os.Exit(1)
}
