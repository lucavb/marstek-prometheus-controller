// Package config_test contains tests for the config package.
package config_test

import (
	"strings"
	"testing"
	"time"

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

// ---- DEVICE_RESTART_SCHEDULE / DEVICE_RESTART_TIMEZONE tests ----

// helper to set the three required env vars.
func setRequired(t *testing.T) {
	t.Helper()
	t.Setenv("PROMETHEUS_BASE_URL", "http://prom:9090")
	t.Setenv("MQTT_BROKER_URL", "tcp://mqtt:1883")
	t.Setenv("MARSTEK_DEVICE_ID", "aabbccdd1122")
}

// TestLoad_DeviceRestart_DisabledByDefault verifies that an empty schedule
// leaves DeviceRestartSchedule as "" and DeviceRestartLocation as nil.
func TestLoad_DeviceRestart_DisabledByDefault(t *testing.T) {
	setRequired(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.DeviceRestartSchedule != "" {
		t.Errorf("DeviceRestartSchedule = %q, want empty", cfg.DeviceRestartSchedule)
	}
	if cfg.DeviceRestartLocation != nil {
		t.Errorf("DeviceRestartLocation = %v, want nil", cfg.DeviceRestartLocation)
	}
}

// TestLoad_DeviceRestart_ValidScheduleUTC accepts a valid spec with the default UTC zone.
func TestLoad_DeviceRestart_ValidScheduleUTC(t *testing.T) {
	setRequired(t)
	t.Setenv("DEVICE_RESTART_SCHEDULE", "0 3 * * *")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.DeviceRestartSchedule != "0 3 * * *" {
		t.Errorf("DeviceRestartSchedule = %q, want %q", cfg.DeviceRestartSchedule, "0 3 * * *")
	}
	if cfg.DeviceRestartLocation == nil {
		t.Fatal("DeviceRestartLocation is nil, want non-nil")
	}
	if cfg.DeviceRestartLocation.String() != "UTC" {
		t.Errorf("DeviceRestartLocation = %q, want UTC", cfg.DeviceRestartLocation.String())
	}
}

// TestLoad_DeviceRestart_ValidScheduleWithTZ accepts a valid spec + IANA zone.
func TestLoad_DeviceRestart_ValidScheduleWithTZ(t *testing.T) {
	setRequired(t)
	t.Setenv("DEVICE_RESTART_SCHEDULE", "0 3 * * *")
	t.Setenv("DEVICE_RESTART_TIMEZONE", "Europe/Berlin")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.DeviceRestartLocation == nil {
		t.Fatal("DeviceRestartLocation is nil, want non-nil")
	}
	if cfg.DeviceRestartLocation.String() != "Europe/Berlin" {
		t.Errorf("DeviceRestartLocation = %q, want Europe/Berlin", cfg.DeviceRestartLocation.String())
	}
}

// TestLoad_DeviceRestart_BadSchedule is a hard config error.
func TestLoad_DeviceRestart_BadSchedule(t *testing.T) {
	setRequired(t)
	t.Setenv("DEVICE_RESTART_SCHEDULE", "not a cron spec")

	_, err := config.Load()
	if err == nil {
		t.Fatal("Load() expected error for bad schedule, got nil")
	}
	if !strings.Contains(err.Error(), "DEVICE_RESTART_SCHEDULE") {
		t.Errorf("error %q should mention DEVICE_RESTART_SCHEDULE", err.Error())
	}
}

// TestLoad_DeviceRestart_BadTimezone is a hard config error.
func TestLoad_DeviceRestart_BadTimezone(t *testing.T) {
	setRequired(t)
	t.Setenv("DEVICE_RESTART_SCHEDULE", "0 3 * * *")
	t.Setenv("DEVICE_RESTART_TIMEZONE", "Not/AZone")

	_, err := config.Load()
	if err == nil {
		t.Fatal("Load() expected error for bad timezone, got nil")
	}
	if !strings.Contains(err.Error(), "DEVICE_RESTART_TIMEZONE") {
		t.Errorf("error %q should mention DEVICE_RESTART_TIMEZONE", err.Error())
	}
}

// TestLoad_DeviceRestart_TimezoneIgnoredWhenScheduleEmpty verifies that an
// invalid timezone is silently ignored when no schedule is configured (opt-in).
func TestLoad_DeviceRestart_TimezoneIgnoredWhenScheduleEmpty(t *testing.T) {
	setRequired(t)
	t.Setenv("DEVICE_RESTART_TIMEZONE", "Invalid/Zone")
	// DEVICE_RESTART_SCHEDULE intentionally NOT set.

	_, err := config.Load()
	if err != nil {
		t.Fatalf("Load() unexpected error when schedule is empty: %v", err)
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

func TestLoad_StabilityDefaults(t *testing.T) {
	setRequired(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.PrometheusTimeout != 12*time.Second {
		t.Errorf("PrometheusTimeout default = %v, want 12s", cfg.PrometheusTimeout)
	}
	if cfg.NearFullIdleEntryExportWatts != 25 {
		t.Errorf("NearFullIdleEntryExportWatts default = %d, want 25", cfg.NearFullIdleEntryExportWatts)
	}
}

func TestLoad_NearFullIdleEntryExportWatts(t *testing.T) {
	t.Run("override", func(t *testing.T) {
		setRequired(t)
		t.Setenv("NEAR_FULL_IDLE_ENTRY_EXPORT_WATTS", "40")
		cfg, err := config.Load()
		if err != nil {
			t.Fatalf("Load() unexpected error: %v", err)
		}
		if cfg.NearFullIdleEntryExportWatts != 40 {
			t.Errorf("NearFullIdleEntryExportWatts = %d, want 40", cfg.NearFullIdleEntryExportWatts)
		}
	})

	t.Run("negative", func(t *testing.T) {
		setRequired(t)
		t.Setenv("NEAR_FULL_IDLE_ENTRY_EXPORT_WATTS", "-1")
		_, err := config.Load()
		if err == nil {
			t.Fatal("Load() expected error for negative entry export threshold, got nil")
		}
		if !strings.Contains(err.Error(), "NEAR_FULL_IDLE_ENTRY_EXPORT_WATTS must be >= 0") {
			t.Errorf("error = %q, want NEAR_FULL_IDLE_ENTRY_EXPORT_WATTS validation", err.Error())
		}
	})
}

func TestLoad_PassthroughRecoveryDefaults(t *testing.T) {
	setRequired(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.PassthroughAutoRecovery {
		t.Error("PassthroughAutoRecovery default = true, want false")
	}
	if cfg.PassthroughStallDetectCycles != 5 {
		t.Errorf("PassthroughStallDetectCycles = %d, want 5", cfg.PassthroughStallDetectCycles)
	}
	if cfg.PassthroughStallMinCommandWatts != cfg.MinOutputWatts {
		t.Errorf("PassthroughStallMinCommandWatts = %d, want MinOutputWatts %d", cfg.PassthroughStallMinCommandWatts, cfg.MinOutputWatts)
	}
}

func TestLoad_PassthroughRecoveryAllowsFlashGuardToBeEnabledSeparately(t *testing.T) {
	setRequired(t)
	t.Setenv("PASSTHROUGH_AUTO_RECOVERY", "true")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if !cfg.PassthroughAutoRecovery {
		t.Error("PassthroughAutoRecovery = false, want true")
	}
	if cfg.AllowFlashWrites {
		t.Error("AllowFlashWrites should remain false unless ALLOW_FLASH_WRITES is explicitly set")
	}
}

func TestLoad_PassthroughRecoveryRejectsNegativeValues(t *testing.T) {
	cases := []struct {
		name    string
		envKey  string
		value   string
		wantMsg string
	}{
		{
			name:    "detect cycles",
			envKey:  "PASSTHROUGH_STALL_DETECT_CYCLES",
			value:   "-1",
			wantMsg: "PASSTHROUGH_STALL_DETECT_CYCLES must be >= 0",
		},
		{
			name:    "min command watts",
			envKey:  "PASSTHROUGH_STALL_MIN_COMMAND_WATTS",
			value:   "-1",
			wantMsg: "PASSTHROUGH_STALL_MIN_COMMAND_WATTS must be >= 0",
		},
		{
			name:    "min interval",
			envKey:  "PASSTHROUGH_AUTO_RECOVERY_MIN_INTERVAL",
			value:   "-1s",
			wantMsg: "PASSTHROUGH_AUTO_RECOVERY_MIN_INTERVAL must be >= 0",
		},
		{
			name:    "restore delay",
			envKey:  "PASSTHROUGH_AUTO_RECOVERY_RESTORE_DELAY",
			value:   "-1s",
			wantMsg: "PASSTHROUGH_AUTO_RECOVERY_RESTORE_DELAY must be >= 0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setRequired(t)
			t.Setenv(tc.envKey, tc.value)
			_, err := config.Load()
			if err == nil {
				t.Fatal("Load() expected error for negative value, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantMsg)
			}
		})
	}
}
