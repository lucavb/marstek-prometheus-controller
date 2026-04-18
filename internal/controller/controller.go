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
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"strings"
	"time"

	"github.com/lucavb/marstek-prometheus-controller/internal/marstek"
	"github.com/lucavb/marstek-prometheus-controller/internal/metrics"
	"github.com/lucavb/marstek-prometheus-controller/internal/promclient"
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
	MinOutputWatts        int
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

	// lastStatus caches the most recent successfully-read device status so that
	// fallback() can preserve the user's other four schedule slots rather than
	// wiping them to zero (see AGENTS.md "Preserve all five slots on every write").
	lastStatus    marstek.Status
	hasLastStatus bool
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

	c.lastStatus = devStatus
	c.hasLastStatus = true

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
	// The grid meter already reflects the battery's contribution, so using the
	// raw grid reading as an absolute target causes the loop to converge to
	// grid_ss = (load + bias)/2 rather than to bias.  The fix is to treat the
	// grid error as a correction on top of what the device is currently
	// delivering (g1+g2).  Fixed-point analysis: B_next = B + grid - k, solved
	// at grid = k — the bias now does exactly what its name says.
	currentOutput := devStatus.Output1Watts + devStatus.Output2Watts
	rawTarget := currentOutput + int(math.Round(smoothed)) - c.cfg.ImportBiasWatts
	if rawTarget < 0 {
		rawTarget = 0
	}
	if rawTarget > c.cfg.MaxOutputWatts {
		rawTarget = c.cfg.MaxOutputWatts
	}
	// Snap dead zone up: device silently clamps v=0..79 to 80W, so we must
	// never target an unreachable wattage. Zero stays zero (stop = disable slot).
	if rawTarget > 0 && rawTarget < c.cfg.MinOutputWatts {
		rawTarget = c.cfg.MinOutputWatts
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
	exporting := smoothed < 0
	if exporting && ramped > rawTarget {
		ramped = rawTarget
	}

	// Collapse dead-zone values after ramping. If we're aiming for 0 and the
	// ramp left us in 1..MinOutputWatts-1, jump to 0 (disable slot). If we're
	// aiming for a positive value the ramp hasn't reached yet, hold at the floor.
	if ramped > 0 && ramped < c.cfg.MinOutputWatts {
		if rawTarget == 0 {
			ramped = 0
		} else {
			ramped = c.cfg.MinOutputWatts
		}
	}

	if abs(ramped-c.lastCommandWatts) < c.cfg.MinCommandDeltaWatts {
		if c.m != nil {
			c.m.CommandSuppressedTotal.WithLabelValues("delta").Inc()
			c.updateState(smoothed)
		}
		c.ready = true
		return nil
	}

	// The export fast-path must remain truly immediate: if the grid is exporting
	// and we are reducing the command, skip the hold-time suppression. Every
	// extra second of discharge during export is energy we cannot recover.
	holdTimeActive := !c.lastCommandTime.IsZero() && now.Sub(c.lastCommandTime) < c.cfg.MinHoldTime
	fastPathBypass := exporting && ramped < c.lastCommandWatts
	if holdTimeActive && !fastPathBypass {
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
	// ramped==0 means "stop discharge"; the device ignores v=0 on an enabled
	// slot and clamps it to 80W, so we must disable the slot instead.
	slots[idx].Enabled = ramped > 0
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

// applyRamp clamps the per-cycle change from current to target by the
// configured ramp limits. A zero RampUp/DownWattsPerCycle means "unlimited" —
// the target passes through unchanged. Negative values are rejected in
// Config.validate.
func (c *Controller) applyRamp(current, target int) int {
	if target > current {
		if c.cfg.RampUpWattsPerCycle <= 0 {
			return target
		}
		limit := current + c.cfg.RampUpWattsPerCycle
		if target > limit {
			return limit
		}
		return target
	}
	if target < current {
		if c.cfg.RampDownWattsPerCycle <= 0 {
			return target
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

	// Prefer the last known device status so the other four schedule slots are
	// preserved. Fall back to a zero status only if we have never successfully
	// read one — in that edge case, we have nothing to preserve anyway.
	base := marstek.Status{}
	if c.hasLastStatus {
		base = c.lastStatus
	}
	slots := marstek.SlotsAsWriteSlots(base)
	idx := c.cfg.ScheduleSlot - 1
	if idx < 0 || idx > 4 {
		idx = 0
	}
	// Disable the slot rather than sending Watts=0: the device silently clamps
	// v=0 to 80W on an enabled slot, so Enabled=false is the only real stop.
	slots[idx] = marstek.Slot{
		Enabled: false,
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
	switch {
	case errors.Is(err, promclient.ErrEmptyResult):
		return "empty"
	case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
		return "timeout"
	case errors.Is(err, promclient.ErrParse):
		return "parse"
	default:
		return "other"
	}
}

// mqttNotConnectedMarker matches the exported prefix of
// mqttclient.ErrNotConnected. The controller package is not permitted to
// import internal/mqttclient directly (see AGENTS.md), so we match the
// sentinel's stable textual prefix rather than the sentinel value itself.
const mqttNotConnectedMarker = "mqttclient: not connected"

func classifyMQTTError(err error) string {
	if err == nil {
		return "none"
	}
	switch {
	case strings.Contains(err.Error(), mqttNotConnectedMarker):
		return "disconnected"
	case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
		return "timeout"
	default:
		return "other"
	}
}

// isTimeout returns true for any net.Error that reports Timeout().
func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
