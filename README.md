<p align="center">
  <img src="assets/icon.png" width="192" alt="system audio subtitles icon">
</p>

# System Audio Subtitles

Live subtitles for whatever your Mac is playing, in a floating native window.
Captures system audio natively (no BlackHole, no ffmpeg), streams it to the
OpenAI Realtime API for word-by-word transcription, and translates each
sentence progressively — a provisional translation appears while the sentence
is still being spoken, then an authoritative one replaces it when the sentence
completes. Finished lines scroll into a timestamped history above the live
area.

## Install

Download the latest `SystemAudioSubtitles-x.y.z.dmg` from
[Releases](https://github.com/SixiS/system-audio-subtitles/releases), drag
**System Audio Subtitles** into Applications, and open it. The app is signed
and notarized — no Gatekeeper warnings. You'll need macOS 14.4 or newer and an
OpenAI API key. On first launch:

1. The window asks for your **OpenAI API key** (stored in your login keychain).
2. macOS asks to allow **System Audio Recording** — click Allow.

That's it — play something and subtitles appear. Settings live behind the
menu-bar captions icon and the gear in the window's top bar.

## Building from source

Requirements:

- macOS 14.4 or newer (uses Core Audio process taps)
- Xcode Command Line Tools (`xcode-select --install`) — provides `swiftc`
- Go 1.22+
- An OpenAI API key

## Build & run

```sh
make                      # builds bin/audiotap, bin/subtitle-window (Swift) and bin/sas (Go)
./bin/sas
```

On first run:

- The window asks for your **OpenAI API key** — paste one, or click
  *Use $OPENAI_API_KEY* if the environment variable is set. The key is stored
  in your **login keychain** (item "sas — OpenAI API key") and reused on later
  runs. Change it any time from the menu-bar item (see below). A key stored in
  the config file by older versions is migrated into the keychain
  automatically on the next run.
- macOS asks to allow **audiotap** to record system audio — click Allow. The
  grant is remembered, but because the helper is ad-hoc signed, **rebuilding it
  re-triggers the prompt** (its code hash changes). If a prompt never appears,
  check System Settings → Privacy & Security → Screen & System Audio Recording.

While running, a **captions icon in the menu bar** (and a gear in the window's
top bar) has the settings:

- *Preferences…* — target/source language, show-original, transcription-only,
  latency, sentence gap, max sentences per segment, and Dock icon. Saved
  changes apply immediately (session-level settings restart the live session)
  and persist in the config file.
- *Edit API Key…* — applies immediately; a broken key shows a red error in the
  window until it's fixed.
- *Clear API Key & Quit* — deletes the key from the keychain (preferences
  survive) and shuts everything down.

The subtitle window floats above other apps; drag it to move, drag the corner
grip to resize, scroll the history independently of the pinned live area.
Closing the window shuts the whole pipeline down. Finalized lines are also
echoed to the terminal as a plain transcript.

## Usage

```sh
./bin/sas [flags]

--target-lang en      language to translate into (default: en)
--source-lang ""      optional source-language hint (ISO 639-1); auto-detect if empty
--show-original       show the untranslated text above each subtitle
                      (default: on; disable with --show-original=false)
--no-translate        transcription only
--stream-delay low    realtime latency/accuracy trade-off: minimal|low|medium|high|xhigh
--word-gap 1000       milliseconds without new transcribed words that ends a segment
--max-sentences 3     force-cut a live segment into history after this many
                      sentences, even without a pause (0 = no limit)
--dock-icon           show a Dock icon while running
                      (default: on; disable with --dock-icon=false)
```

All of these are also editable in the window's Preferences dialog, which
stores them in the config file. Stored preferences are the defaults on later
runs; an explicitly passed flag overrides the stored value for that run.

Examples:

```sh
./bin/sas --target-lang de                   # German subtitles + original text
./bin/sas --no-translate --source-lang ja    # Japanese transcript, no translation
```

Press Ctrl-C (or close the window) to stop; the minutes of audio streamed are
reported on exit. Transcription is billed per audio-minute by the Realtime
API, plus `gpt-4o-mini` tokens for translation.

## How it works

```
┌──────────────┐   ┌────────────────────────┐   ┌────────────┐   ┌─────────────────┐
│ audiotap     │──▶│ Realtime API (ws)      │──▶│ translator │──▶│ subtitle-window │
│ (Swift, PCM) │   │ deltas, word-gap cuts  │   │ (OpenAI)   │   │ (Swift, AppKit) │
└──────────────┘   └────────────────────────┘   └────────────┘   └─────────────────┘
```

- **`helper/audiotap.swift`** — system-audio capture. Creates a global Core
  Audio process tap (macOS 14.4+), wraps it in a private aggregate device,
  converts to 24 kHz mono s16le, and writes raw PCM to stdout.
- **`helper/subtitlewindow.swift`** — the floating overlay. Reads JSON lines on
  stdin: `append` messages go into the scrollable timestamped history, `live`
  messages replace the pinned in-progress area at the bottom.
- **The Go pipeline** (`bin/sas`) spawns both helpers and connects them:
  - *realtime client* (`realtime.go`) — streams PCM to the Realtime API
    (`gpt-realtime-whisper`) over a WebSocket. The model streams source-language
    deltas continuously; a segment is *committed* once it has words but no new
    delta has arrived for `--word-gap`. Cutting on transcription activity
    rather than acoustic silence means background music can't hold a segment
    open — only speech does — and wordless audio never commits at all. Commits
    arrive in strict audio order and define subtitle ordering (delta arrival
    order across segments does not).
  - *translation manager* (`pipeline.go`) — races provisional fragment
    translations (every couple of new words, via `gpt-4o-mini`) against the
    authoritative full-sentence translation requested when the segment
    completes, guarded by per-segment versioning so a stale result can never
    overwrite a newer one. A rolling window of recent lines gives the
    translator pronoun/tense continuity.
  - *display* (`display.go`) — drives the window over its stdin protocol and
    echoes finalized lines to the terminal.

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

Dev modes that don't need live audio:

```sh
./bin/sas --record clip.wav --seconds 10   # capture a test WAV; reports peak/RMS
./bin/sas --input clip.wav                 # run the full pipeline on a file
```

The helpers are independently testable too: `bin/audiotap > out.pcm` while
audio plays, then inspect/convert the raw PCM (24 kHz mono s16le); and
`echo '{"type":"append","translation":"hello","time":"12:00:00"}' | bin/subtitle-window`
drives the window by hand.

Layout:

```
helper/audiotap.swift        system-audio capture (Swift, Core Audio process tap)
helper/subtitlewindow.swift  floating subtitle overlay (Swift, AppKit)
main.go                      flags and wiring
config.go                    stored preferences (~/Library/Application Support/sas)
keychain.go                  API key storage in the login keychain (via /usr/bin/security)
capture.go                   helper spawn / WAV input / --record mode
realtime.go                  Realtime API WebSocket client + segmentation
pipeline.go                  orchestrator: progressive-translation event loop
translate.go                 translation REST client (gpt-4o-mini, retries)
display.go                   subtitle-window driver
wav.go                       WAV encode/decode helpers
assets/                      app icon (PNG + .icns) and the script that draws it
packaging/                   app-bundle Info.plist and entitlements
scripts/release.sh           builds the universal .app, signs, notarizes, makes the DMG
PLAN.md                      design decisions, milestones, roadmap
RELEASING.md                 how releases are cut (Apple Developer setup, make release)
```

`make app` assembles an ad-hoc-signed `System Audio Subtitles.app` in `dist/`
for local testing; `make release VERSION=vX.Y.Z` produces the signed,
notarized DMG (see [RELEASING.md](RELEASING.md)).

## Contributing

Issues and PRs welcome. A few ground rules:

- Keep Go dependencies minimal (currently just `coder/websocket`); prefer stdlib.
- `gofmt` and `go vet ./...` must pass; test with the dev modes above before
  opening a PR (a short `--input` WAV in a foreign language is the quickest
  end-to-end check).
- The capture helper's contract is exactly "24 kHz mono s16le PCM on stdout,
  diagnostics on stderr" — alternative capture backends (e.g. ScreenCaptureKit)
  are welcome as long as they honor it.
- Roadmap ideas live in [PLAN.md](PLAN.md) — currently: helper auto-restart,
  SRT export, realtime-session reconnect, and following default-output-device
  changes mid-run.

## License

[MIT](LICENSE)
