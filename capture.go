package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

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

	cmd := exec.CommandContext(ctx, cfg.Helper)
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
func recordWAV(helperPath, outPath string, seconds int) error {
	cmd := exec.Command(helperPath)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w (run `make` first?)", helperPath, err)
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
