// Package controller implements the battery discharge control loop.
//
// Each iteration (Step):
//  1. Query Prometheus and obtain fresh MQTT status.
//  2. Derive device facts and enforce runtime authority.
//  3. Choose a policy outcome: safety idle, full-charge pass-through idle, or
//     normal discharge control.
//  4. Compute one discharge target, then apply ramp and publish suppression.
//
// Control is intentionally biased toward slight grid import to avoid wasting
// energy through accidental export. The B2500 enforces its own DoD/SOC floor.
package controller

import (
	"context"
	"log/slog"
	"time"

	"github.com/lucavb/marstek-prometheus-controller/internal/marstek"
	"github.com/lucavb/marstek-prometheus-controller/internal/metrics"
)

// Config holds all tunable parameters for the control loop.
type Config struct {
	// Prometheus
	PrometheusStaleAfter time.Duration

	// MQTT status freshness
	StatusStaleAfter    time.Duration
	StatusPollTimeout   time.Duration
	StatusHardFailAfter time.Duration
	DeviceID            string

	// Control
	ControlInterval               time.Duration
	SmoothingAlpha                float64
	DeadbandWatts                 float64
	ImportBiasWatts               int // subtract from raw target; keeps a deliberate grid import margin
	RampUpWattsPerCycle           int
	RampDownWattsPerCycle         int
	MinCommandDeltaWatts          int
	MinCommandDeltaWattsExporting int // applied when smoothed grid < 0 (exporting)
	MinHoldTime                   time.Duration
	MinOutputWatts                int
	MaxOutputWatts                int

	// Schedule slot
	ControlTopic  string
	ScheduleSlot  int    // 1-based, 1–5
	ScheduleStart string // HH:MM
	ScheduleEnd   string // HH:MM

	// Flash writes
	PersistToFlash   bool
	AllowFlashWrites bool

	// Battery SoC soft floor — prevents commanding discharge when the BMS will
	// gate us anyway (see AGENTS.md "Don't fight the BMS").
	BatterySoCFloorMarginPercent   int
	BatterySoCHysteresisPercent    int
	BatterySoCFloorFallbackPercent int

	// Top-charge idle — disables the controlled slot only when the battery is
	// truly full and there is evidence that PV can be passed through by firmware.
	NearFullIdleEnabled               bool
	NearFullIdleEnterPercent          int // default 100: only true full charge enters top-charge idle
	NearFullIdleConsecutiveSamples    int // consecutive samples for entry and SoC exit debounce
	NearFullIdleGridImportExitWatts   int
	NearFullIdleGridImportExitSamples int

	// Surplus feed-in authority. The device exposes this as a persistent setting,
	// so automatic remediation is gated by AllowFlashWrites and rate-limited.
	SurplusFeedInRecoveryMinInterval time.Duration

	// Nuclear restart recovery. This is a deliberately opt-in last resort for a
	// device that accepted runtime commands but still contributes no battery
	// output. cd=10 can drop WiFi, so config requires an explicit recovery ack.
	NuclearRestartEnabled         bool
	NuclearRestartAckWiFiRecovery bool
	NuclearRestartBlockedCycles   int
	NuclearRestartMinInterval     time.Duration
}

// Controller is the main control loop.
type Controller struct {
	cfg    Config
	prom   PromReader
	pub    Publisher
	status StatusSource
	clock  Clock
	m      *metrics.Metrics

	// State
	smoothed                float64
	hasSmoothed             bool
	lastCommandWatts        int
	lastCommandTime         time.Time
	loggedFirmware          bool
	ready                   bool
	lastStatusWarnAt        time.Time
	authorityPendingPayload string
	authorityPendingSince   time.Time
	authorityPendingSeenAt  time.Time

	// lastStatus caches the most recent successfully-read device status so that
	// fallback() can preserve the user's other four schedule slots rather than
	// wiping them to zero (see AGENTS.md "Preserve all five slots on every write").
	lastStatus    marstek.Status
	hasLastStatus bool

	// socFloorActive is true while SoC is below the derived soft floor.
	// It stays true until SoC climbs back above (softFloor + hysteresis).
	socFloorActive bool

	// topChargeIdleActive is true while the controller is intentionally leaving
	// the controlled slot disabled so full-charge PV can pass through firmware.
	topChargeIdleActive         bool
	topChargeIdleEnterSamples   int
	topChargeIdleSoCExitSamples int
	topChargeIdleImportSamples  int

	// outputBlockedCycles counts consecutive cycles where the controller wants
	// discharge, the slot is already armed, but the device still reports no
	// solar and no output. This is treated as a runtime precondition failure
	// (outputs blocked), not as a control-law problem.
	outputBlockedCycles int

	lastSurplusFeedInRecoveryAt time.Time
	loggedSurplusFeedInBlocked  bool
	lastOutputEnableAttemptAt   time.Time
	lastNuclearRestartAt        time.Time
	loggedNuclearRestartBlocked bool
}

// New creates a Controller. All fields of cfg must be set; clock may be nil
// (defaults to RealClock).
func New(cfg Config, prom PromReader, pub Publisher, status StatusSource, clock Clock, m *metrics.Metrics) *Controller {
	if clock == nil {
		clock = RealClock{}
	}
	c := &Controller{
		cfg:    cfg,
		prom:   prom,
		pub:    pub,
		status: status,
		clock:  clock,
		m:      m,
	}
	if m != nil {
		m.SlotIndex.Set(float64(cfg.ScheduleSlot))
		m.MinOutputWatts.Set(float64(cfg.MinOutputWatts))
		m.MaxOutputWatts.Set(float64(cfg.MaxOutputWatts))
		m.SetState(metrics.StateStarting)
	}
	slog.Info("controller configured",
		"min_output_watts", cfg.MinOutputWatts,
		"max_output_watts", cfg.MaxOutputWatts,
		"import_bias_watts", cfg.ImportBiasWatts,
		"ramp_up_w_per_cycle", cfg.RampUpWattsPerCycle,
		"ramp_down_w_per_cycle", cfg.RampDownWattsPerCycle,
		"smoothing_alpha", cfg.SmoothingAlpha,
	)
	return c
}

// Run executes the control loop until ctx is cancelled.
func (c *Controller) Run(ctx context.Context) error {
	if err := c.Step(ctx); err != nil {
		slog.Warn("initial controller step failed", "err", err)
	}
	ticker := time.NewTicker(c.cfg.ControlInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := c.Step(ctx); err != nil {
				slog.Warn("controller step failed", "err", err)
			}
		}
	}
}

// Ready returns true once the controller has completed at least one full
// control step that successfully read Prometheus and observed a live device
// status over MQTT. Steps suppressed by deadband, hold-time, or
// command-delta still count because both dependency checks passed. An MQTT
// publish is not required: the very first sample may legitimately be within
// the deadband and suppress the write.
func (c *Controller) Ready() bool {
	return c.ready
}

// resetTopChargeIdleState clears top-charge idle state. When the mode was
// active, metrics record the exit reason.
func (c *Controller) resetTopChargeIdleState(reason string) bool {
	wasActive := c.topChargeIdleActive
	c.topChargeIdleActive = false
	c.topChargeIdleEnterSamples = 0
	c.topChargeIdleSoCExitSamples = 0
	c.topChargeIdleImportSamples = 0
	if c.m != nil {
		c.m.TopChargeIdleActive.Set(0)
		if wasActive {
			c.m.TopChargeIdleExited.Inc()
			if reason != "" {
				c.m.TopChargeIdleExitReasonTotal.WithLabelValues(reason).Inc()
			}
		}
	}
	return wasActive
}
