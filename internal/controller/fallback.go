package controller

import (
	"context"
	"log/slog"
	"time"

	"github.com/lucavb/marstek-prometheus-controller/internal/marstek"
	"github.com/lucavb/marstek-prometheus-controller/internal/metrics"
)

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

// commandZero disables the controlled slot and sets state to idle. It is used
// when the SoC soft floor is active to avoid publishing commands that the BMS
// will silently gate. Unlike fallback(), it increments CommandSuppressedTotal
// rather than FallbackTotal, and it always preserves the other four slots from
// the freshly-read devStatus (not the cached lastStatus).
func (c *Controller) commandZero(ctx context.Context, now time.Time, devStatus marstek.Status) error {
	if c.m != nil {
		c.m.CommandSuppressedTotal.WithLabelValues("soc_floor").Inc()
		c.m.SetState(metrics.StateIdle)
	}

	if c.lastCommandWatts == 0 {
		return nil
	}

	slots := marstek.SlotsAsWriteSlots(devStatus)
	idx := c.cfg.ScheduleSlot - 1
	slots[idx] = marstek.Slot{
		Enabled: false,
		Start:   c.cfg.ScheduleStart,
		End:     c.cfg.ScheduleEnd,
		Watts:   0,
	}

	payload := marstek.BuildTimedDischargePayload(slots, false)
	if err := c.pub.Publish(c.cfg.ControlTopic, payload); err != nil {
		slog.Warn("commandZero publish failed", "err", err)
		if c.m != nil {
			c.m.MQTTPublishErrorsTotal.WithLabelValues(classifyMQTTError(err)).Inc()
		}
		return err
	}

	slog.Info("soc floor: disabled discharge slot",
		"slot", c.cfg.ScheduleSlot,
		"prev_watts", c.lastCommandWatts)
	c.lastCommandWatts = 0
	c.lastCommandTime = now
	if c.m != nil {
		c.m.CommandedSlotPowerWatts.Set(0)
		c.m.MQTTPublishesTotal.WithLabelValues("write").Inc()
		c.m.RecordLastMQTTPublish(now)
	}
	return nil
}
