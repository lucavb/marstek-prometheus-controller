package controller

import (
	"log/slog"
	"math"

	"github.com/lucavb/marstek-prometheus-controller/internal/marstek"
)

func (c *Controller) updateTopChargeIdle(devStatus marstek.Status, smoothed float64) bool {
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

		exitReason := ""
		switch {
		case c.topChargeIdleSoCExitSamples >= requiredSamples:
			exitReason = "soc_exit"
		case importExitEnabled && c.topChargeIdleImportSamples >= c.cfg.NearFullIdleGridImportExitSamples:
			exitReason = "grid_import"
		}
		if exitReason != "" {
			c.resetTopChargeIdleState(exitReason)
			slog.Info("top-charge idle deactivated",
				"soc_pct", devStatus.SOCPercent,
				"smoothed_grid_watts", math.Round(smoothed),
				"reason", exitReason)
			return false
		}
		return true
	}

	if c.topChargeEntryEligible(devStatus, smoothed) {
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
		"smoothed_grid_watts", math.Round(smoothed),
		"solar_input_watts", devStatus.Solar1Watts+devStatus.Solar2Watts,
		"output_watts", devStatus.Output1Watts+devStatus.Output2Watts,
		"passthrough_active", devStatus.PassThroughActive())
	return true
}

func (c *Controller) topChargeEntryEligible(devStatus marstek.Status, smoothed float64) bool {
	if devStatus.SOCPercent < c.cfg.NearFullIdleEnterPercent {
		return false
	}
	// Meaningful import means solar no longer covers load, so do not enter a
	// pass-through idle that would strand the house on grid power.
	if smoothed > float64(c.cfg.ImportBiasWatts) {
		return false
	}
	solarInputWatts := devStatus.Solar1Watts + devStatus.Solar2Watts
	outputWatts := devStatus.Output1Watts + devStatus.Output2Watts
	return solarInputWatts > 0 || outputWatts > 0 || devStatus.PassThroughActive() || smoothed <= float64(c.cfg.ImportBiasWatts)
}
