package main

import (
	"context"
	"encoding/binary"
	"io"
	"math"
)

const (
	frameSamples = 480 // 30 ms at 16 kHz
	frameBytes   = frameSamples * bytesPerSample

	prePadFrames     = 6   // 180 ms of audio kept from before speech onset
	silenceCutFrames = 17  // ~510 ms of trailing silence ends a chunk
	minSpeechFrames  = 10  // chunks with <300 ms of speech are dropped
	maxChunkFrames   = 267 // ~8 s hard cut for long uninterrupted speech
)

type Chunk struct {
	Seq int
	PCM []byte
}

func frameRMS(frame []byte) float64 {
	var sum float64
	for i := 0; i+1 < len(frame); i += bytesPerSample {
		s := float64(int16(binary.LittleEndian.Uint16(frame[i:]))) / 32768
		sum += s * s
	}
	return math.Sqrt(sum / float64(len(frame)/bytesPerSample))
}

// chunker segments the PCM stream into speech chunks using energy-based VAD:
// a chunk starts at the first frame above threshold (plus a little pre-pad),
// and ends after ~500 ms of silence or at the ~8 s hard cut. All-silent
// stretches never produce chunks, which keeps hallucination-prone empty audio
// away from the transcriber. Closes out when the source ends.
func chunker(ctx context.Context, src io.Reader, threshold float64, out chan<- Chunk) {
	defer close(out)

	var (
		pre          [][]byte
		cur          []byte
		speechFrames int
		chunkFrames  int
		trailing     int
		inSpeech     bool
		seq          int
	)

	emit := func() {
		if speechFrames >= minSpeechFrames {
			pcm := make([]byte, len(cur))
			copy(pcm, cur)
			select {
			case out <- Chunk{Seq: seq, PCM: pcm}:
				seq++
			case <-ctx.Done():
			}
		}
		cur = cur[:0]
		speechFrames, chunkFrames, trailing = 0, 0, 0
	}

	frame := make([]byte, frameBytes)
	for ctx.Err() == nil {
		if _, err := io.ReadFull(src, frame); err != nil {
			if inSpeech {
				emit()
			}
			return
		}
		speech := frameRMS(frame) >= threshold

		if !inSpeech {
			if !speech {
				f := make([]byte, frameBytes)
				copy(f, frame)
				pre = append(pre, f)
				if len(pre) > prePadFrames {
					pre = pre[1:]
				}
				continue
			}
			inSpeech = true
			for _, f := range pre {
				cur = append(cur, f...)
				chunkFrames++
			}
			pre = pre[:0]
		}

		cur = append(cur, frame...)
		chunkFrames++
		if speech {
			speechFrames++
			trailing = 0
		} else {
			trailing++
		}

		if trailing >= silenceCutFrames {
			emit()
			inSpeech = false
		} else if chunkFrames >= maxChunkFrames {
			// Hard cut mid-speech: emit and keep collecting immediately.
			emit()
		}
	}
}
