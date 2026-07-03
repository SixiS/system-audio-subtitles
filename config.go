package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Preferences live in a user-only config file so the app works without flags
// after the first run; explicit CLI flags still override stored preferences
// for that run. The API key lives in the login keychain (keychain.go) — the
// config file only ever sees it transiently, when migrating a key stored
// there by older versions.

// Prefs mirrors the tunable subset of Config that the window's Preferences
// dialog edits. The JSON shape is shared with the window app.
type Prefs struct {
	TargetLang   string `json:"target_lang"`
	SourceLang   string `json:"source_lang"`
	ShowOriginal bool   `json:"show_original"`
	NoTranslate  bool   `json:"no_translate"`
	StreamDelay  string `json:"stream_delay"`
	WordGapMS    int    `json:"word_gap_ms"`
	MaxSentences int    `json:"max_sentences"`
	DockIcon     bool   `json:"dock_icon"`
}

type storedConfig struct {
	// Legacy field: keys are stored in the keychain now. Kept so a key
	// written by an older version is found and migrated on load.
	OpenAIAPIKey string `json:"openai_api_key,omitempty"`
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

func loadKey() string {
	key, err := keychainLoad()
	if err != nil {
		fmt.Fprintf(os.Stderr, "sas: reading API key from keychain: %v\n", err)
		return ""
	}
	if key != "" {
		return key
	}
	// Older versions stored the key in the config file — migrate it into the
	// keychain and scrub the plaintext copy.
	c := loadConfig()
	if c.OpenAIAPIKey == "" {
		return ""
	}
	if err := keychainSave(c.OpenAIAPIKey); err != nil {
		fmt.Fprintf(os.Stderr, "sas: migrating API key to keychain: %v\n", err)
		return c.OpenAIAPIKey // keep working off the file copy
	}
	key = c.OpenAIAPIKey
	c.OpenAIAPIKey = ""
	if err := writeConfig(c); err != nil {
		fmt.Fprintf(os.Stderr, "sas: scrubbing migrated API key from config file: %v\n", err)
	} else {
		fmt.Fprintln(os.Stderr, "sas: API key moved from config file to the login keychain")
	}
	return key
}

func saveKey(key string) error {
	if err := keychainSave(key); err != nil {
		return err
	}
	if c := loadConfig(); c.OpenAIAPIKey != "" { // scrub any legacy file copy
		c.OpenAIAPIKey = ""
		return writeConfig(c)
	}
	return nil
}

// clearKey removes the stored key but keeps preferences.
func clearKey() error {
	err := keychainDelete()
	if c := loadConfig(); c.OpenAIAPIKey != "" { // legacy file copy goes too
		c.OpenAIAPIKey = ""
		if werr := writeConfig(c); werr != nil && err == nil {
			err = werr
		}
	}
	return err
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
		MaxSentences: cfg.MaxSentences,
		DockIcon:     cfg.DockIcon,
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
	cfg.MaxSentences = max(p.MaxSentences, 0) // 0 = no limit
	cfg.DockIcon = p.DockIcon
}
