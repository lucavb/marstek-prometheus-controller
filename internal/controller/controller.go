// Package controller implements the battery discharge control loop.
//
// Each iteration (Step):
//  1. Query Prometheus for the current grid power (electricity_power_watts).
//  2. Obtain live device status from the MQTT status source (with self-poll
//     watchdog if stale).
//  3. Apply EMA smoothing to the grid power signal.
//  4. Compute a raw target: (smoothed - ImportBiasWatts) clamped to [0, maxOutput].
//     The bias keeps a small deliberate grid import so the battery never tries to
//     reach exact zero — exported energy cannot be recovered.
//  5. Apply ramp limits (export fast-path skips ramp-down when grid is negative)
//     and min-hold-time suppression.
//  6. Publish a full 5-slot timed-discharge command with the new slot power.
//  7. Update all Prometheus metrics.
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

	// Near-full idle — disables the controlled slot near full charge so the
	// controller stops fighting the BMS and lets the device's firmware
	// surplus-feed-in path route excess PV to the grid. Gated on the device
	// reporting SurplusFeedIn=true; without it, normal control runs because
	// idling would strand PV at full SoC.
	NearFullIdleEnabled            bool
	NearFullIdleEnterPercent       int
	NearFullIdleExitPercent        int
	NearFullIdleConsecutiveSamples int
	// NearFullIdleEntryExportWatts is the minimum smoothed export required to
	// enter idle; it keeps meter noise around zero from disabling discharge.
	NearFullIdleEntryExportWatts int

	// Secondary exit out of near-full idle based on sustained grid import.
	// The SoC-based exit alone deadlocks at full charge when no discharge is
	// happening (SoC cannot fall, so the exit threshold is never reached);
	// when smoothed grid import exceeds NearFullIdleGridImportExitWatts for
	// NearFullIdleGridImportExitSamples consecutive cycles, idle exits and
	// normal control resumes, letting the battery cover house load.
	// Samples=0 disables this exit path entirely.
	NearFullIdleGridImportExitWatts   int
	NearFullIdleGridImportExitSamples int

	// Pass-through stall detection and opt-in recovery. Auto-recovery publishes
	// the device's flash-only surplus-feed-in command, so AllowFlashWrites must
	// also be true before any recovery write is attempted.
	PassthroughStallDetectCycles        int
	PassthroughStallMinCommandWatts     int
	PassthroughAutoRecovery             bool
	PassthroughAutoRecoveryMinInterval  time.Duration
	PassthroughAutoRecoveryRestoreDelay time.Duration
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
	smoothed         float64
	hasSmoothed      bool
	lastCommandWatts int
	lastCommandTime  time.Time
	loggedFirmware   bool
	ready            bool
	lastStatusWarnAt time.Time

	// lastStatus caches the most recent successfully-read device status so that
	// fallback() can preserve the user's other four schedule slots rather than
	// wiping them to zero (see AGENTS.md "Preserve all five slots on every write").
	lastStatus    marstek.Status
	hasLastStatus bool

	// socFloorActive is true while SoC is below the derived soft floor.
	// It stays true until SoC climbs back above (softFloor + hysteresis).
	socFloorActive bool

	// nearFullIdleActive is true while the controller is suppressing discharge
	// because SoC is in the near-full band. Entry and exit are debounced by
	// NearFullIdleConsecutiveSamples samples each so LFP top-end SoC flicker
	// does not flap the mode.
	nearFullIdleActive bool
	// nearFullIdleEnterSamples counts consecutive inactive-state cycles where
	// SoC >= NearFullIdleEnterPercent and the regime is otherwise gated on.
	nearFullIdleEnterSamples int
	// nearFullIdleExitSamples counts consecutive active-state cycles where
	// SoC < NearFullIdleExitPercent.
	nearFullIdleExitSamples int
	// nearFullIdleGridImportSamples counts consecutive active-state cycles
	// where smoothed grid power exceeds NearFullIdleGridImportExitWatts. It
	// feeds the secondary "grid_import" exit reason, which breaks the
	// SoC-deadlock when solar drops below house load while the battery sits
	// at full charge.
	nearFullIdleGridImportSamples int

	// transientZeroFiredLastCycle prevents the transient-zero-output guard from
	// holding for more than one consecutive cycle.
	transientZeroFiredLastCycle bool

	// pass-through stall/recovery state.
	passthroughStallCycles            int
	passthroughStallActive            bool
	passthroughRecoveryActive         bool
	passthroughRecoveryStartedAt      time.Time
	lastPassthroughRecoveryAt         time.Time
	surplusFeedInDisabledByController bool
	loggedPassthroughRecoveryBlocked  bool
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

// resetNearFullIdleState clears all near-full idle state. When the mode was
// previously active the exit counters are bumped so operators can distinguish
// deliberate SoC-driven exits from fallback / config-driven resets. Returns
// true when the reset took effect from an active state.
func (c *Controller) resetNearFullIdleState(reason string) bool {
	wasActive := c.nearFullIdleActive
	c.nearFullIdleActive = false
	c.nearFullIdleEnterSamples = 0
	c.nearFullIdleExitSamples = 0
	c.nearFullIdleGridImportSamples = 0
	if c.m != nil {
		c.m.NearFullIdleActive.Set(0)
		if wasActive {
			c.m.NearFullIdleExited.Inc()
			if reason != "" {
				c.m.NearFullIdleExitReasonTotal.WithLabelValues(reason).Inc()
			}
		}
	}
	return wasActive
}
