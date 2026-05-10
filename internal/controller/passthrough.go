package controller

import (
	"context"
	"log/slog"
	"math"
	"time"

	"github.com/lucavb/marstek-prometheus-controller/internal/marstek"
)

func (c *Controller) passthroughImportThresholdWatts() float64 {
	threshold := c.cfg.ImportBiasWatts
	if c.cfg.NearFullIdleGridImportExitWatts > threshold {
		threshold = c.cfg.NearFullIdleGridImportExitWatts
	}
	return float64(threshold)
}

func (c *Controller) resetPassthroughStall() {
	c.passthroughStallCycles = 0
	c.passthroughStallActive = false
	c.passthroughNudgePending = false
	c.passthroughNudgeAt = time.Time{}
	c.loggedPassthroughUnresolved = false
}

func (c *Controller) maybeHandlePassthroughStall(ctx context.Context, now time.Time, devStatus marstek.Status, smoothed float64, targetWatts int) (bool, error) {
	_ = ctx
	if c.cfg.PassthroughStallDetectCycles == 0 {
		c.resetPassthroughStall()
		return false, nil
	}

	currentOutput := devStatus.Output1Watts + devStatus.Output2Watts
	passThroughActive := devStatus.PassThroughActive()
	importingEnough := smoothed > c.passthroughImportThresholdWatts()
	commandedEnough := c.lastCommandWatts >= c.cfg.PassthroughStallMinCommandWatts ||
		targetWatts >= c.cfg.PassthroughStallMinCommandWatts

	// Firmware pass-through can stall timed discharge outside the near-full band
	// as well, so detection is based on observed behavior (import + commanded
	// discharge + zero output + pass-through active), not SoC thresholds.
	if !passThroughActive || !importingEnough || !commandedEnough || currentOutput > 0 {
		c.resetPassthroughStall()
		return false, nil
	}

	c.passthroughStallCycles++
	if c.passthroughStallCycles < c.cfg.PassthroughStallDetectCycles {
		return false, nil
	}

	if !c.passthroughStallActive {
		c.passthroughStallActive = true
		if c.m != nil {
			c.m.PassthroughStallDetected.Inc()
		}
		slog.Warn("pass-through stall detected: commanded discharge but device output remains zero",
			"stall_cycles", c.passthroughStallCycles,
			"target_watts", targetWatts,
			"last_command_watts", c.lastCommandWatts,
			"smoothed_grid_watts", math.Round(smoothed),
			"soc_pct", devStatus.SOCPercent,
			"surplus_feed_in", devStatus.SurplusFeedIn,
			"p1", devStatus.Solar1Mode,
			"p2", devStatus.Solar2Mode)
	}

	return c.maybeStartPassthroughRecovery(now, devStatus, targetWatts)
}

func (c *Controller) maybeStartPassthroughRecovery(now time.Time, devStatus marstek.Status, targetWatts int) (bool, error) {
	if !c.cfg.PassthroughAutoRecovery {
		return false, nil
	}
	if c.passthroughRecoveryActive {
		return false, nil
	}

	if !c.passthroughNudgePending {
		if !c.lastPassthroughRecoveryAt.IsZero() &&
			now.Sub(c.lastPassthroughRecoveryAt) < c.cfg.PassthroughAutoRecoveryMinInterval {
			if c.m != nil {
				c.m.PassthroughRecoveryTotal.WithLabelValues("rate_limited").Inc()
			}
			slog.Warn("pass-through auto-recovery rate limited",
				"last_recovery_at", c.lastPassthroughRecoveryAt,
				"min_interval", c.cfg.PassthroughAutoRecoveryMinInterval)
			return false, nil
		}
		if err := c.publishPassthroughNudge(marstek.BuildChargingModePayload(0), "nudge_charging_mode"); err != nil {
			return false, err
		}
		if err := c.publishPassthroughNudge(c.runtimeScheduleNudgePayload(devStatus, targetWatts), "nudge_schedule"); err != nil {
			return false, err
		}
		c.passthroughNudgePending = true
		c.passthroughNudgeAt = now
		c.lastPassthroughRecoveryAt = now
		if c.m != nil {
			c.m.PassthroughRecoveryTotal.WithLabelValues("nudge_started").Inc()
		}
		slog.Warn("pass-through auto-recovery nudges published while preserving surplus feed-in",
			"soc_pct", devStatus.SOCPercent,
			"target_watts", targetWatts,
			"p1", devStatus.Solar1Mode,
			"p2", devStatus.Solar2Mode)
		return true, nil
	}

	requiredCycles := c.cfg.PassthroughStallDetectCycles * 2
	if requiredCycles < c.cfg.PassthroughStallDetectCycles+1 {
		requiredCycles = c.cfg.PassthroughStallDetectCycles + 1
	}
	if c.passthroughStallCycles < requiredCycles {
		return false, nil
	}

	if !c.cfg.PassthroughAutoRecoveryFlashFallback {
		if c.m != nil {
			c.m.PassthroughRecoveryTotal.WithLabelValues("unresolved_after_nudge").Inc()
		}
		if !c.loggedPassthroughUnresolved {
			c.loggedPassthroughUnresolved = true
			slog.Warn("pass-through stall persists after non-flash nudges; preserving surplus feed-in (flash fallback disabled)",
				"soc_pct", devStatus.SOCPercent,
				"target_watts", targetWatts,
				"p1", devStatus.Solar1Mode,
				"p2", devStatus.Solar2Mode)
		}
		return false, nil
	}

	if !c.cfg.AllowFlashWrites {
		if c.m != nil {
			c.m.PassthroughRecoveryTotal.WithLabelValues("blocked_flash_guard").Inc()
		}
		if !c.loggedPassthroughRecoveryBlocked {
			c.loggedPassthroughRecoveryBlocked = true
			slog.Error("pass-through flash fallback blocked because ALLOW_FLASH_WRITES is false",
				"soc_pct", devStatus.SOCPercent,
				"surplus_feed_in", devStatus.SurplusFeedIn,
				"p1", devStatus.Solar1Mode,
				"p2", devStatus.Solar2Mode)
		}
		return false, nil
	}

	if err := c.publishSurplusFeedIn(false, "disable"); err != nil {
		if c.m != nil {
			c.m.PassthroughRecoveryTotal.WithLabelValues("publish_error").Inc()
		}
		return false, err
	}

	c.passthroughRecoveryActive = true
	c.passthroughRecoveryStartedAt = now
	c.surplusFeedInDisabledByController = true
	c.passthroughNudgePending = false
	c.passthroughNudgeAt = time.Time{}
	if c.m != nil {
		c.m.PassthroughRecoveryTotal.WithLabelValues("started").Inc()
	}
	slog.Warn("pass-through auto-recovery started: surplus feed-in disabled temporarily",
		"soc_pct", devStatus.SOCPercent,
		"p1", devStatus.Solar1Mode,
		"p2", devStatus.Solar2Mode)
	return true, nil
}

func (c *Controller) runtimeScheduleNudgePayload(devStatus marstek.Status, targetWatts int) string {
	desired, slots := c.desiredControlledSlot(devStatus, targetWatts)
	idx := c.cfg.ScheduleSlot - 1
	if idx < 0 || idx > 4 {
		idx = 0
	}
	slots[idx] = desired
	return marstek.BuildTimedDischargePayload(slots, false)
}

func (c *Controller) publishPassthroughNudge(payload, outcome string) error {
	if err := c.pub.Publish(c.cfg.ControlTopic, payload); err != nil {
		slog.Warn("pass-through recovery nudge publish failed", "err", err, "payload", payload, "outcome", outcome)
		if c.m != nil {
			c.m.MQTTPublishErrorsTotal.WithLabelValues(classifyMQTTError(err)).Inc()
			c.m.PassthroughRecoveryTotal.WithLabelValues("publish_error").Inc()
		}
		return err
	}
	if c.m != nil {
		c.m.MQTTPublishesTotal.WithLabelValues("write").Inc()
		c.m.PassthroughRecoveryTotal.WithLabelValues(outcome).Inc()
		c.m.RecordLastMQTTPublish(c.clock.Now())
	}
	return nil
}

func (c *Controller) maybeRestoreSurplusFeedIn(now time.Time, devStatus marstek.Status, smoothed float64) error {
	if !c.passthroughRecoveryActive || !c.surplusFeedInDisabledByController {
		return nil
	}

	elapsed := now.Sub(c.passthroughRecoveryStartedAt)
	restoreDue := devStatus.SOCPercent < c.cfg.NearFullIdleExitPercent
	if c.cfg.PassthroughAutoRecoveryRestoreDelay == 0 ||
		elapsed >= c.cfg.PassthroughAutoRecoveryRestoreDelay {
		restoreDue = restoreDue || smoothed <= c.passthroughImportThresholdWatts()
	}
	if !restoreDue {
		return nil
	}

	if err := c.publishSurplusFeedIn(true, "restore"); err != nil {
		if c.m != nil {
			c.m.PassthroughRecoveryTotal.WithLabelValues("publish_error").Inc()
		}
		return err
	}

	c.passthroughRecoveryActive = false
	c.surplusFeedInDisabledByController = false
	c.resetPassthroughStall()
	if c.m != nil {
		c.m.PassthroughRecoveryTotal.WithLabelValues("restored").Inc()
	}
	slog.Warn("pass-through auto-recovery restored surplus feed-in",
		"soc_pct", devStatus.SOCPercent,
		"elapsed", elapsed.Round(time.Second))
	return nil
}

func (c *Controller) publishSurplusFeedIn(enable bool, direction string) error {
	payload := marstek.BuildSurplusFeedInPayload(enable)
	if err := c.pub.Publish(c.cfg.ControlTopic, payload); err != nil {
		slog.Warn("surplus feed-in publish failed", "err", err, "direction", direction)
		if c.m != nil {
			c.m.MQTTPublishErrorsTotal.WithLabelValues(classifyMQTTError(err)).Inc()
		}
		return err
	}
	if c.m != nil {
		c.m.MQTTPublishesTotal.WithLabelValues("write").Inc()
		c.m.SurplusFeedInToggledTotal.WithLabelValues(direction).Inc()
		c.m.RecordLastMQTTPublish(c.clock.Now())
	}
	slog.Warn("surplus feed-in toggled by pass-through recovery",
		"direction", direction,
		"enabled", enable,
		"payload", payload)
	return nil
}
