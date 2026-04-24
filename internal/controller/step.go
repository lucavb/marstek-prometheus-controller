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
			if !statusReceivedAt.IsZero() && statusAge > c.cfg.StatusHardFailAfter {
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
		c.m.DeviceLastStatusSecs.Set(statusAge.Seconds())
		c.m.LastStatusAgeSecs.Set(statusAge.Seconds())
	}
	passThroughActive := devStatus.PassThroughActive()
	if c.m != nil {
		passThroughVal := 0.0
		if passThroughActive {
			passThroughVal = 1.0
		}
		c.m.PassthroughActive.Set(passThroughVal)
	}

	// Capture the previous status before overwriting — the transient-zero guard
	// below needs to compare the current output against what was reported last
	// cycle, not the current cycle's (already-updated) lastStatus.
	prevStatus := c.lastStatus
	hasPrevStatus := c.hasLastStatus
	c.lastStatus = devStatus
	c.hasLastStatus = true

	// ── 2a. SoC soft floor — don't fight the BMS ─────────────────────────────
	// Derive the soft floor from the device's own DoD setting so a change in
	// the Marstek app flows through automatically without a redeploy.
	softFloor := c.cfg.BatterySoCFloorFallbackPercent
	if devStatus.DoDPercent > 0 {
		softFloor = (100 - devStatus.DoDPercent) + c.cfg.BatterySoCFloorMarginPercent
	}
	resumeAt := softFloor + c.cfg.BatterySoCHysteresisPercent

	if c.m != nil {
		c.m.BatterySoCPercent.Set(float64(devStatus.SOCPercent))
		c.m.BatterySoCSoftFloorPercent.Set(float64(softFloor))
		c.m.BatteryTempMinCelsius.Set(float64(devStatus.TempMinC))
		c.m.BatteryTempMaxCelsius.Set(float64(devStatus.TempMaxC))
	}

	if devStatus.SOCPercent <= softFloor {
		c.socFloorActive = true
	} else if devStatus.SOCPercent >= resumeAt {
		c.socFloorActive = false
	}

	// One-time startup warnings — logged as soon as we have valid status,
	// before the SoC floor check so they appear even when discharge is suppressed.
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
		if c.cfg.NearFullIdleEnabled && !devStatus.SurplusFeedIn {
			slog.Warn("near-full idle is enabled but surplus feed-in is disabled in the app (tc_dis=1); idle will not engage until surplus feed-in is re-enabled, otherwise PV would be stranded at full SoC")
		}
		if c.cfg.PassthroughAutoRecovery && !devStatus.SurplusFeedIn && !c.surplusFeedInDisabledByController {
			slog.Warn("pass-through auto-recovery is enabled but surplus feed-in is already disabled; controller will not restore a setting it did not change")
		}
	}

	if c.m != nil {
		surplusVal := 0.0
		if devStatus.SurplusFeedIn {
			surplusVal = 1.0
		}
		c.m.SurplusFeedInEnabled.Set(surplusVal)
	}
	if c.socFloorActive {
		slog.Debug("soc below soft floor, suppressing discharge",
			"soc_pct", devStatus.SOCPercent,
			"soft_floor_pct", softFloor,
			"resume_at_pct", resumeAt,
			"dod_pct", devStatus.DoDPercent)
		// Both Prometheus and device status are healthy; we're just choosing not
		// to discharge. Mark ready so the readiness probe reflects that the
		// controller is operating normally (same logic as deadband suppression).
		c.ready = true
		c.transientZeroFiredLastCycle = false
		c.resetPassthroughStall()
		return c.commandIdle(ctx, now, devStatus, "soc_floor")
	}

	// ── 2b. Smooth the grid power signal ─────────────────────────────────────
	// Smoothing is updated every cycle (including while idle is active) so the
	// near-full idle grid-import exit below sees the same smoothed value the
	// rest of the control loop consumes, and so the smoothed gauge never
	// freezes during idle.
	smoothed := c.smooth(sample.Watts)
	if c.m != nil {
		c.m.SmoothedGridPowerWatts.Set(smoothed)
	}
	if err := c.maybeRestoreSurplusFeedIn(now, devStatus, smoothed); err != nil {
		return err
	}

	// ── 2c. Near-full idle ───────────────────────────────────────────────────
	// Near the top of charge we deliberately disable the controlled slot rather
	// than command any discharge. Excess PV is routed to the grid by the
	// device's firmware surplus-feed-in path (Marstek app setting tc_dis=0),
	// which the device echoes back as devStatus.SurplusFeedIn. The regime is
	// hard-gated on that flag: if surplus feed-in is off, idling would strand
	// PV (firmware MPPT-curtails at full SoC when there is no export path), so
	// normal control runs instead. Entry and exit are each debounced by
	// NearFullIdleConsecutiveSamples samples to ride through LFP top-end SoC
	// flicker without flapping.
	//
	// A secondary "grid_import" exit breaks the SoC-deadlock: with the slot
	// disabled, SoC cannot fall (no discharge), so the SoC exit path never
	// fires when solar drops below house load. When smoothed grid import
	// exceeds NearFullIdleGridImportExitWatts for
	// NearFullIdleGridImportExitSamples consecutive cycles, idle exits and
	// normal control resumes — the battery immediately begins covering load.
	//
	// Entry is gated on smoothed <= 0 in addition to SoC, so after a
	// grid_import exit the enter counter cannot re-arm while the grid is
	// still importing on the LFP plateau (the SoC-only gate flapped idle
	// back on within two cycles; see the 2026-04 incident).
	gatingOK := c.cfg.NearFullIdleEnabled && devStatus.SurplusFeedIn
	if !gatingOK {
		if c.nearFullIdleActive {
			reason := "disabled"
			if !devStatus.SurplusFeedIn {
				reason = "surplus_feed_in_disabled"
			}
			c.resetNearFullIdleState(reason)
			slog.Info("near-full idle deactivated",
				"soc_pct", devStatus.SOCPercent,
				"surplus_feed_in", devStatus.SurplusFeedIn,
				"reason", reason)
		} else {
			c.nearFullIdleEnterSamples = 0
			c.nearFullIdleExitSamples = 0
			c.nearFullIdleGridImportSamples = 0
		}
	} else {
		requiredSamples := c.cfg.NearFullIdleConsecutiveSamples
		if requiredSamples < 1 {
			requiredSamples = 1
		}
		if !c.nearFullIdleActive {
			// Idle entry requires both high SoC AND a non-importing grid. The
			// grid gate breaks the flap the SoC-only check allowed: after a
			// grid_import exit, SoC is still pinned at the enter threshold on
			// the LFP plateau, so a SoC-only counter re-fires in two cycles
			// and idle flaps right back on while the grid is still importing.
			// Requiring smoothed <= 0 means "we genuinely have surplus to
			// feed back"; if the grid is importing at all, idling would just
			// re-cause that import.
			if devStatus.SOCPercent >= c.cfg.NearFullIdleEnterPercent && smoothed <= 0 {
				c.nearFullIdleEnterSamples++
			} else {
				c.nearFullIdleEnterSamples = 0
			}
			if c.nearFullIdleEnterSamples >= requiredSamples {
				c.nearFullIdleActive = true
				c.nearFullIdleEnterSamples = 0
				c.nearFullIdleExitSamples = 0
				c.nearFullIdleGridImportSamples = 0
				if c.m != nil {
					c.m.NearFullIdleActive.Set(1)
					c.m.NearFullIdleEntered.Inc()
				}
				slog.Info("near-full idle activated",
					"soc_pct", devStatus.SOCPercent,
					"enter_pct", c.cfg.NearFullIdleEnterPercent,
					"exit_pct", c.cfg.NearFullIdleExitPercent,
					"smoothed_grid_watts", math.Round(smoothed),
					"surplus_feed_in", devStatus.SurplusFeedIn)
			}
		} else {
			if devStatus.SOCPercent < c.cfg.NearFullIdleExitPercent {
				c.nearFullIdleExitSamples++
			} else {
				c.nearFullIdleExitSamples = 0
			}
			gridExitEnabled := c.cfg.NearFullIdleGridImportExitSamples > 0 && !passThroughActive
			if gridExitEnabled && smoothed > float64(c.cfg.NearFullIdleGridImportExitWatts) {
				c.nearFullIdleGridImportSamples++
			} else {
				c.nearFullIdleGridImportSamples = 0
			}

			exitReason := ""
			switch {
			case c.nearFullIdleExitSamples >= requiredSamples:
				exitReason = "soc_exit"
			case gridExitEnabled &&
				c.nearFullIdleGridImportSamples >= c.cfg.NearFullIdleGridImportExitSamples:
				exitReason = "grid_import"
			}
			if exitReason != "" {
				c.resetNearFullIdleState(exitReason)
				slog.Info("near-full idle deactivated",
					"soc_pct", devStatus.SOCPercent,
					"exit_pct", c.cfg.NearFullIdleExitPercent,
					"smoothed_grid_watts", math.Round(smoothed),
					"reason", exitReason,
					"samples", requiredSamples)
			}
		}
	}

	if c.nearFullIdleActive {
		currentOutput := devStatus.Output1Watts + devStatus.Output2Watts
		idleTarget := currentOutput + int(math.Round(smoothed)) - c.cfg.ImportBiasWatts
		if idleTarget < 0 {
			idleTarget = 0
		}
		if idleTarget > c.cfg.MaxOutputWatts {
			idleTarget = c.cfg.MaxOutputWatts
		}
		if idleTarget > 0 && idleTarget < c.cfg.MinOutputWatts {
			idleTarget = c.cfg.MinOutputWatts
		}
		recoveryStarted, err := c.maybeHandlePassthroughStall(ctx, now, devStatus, smoothed, idleTarget)
		if err != nil {
			return err
		}
		if recoveryStarted {
			c.resetNearFullIdleState("passthrough_recovery")
			c.ready = true
			return nil
		}
		c.ready = true
		c.transientZeroFiredLastCycle = false
		return c.commandIdle(ctx, now, devStatus, "near_full_idle")
	}

	// ── 2d. Transient-zero-output guard ──────────────────────────────────────
	// A single-cycle g1=g2=0 report while we were actively commanding output is
	// a device reporting blip. Recomputing rawTarget from zero output causes a
	// snap to MinOutputWatts=80. Hold the previous command for exactly one
	// cycle; if the next cycle is still zero we proceed normally so we can
	// never get stuck.
	currentZeroOutput := devStatus.Output1Watts == 0 && devStatus.Output2Watts == 0
	prevHadOutput := hasPrevStatus && (prevStatus.Output1Watts+prevStatus.Output2Watts) > 0
	if currentZeroOutput && prevHadOutput && c.lastCommandWatts > c.cfg.MinOutputWatts &&
		!c.transientZeroFiredLastCycle {
		c.transientZeroFiredLastCycle = true
		if c.m != nil {
			c.m.CommandSuppressedTotal.WithLabelValues("transient_zero_output").Inc()
		}
		slog.Debug("transient zero output suppressed",
			"last_command_watts", c.lastCommandWatts,
			"soc_pct", devStatus.SOCPercent)
		c.ready = true
		return nil
	}
	c.transientZeroFiredLastCycle = false

	// ── 3. Compute raw target ─────────────────────────────────────────────────
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
	recoveryStarted, err := c.maybeHandlePassthroughStall(ctx, now, devStatus, smoothed, rawTarget)
	if err != nil {
		return err
	}
	if recoveryStarted {
		c.ready = true
		return nil
	}

	// ── 4. Apply ramp and hold-time suppression ───────────────────────────────
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

	// Delta gate is asymmetric: during active export a reduction should be
	// published even if small, because every watt still discharging is lost
	// energy. The exporting threshold defaults to 5 W so -4..+4 W meter noise
	// around zero doesn't republish the same schedule, while anything larger
	// passes through. A zero delta is always suppressed so we never republish
	// the same schedule regardless of threshold — this also protects against
	// MQTT spam when MIN_COMMAND_DELTA_WATTS_EXPORTING is set to 0.
	deltaThreshold := c.cfg.MinCommandDeltaWatts
	if exporting {
		deltaThreshold = c.cfg.MinCommandDeltaWattsExporting
	}
	delta := abs(ramped - c.lastCommandWatts)
	if delta == 0 || delta < deltaThreshold {
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

	// ── 5. Publish the new schedule power ────────────────────────────────────
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
