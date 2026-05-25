package controller

import (
	"log/slog"
	"math"

	"github.com/lucavb/marstek-prometheus-controller/internal/marstek"
)

type highSoCStage int

const (
	highSoCStageInactive highSoCStage = iota
	highSoCStageNetZero
	highSoCStageAggressive
	highSoCStageMax
)

func (s highSoCStage) allowExport() bool {
	return s >= highSoCStageAggressive
}

func (s highSoCStage) active() bool {
	return s != highSoCStageInactive
}

func (s highSoCStage) String() string {
	switch s {
	case highSoCStageNetZero:
		return "net_zero"
	case highSoCStageAggressive:
		return "aggressive"
	case highSoCStageMax:
		return "max"
	default:
		return "inactive"
	}
}

func (c *Controller) highSoCStageForStatus(devStatus marstek.Status) highSoCStage {
	if c.highSoCStage == highSoCStageMax && devStatus.SOCPercent >= c.cfg.HighSoCAggressivePercent {
		return highSoCStageMax
	}
	switch {
	case devStatus.SOCPercent >= c.cfg.HighSoCMaxPercent:
		return highSoCStageMax
	case devStatus.SOCPercent >= c.cfg.HighSoCAggressivePercent:
		return highSoCStageAggressive
	case devStatus.SOCPercent >= c.cfg.HighSoCDisableImportBiasPercent:
		return highSoCStageNetZero
	default:
		return highSoCStageInactive
	}
}

func (c *Controller) clampTargetWatts(target int) int {
	if target < 0 {
		target = 0
	}
	if target > c.cfg.MaxOutputWatts {
		target = c.cfg.MaxOutputWatts
	}
	if target > 0 && target < c.cfg.MinOutputWatts {
		target = c.cfg.MinOutputWatts
	}
	return target
}

func (c *Controller) computeRawTargetWithBias(devStatus marstek.Status, smoothed float64, importBiasWatts int) int {
	currentOutput := devStatus.Output1Watts + devStatus.Output2Watts
	rawTarget := currentOutput + int(math.Round(smoothed)) - importBiasWatts
	return c.clampTargetWatts(rawTarget)
}

func (c *Controller) computeTarget(devStatus marstek.Status, smoothed float64) (int, highSoCStage) {
	prevStage := c.highSoCStage
	stage := c.highSoCStageForStatus(devStatus)
	c.highSoCStage = stage

	if stage != prevStage {
		slog.Info("high-soc policy transitioned",
			"from", prevStage.String(),
			"to", stage.String(),
			"soc_pct", devStatus.SOCPercent,
			"smoothed_grid_watts", math.Round(smoothed),
			"import_bias_disabled", stage >= highSoCStageNetZero,
			"export_allowed", stage.allowExport())
	}

	switch stage {
	case highSoCStageNetZero:
		return c.computeRawTargetWithBias(devStatus, smoothed, 0), stage
	case highSoCStageAggressive:
		zeroBiasTarget := c.computeRawTargetWithBias(devStatus, smoothed, 0)
		solarTraceTarget := int(math.Round(float64(devStatus.Solar1Watts+devStatus.Solar2Watts) * c.cfg.HighSoCSolarTraceFactor))
		return c.clampTargetWatts(max(zeroBiasTarget, solarTraceTarget)), stage
	case highSoCStageMax:
		return c.cfg.MaxOutputWatts, stage
	default:
		return c.computeRawTargetWithBias(devStatus, smoothed, c.cfg.ImportBiasWatts), stage
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
