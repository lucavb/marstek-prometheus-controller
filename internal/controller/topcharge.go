package controller

import (
	"log/slog"
	"math"
	"time"

	"github.com/lucavb/marstek-prometheus-controller/internal/marstek"
)

func (c *Controller) updateTopChargeIdle(now time.Time, devStatus marstek.Status, rawGridWatts, smoothed float64) bool {
	requiredSamples := c.cfg.NearFullIdleConsecutiveSamples
	if requiredSamples < 1 {
		requiredSamples = 1
	}

	if !c.cfg.NearFullIdleEnabled {
		if c.resetTopChargeIdleState("disabled") {
			slog.Info("top-charge idle deactivated", "reason", "disabled")
		}
		return false
	}
	if !c.cfg.EnforceSurplusFeedIn {
		if c.resetTopChargeIdleState("surplus_feed_in_policy_disabled") {
			slog.Info("top-charge idle deactivated", "reason", "surplus_feed_in_policy_disabled")
		}
		return false
	}
	if !devStatus.SurplusFeedIn {
		if c.resetTopChargeIdleState("surplus_feed_in_disabled") {
			slog.Info("top-charge idle deactivated", "reason", "surplus_feed_in_disabled")
		}
		return false
	}

	if c.topChargeIdleActive {
		if devStatus.SOCPercent < c.cfg.NearFullIdleEnterPercent {
			c.topChargeIdleSoCExitSamples++
		} else {
			c.topChargeIdleSoCExitSamples = 0
		}

		importExitEnabled := c.cfg.NearFullIdleGridImportExitSamples > 0
		if importExitEnabled && smoothed > float64(c.cfg.NearFullIdleGridImportExitWatts) {
			c.topChargeIdleImportSamples++
		} else {
			c.topChargeIdleImportSamples = 0
		}
		if importExitEnabled && rawGridWatts > float64(c.cfg.NearFullIdleGridImportExitWatts) {
			c.topChargeIdleRawImportSamples++
		} else {
			c.topChargeIdleRawImportSamples = 0
		}
		if c.topChargeIdleHealthy(devStatus) {
			c.topChargeIdlePassThroughLostSamples = 0
		} else {
			c.topChargeIdlePassThroughLostSamples++
		}

		exitReason := ""
		switch {
		case c.topChargeIdleSoCExitSamples >= requiredSamples:
			exitReason = "soc_exit"
		case c.topChargeIdlePassThroughLostSamples >= requiredSamples:
			exitReason = "passthrough_lost"
		case importExitEnabled && c.topChargeIdleRawImportSamples >= c.cfg.NearFullIdleGridImportExitSamples:
			exitReason = "raw_grid_import"
		case importExitEnabled && c.topChargeIdleImportSamples >= c.cfg.NearFullIdleGridImportExitSamples:
			exitReason = "grid_import"
		}
		if exitReason != "" {
			c.resetTopChargeIdleState(exitReason)
			blockedUntil := time.Time{}
			switch exitReason {
			case "passthrough_lost", "raw_grid_import", "grid_import":
				blockedUntil = c.blockTopChargeIdleUntil(now)
			}
			slog.Info("top-charge idle deactivated",
				"soc_pct", devStatus.SOCPercent,
				"raw_grid_watts", math.Round(rawGridWatts),
				"smoothed_grid_watts", math.Round(smoothed),
				"reason", exitReason,
				"blocked_until", blockedUntil)
			return false
		}
		return true
	}

	if !c.topChargeIdleBlockedUntil.IsZero() && now.Before(c.topChargeIdleBlockedUntil) {
		return false
	}

	if c.topChargeEntryEligible(devStatus, rawGridWatts, smoothed) {
		c.topChargeIdleEnterSamples++
	} else {
		c.topChargeIdleEnterSamples = 0
	}
	if c.topChargeIdleEnterSamples < requiredSamples {
		return false
	}

	c.topChargeIdleActive = true
	c.topChargeIdleEnterSamples = 0
	c.topChargeIdleSoCExitSamples = 0
	c.topChargeIdleImportSamples = 0
	if c.m != nil {
		c.m.TopChargeIdleActive.Set(1)
		c.m.TopChargeIdleEntered.Inc()
	}
	slog.Info("top-charge idle activated",
		"soc_pct", devStatus.SOCPercent,
		"enter_pct", c.cfg.NearFullIdleEnterPercent,
		"raw_grid_watts", math.Round(rawGridWatts),
		"smoothed_grid_watts", math.Round(smoothed),
		"solar_input_watts", devStatus.Solar1Watts+devStatus.Solar2Watts,
		"output_watts", devStatus.Output1Watts+devStatus.Output2Watts,
		"passthrough_active", devStatus.PassThroughActive())
	return true
}

func (c *Controller) topChargeEntryEligible(devStatus marstek.Status, rawGridWatts, smoothed float64) bool {
	if devStatus.SOCPercent < c.cfg.NearFullIdleEnterPercent {
		return false
	}
	// Meaningful import means solar no longer covers load, so do not enter a
	// pass-through idle that would strand the house on grid power.
	if smoothed > float64(c.cfg.ImportBiasWatts) || rawGridWatts > float64(c.cfg.ImportBiasWatts) {
		return false
	}
	solarInputWatts := devStatus.Solar1Watts + devStatus.Solar2Watts
	outputWatts := devStatus.Output1Watts + devStatus.Output2Watts
	return solarInputWatts > 0 || outputWatts > c.topChargeIdleNearZeroLimit() || devStatus.PassThroughActive()
}

func (c *Controller) topChargeIdleHealthy(devStatus marstek.Status) bool {
	outputWatts := devStatus.Output1Watts + devStatus.Output2Watts
	return devStatus.PassThroughActive() || outputWatts > c.topChargeIdleNearZeroLimit()
}

func (c *Controller) topChargeIdleNearZeroLimit() int {
	nearZeroLimit := c.cfg.MinOutputWatts / 2
	if nearZeroLimit < 20 {
		nearZeroLimit = 20
	}
	return nearZeroLimit
}
