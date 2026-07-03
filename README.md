# system audio subtitles

Live subtitles in your terminal for whatever your Mac is playing. Captures
system audio natively (no BlackHole, no ffmpeg), transcribes it with the OpenAI
API, translates it, and prints it as the audio plays.

```
$ ./bin/sas --show-original
audiotap: capturing system audio: 48000 Hz 2ch → 16000 Hz mono s16le on stdout
Goeie aand en al julle luisteraars. Wat ons in die Noord-Kaap gesê het...
Good evening to all your listeners. What we said to the government in the Northern Cape...
```

Subtitles trail the audio by a few seconds (audio is sent in speech-sized
chunks). Cost is roughly $0.20 per hour of audio at the defaults.

## Requirements

- macOS 14.4 or newer (uses Core Audio process taps)
- Xcode Command Line Tools (`xcode-select --install`) — provides `swiftc`
- Go 1.22+
- An OpenAI API key in `OPENAI_API_KEY`

## Build & run

```sh
make                      # builds bin/audiotap, bin/subtitle-window (Swift) and bin/sas (Go)
export OPENAI_API_KEY=sk-...
./bin/sas --stream --window --show-original   # live subtitles in a floating window
./bin/sas                                     # chunked mode, terminal output
```

On first run macOS asks to allow **audiotap** to record system audio — click
Allow. The grant is remembered, but because the helper is ad-hoc signed,
**rebuilding it re-triggers the prompt** (its code hash changes). If a prompt
never appears, check System Settings → Privacy & Security → Screen & System
Audio Recording.

## Usage

```sh
./bin/sas [flags]

--stream              use the OpenAI Realtime API: word-by-word live captions,
                      progressive translations that refine as the sentence grows
--window              floating native subtitle overlay with a close button
                      (requires --stream); drag to move
--stream-delay low    realtime latency/accuracy trade-off: minimal|low|medium|high|xhigh
--target-lang en      language to translate into (default: en)
--source-lang ""      optional source-language hint (ISO 639-1); auto-detect if empty
--model NAME          transcription model (default: gpt-4o-mini-transcribe;
                      gpt-4o-transcribe is more accurate, ~2x the price)
--fast                single-call whisper-1 mode: transcribes and translates in one
                      request (lower latency/cost, older model, English output only —
                      cannot be combined with --target-lang)
--show-original       print the untranslated text dimmed above each subtitle
--no-translate        transcription only
--vad-threshold 0.01  RMS level above which a frame counts as speech
--max-chunk 8         hard cut for continuous speech, in seconds
--silence-cut 510     trailing silence that ends a chunk, in milliseconds
--timing              log per-chunk audio length and API latency to stderr
```

**Streaming vs chunked.** `--stream` is the low-latency mode: audio goes to the
Realtime API (`gpt-realtime-whisper`) over a WebSocket, source-language text
arrives word by word *while the sentence is still being spoken*, and each
segment is translated progressively — a provisional translation every couple of
new words, then one authoritative translation of the full sentence that
replaces it when the segment completes. Segments end at natural pauses
(`--silence-cut`).

Chunked mode (the default, no flag) buffers speech-sized chunks and POSTs them;
its subtitle lag ≈ chunk length + API round-trip, and the round-trip is
~1–2.5 s **regardless of chunk size**, so smaller `--max-chunk` values lower
latency — but transcription quality drops off fast below ~8 s (at 2 s both
models hallucinate freely; language auto-detection breaks even with a
`--source-lang` hint). Treat `--max-chunk 4` as the floor; if latency matters,
use `--stream` instead.

Examples:

```sh
./bin/sas --target-lang de --show-original   # German subtitles + original text
./bin/sas --fast                             # cheapest/fastest, English only
./bin/sas --no-translate --source-lang ja    # Japanese transcript, no translation
```

Press Ctrl-C to stop; a summary of audio minutes sent and estimated cost is
printed on exit.

## How it works

```
┌──────────────┐   ┌─────────┐   ┌──────────────┐   ┌────────────┐   ┌──────────┐
│ audiotap     │──▶│ chunker │──▶│ transcriber  │──▶│ translator │──▶│ terminal │
│ (Swift, PCM) │   │ (VAD)   │   │ (OpenAI STT) │   │ (OpenAI)   │   │ renderer │
└──────────────┘   └─────────┘   └──────────────┘   └────────────┘   └──────────┘
```

- **`helper/audiotap.swift`** — the only platform-specific code. Creates a
  global Core Audio process tap (macOS 14.4+), wraps it in a private aggregate
  device, converts to 16 kHz mono s16le, and writes raw PCM to stdout.
- **The Go pipeline** (`bin/sas`) spawns the helper and runs concurrent stages
  connected by channels:
  - *chunker* — energy-based VAD; cuts chunks at ~500 ms silence gaps or an 8 s
    hard limit, and drops silent stretches entirely (whisper-style models
    hallucinate text on silence).
  - *transcriber* — 3 workers posting WAV chunks to `/v1/audio/transcriptions`.
    The tail of the previous transcript is passed as `prompt` so sentences that
    straddle chunk boundaries stay coherent.
  - *translator* — `gpt-4o-mini` chat call per line, with a rolling window of
    recent lines for pronoun/tense continuity. Skipped with `--no-translate`;
    replaced by whisper-1's one-call translations endpoint with `--fast`.
  - *renderer* — reorders results by sequence number (workers finish out of
    order) and prints one line per chunk.

In `--stream` mode the chunker/transcriber stages are replaced by a Realtime
API WebSocket client (`realtime.go`): our energy VAD decides when to *commit* a
segment (the model streams deltas continuously; commits just mark segment
ends — they arrive in strict audio order and define subtitle ordering), and a
translation manager (`streamrun.go`) races provisional fragment translations
against the final full-sentence one, guarded by per-segment versioning so a
stale result can never overwrite a newer one.

Dependencies: the Go side is stdlib plus `coder/websocket` (needed for
`--stream`); the helpers are single Swift files compiled with `swiftc`.

### The permission dance

macOS attributes a CLI tool's permission request to the terminal app that
launched it, and refuses to even show the System Audio Recording prompt when
that app lacks `NSAudioCaptureUsageDescription` in its Info.plist (which
terminals don't have). To get its own prompt, audiotap re-execs itself with
*disclaimed responsibility* (`posix_spawn` + `POSIX_SPAWN_SETEXEC` +
`responsibility_spawnattrs_setdisclaim`), becoming its own TCC subject with its
embedded Info.plist honored. If Apple ever removes that private API, the
fallback is granting the permission to your terminal manually — or the
ScreenCaptureKit route described in [PLAN.md](PLAN.md).

## Development

Dev modes that don't need live audio or an API key:

```sh
./bin/sas --record clip.wav --seconds 10   # capture a test WAV; reports peak/RMS
./bin/sas --chunk-debug --input clip.wav   # log VAD chunk boundaries, no API calls
./bin/sas --input clip.wav                 # run the full pipeline on a file
```

The helper is independently testable too: `bin/audiotap > out.pcm` while audio
plays, then inspect/convert the raw PCM (16 kHz mono s16le).

Layout:

```
helper/audiotap.swift        system-audio capture (Swift, Core Audio process tap)
helper/subtitlewindow.swift  floating subtitle overlay (Swift, AppKit)
main.go                      flags and wiring
capture.go                   helper spawn / WAV input / --record mode
chunk.go                     energy-based VAD chunker
openai.go                    REST API client (transcribe, translate, retries)
realtime.go                  Realtime API WebSocket client (--stream)
streamrun.go                 progressive-translation manager (--stream)
pipeline.go                  chunked-mode worker pool and renderer
display.go                   terminal + native-window renderers
wav.go                       WAV encode/decode helpers
PLAN.md                      design decisions, milestones, roadmap
```

## Contributing

Issues and PRs welcome. A few ground rules:

- Keep Go dependencies minimal (currently just `coder/websocket`); prefer stdlib.
- `gofmt` and `go vet ./...` must pass; test with the dev modes above before
  opening a PR (a short `--input` WAV in a foreign language is the quickest
  end-to-end check).
- The helper's contract is exactly "16 kHz mono s16le PCM on stdout, diagnostics
  on stderr" — alternative capture backends (e.g. ScreenCaptureKit) are welcome
  as long as they honor it.
- Roadmap ideas live in [PLAN.md](PLAN.md) — currently: helper auto-restart,
  SRT export, realtime-session reconnect, and following default-output-device
  changes mid-run.

## License

No license file yet — if you're the repo owner, pick one before accepting
contributions (MIT is the path of least resistance).
