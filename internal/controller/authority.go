package controller

import (
	"log/slog"
	"time"

	"github.com/lucavb/marstek-prometheus-controller/internal/marstek"
)

func (c *Controller) authorityPublishCooldown() time.Duration {
	cooldown := c.cfg.ControlInterval
	if c.cfg.StatusPollTimeout > cooldown {
		cooldown = c.cfg.StatusPollTimeout
	}
	if cooldown <= 0 {
		cooldown = 15 * time.Second
	}
	return 2 * cooldown
}

func (c *Controller) clearPendingAuthorityPayload(payload string) {
	if payload == "" || c.authorityPendingPayload != payload {
		return
	}
	c.authorityPendingPayload = ""
	c.authorityPendingSince = time.Time{}
	c.authorityPendingSeenAt = time.Time{}
}

func (c *Controller) publishAuthorityPayload(now, observedAt time.Time, payload, kind string, attrs ...any) (bool, bool, error) {
	if payload == "" {
		return false, false, nil
	}
	if payload == c.authorityPendingPayload &&
		!c.authorityPendingSince.IsZero() &&
		(observedAt.IsZero() || observedAt.Equal(c.authorityPendingSeenAt)) &&
		now.Sub(c.authorityPendingSince) < c.authorityPublishCooldown() {
		slog.Debug("authority remediation awaiting status echo",
			"kind", kind,
			"payload", payload,
			"age", now.Sub(c.authorityPendingSince).Round(time.Second))
		return true, false, nil
	}
	if err := c.pub.Publish(c.cfg.ControlTopic, payload); err != nil {
		slog.Warn("authority remediation publish failed", "err", err, "kind", kind, "payload", payload)
		if c.m != nil {
			c.m.MQTTPublishErrorsTotal.WithLabelValues(classifyMQTTError(err)).Inc()
		}
		return false, false, err
	}
	c.authorityPendingPayload = payload
	c.authorityPendingSince = now
	c.authorityPendingSeenAt = observedAt
	if c.m != nil {
		c.m.MQTTPublishesTotal.WithLabelValues("write").Inc()
		c.m.RecordLastMQTTPublish(now)
	}
	logAttrs := append([]any{"kind", kind, "payload", payload}, attrs...)
	slog.Warn("controller authority remediation published", logAttrs...)
	return true, true, nil
}

func (c *Controller) ensureDeviceConfig(now, observedAt time.Time, devStatus marstek.Status) (bool, error) {
	chargingModePayload := marstek.BuildChargingModePayload(0)
	if devStatus.ChargingMode == 0 {
		c.clearPendingAuthorityPayload(chargingModePayload)
	} else {
		handled, _, err := c.publishAuthorityPayload(now, observedAt, chargingModePayload, "charging_mode",
			"observed_mode", devStatus.ChargingMode,
			"desired_mode", 0)
		return handled, err
	}

	outputEnablePayload := marstek.BuildOutputEnablePayload(true, true)
	if devStatus.Output1Enabled == 1 && devStatus.Output2Enabled == 1 {
		c.clearPendingAuthorityPayload(outputEnablePayload)
		return false, nil
	}
	handled, _, err := c.publishAuthorityPayload(now, observedAt, outputEnablePayload, "output_enable",
		"o1", devStatus.Output1Enabled,
		"o2", devStatus.Output2Enabled,
		"desired_mask", 3)
	return handled, err
}

func (c *Controller) desiredControlledSlot(devStatus marstek.Status, desiredWatts int) (marstek.Slot, [5]marstek.Slot) {
	slots := marstek.SlotsAsWriteSlots(devStatus)
	if desiredWatts < 0 {
		desiredWatts = 0
	}
	desired := marstek.Slot{
		Enabled: desiredWatts > 0,
		Start:   c.cfg.ScheduleStart,
		End:     c.cfg.ScheduleEnd,
		Watts:   desiredWatts,
	}
	return desired, slots
}

func (c *Controller) ensureControlledSlot(now, observedAt time.Time, devStatus marstek.Status, desiredWatts int, reason string) (bool, bool, error) {
	desired, slots := c.desiredControlledSlot(devStatus, desiredWatts)
	idx := c.cfg.ScheduleSlot - 1
	if idx < 0 || idx > 4 {
		idx = 0
	}
	current := slots[idx]
	slots[idx] = desired
	payload := marstek.BuildTimedDischargePayload(slots, false)
	if controlledSlotMatches(current, desired) {
		c.clearPendingAuthorityPayload(payload)
		return false, false, nil
	}
	return c.publishAuthorityPayload(now, observedAt, payload, "controlled_slot",
		"reason", reason,
		"slot", c.cfg.ScheduleSlot,
		"desired_enabled", desired.Enabled,
		"desired_watts", desired.Watts,
		"current_enabled", current.Enabled,
		"current_start", current.Start,
		"current_end", current.End,
		"current_watts", current.Watts)
}

func controlledSlotMatches(current, desired marstek.Slot) bool {
	if current.Enabled != desired.Enabled || current.Start != desired.Start || current.End != desired.End {
		return false
	}
	if !desired.Enabled {
		return true
	}
	return current.Watts == desired.Watts
}
