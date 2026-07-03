package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
)

const (
	sampleRate     = 16000
	channels       = 1
	bytesPerSample = 2
)

// wavEncode wraps raw 16 kHz mono s16le PCM in a WAV header.
func wavEncode(pcm []byte) []byte {
	var b bytes.Buffer
	b.Grow(44 + len(pcm))
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+len(pcm)))
	b.WriteString("WAVE")
	b.WriteString("fmt ")
	binary.Write(&b, binary.LittleEndian, uint32(16))
	binary.Write(&b, binary.LittleEndian, uint16(1)) // PCM
	binary.Write(&b, binary.LittleEndian, uint16(channels))
	binary.Write(&b, binary.LittleEndian, uint32(sampleRate))
	binary.Write(&b, binary.LittleEndian, uint32(sampleRate*channels*bytesPerSample))
	binary.Write(&b, binary.LittleEndian, uint16(channels*bytesPerSample))
	binary.Write(&b, binary.LittleEndian, uint16(8*bytesPerSample))
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(len(pcm)))
	b.Write(pcm)
	return b.Bytes()
}

func writeWAV(path string, pcm []byte) error {
	return os.WriteFile(path, wavEncode(pcm), 0o644)
}

// wavDataReader validates that r is a 16 kHz mono 16-bit PCM WAV and returns
// a reader over its data chunk.
func wavDataReader(r io.Reader) (io.Reader, error) {
	var riff [12]byte
	if _, err := io.ReadFull(r, riff[:]); err != nil {
		return nil, fmt.Errorf("reading RIFF header: %w", err)
	}
	if string(riff[0:4]) != "RIFF" || string(riff[8:12]) != "WAVE" {
		return nil, fmt.Errorf("not a WAV file")
	}
	for {
		var hdr [8]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return nil, fmt.Errorf("no data chunk found")
		}
		size := binary.LittleEndian.Uint32(hdr[4:])
		switch string(hdr[0:4]) {
		case "fmt ":
			buf := make([]byte, size)
			if _, err := io.ReadFull(r, buf); err != nil {
				return nil, err
			}
			format := binary.LittleEndian.Uint16(buf[0:])
			ch := binary.LittleEndian.Uint16(buf[2:])
			rate := binary.LittleEndian.Uint32(buf[4:])
			bits := binary.LittleEndian.Uint16(buf[14:])
			if format != 1 || ch != channels || rate != sampleRate || bits != 8*bytesPerSample {
				return nil, fmt.Errorf("need %d Hz mono 16-bit PCM, got format=%d channels=%d rate=%d bits=%d",
					sampleRate, format, ch, rate, bits)
			}
		case "data":
			return io.LimitReader(r, int64(size)), nil
		default:
			// Chunks are word-aligned; skip padding byte on odd sizes.
			if _, err := io.CopyN(io.Discard, r, int64(size)+int64(size%2)); err != nil {
				return nil, err
			}
		}
	}
}

func analyze(pcm []byte) (peak, rms float64) {
	n := len(pcm) / bytesPerSample
	if n == 0 {
		return
	}
	var sumSquares float64
	for i := 0; i < n; i++ {
		s := float64(int16(binary.LittleEndian.Uint16(pcm[bytesPerSample*i:]))) / 32768.0
		if a := math.Abs(s); a > peak {
			peak = a
		}
		sumSquares += s * s
	}
	rms = math.Sqrt(sumSquares / float64(n))
	return
}
