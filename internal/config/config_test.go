// Package config_test contains tests for the config package.
package config_test

import (
	"strings"
	"testing"

	"github.com/lucavb/marstek-prometheus-controller/internal/config"
)

// TestLoad_MinCommandDeltaDefaults verifies that both new env vars default to
// the expected values when unset.
func TestLoad_MinCommandDeltaDefaults(t *testing.T) {
	// Provide the three required env vars so Load() doesn't fail on them.
	t.Setenv("PROMETHEUS_BASE_URL", "http://prom:9090")
	t.Setenv("MQTT_BROKER_URL", "tcp://mqtt:1883")
	t.Setenv("MARSTEK_DEVICE_ID", "aabbccdd1122")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.MinCommandDeltaWatts != 25 {
		t.Errorf("MinCommandDeltaWatts default = %d, want 25", cfg.MinCommandDeltaWatts)
	}
	if cfg.MinCommandDeltaWattsExporting != 5 {
		t.Errorf("MinCommandDeltaWattsExporting default = %d, want 5", cfg.MinCommandDeltaWattsExporting)
	}
}

// TestLoad_MinCommandDeltaValidInts verifies both vars accept arbitrary valid
// integer values.
func TestLoad_MinCommandDeltaValidInts(t *testing.T) {
	t.Setenv("PROMETHEUS_BASE_URL", "http://prom:9090")
	t.Setenv("MQTT_BROKER_URL", "tcp://mqtt:1883")
	t.Setenv("MARSTEK_DEVICE_ID", "aabbccdd1122")
	t.Setenv("MIN_COMMAND_DELTA_WATTS", "10")
	t.Setenv("MIN_COMMAND_DELTA_WATTS_EXPORTING", "20")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.MinCommandDeltaWatts != 10 {
		t.Errorf("MinCommandDeltaWatts = %d, want 10", cfg.MinCommandDeltaWatts)
	}
	if cfg.MinCommandDeltaWattsExporting != 20 {
		t.Errorf("MinCommandDeltaWattsExporting = %d, want 20", cfg.MinCommandDeltaWattsExporting)
	}
}

// TestLoad_MinCommandDeltaZeroIsValid verifies that zero is accepted by both
// vars (0 = never filter).
func TestLoad_MinCommandDeltaZeroIsValid(t *testing.T) {
	t.Setenv("PROMETHEUS_BASE_URL", "http://prom:9090")
	t.Setenv("MQTT_BROKER_URL", "tcp://mqtt:1883")
	t.Setenv("MARSTEK_DEVICE_ID", "aabbccdd1122")
	t.Setenv("MIN_COMMAND_DELTA_WATTS", "0")
	t.Setenv("MIN_COMMAND_DELTA_WATTS_EXPORTING", "0")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.MinCommandDeltaWatts != 0 {
		t.Errorf("MinCommandDeltaWatts = %d, want 0", cfg.MinCommandDeltaWatts)
	}
	if cfg.MinCommandDeltaWattsExporting != 0 {
		t.Errorf("MinCommandDeltaWattsExporting = %d, want 0", cfg.MinCommandDeltaWattsExporting)
	}
}

// TestLoad_MinCommandDeltaNegative_Errors verifies that a negative value is
// rejected by validate(). Both vars are tested independently so the error
// message regression is pinned.
func TestLoad_MinCommandDeltaNegative_Errors(t *testing.T) {
	cases := []struct {
		name    string
		envKey  string
		wantMsg string
	}{
		{
			name:    "MIN_COMMAND_DELTA_WATTS negative",
			envKey:  "MIN_COMMAND_DELTA_WATTS",
			wantMsg: "MIN_COMMAND_DELTA_WATTS must be >= 0",
		},
		{
			name:    "MIN_COMMAND_DELTA_WATTS_EXPORTING negative",
			envKey:  "MIN_COMMAND_DELTA_WATTS_EXPORTING",
			wantMsg: "MIN_COMMAND_DELTA_WATTS_EXPORTING must be >= 0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PROMETHEUS_BASE_URL", "http://prom:9090")
			t.Setenv("MQTT_BROKER_URL", "tcp://mqtt:1883")
			t.Setenv("MARSTEK_DEVICE_ID", "aabbccdd1122")
			t.Setenv(tc.envKey, "-1")

			_, err := config.Load()
			if err == nil {
				t.Fatal("Load() expected an error for negative value, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantMsg)
			}
		})
	}
}

// TestLoad_MinCommandDeltaMalformed_FallsBackToDefault verifies that a
// non-integer value causes a warning and falls back to the default rather than
// hard-failing. (The log output itself is not asserted — only the field value.)
func TestLoad_MinCommandDeltaMalformed_FallsBackToDefault(t *testing.T) {
	t.Setenv("PROMETHEUS_BASE_URL", "http://prom:9090")
	t.Setenv("MQTT_BROKER_URL", "tcp://mqtt:1883")
	t.Setenv("MARSTEK_DEVICE_ID", "aabbccdd1122")
	t.Setenv("MIN_COMMAND_DELTA_WATTS", "not-a-number")
	t.Setenv("MIN_COMMAND_DELTA_WATTS_EXPORTING", "also-not-a-number")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() unexpected error for malformed int: %v", err)
	}
	if cfg.MinCommandDeltaWatts != 25 {
		t.Errorf("MinCommandDeltaWatts = %d, want fallback 25", cfg.MinCommandDeltaWatts)
	}
	if cfg.MinCommandDeltaWattsExporting != 5 {
		t.Errorf("MinCommandDeltaWattsExporting = %d, want fallback 5", cfg.MinCommandDeltaWattsExporting)
	}
}
