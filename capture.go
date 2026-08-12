package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// defaultHelper locates audiotap next to the sas executable — true in both
// the dev layout (bin/) and the app bundle (Contents/MacOS/), and immune to
// the working directory (a Finder-launched app runs with cwd=/). Falls back
// to the repo-relative path for `go run .`. The subtitle-window helper is
// resolved from the same directory (see newDisplay).
func defaultHelper() string {
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "audiotap")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "bin/audiotap"
}

// helperArgs builds the audiotap argument list from the config.
func helperArgs(cfg Config) []string {
	if cfg.CaptureDevice != "" {
		return []string{"--device", cfg.CaptureDevice}
	}
	return nil
}

// printDevices asks the helper for the output-device list (--list-devices)
// and copies it to stdout: one "uid\tname" line per device.
func printDevices(helperPath string) error {
	cmd := exec.Command(helperPath, "--list-devices")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running %s --list-devices: %w (run `make` first?)", helperPath, err)
	}
	return nil
}

// openPCMSource returns a stream of 24 kHz mono s16le PCM: either the audiotap
// helper's stdout (live capture) or the data chunk of a WAV file (--input).
func openPCMSource(ctx context.Context, cfg Config) (io.Reader, func(), error) {
	if cfg.Input != "" {
		f, err := os.Open(cfg.Input)
		if err != nil {
			return nil, nil, err
		}
		r, err := wavDataReader(f)
		if err != nil {
			f.Close()
			return nil, nil, fmt.Errorf("%s: %w", cfg.Input, err)
		}
		return r, func() { f.Close() }, nil
	}

	cmd := exec.CommandContext(ctx, cfg.Helper, helperArgs(cfg)...)
	cmd.Stderr = os.Stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("starting %s: %w (run `make` first?)", cfg.Helper, err)
	}
	cleanup := func() {
		cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			cmd.Process.Kill()
			<-done
		}
	}
	return stdout, cleanup, nil
}

// recordWAV is the milestone-1 diagnostic mode: record N seconds and report
// peak/RMS so the capture path can be verified without touching the API.
func recordWAV(cfg Config) error {
	outPath, seconds := cfg.Record, cfg.Seconds
	cmd := exec.Command(cfg.Helper, helperArgs(cfg)...)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w (run `make` first?)", cfg.Helper, err)
	}

	fmt.Fprintf(os.Stderr, "sas: recording %d s of system audio...\n", seconds)
	pcm := make([]byte, sampleRate*channels*bytesPerSample*seconds)
	_, readErr := io.ReadFull(stdout, pcm)

	cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		cmd.Process.Kill()
		<-done
	}

	if readErr != nil {
		return fmt.Errorf("helper stream ended early: %w", readErr)
	}
	if err := writeWAV(outPath, pcm); err != nil {
		return err
	}

	peak, rms := analyze(pcm)
	fmt.Fprintf(os.Stderr, "sas: wrote %s (%d s, peak %.3f, rms %.4f)\n", outPath, seconds, peak, rms)
	if peak == 0 {
		fmt.Fprintln(os.Stderr, "sas: WARNING: capture is pure silence — was audio playing? is the permission granted?")
	}
	return nil
}
