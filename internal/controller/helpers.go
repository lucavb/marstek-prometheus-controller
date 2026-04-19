package controller

import (
	"context"
	"errors"
	"math"
	"net"
	"strings"

	"github.com/lucavb/marstek-prometheus-controller/internal/metrics"
	"github.com/lucavb/marstek-prometheus-controller/internal/promclient"
)

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
