package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/lucavb/marstek-prometheus-controller/internal/marstek"
	"github.com/lucavb/marstek-prometheus-controller/internal/metrics"
)

var errStepHandled = errors.New("controller step handled")

// Step executes one control-loop iteration. It is exported for testing.
func (c *Controller) Step(ctx context.Context) error {
	start := c.clock.Now()
	if c.m != nil {
		c.m.ControlCyclesTotal.Inc()
		defer func() {
			c.m.ControlLoopDurationSecs.Observe(time.Since(start).Seconds())
		}()
	}

	sample, now, err := c.readGridSample(ctx)
	if err != nil {
		if errors.Is(err, errStepHandled) {
			return nil
		}
		return err
	}

	devStatus, statusReceivedAt, statusAge, err := c.readDeviceStatus(ctx, now)
	if err != nil {
		if errors.Is(err, errStepHandled) {
			return nil
		}
		return err
	}
	c.recordDeviceStatus(devStatus, statusAge)

	c.lastStatus = devStatus
	c.hasLastStatus = true

	softFloor, resumeAt := c.updateSoCFloor(devStatus)
	c.logFirstStatus(devStatus)

	if handled, err := c.ensureDeviceConfig(now, statusReceivedAt, devStatus); err != nil {
		return err
	} else if handled {
		c.ready = true
		return nil
	}
	if handled, err := c.ensureSurplusFeedIn(now, devStatus); err != nil {
		return err
	} else if handled {
		c.ready = true
		return nil
	}

	smoothed := c.smooth(sample.Watts)
	if c.m != nil {
		c.m.SmoothedGridPowerWatts.Set(smoothed)
	}

	if c.socFloorActive {
		slog.Debug("soc below soft floor, suppressing discharge",
			"soc_pct", devStatus.SOCPercent,
			"soft_floor_pct", softFloor,
			"resume_at_pct", resumeAt,
			"dod_pct", devStatus.DoDPercent)
		c.ready = true
		c.resetTopChargeIdleState("soc_floor")
		c.outputBlockedCycles = 0
		c.lastOutputEnableAttemptAt = time.Time{}
		c.loggedNuclearRestartBlocked = false
		return c.commandIdle(ctx, now, statusReceivedAt, devStatus, "soc_floor")
	}

	if c.updateTopChargeIdle(devStatus, smoothed) {
		c.ready = true
		c.outputBlockedCycles = 0
		c.lastOutputEnableAttemptAt = time.Time{}
		c.loggedNuclearRestartBlocked = false
		return c.commandIdle(ctx, now, statusReceivedAt, devStatus, "top_charge_idle")
	}

	rawTarget := c.computeRawTarget(devStatus, smoothed)
	if c.m != nil {
		c.m.TargetSlotPowerWatts.Set(float64(rawTarget))
	}
	if handled, err := c.maybeEnsureOutputEnabled(now, devStatus, rawTarget, smoothed); err != nil {
		return err
	} else if handled {
		c.ready = true
		return nil
	}

	return c.publishNormalControl(ctx, now, statusReceivedAt, devStatus, sample.Watts, smoothed, rawTarget)
}

func (c *Controller) readGridSample(ctx context.Context) (sampleWatts struct {
	Watts      float64
	SampleTime time.Time
}, now time.Time, err error) {
	if c.m != nil {
		c.m.PrometheusQueriesTotal.Inc()
	}
	sample, promErr := c.prom.Query(ctx)
	now = c.clock.Now()
	if promErr != nil {
		slog.Warn("prometheus query failed", "err", promErr)
		if c.m != nil {
			c.m.PrometheusErrorsTotal.WithLabelValues(classifyPromError(promErr)).Inc()
			c.m.PrometheusUp.Set(0)
		}
		if err := c.fallback(ctx, "prometheus_error"); err != nil {
			return sampleWatts, now, err
		}
		return sampleWatts, now, errStepHandled
	}
	if now.Sub(sample.SampleTime) > c.cfg.PrometheusStaleAfter {
		slog.Warn("prometheus sample is stale",
			"age", now.Sub(sample.SampleTime).Round(time.Second),
			"threshold", c.cfg.PrometheusStaleAfter)
		if c.m != nil {
			c.m.PrometheusErrorsTotal.WithLabelValues("stale").Inc()
			c.m.PrometheusUp.Set(0)
		}
		if err := c.fallback(ctx, "prometheus_stale"); err != nil {
			return sampleWatts, now, err
		}
		return sampleWatts, now, errStepHandled
	}
	if c.m != nil {
		c.m.GridPowerWatts.Set(sample.Watts)
		c.m.RecordLastPrometheusSuccess(now)
	}
	return struct {
		Watts      float64
		SampleTime time.Time
	}{Watts: sample.Watts, SampleTime: sample.SampleTime}, now, nil
}

func (c *Controller) readDeviceStatus(ctx context.Context, now time.Time) (marstek.Status, time.Time, time.Duration, error) {
	devStatus, statusReceivedAt := c.status.LatestStatus()
	statusAge := now.Sub(statusReceivedAt)
	statusWarnThreshold := c.cfg.StatusHardFailAfter / 2

	if !statusReceivedAt.IsZero() && statusWarnThreshold > 0 && statusAge > statusWarnThreshold {
		if c.lastStatusWarnAt.IsZero() || now.Sub(c.lastStatusWarnAt) >= time.Minute {
			slog.Warn("device status has been silent for too long",
				"device_id", c.cfg.DeviceID,
				"status_silent_seconds", statusAge.Seconds(),
				"warn_after_seconds", statusWarnThreshold.Seconds(),
				"hard_fail_after_seconds", c.cfg.StatusHardFailAfter.Seconds())
			c.lastStatusWarnAt = now
		}
	} else {
		c.lastStatusWarnAt = time.Time{}
	}

	if !statusReceivedAt.IsZero() && statusAge <= c.cfg.StatusStaleAfter {
		return devStatus, statusReceivedAt, statusAge, nil
	}

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
		if !statusReceivedAt.IsZero() && statusAge > c.cfg.StatusHardFailAfter {
			slog.Error("device status hard fail, falling back to zero discharge",
				"age", statusAge.Round(time.Second))
			if err := c.fallback(ctx, "mqtt_status_stale"); err != nil {
				return devStatus, statusReceivedAt, statusAge, err
			}
			return devStatus, statusReceivedAt, statusAge, errStepHandled
		}
		if c.m != nil {
			c.m.CommandSuppressedTotal.WithLabelValues("status_stale").Inc()
			c.m.SetState(metrics.StateHolding)
		}
		slog.Warn("freezing control until device status returns",
			"label", label, "last_command_watts", c.lastCommandWatts)
		return devStatus, statusReceivedAt, statusAge, errStepHandled
	}
	return polled, now, 0, nil
}

func (c *Controller) recordDeviceStatus(devStatus marstek.Status, statusAge time.Duration) {
	passThroughVal := 0.0
	if devStatus.PassThroughActive() {
		passThroughVal = 1.0
	}
	surplusVal := 0.0
	if devStatus.SurplusFeedIn {
		surplusVal = 1.0
	}
	if c.m == nil {
		return
	}
	c.m.DeviceLastStatusSecs.Set(statusAge.Seconds())
	c.m.LastStatusAgeSecs.Set(statusAge.Seconds())
	c.m.PassthroughActive.Set(passThroughVal)
	c.m.SurplusFeedInEnabled.Set(surplusVal)
	c.m.BatterySoCPercent.Set(float64(devStatus.SOCPercent))
	c.m.BatteryTempMinCelsius.Set(float64(devStatus.TempMinC))
	c.m.BatteryTempMaxCelsius.Set(float64(devStatus.TempMaxC))
}

func (c *Controller) updateSoCFloor(devStatus marstek.Status) (int, int) {
	softFloor := c.cfg.BatterySoCFloorFallbackPercent
	if devStatus.DoDPercent > 0 {
		softFloor = (100 - devStatus.DoDPercent) + c.cfg.BatterySoCFloorMarginPercent
	}
	resumeAt := softFloor + c.cfg.BatterySoCHysteresisPercent
	if c.m != nil {
		c.m.BatterySoCSoftFloorPercent.Set(float64(softFloor))
	}
	if devStatus.SOCPercent <= softFloor {
		c.socFloorActive = true
	} else if devStatus.SOCPercent >= resumeAt {
		c.socFloorActive = false
	}
	return softFloor, resumeAt
}

func (c *Controller) logFirstStatus(devStatus marstek.Status) {
	if c.loggedFirmware {
		return
	}
	c.loggedFirmware = true
	slog.Info("device status received",
		"firmware", fmt.Sprintf("%d.%d", devStatus.FirmwareMajor, devStatus.FirmwareSub),
		"soc_pct", devStatus.SOCPercent,
		"rated_output_w", devStatus.RatedOutputWatts,
		"charging_mode", devStatus.ChargingMode,
		"o1", devStatus.Output1Enabled, "o2", devStatus.Output2Enabled,
		"surplus_feed_in", devStatus.SurplusFeedIn)
	if c.cfg.NearFullIdleEnabled && !devStatus.SurplusFeedIn {
		slog.Warn("surplus feed-in is disabled; full-charge pass-through idle will not engage until it is enabled")
	}
}

func (c *Controller) computeRawTarget(devStatus marstek.Status, smoothed float64) int {
	currentOutput := devStatus.Output1Watts + devStatus.Output2Watts
	rawTarget := currentOutput + int(math.Round(smoothed)) - c.cfg.ImportBiasWatts
	if rawTarget < 0 {
		rawTarget = 0
	}
	if rawTarget > c.cfg.MaxOutputWatts {
		rawTarget = c.cfg.MaxOutputWatts
	}
	if rawTarget > 0 && rawTarget < c.cfg.MinOutputWatts {
		rawTarget = c.cfg.MinOutputWatts
	}
	return rawTarget
}

func (c *Controller) publishNormalControl(ctx context.Context, now, statusReceivedAt time.Time, devStatus marstek.Status, gridWatts, smoothed float64, rawTarget int) error {
	_ = ctx
	ramped := c.applyRamp(c.lastCommandWatts, rawTarget)
	exporting := smoothed < 0
	if exporting && ramped > rawTarget {
		ramped = rawTarget
	}
	if ramped > 0 && ramped < c.cfg.MinOutputWatts {
		if rawTarget == 0 {
			ramped = 0
		} else {
			ramped = c.cfg.MinOutputWatts
		}
	}

	deltaThreshold := c.cfg.MinCommandDeltaWatts
	if exporting {
		deltaThreshold = c.cfg.MinCommandDeltaWattsExporting
	}
	delta := abs(ramped - c.lastCommandWatts)
	if delta == 0 || delta < deltaThreshold {
		return c.suppressOrReassert(now, statusReceivedAt, devStatus, c.lastCommandWatts, "delta", "delta_suppressed", smoothed)
	}

	holdTimeActive := !c.lastCommandTime.IsZero() && now.Sub(c.lastCommandTime) < c.cfg.MinHoldTime
	fastPathBypass := exporting && ramped < c.lastCommandWatts
	if holdTimeActive && !fastPathBypass {
		return c.suppressOrReassert(now, statusReceivedAt, devStatus, c.lastCommandWatts, "hold_time", "hold_time", smoothed)
	}

	slots := marstek.SlotsAsWriteSlots(devStatus)
	idx := c.cfg.ScheduleSlot - 1
	if idx < 0 || idx > 4 {
		idx = 0
	}
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
	c.authorityPendingPayload = payload
	c.authorityPendingSince = now
	c.authorityPendingSeenAt = statusReceivedAt
	if c.m != nil {
		c.m.CommandedSlotPowerWatts.Set(float64(ramped))
		c.m.MQTTPublishesTotal.WithLabelValues("write").Inc()
		c.m.RecordLastMQTTPublish(now)
		c.updateState(smoothed)
	}
	c.ready = true

	slog.Info("schedule updated",
		"slot", c.cfg.ScheduleSlot, "watts", ramped,
		"grid_w", math.Round(gridWatts),
		"smoothed_w", math.Round(smoothed),
		"soc_pct", devStatus.SOCPercent)
	return nil
}

func (c *Controller) suppressOrReassert(now, statusReceivedAt time.Time, devStatus marstek.Status, watts int, metricReason, authorityReason string, smoothed float64) error {
	handled, published, err := c.ensureControlledSlot(now, statusReceivedAt, devStatus, watts, authorityReason)
	if err != nil {
		return err
	}
	if published {
		c.lastCommandTime = now
		if c.m != nil {
			c.m.CommandedSlotPowerWatts.Set(float64(watts))
		}
	}
	if handled {
		c.ready = true
		return nil
	}
	if c.m != nil {
		c.m.CommandSuppressedTotal.WithLabelValues(metricReason).Inc()
		c.updateState(smoothed)
	}
	c.ready = true
	return nil
}
