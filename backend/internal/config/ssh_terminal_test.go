package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testSSHTerminalToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func assertSSHTokenGoneFromEnvironment(t *testing.T) {
	t.Helper()
	if _, present := os.LookupEnv("AO_SSH_TERMINAL_TOKEN"); present {
		t.Fatal("AO_SSH_TERMINAL_TOKEN is still set after Load")
	}
	for _, entry := range os.Environ() {
		if strings.HasPrefix(strings.ToUpper(entry), "AO_SSH_TERMINAL_TOKEN=") {
			t.Fatal("AO_SSH_TERMINAL_TOKEN is still in os.Environ() after Load")
		}
	}
}

func TestLoadSSHTerminalSettings(t *testing.T) {
	knownHosts := filepath.Join(t.TempDir(), "integrations", "ssh-known-hosts")
	t.Setenv("AO_SSH_KNOWN_HOSTS", knownHosts)
	t.Setenv("AO_SSH_TERMINAL_TOKEN", testSSHTerminalToken)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SSHKnownHostsFile != knownHosts {
		t.Errorf("SSHKnownHostsFile = %q, want %q", cfg.SSHKnownHostsFile, knownHosts)
	}
	if !cfg.SSHTerminalToken.IsSet() || cfg.SSHTerminalToken.Reveal() != testSSHTerminalToken {
		t.Error("SSHTerminalToken was not read")
	}
	assertSSHTokenGoneFromEnvironment(t)
}

func TestLoadSSHTerminalSettingsDefaultOff(t *testing.T) {
	t.Setenv("AO_SSH_KNOWN_HOSTS", "")
	t.Setenv("AO_SSH_TERMINAL_TOKEN", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SSHKnownHostsFile != "" || cfg.SSHTerminalToken.IsSet() {
		t.Fatalf("SSH terminal settings = %q, set=%v; want both off", cfg.SSHKnownHostsFile, cfg.SSHTerminalToken.IsSet())
	}
	assertSSHTokenGoneFromEnvironment(t)
}

func TestLoadAbsolutizesRelativeSSHKnownHosts(t *testing.T) {
	t.Setenv("AO_SSH_KNOWN_HOSTS", filepath.Join("rel-integrations", "ssh-known-hosts"))
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(cwd, "rel-integrations", "ssh-known-hosts"); cfg.SSHKnownHostsFile != want {
		t.Errorf("SSHKnownHostsFile = %q, want %q", cfg.SSHKnownHostsFile, want)
	}
}

// A token the app would never send keeps the route off, and is removed from
// the environment all the same.
func TestLoadSSHTerminalTokenShapes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token string
		on    bool
	}{
		{"64 hex", testSSHTerminalToken, true},
		{"32 hex", testSSHTerminalToken[:32], true},
		{"upper-case hex", strings.ToUpper(testSSHTerminalToken), true},
		{"31 hex", testSSHTerminalToken[:31], false},
		{"not hex", strings.Repeat("zz", 32), false},
		{"trailing newline", testSSHTerminalToken + "\n", false},
		{"inner space", testSSHTerminalToken[:32] + " " + testSSHTerminalToken[:32], false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AO_SSH_TERMINAL_TOKEN", tc.token)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.SSHTerminalToken.IsSet() != tc.on {
				t.Fatalf("token set = %v, want %v", cfg.SSHTerminalToken.IsSet(), tc.on)
			}
			if tc.on && cfg.SSHTerminalToken.Reveal() != tc.token {
				t.Fatal("token value changed on load")
			}
			assertSSHTokenGoneFromEnvironment(t)
		})
	}
}

// The token is taken before any other setting is read, so a Load that fails
// on a malformed value still leaves nothing for a child process to inherit.
func TestLoadRemovesSSHTerminalTokenWhenLoadFails(t *testing.T) {
	t.Setenv("AO_SSH_TERMINAL_TOKEN", testSSHTerminalToken)
	t.Setenv("AO_PORT", "not-a-port")
	if _, err := Load(); err == nil {
		t.Fatal("Load() = nil error, want error for malformed AO_PORT")
	} else if strings.Contains(err.Error(), testSSHTerminalToken) {
		t.Fatal("Load error carries the token")
	}
	assertSSHTokenGoneFromEnvironment(t)
}

func TestSSHTerminalTokenNeverPrints(t *testing.T) {
	cfg := Config{SSHKnownHostsFile: `G:\data\ssh-known-hosts`, SSHTerminalToken: NewSecret(testSSHTerminalToken)}
	var outputs []string
	for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d"} {
		outputs = append(outputs, fmt.Sprintf(format, cfg), fmt.Sprintf(format, &cfg), fmt.Sprintf(format, cfg.SSHTerminalToken))
	}
	outputs = append(outputs, fmt.Sprint(cfg), fmt.Sprintln(cfg.SSHTerminalToken), cfg.SSHTerminalToken.String(), cfg.SSHTerminalToken.GoString())
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	outputs = append(outputs, string(encoded))
	text, err := cfg.SSHTerminalToken.MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	outputs = append(outputs, string(text))
	var logged bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logged, nil))
	logger.Info("config", "cfg", cfg, "token", cfg.SSHTerminalToken)
	jsonLogger := slog.New(slog.NewJSONHandler(&logged, nil))
	jsonLogger.Info("config", "cfg", cfg, "token", cfg.SSHTerminalToken)
	outputs = append(outputs, logged.String())

	hexForm := fmt.Sprintf("%x", testSSHTerminalToken)
	for _, out := range outputs {
		if strings.Contains(out, testSSHTerminalToken) || strings.Contains(out, testSSHTerminalToken[:16]) || strings.Contains(out, hexForm[:16]) {
			t.Fatalf("output reveals the token: %s", out)
		}
	}
	if !strings.Contains(string(encoded), secretPlaceholder) {
		t.Fatalf("json = %s, want the placeholder", encoded)
	}
}
