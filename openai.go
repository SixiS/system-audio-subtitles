package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

const apiBase = "https://api.openai.com/v1"

type Client struct {
	key  string
	http *http.Client
}

func newClient(key string) *Client {
	return &Client{key: key, http: &http.Client{Timeout: 90 * time.Second}}
}

// Transcribe sends one PCM chunk to /audio/transcriptions. prompt carries the
// tail of the previous transcript so sentences split across chunks stay
// coherent; language is an optional ISO 639-1 hint.
func (c *Client) Transcribe(ctx context.Context, pcm []byte, model, language, prompt string) (string, error) {
	fields := map[string]string{"model": model, "response_format": "json"}
	if language != "" {
		fields["language"] = language
	}
	if prompt != "" {
		fields["prompt"] = prompt
	}
	return c.audioRequest(ctx, apiBase+"/audio/transcriptions", fields, pcm)
}

// TranslateAudio is the --fast path: whisper-1's translations endpoint
// transcribes and translates to English in one call.
func (c *Client) TranslateAudio(ctx context.Context, pcm []byte, prompt string) (string, error) {
	fields := map[string]string{"model": "whisper-1", "response_format": "json"}
	if prompt != "" {
		fields["prompt"] = prompt
	}
	return c.audioRequest(ctx, apiBase+"/audio/translations", fields, pcm)
}

func (c *Client) audioRequest(ctx context.Context, url string, fields map[string]string, pcm []byte) (string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		w.WriteField(k, v)
	}
	fw, err := w.CreateFormFile("file", "chunk.wav")
	if err != nil {
		return "", err
	}
	fw.Write(wavEncode(pcm))
	w.Close()

	data, err := c.doWithRetry(ctx, url, w.FormDataContentType(), buf.Bytes())
	if err != nil {
		return "", err
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("parsing response: %w", err)
	}
	return strings.TrimSpace(out.Text), nil
}

// TranslateText translates one subtitle line with gpt-4o-mini. history holds
// recent "source → translation" pairs for continuity. partial marks an
// in-progress fragment that must not be completed or embellished.
func (c *Client) TranslateText(ctx context.Context, text, targetLang string, history []string, partial bool) (string, error) {
	system := fmt.Sprintf(
		"You translate live subtitles into %q. Reply with only the translation of the user's message — no quotes, no commentary. "+
			"If the message is already in %q, reply with the message unchanged.", targetLang, targetLang)
	if partial {
		system = fmt.Sprintf(
			"You translate live, in-progress speech into %q. The user's message is an incomplete fragment still being spoken: "+
				"translate exactly what is there, never complete the sentence or invent words. Reply with only the translation — "+
				"no quotes, no commentary. If it is already in %q, reply with it unchanged.", targetLang, targetLang)
	}
	if len(history) > 0 {
		system += "\nRecent subtitles, for context:\n" + strings.Join(history, "\n")
	}
	payload := map[string]any{
		"model":       "gpt-4o-mini",
		"temperature": 0.2,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": text},
		},
	}
	body, _ := json.Marshal(payload)

	data, err := c.doWithRetry(ctx, apiBase+"/chat/completions", "application/json", body)
	if err != nil {
		return "", err
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("parsing response: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("empty completion")
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}

func (c *Client) doWithRetry(ctx context.Context, url, contentType string, body []byte) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(1<<(attempt-1)) * time.Second):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.key)
		req.Header.Set("Content-Type", contentType)

		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			continue
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return data, nil
		}
		lastErr = fmt.Errorf("%s: %s", resp.Status, truncate(string(data), 200))
		// Retry only rate limits and server errors.
		if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
			return nil, lastErr
		}
	}
	return nil, lastErr
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
