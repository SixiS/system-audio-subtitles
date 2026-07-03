package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"
)

const transcribeWorkers = 3

type Result struct {
	Seq        int
	Original   string
	Translated string
	Err        error
}

func run(ctx context.Context, cfg Config, key string) error {
	src, cleanup, err := openPCMSource(ctx, cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	chunks := make(chan Chunk, 8)
	go chunker(ctx, src, cfg, chunks)

	if cfg.ChunkDebug {
		for c := range chunks {
			secs := float64(len(c.PCM)) / (sampleRate * bytesPerSample)
			peak, rms := analyze(c.PCM)
			fmt.Fprintf(os.Stderr, "sas: chunk %d: %.2fs (peak %.3f, rms %.4f)\n", c.Seq, secs, peak, rms)
		}
		return nil
	}

	client := newClient(key)
	results := make(chan Result, 64)

	var (
		mu             sync.Mutex
		prevTranscript string
		history        []string // recent "source → translation" pairs
		audioSeconds   float64
		chunkCount     int
	)

	var wg sync.WaitGroup
	for i := 0; i < transcribeWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for chunk := range chunks {
				res := Result{Seq: chunk.Seq}
				mu.Lock()
				prompt := prevTranscript
				recent := append([]string(nil), history...)
				mu.Unlock()

				start := time.Now()
				if cfg.Fast {
					res.Translated, res.Err = client.TranslateAudio(ctx, chunk.PCM, prompt)
					res.Original = res.Translated
				} else {
					res.Original, res.Err = client.Transcribe(ctx, chunk.PCM, cfg.Model, cfg.SourceLang, prompt)
					if res.Err == nil && res.Original != "" && !cfg.NoTranslate {
						res.Translated, res.Err = client.TranslateText(ctx, res.Original, cfg.TargetLang, recent, false)
					} else {
						res.Translated = res.Original
					}
				}
				if cfg.Timing {
					fmt.Fprintf(os.Stderr, "sas: chunk %d: %.1fs audio, api %.2fs\n",
						chunk.Seq, float64(len(chunk.PCM))/(sampleRate*bytesPerSample), time.Since(start).Seconds())
				}

				mu.Lock()
				audioSeconds += float64(len(chunk.PCM)) / (sampleRate * bytesPerSample)
				chunkCount++
				if res.Err == nil && res.Original != "" {
					prevTranscript = tail(res.Original, 200)
					if res.Translated != "" && !cfg.NoTranslate && !cfg.Fast {
						history = append(history, res.Original+" → "+res.Translated)
						if len(history) > 3 {
							history = history[1:]
						}
					}
				}
				mu.Unlock()

				select {
				case results <- res:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	render(cfg, results)

	mu.Lock()
	defer mu.Unlock()
	if chunkCount > 0 {
		minutes := audioSeconds / 60
		fmt.Fprintf(os.Stderr, "sas: %d chunks, %.1f min of audio sent (~$%.3f + translation tokens)\n",
			chunkCount, minutes, minutes*costPerMinute(cfg))
	}
	return nil
}

func costPerMinute(cfg Config) float64 {
	if cfg.Fast {
		return 0.006 // whisper-1
	}
	switch cfg.Model {
	case "gpt-4o-transcribe":
		return 0.006
	default: // gpt-4o-mini-transcribe
		return 0.003
	}
}

// render prints results in chunk order, buffering any that finish early.
func render(cfg Config, results <-chan Result) {
	dim, reset := "\x1b[2m", "\x1b[0m"
	if !stdoutIsTTY() {
		dim, reset = "", ""
	}
	pending := map[int]Result{}
	next := 0
	for r := range results {
		pending[r.Seq] = r
		for {
			cur, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			next++
			if cur.Err != nil {
				fmt.Fprintf(os.Stderr, "sas: chunk %d failed: %v\n", cur.Seq, cur.Err)
				continue
			}
			if cfg.ShowOriginal && cur.Original != "" && cur.Original != cur.Translated {
				fmt.Println(dim + cur.Original + reset)
			}
			if cur.Translated != "" {
				fmt.Println(cur.Translated)
			}
		}
	}
}

func stdoutIsTTY() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// tail returns the last n runes of s without splitting characters.
func tail(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[len(runes)-n:])
}
