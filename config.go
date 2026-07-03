package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Settings live in a user-only config file so the app works without flags or
// OPENAI_API_KEY in the environment after the first run. Explicit CLI flags
// still override stored preferences for that run.

// Prefs mirrors the tunable subset of Config that the window's Preferences
// dialog edits. The JSON shape is shared with the window app.
type Prefs struct {
	TargetLang   string `json:"target_lang"`
	SourceLang   string `json:"source_lang"`
	ShowOriginal bool   `json:"show_original"`
	NoTranslate  bool   `json:"no_translate"`
	StreamDelay  string `json:"stream_delay"`
	WordGapMS    int    `json:"word_gap_ms"`
}

type storedConfig struct {
	OpenAIAPIKey string `json:"openai_api_key"`
	Prefs        *Prefs `json:"prefs,omitempty"`
}

func configPath() (string, error) {
	dir, err := os.UserConfigDir() // ~/Library/Application Support on macOS
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "sas", "config.json"), nil
}

func loadConfig() storedConfig {
	var c storedConfig
	path, err := configPath()
	if err != nil {
		return c
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	json.Unmarshal(data, &c)
	return c
}

func writeConfig(c storedConfig) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func loadKey() string { return loadConfig().OpenAIAPIKey }

func saveKey(key string) error {
	c := loadConfig()
	c.OpenAIAPIKey = key
	return writeConfig(c)
}

// clearKey removes the stored key but keeps preferences.
func clearKey() error {
	c := loadConfig()
	c.OpenAIAPIKey = ""
	return writeConfig(c)
}

func loadPrefs() *Prefs { return loadConfig().Prefs }

func savePrefs(p Prefs) error {
	c := loadConfig()
	c.Prefs = &p
	return writeConfig(c)
}

func prefsFromConfig(cfg Config) Prefs {
	return Prefs{
		TargetLang:   cfg.TargetLang,
		SourceLang:   cfg.SourceLang,
		ShowOriginal: cfg.ShowOriginal,
		NoTranslate:  cfg.NoTranslate,
		StreamDelay:  cfg.StreamDelay,
		WordGapMS:    cfg.WordGapMS,
	}
}

// applyPrefs merges edited preferences into the runtime config, ignoring
// values that would break the pipeline.
func applyPrefs(cfg *Config, p Prefs) {
	if p.TargetLang != "" {
		cfg.TargetLang = p.TargetLang
	}
	cfg.SourceLang = p.SourceLang
	cfg.ShowOriginal = p.ShowOriginal
	cfg.NoTranslate = p.NoTranslate
	if p.StreamDelay != "" {
		cfg.StreamDelay = p.StreamDelay
	}
	if p.WordGapMS > 0 {
		cfg.WordGapMS = max(p.WordGapMS, 150)
	}
}
