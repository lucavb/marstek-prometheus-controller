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
	if c.topChargeIdle.reset("fallback") {
		blockedUntil := c.topChargeIdle.blockUntil(c.clock.Now())
		slog.Info("top-charge idle deactivated", "reason", "fallback", "blocked_until", blockedUntil)
	}
	c.escalation.reset()

	// Prefer the last known device status so the other four schedule slots are
	// preserved. Fall back to a zero status only if we have never successfully
	// read one — in that edge case, we have nothing to preserve anyway.
	base := marstek.Status{}
	if c.hasLastStatus {
		base = c.lastStatus
	}
	desired := c.disabledControlledSlot()
	current, payload := c.buildSlotPayload(base, desired, false)
	if current == desired {
		c.noteIdleWithoutPublish(false)
		return nil
	}

	if err := c.pub.Publish(c.cfg.ControlTopic, payload); err != nil {
		slog.Warn("fallback publish failed", "err", err, "reason", reason)
		if c.m != nil {
			c.m.MQTTPublishErrorsTotal.WithLabelValues(classifyMQTTError(err)).Inc()
		}
		return err
	}

	slog.Warn("fallback: commanded zero discharge", "reason", reason)
	now := c.clock.Now()
	c.recordSuccessfulPublish(payload, now, time.Time{})
	c.recordCommandedWatts(0, now)
	return nil
}

// commandIdle disables the controlled slot and sets state to idle. It is used
// in deliberately-idle regimes where commanding anything would either fight the
// BMS or force a pointless discharge. Unlike fallback(), it increments CommandSuppressedTotal
// with the caller-supplied reason rather than FallbackTotal, and always
// preserves the other four slots from the freshly-read devStatus (not the
// cached lastStatus).
func (c *Controller) commandIdle(ctx context.Context, now, observedAt time.Time, devStatus marstek.Status, reason string) error {
	if c.m != nil {
		c.m.CommandSuppressedTotal.WithLabelValues(reason).Inc()
		c.m.SetState(metrics.StateIdle)
	}

	handled, published, err := c.ensureControlledSlot(now, observedAt, devStatus, 0, reason)
	if err != nil {
		return err
	}
	if handled {
		if published {
			c.recordCommandedWatts(0, now)
		} else {
			c.noteIdleWithoutPublish(false)
		}
		return nil
	}
	c.noteIdleWithoutPublish(true)
	return nil
}
