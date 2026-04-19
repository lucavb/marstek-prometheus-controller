// Package config loads and validates all controller configuration from
// environment variables.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds the full runtime configuration.
type Config struct {
	// Logging
	LogLevel  string // debug|info|warn|error
	LogFormat string // text|json

	// HTTP
	HTTPListenAddr string

	// Prometheus (input source for grid power)
	PrometheusBaseURL    string
	PrometheusQuery      string
	PrometheusTimeout    time.Duration
	PrometheusStaleAfter time.Duration

	// MQTT
	MQTTBrokerURL           string
	MQTTClientID            string
	MQTTUsername            string
	MQTTPassword            string
	MarstekDeviceType       string
	MarstekDeviceID         string
	MQTTStatusStaleAfter    time.Duration
	MQTTStatusPollTimeout   time.Duration
	MQTTStatusHardFailAfter time.Duration

	// Controller
	ControlInterval       time.Duration
	SmoothingAlpha        float64
	DeadbandWatts         float64
	ImportBiasWatts       int // deliberate grid-import headroom; subtracted from raw target
	RampUpWattsPerCycle   int
	RampDownWattsPerCycle int
	MinCommandDeltaWatts  int
	MinHoldTime           time.Duration
	MinOutputWatts        int
	MaxOutputWatts        int
	PersistToFlash        bool
	AllowFlashWrites      bool

	// Schedule slot
	ScheduleSlot  int    // 1–5
	ScheduleStart string // HH:MM
	ScheduleEnd   string // HH:MM

	// Battery SoC soft floor — prevents commanding discharge when the BMS will
	// gate us anyway. Derived at runtime from devStatus.DoDPercent; these env
	// vars tune the margin and hysteresis around that derived value.
	BatterySoCFloorMarginPercent   int // added to (100 − DoDPercent); default 2
	BatterySoCHysteresisPercent    int // resume SoC = soft floor + this; default 5
	BatterySoCFloorFallbackPercent int // used when DoDPercent is 0/unknown; default 15
}

// Load reads all configuration from environment variables and returns a
// validated Config. Returns an error describing any missing or invalid values.
func Load() (Config, error) {
	pid := os.Getpid()
	cfg := Config{
		LogLevel:  getEnv("LOG_LEVEL", "info"),
		LogFormat: getEnv("LOG_FORMAT", "text"),

		HTTPListenAddr: getEnv("HTTP_LISTEN_ADDR", ":8080"),

		PrometheusBaseURL:    getEnv("PROMETHEUS_BASE_URL", ""),
		PrometheusQuery:      getEnv("PROMETHEUS_GRID_POWER_QUERY", "electricity_power_watts"),
		PrometheusTimeout:    getEnvDuration("PROMETHEUS_TIMEOUT", 5*time.Second),
		PrometheusStaleAfter: getEnvDuration("PROMETHEUS_STALE_AFTER", 60*time.Second),

		MQTTBrokerURL:           getEnv("MQTT_BROKER_URL", ""),
		MQTTClientID:            getEnv("MQTT_CLIENT_ID", fmt.Sprintf("marstek-controller-%d", pid)),
		MQTTUsername:            getEnv("MQTT_USERNAME", ""),
		MQTTPassword:            getEnv("MQTT_PASSWORD", ""),
		MarstekDeviceType:       getEnv("MARSTEK_DEVICE_TYPE", "HMJ-2"),
		MarstekDeviceID:         getEnv("MARSTEK_DEVICE_ID", ""),
		MQTTStatusStaleAfter:    getEnvDuration("MQTT_STATUS_STALE_AFTER", 2*time.Minute),
		MQTTStatusPollTimeout:   getEnvDuration("MQTT_STATUS_POLL_TIMEOUT", 5*time.Second),
		MQTTStatusHardFailAfter: getEnvDuration("MQTT_STATUS_HARD_FAIL_AFTER", 5*time.Minute),

		ControlInterval:       getEnvDuration("CONTROL_INTERVAL", 15*time.Second),
		SmoothingAlpha:        getEnvFloat("SMOOTHING_ALPHA", 0.5),
		DeadbandWatts:         getEnvFloat("DEADBAND_WATTS", 25),
		ImportBiasWatts:       getEnvInt("IMPORT_BIAS_WATTS", 50),
		RampUpWattsPerCycle:   getEnvInt("RAMP_UP_WATTS_PER_CYCLE", 150),
		RampDownWattsPerCycle: getEnvInt("RAMP_DOWN_WATTS_PER_CYCLE", 300),
		MinCommandDeltaWatts:  getEnvInt("MIN_COMMAND_DELTA_WATTS", 25),
		MinHoldTime:           getEnvDuration("MIN_HOLD_TIME", 30*time.Second),
		MinOutputWatts:        getEnvInt("MIN_OUTPUT_WATTS", 80),
		MaxOutputWatts:        getEnvInt("MAX_OUTPUT_WATTS", 800),
		AllowFlashWrites:      getEnvBool("ALLOW_FLASH_WRITES", false),
		PersistToFlash:        getEnvBool("PERSIST_TO_FLASH", false),

		ScheduleSlot:  getEnvInt("SCHEDULE_SLOT", 1),
		ScheduleStart: getEnv("SCHEDULE_START", "00:00"),
		ScheduleEnd:   getEnv("SCHEDULE_END", "23:59"),

		BatterySoCFloorMarginPercent:   getEnvInt("BATTERY_SOC_FLOOR_MARGIN_PERCENT", 2),
		BatterySoCHysteresisPercent:    getEnvInt("BATTERY_SOC_HYSTERESIS_PERCENT", 5),
		BatterySoCFloorFallbackPercent: getEnvInt("BATTERY_SOC_FLOOR_FALLBACK_PERCENT", 15),
	}

	return cfg, cfg.validate()
}

func (c *Config) validate() error {
	var errs []string

	if c.PrometheusBaseURL == "" {
		errs = append(errs, "PROMETHEUS_BASE_URL is required")
	}
	if c.MQTTBrokerURL == "" {
		errs = append(errs, "MQTT_BROKER_URL is required")
	}
	if c.MarstekDeviceID == "" {
		errs = append(errs, "MARSTEK_DEVICE_ID is required")
	}
	if c.SmoothingAlpha <= 0 || c.SmoothingAlpha > 1 {
		errs = append(errs, "SMOOTHING_ALPHA must be in range (0, 1]")
	}
	if c.MaxOutputWatts > 800 {
		c.MaxOutputWatts = 800
	}
	if c.MaxOutputWatts < 0 {
		errs = append(errs, "MAX_OUTPUT_WATTS must be >= 0")
	}
	if c.MinOutputWatts < 0 {
		errs = append(errs, "MIN_OUTPUT_WATTS must be >= 0")
	}
	if c.MinOutputWatts > c.MaxOutputWatts {
		errs = append(errs, "MIN_OUTPUT_WATTS must be <= MAX_OUTPUT_WATTS")
	}
	if c.ScheduleSlot < 1 || c.ScheduleSlot > 5 {
		errs = append(errs, "SCHEDULE_SLOT must be 1–5")
	}
	if !isValidTime(c.ScheduleStart) {
		errs = append(errs, fmt.Sprintf("SCHEDULE_START %q is not a valid HH:MM time", c.ScheduleStart))
	}
	if !isValidTime(c.ScheduleEnd) {
		errs = append(errs, fmt.Sprintf("SCHEDULE_END %q is not a valid HH:MM time", c.ScheduleEnd))
	}
	if c.PersistToFlash && !c.AllowFlashWrites {
		errs = append(errs, "PERSIST_TO_FLASH=true requires ALLOW_FLASH_WRITES=true (foot-gun protection)")
	}
	if c.ControlInterval <= 0 {
		errs = append(errs, "CONTROL_INTERVAL must be positive")
	}
	if c.RampUpWattsPerCycle < 0 {
		errs = append(errs, "RAMP_UP_WATTS_PER_CYCLE must be >= 0 (0 = unlimited)")
	}
	if c.RampDownWattsPerCycle < 0 {
		errs = append(errs, "RAMP_DOWN_WATTS_PER_CYCLE must be >= 0 (0 = unlimited)")
	}
	if c.MinHoldTime < c.ControlInterval {
		// Warn in log rather than hard-fail; set to one control interval minimum.
		c.MinHoldTime = c.ControlInterval
	}
	if c.BatterySoCFloorMarginPercent < 0 || c.BatterySoCFloorMarginPercent > 30 {
		errs = append(errs, "BATTERY_SOC_FLOOR_MARGIN_PERCENT must be 0–30")
	}
	if c.BatterySoCHysteresisPercent < 1 || c.BatterySoCHysteresisPercent > 20 {
		errs = append(errs, "BATTERY_SOC_HYSTERESIS_PERCENT must be 1–20")
	}
	if c.BatterySoCFloorFallbackPercent < 5 || c.BatterySoCFloorFallbackPercent > 50 {
		errs = append(errs, "BATTERY_SOC_FLOOR_FALLBACK_PERCENT must be 5–50")
	}

	if len(errs) > 0 {
		return errors.New("config: " + strings.Join(errs, "; "))
	}
	return nil
}

// isValidTime accepts "HH:MM" with optional single-digit components (e.g. "0:0").
func isValidTime(s string) bool {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return false
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	return err1 == nil && err2 == nil && h >= 0 && h <= 23 && m >= 0 && m <= 59
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}

// warnInvalid logs when an env var is set but not parseable. We keep the
// permissive fallback-to-default behaviour for backward compatibility, but
// make the bad value visible instead of silently swallowing it.
func warnInvalid(key, value string, fallback any, err error) {
	slog.Warn("config: invalid env var, using default",
		"key", key, "value", value, "default", fallback, "err", err)
}

func getEnvInt(key string, fallback int) int {
	if v, ok := os.LookupEnv(key); ok {
		n, err := strconv.Atoi(v)
		if err == nil {
			return n
		}
		warnInvalid(key, v, fallback, err)
	}
	return fallback
}

func getEnvFloat(key string, fallback float64) float64 {
	if v, ok := os.LookupEnv(key); ok {
		f, err := strconv.ParseFloat(v, 64)
		if err == nil {
			return f
		}
		warnInvalid(key, v, fallback, err)
	}
	return fallback
}

func getEnvDuration(key string, fallback time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		d, err := time.ParseDuration(v)
		if err == nil {
			return d
		}
		warnInvalid(key, v, fallback, err)
	}
	return fallback
}

func getEnvBool(key string, fallback bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		b, err := strconv.ParseBool(v)
		if err == nil {
			return b
		}
		warnInvalid(key, v, fallback, err)
	}
	return fallback
}
