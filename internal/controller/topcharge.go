package controller

import (
	"log/slog"
	"math"
	"time"

	"github.com/lucavb/marstek-prometheus-controller/internal/marstek"
	"github.com/lucavb/marstek-prometheus-controller/internal/metrics"
)

// topChargeIdleMachine owns the deliberate full-charge pass-through idle
// regime: the controller leaves the controlled slot disabled so firmware can
// pass full-charge PV through instead of forcing a pointless discharge. The
// machine debounces entry and every exit condition, enforces a cooldown
// after failed exits, and records its own metrics. Its state is mutated
// only through its methods.
type topChargeIdleMachine struct {
	cfg Config
	m   *metrics.Metrics

	active                 bool
	enterSamples           int
	socExitSamples         int
	importSamples          int
	rawImportSamples       int
	passThroughLostSamples int
	blockedUntil           time.Time
}

// update advances the regime by one control cycle and reports whether the
// regime is engaged (Step then idles instead of discharging).
func (t *topChargeIdleMachine) update(now time.Time, devStatus marstek.Status, rawGridWatts, smoothed float64) bool {
	requiredSamples := t.cfg.NearFullIdleConsecutiveSamples
	if requiredSamples < 1 {
		requiredSamples = 1
	}

	if !t.cfg.NearFullIdleEnabled {
		if t.reset("disabled") {
			slog.Info("top-charge idle deactivated", "reason", "disabled")
		}
		return false
	}
	if !t.cfg.EnforceSurplusFeedIn {
		if t.reset("surplus_feed_in_policy_disabled") {
			slog.Info("top-charge idle deactivated", "reason", "surplus_feed_in_policy_disabled")
		}
		return false
	}
	if !devStatus.SurplusFeedIn {
		if t.reset("surplus_feed_in_disabled") {
			slog.Info("top-charge idle deactivated", "reason", "surplus_feed_in_disabled")
		}
		return false
	}

	if t.active {
		if devStatus.SOCPercent < t.cfg.NearFullIdleEnterPercent {
			t.socExitSamples++
		} else {
			t.socExitSamples = 0
		}

		importExitEnabled := t.cfg.NearFullIdleGridImportExitSamples > 0
		if importExitEnabled && smoothed > float64(t.cfg.NearFullIdleGridImportExitWatts) {
			t.importSamples++
		} else {
			t.importSamples = 0
		}
		if importExitEnabled && rawGridWatts > float64(t.cfg.NearFullIdleGridImportExitWatts) {
			t.rawImportSamples++
		} else {
			t.rawImportSamples = 0
		}
		if t.healthy(devStatus) {
			t.passThroughLostSamples = 0
		} else {
			t.passThroughLostSamples++
		}

		exitReason := ""
		switch {
		case t.socExitSamples >= requiredSamples:
			exitReason = "soc_exit"
		case t.passThroughLostSamples >= requiredSamples:
			exitReason = "passthrough_lost"
		case importExitEnabled && t.rawImportSamples >= t.cfg.NearFullIdleGridImportExitSamples:
			exitReason = "raw_grid_import"
		case importExitEnabled && t.importSamples >= t.cfg.NearFullIdleGridImportExitSamples:
			exitReason = "grid_import"
		}
		if exitReason != "" {
			t.reset(exitReason)
			blockedUntil := time.Time{}
			switch exitReason {
			case "passthrough_lost", "raw_grid_import", "grid_import":
				blockedUntil = t.blockUntil(now)
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

	if !t.blockedUntil.IsZero() && now.Before(t.blockedUntil) {
		return false
	}

	if t.entryEligible(devStatus, rawGridWatts, smoothed) {
		t.enterSamples++
	} else {
		t.enterSamples = 0
	}
	if t.enterSamples < requiredSamples {
		return false
	}

	t.active = true
	t.enterSamples = 0
	t.socExitSamples = 0
	t.importSamples = 0
	if t.m != nil {
		t.m.TopChargeIdleActive.Set(1)
		t.m.TopChargeIdleEntered.Inc()
	}
	slog.Info("top-charge idle activated",
		"soc_pct", devStatus.SOCPercent,
		"enter_pct", t.cfg.NearFullIdleEnterPercent,
		"raw_grid_watts", math.Round(rawGridWatts),
		"smoothed_grid_watts", math.Round(smoothed),
		"solar_input_watts", devStatus.Solar1Watts+devStatus.Solar2Watts,
		"output_watts", devStatus.Output1Watts+devStatus.Output2Watts,
		"passthrough_active", devStatus.PassThroughActive())
	return true
}

// reset disengages the regime and clears every debounce counter. When the
// regime was active it records the exit metrics with the given reason and
// returns true so the caller can log the transition.
func (t *topChargeIdleMachine) reset(reason string) bool {
	wasActive := t.active
	t.active = false
	t.enterSamples = 0
	t.socExitSamples = 0
	t.importSamples = 0
	t.rawImportSamples = 0
	t.passThroughLostSamples = 0
	if t.m != nil {
		t.m.TopChargeIdleActive.Set(0)
		if wasActive {
			t.m.TopChargeIdleExited.Inc()
			if reason != "" {
				t.m.TopChargeIdleExitReasonTotal.WithLabelValues(reason).Inc()
			}
		}
	}
	return wasActive
}

// blockUntil starts the cooldown that delays re-entry after a failed exit and
// returns the new deadline.
func (t *topChargeIdleMachine) blockUntil(now time.Time) time.Time {
	t.blockedUntil = now.Add(t.failureCooldown())
	return t.blockedUntil
}

// failureCooldown is how long re-entry stays blocked after the regime exits
// on evidence the pass-through is not working: at least the authority
// publish cooldown, and never below one minute.
func (t *topChargeIdleMachine) failureCooldown() time.Duration {
	cooldown := authorityPublishCooldown(t.cfg)
	if cooldown < time.Minute {
		cooldown = time.Minute
	}
	return cooldown
}

// entryEligible reports whether this cycle shows evidence that the battery
// is truly full and PV can be passed through by firmware.
func (t *topChargeIdleMachine) entryEligible(devStatus marstek.Status, rawGridWatts, smoothed float64) bool {
	if devStatus.SOCPercent < t.cfg.NearFullIdleEnterPercent {
		return false
	}
	// Meaningful import means solar no longer covers load, so do not enter a
	// pass-through idle that would strand the house on grid power.
	if smoothed > float64(t.cfg.ImportBiasWatts) || rawGridWatts > float64(t.cfg.ImportBiasWatts) {
		return false
	}
	solarInputWatts := devStatus.Solar1Watts + devStatus.Solar2Watts
	outputWatts := devStatus.Output1Watts + devStatus.Output2Watts
	return solarInputWatts > 0 || outputWatts > nearZeroOutputWatts(t.cfg.MinOutputWatts) || devStatus.PassThroughActive()
}

// healthy reports whether pass-through is still flowing while the regime is
// engaged.
func (t *topChargeIdleMachine) healthy(devStatus marstek.Status) bool {
	outputWatts := devStatus.Output1Watts + devStatus.Output2Watts
	return devStatus.PassThroughActive() || outputWatts > nearZeroOutputWatts(t.cfg.MinOutputWatts)
}
