package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// The API key is stored in a user-only config file so the app works without
// OPENAI_API_KEY in the environment after the first run.

type storedConfig struct {
	OpenAIAPIKey string `json:"openai_api_key"`
}

func configPath() (string, error) {
	dir, err := os.UserConfigDir() // ~/Library/Application Support on macOS
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "sas", "config.json"), nil
}

func loadKey() string {
	path, err := configPath()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var c storedConfig
	if json.Unmarshal(data, &c) != nil {
		return ""
	}
	return c.OpenAIAPIKey
}

func saveKey(key string) error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(storedConfig{OpenAIAPIKey: key}, "", "  ")
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func clearKey() error {
	path, err := configPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
