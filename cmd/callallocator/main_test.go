package main

import (
	"log/slog"
	"testing"
	"time"
)

func TestLoadConfig_Defaults(t *testing.T) {
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	if cfg.addr != ":8080" {
		t.Errorf("addr = %q, want :8080", cfg.addr)
	}
	if cfg.nodeTTL != 30*time.Second {
		t.Errorf("nodeTTL = %v, want 30s", cfg.nodeTTL)
	}
	if cfg.shutdownTimeout != 10*time.Second {
		t.Errorf("shutdownTimeout = %v, want 10s", cfg.shutdownTimeout)
	}
	if cfg.logLevel != slog.LevelInfo {
		t.Errorf("logLevel = %v, want info", cfg.logLevel)
	}
}

func TestLoadConfig_Overrides(t *testing.T) {
	t.Setenv("PORT", "9090")
	t.Setenv("NODE_TTL", "45s")
	t.Setenv("SHUTDOWN_TIMEOUT", "2s")
	t.Setenv("LOG_LEVEL", "debug")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}

	if cfg.addr != ":9090" {
		t.Errorf("addr = %q, want :9090", cfg.addr)
	}
	if cfg.nodeTTL != 45*time.Second {
		t.Errorf("nodeTTL = %v, want 45s", cfg.nodeTTL)
	}
	if cfg.shutdownTimeout != 2*time.Second {
		t.Errorf("shutdownTimeout = %v, want 2s", cfg.shutdownTimeout)
	}
	if cfg.logLevel != slog.LevelDebug {
		t.Errorf("logLevel = %v, want debug", cfg.logLevel)
	}
}

// A mistyped value must stop the process rather than quietly reverting to a
// default: a NODE_TTL that silently stays at 30s is how a fleet gets declared
// dead at the wrong moment.
func TestLoadConfig_RejectsBadValues(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{"port is not a number", "PORT", "abc"},
		{"port is out of range", "PORT", "70000"},
		{"port is zero", "PORT", "0"},
		{"ttl is not a duration", "NODE_TTL", "banana"},
		{"ttl is negative", "NODE_TTL", "-5s"},
		{"ttl is zero", "NODE_TTL", "0s"},
		{"shutdown timeout is not a duration", "SHUTDOWN_TIMEOUT", "soon"},
		{"log level is unknown", "LOG_LEVEL", "chatty"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			if _, err := loadConfig(); err == nil {
				t.Errorf("%s=%q was accepted, want a startup failure", tc.key, tc.value)
			}
		})
	}
}
