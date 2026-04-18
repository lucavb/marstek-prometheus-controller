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
	"fmt"
	"log/slog"
	"math"
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

	// Control
	ControlInterval       time.Duration
	SmoothingAlpha        float64
	DeadbandWatts         float64
	ImportBiasWatts       int // subtract from raw target; keeps a deliberate grid import margin
	RampUpWattsPerCycle   int
	RampDownWattsPerCycle int
	MinCommandDeltaWatts  int
	MinHoldTime           time.Duration
	MaxOutputWatts        int

	// Schedule slot
	ControlTopic  string
	ScheduleSlot  int    // 1-based, 1–5
	ScheduleStart string // HH:MM
	ScheduleEnd   string // HH:MM

	// Flash writes
	PersistToFlash bool
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
		m.MaxOutputWatts.Set(float64(cfg.MaxOutputWatts))
		m.SetState(metrics.StateStarting)
	}
	slog.Info("controller configured",
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

// Step executes one control-loop iteration. It is exported for testing.
func (c *Controller) Step(ctx context.Context) error {
	start := c.clock.Now()
	if c.m != nil {
		c.m.ControlCyclesTotal.Inc()
		defer func() {
			c.m.ControlLoopDurationSecs.Observe(time.Since(start).Seconds())
		}()
	}

	// ── 1. Read grid power from Prometheus ───────────────────────────────────
	if c.m != nil {
		c.m.PrometheusQueriesTotal.Inc()
	}
	sample, promErr := c.prom.Query(ctx)
	if promErr != nil {
		slog.Warn("prometheus query failed", "err", promErr)
		if c.m != nil {
			c.m.PrometheusErrorsTotal.WithLabelValues(classifyPromError(promErr)).Inc()
			c.m.PrometheusUp.Set(0)
		}
		return c.fallback(ctx, "prometheus_error")
	}

	now := c.clock.Now()
	if now.Sub(sample.SampleTime) > c.cfg.PrometheusStaleAfter {
		slog.Warn("prometheus sample is stale",
			"age", now.Sub(sample.SampleTime).Round(time.Second),
			"threshold", c.cfg.PrometheusStaleAfter)
		if c.m != nil {
			c.m.PrometheusErrorsTotal.WithLabelValues("stale").Inc()
			c.m.PrometheusUp.Set(0)
		}
		return c.fallback(ctx, "prometheus_stale")
	}

	if c.m != nil {
		c.m.GridPowerWatts.Set(sample.Watts)
		c.m.RecordLastPrometheusSuccess(now)
	}

	// ── 2. Obtain device status ───────────────────────────────────────────────
	devStatus, statusReceivedAt := c.status.LatestStatus()
	statusAge := now.Sub(statusReceivedAt)

	if statusReceivedAt.IsZero() || statusAge > c.cfg.StatusStaleAfter {
		label := "status_stale"
		if statusReceivedAt.IsZero() {
			label = "no_status_yet"
			slog.Info("no device status yet, issuing self-poll")
		} else {
			slog.Warn("device status stale, issuing self-poll", "age", statusAge.Round(time.Second))
		}
		if c.m != nil {
			c.m.SelfPollsTotal.Inc()
			c.m.MQTTPublishesTotal.WithLabelValues("self_poll").Inc()
		}
		polled, err := c.status.Poll(ctx, c.cfg.StatusPollTimeout)
		if err != nil {
			slog.Warn("self-poll failed", "err", err)
			if statusAge > c.cfg.StatusHardFailAfter {
				slog.Error("device status hard fail, falling back to zero discharge",
					"age", statusAge.Round(time.Second))
				return c.fallback(ctx, "mqtt_status_stale")
			}
			// Freeze control at last commanded value until status returns.
			if c.m != nil {
				c.m.CommandSuppressedTotal.WithLabelValues("status_stale").Inc()
				c.m.SetState(metrics.StateHolding)
			}
			slog.Warn("freezing control until device status returns",
				"label", label, "last_command_watts", c.lastCommandWatts)
			return nil
		}
		devStatus = polled
		statusReceivedAt = now
		statusAge = 0
	}

	if c.m != nil {
		c.m.LastStatusAgeSecs.Set(statusAge.Seconds())
	}

	// One-time startup warnings.
	if !c.loggedFirmware {
		c.loggedFirmware = true
		slog.Info("device status received",
			"firmware", fmt.Sprintf("%d.%d", devStatus.FirmwareMajor, devStatus.FirmwareSub),
			"soc_pct", devStatus.SOCPercent,
			"rated_output_w", devStatus.RatedOutputWatts,
			"charging_mode", devStatus.ChargingMode,
			"o1", devStatus.Output1Enabled, "o2", devStatus.Output2Enabled,
			"surplus_feed_in", devStatus.SurplusFeedIn)
		if devStatus.ChargingMode != 0 {
			slog.Warn("device is NOT in simultaneous charge+discharge mode (cs!=0); zero-export control may be ineffective")
		}
		if devStatus.Output1Enabled == 0 && devStatus.Output2Enabled == 0 {
			slog.Warn("both output ports are disabled; discharge commands will produce 0 W until outputs are enabled")
		}
		if devStatus.SurplusFeedIn {
			slog.Warn("surplus feed-in is enabled; this may interfere with zero-export control")
		}
	}

	// ── 3. Smooth the grid power signal ──────────────────────────────────────
	smoothed := c.smooth(sample.Watts)
	if c.m != nil {
		c.m.SmoothedGridPowerWatts.Set(smoothed)
	}

	// ── 4. Compute raw target ─────────────────────────────────────────────────
	// Positive grid power = importing from grid → battery should discharge more.
	// Negative grid power = exporting to grid → reduce battery discharge.
	// ImportBiasWatts is subtracted so the setpoint always aims to leave a small
	// deliberate import on the grid rather than driving to exact zero.  Exported
	// energy cannot be recovered, so erring on the side of slight import is
	// preferable to accidental export.
	rawTarget := int(math.Round(smoothed)) - c.cfg.ImportBiasWatts
	if rawTarget < 0 {
		rawTarget = 0
	}
	if rawTarget > c.cfg.MaxOutputWatts {
		rawTarget = c.cfg.MaxOutputWatts
	}

	if c.m != nil {
		c.m.TargetSlotPowerWatts.Set(float64(rawTarget))
	}

	// ── 5. Apply ramp and hold-time suppression ───────────────────────────────
	ramped := c.applyRamp(c.lastCommandWatts, rawTarget)
	// Export fast-path: if the grid is currently exporting (smoothed < 0) the
	// ramp-down limit must not slow our response — every watt still discharging
	// is energy we are giving away and cannot recover.  Jump straight to the
	// rawTarget (which is 0 once the bias is applied) regardless of ramp pace.
	if smoothed < 0 && ramped > rawTarget {
		ramped = rawTarget
	}

	if abs(ramped-c.lastCommandWatts) < c.cfg.MinCommandDeltaWatts {
		if c.m != nil {
			c.m.CommandSuppressedTotal.WithLabelValues("delta").Inc()
			c.updateState(smoothed)
		}
		c.ready = true
		return nil
	}

	if !c.lastCommandTime.IsZero() && now.Sub(c.lastCommandTime) < c.cfg.MinHoldTime {
		if c.m != nil {
			c.m.CommandSuppressedTotal.WithLabelValues("hold_time").Inc()
			c.updateState(smoothed)
		}
		c.ready = true
		return nil
	}

	// ── 6. Publish the new schedule power ────────────────────────────────────
	slots := marstek.SlotsAsWriteSlots(devStatus)
	idx := c.cfg.ScheduleSlot - 1
	slots[idx].Enabled = true
	slots[idx].Start = c.cfg.ScheduleStart
	slots[idx].End = c.cfg.ScheduleEnd
	slots[idx].Watts = ramped

	payload := marstek.BuildTimedDischargePayload(slots, c.cfg.PersistToFlash)
	slog.Debug("publishing schedule update",
		"slot", c.cfg.ScheduleSlot, "watts", ramped,
		"prev_watts", c.lastCommandWatts, "payload", payload)

	if err := c.pub.Publish(c.cfg.ControlTopic, payload); err != nil {
		slog.Warn("mqtt publish failed", "err", err)
		if c.m != nil {
			c.m.MQTTPublishErrorsTotal.WithLabelValues(classifyMQTTError(err)).Inc()
			c.m.CommandSuppressedTotal.WithLabelValues("disconnected").Inc()
		}
		return err
	}

	c.lastCommandWatts = ramped
	c.lastCommandTime = now
	if c.m != nil {
		c.m.CommandedSlotPowerWatts.Set(float64(ramped))
		c.m.MQTTPublishesTotal.WithLabelValues("write").Inc()
		c.m.RecordLastMQTTPublish(now)
		c.updateState(smoothed)
	}
	c.ready = true

	slog.Info("schedule updated",
		"slot", c.cfg.ScheduleSlot, "watts", ramped,
		"grid_w", math.Round(sample.Watts),
		"smoothed_w", math.Round(smoothed),
		"soc_pct", devStatus.SOCPercent)
	return nil
}

// Ready returns true once the controller has completed at least one successful
// Prometheus read and one successful MQTT publish.
func (c *Controller) Ready() bool {
	return c.ready
}

func (c *Controller) smooth(watts float64) float64 {
	if !c.hasSmoothed {
		c.smoothed = watts
		c.hasSmoothed = true
		return watts
	}
	c.smoothed = c.cfg.SmoothingAlpha*watts + (1-c.cfg.SmoothingAlpha)*c.smoothed
	return c.smoothed
}

func (c *Controller) applyRamp(current, target int) int {
	if target > current {
		limit := current + c.cfg.RampUpWattsPerCycle
		if c.cfg.RampUpWattsPerCycle == 0 {
			return current
		}
		if target > limit {
			return limit
		}
		return target
	}
	if target < current {
		if c.cfg.RampDownWattsPerCycle == 0 {
			return current
		}
		limit := current - c.cfg.RampDownWattsPerCycle
		if target < limit {
			return limit
		}
		return target
	}
	return target
}

func (c *Controller) updateState(smoothed float64) {
	if c.m == nil {
		return
	}
	switch {
	case c.lastCommandWatts > 0:
		c.m.SetState(metrics.StateDischarging)
	case math.Abs(smoothed) <= c.cfg.DeadbandWatts:
		c.m.SetState(metrics.StateHolding)
	default:
		c.m.SetState(metrics.StateIdle)
	}
}

// fallback commands zero discharge and marks the fallback counter.
func (c *Controller) fallback(ctx context.Context, reason string) error {
	if c.m != nil {
		c.m.FallbackTotal.WithLabelValues(reason).Inc()
		c.m.SetState(metrics.StateFallback)
	}

	if c.lastCommandWatts == 0 {
		return nil
	}

	slots := marstek.SlotsAsWriteSlots(marstek.Status{})
	idx := c.cfg.ScheduleSlot - 1
	if idx < 0 || idx > 4 {
		idx = 0
	}
	slots[idx] = marstek.Slot{
		Enabled: true,
		Start:   c.cfg.ScheduleStart,
		End:     c.cfg.ScheduleEnd,
		Watts:   0,
	}

	payload := marstek.BuildTimedDischargePayload(slots, false)
	if err := c.pub.Publish(c.cfg.ControlTopic, payload); err != nil {
		slog.Warn("fallback publish failed", "err", err, "reason", reason)
		if c.m != nil {
			c.m.MQTTPublishErrorsTotal.WithLabelValues(classifyMQTTError(err)).Inc()
		}
		return err
	}

	slog.Warn("fallback: commanded zero discharge", "reason", reason)
	c.lastCommandWatts = 0
	c.lastCommandTime = c.clock.Now()
	if c.m != nil {
		c.m.CommandedSlotPowerWatts.Set(0)
		c.m.MQTTPublishesTotal.WithLabelValues("write").Inc()
		c.m.RecordLastMQTTPublish(c.lastCommandTime)
	}
	return nil
}

func classifyPromError(err error) string {
	if err == nil {
		return "none"
	}
	msg := err.Error()
	switch {
	case contains(msg, "empty result"):
		return "empty"
	case contains(msg, "context deadline") || contains(msg, "timeout"):
		return "timeout"
	case contains(msg, "parse"):
		return "parse"
	default:
		return "other"
	}
}

func classifyMQTTError(err error) string {
	if err == nil {
		return "none"
	}
	msg := err.Error()
	switch {
	case contains(msg, "not connected") || contains(msg, "disconnected"):
		return "disconnected"
	case contains(msg, "timeout"):
		return "timeout"
	default:
		return "other"
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(substr); i++ {
				if s[i:i+len(substr)] == substr {
					return true
				}
			}
			return false
		}())
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
