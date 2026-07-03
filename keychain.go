package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
)

// The OpenAI API key lives in the login keychain, accessed through
// /usr/bin/security. Items created by the security CLI carry its ACL, so
// later reads by this app never trigger a keychain prompt — and unlike the
// ad-hoc-signed helpers, rebuilding sas can't invalidate the grant.

const (
	keychainService = "sas"
	keychainAccount = "openai_api_key"
	keychainLabel   = "sas — OpenAI API key"
)

// keychainLoad returns the stored key, or "" (no error) if none is stored.
func keychainLoad() (string, error) {
	out, err := exec.Command("/usr/bin/security", "find-generic-password",
		"-s", keychainService, "-a", keychainAccount, "-w").Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			if ee.ExitCode() == 44 || strings.Contains(string(ee.Stderr), "could not be found") {
				return "", nil
			}
			return "", fmt.Errorf("security find-generic-password: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// keychainSave stores (or replaces) the key. The command is fed to
// `security -i` on stdin so the key never appears in an argument list,
// where any process could read it out of ps.
func keychainSave(key string) error {
	quote := func(s string) string {
		s = strings.ReplaceAll(s, `\`, `\\`)
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	cmd := exec.Command("/usr/bin/security", "-i")
	cmd.Stdin = strings.NewReader(strings.Join([]string{
		"add-generic-password", "-U",
		"-s", quote(keychainService),
		"-a", quote(keychainAccount),
		"-l", quote(keychainLabel),
		"-w", quote(key),
	}, " ") + "\n")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("security add-generic-password: %s", strings.TrimSpace(out.String()))
	}
	return nil
}

// keychainDelete removes the stored key; a missing item is not an error.
func keychainDelete() error {
	out, err := exec.Command("/usr/bin/security", "delete-generic-password",
		"-s", keychainService, "-a", keychainAccount).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "could not be found") {
		return fmt.Errorf("security delete-generic-password: %s", strings.TrimSpace(string(out)))
	}
	return nil
}
