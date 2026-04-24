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
	nearFull := devStatus.SOCPercent >= c.cfg.NearFullIdleEnterPercent

	if !passThroughActive || !importingEnough || !commandedEnough || currentOutput > 0 || !nearFull {
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

	return c.maybeStartPassthroughRecovery(now, devStatus)
}

func (c *Controller) maybeStartPassthroughRecovery(now time.Time, devStatus marstek.Status) (bool, error) {
	if !c.cfg.PassthroughAutoRecovery {
		return false, nil
	}
	if !c.cfg.AllowFlashWrites {
		if c.m != nil {
			c.m.PassthroughRecoveryTotal.WithLabelValues("blocked_flash_guard").Inc()
		}
		if !c.loggedPassthroughRecoveryBlocked {
			c.loggedPassthroughRecoveryBlocked = true
			slog.Error("pass-through auto-recovery blocked because ALLOW_FLASH_WRITES is false",
				"soc_pct", devStatus.SOCPercent,
				"surplus_feed_in", devStatus.SurplusFeedIn,
				"p1", devStatus.Solar1Mode,
				"p2", devStatus.Solar2Mode)
		}
		return false, nil
	}
	if c.passthroughRecoveryActive {
		return false, nil
	}
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

	if err := c.publishSurplusFeedIn(false, "disable"); err != nil {
		if c.m != nil {
			c.m.PassthroughRecoveryTotal.WithLabelValues("publish_error").Inc()
		}
		return false, err
	}

	c.passthroughRecoveryActive = true
	c.passthroughRecoveryStartedAt = now
	c.lastPassthroughRecoveryAt = now
	c.surplusFeedInDisabledByController = true
	if c.m != nil {
		c.m.PassthroughRecoveryTotal.WithLabelValues("started").Inc()
	}
	slog.Warn("pass-through auto-recovery started: surplus feed-in disabled temporarily",
		"soc_pct", devStatus.SOCPercent,
		"p1", devStatus.Solar1Mode,
		"p2", devStatus.Solar2Mode)
	return true, nil
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
