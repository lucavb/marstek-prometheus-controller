package controller

import (
	"time"

	"github.com/lucavb/marstek-prometheus-controller/internal/marstek"
)

// controlledSlotIndex returns the 0-based index of the configured schedule
// slot. Config validation already enforces SCHEDULE_SLOT in 1-5; the clamp
// is defence in depth so a bad index can never write an out-of-range slot.
func (c *Controller) controlledSlotIndex() int {
	idx := c.cfg.ScheduleSlot - 1
	if idx < 0 || idx > 4 {
		idx = 0
	}
	return idx
}

// disabledControlledSlot is the desired slot state for every stop: the slot
// is disabled because the device silently clamps Watts=0 on an enabled slot
// up to its 80 W floor — only Enabled=false is a real stop.
func (c *Controller) disabledControlledSlot() marstek.Slot {
	return marstek.Slot{
		Enabled: false,
		Start:   c.cfg.ScheduleStart,
		End:     c.cfg.ScheduleEnd,
		Watts:   0,
	}
}

// desiredControlledSlot describes the controlled slot state the controller
// wants on the device for the given discharge watts.
func (c *Controller) desiredControlledSlot(desiredWatts int) marstek.Slot {
	if desiredWatts < 0 {
		desiredWatts = 0
	}
	return marstek.Slot{
		Enabled: desiredWatts > 0,
		Start:   c.cfg.ScheduleStart,
		End:     c.cfg.ScheduleEnd,
		Watts:   desiredWatts,
	}
}

// buildSlotPayload merges desired into the device's current five slots and
// encodes the timed-discharge payload. The other four slots are always
// preserved (see AGENTS.md "Preserve all five slots on every write"). It
// returns the controlled slot as currently reported by the device so callers
// can decide whether a publish is needed at all.
func (c *Controller) buildSlotPayload(devStatus marstek.Status, desired marstek.Slot, persistToFlash bool) (current marstek.Slot, payload string) {
	slots := marstek.SlotsAsWriteSlots(devStatus)
	idx := c.controlledSlotIndex()
	current = slots[idx]
	slots[idx] = desired
	return current, marstek.BuildTimedDischargePayload(slots, persistToFlash)
}

// controlledSlotMatches reports whether the device's controlled slot already
// equals the desired slot. A disabled desired slot matches on the schedule
// times regardless of the reported watts (a disabled slot's watts are
// meaningless); an enabled slot must also match watts.
func controlledSlotMatches(current, desired marstek.Slot) bool {
	if current.Enabled != desired.Enabled || current.Start != desired.Start || current.End != desired.End {
		return false
	}
	if !desired.Enabled {
		return true
	}
	return current.Watts == desired.Watts
}

// pendingEchoAwaited reports whether payload was published recently and the
// device status has not echoed it back yet, so re-publishing would be
// redundant.
func (c *Controller) pendingEchoAwaited(payload string, now, observedAt time.Time) bool {
	return payload == c.authorityPendingPayload &&
		!c.authorityPendingSince.IsZero() &&
		(observedAt.IsZero() || observedAt.Equal(c.authorityPendingSeenAt)) &&
		now.Sub(c.authorityPendingSince) < authorityPublishCooldown(c.cfg)
}

// clearPendingAuthorityPayload clears the pending-echo state once the device
// status has echoed payload (or the payload is no longer wanted).
func (c *Controller) clearPendingAuthorityPayload(payload string) {
	if payload == "" || c.authorityPendingPayload != payload {
		return
	}
	c.authorityPendingPayload = ""
	c.authorityPendingSince = time.Time{}
	c.authorityPendingSeenAt = time.Time{}
}

// authorityPublishCooldown is how long the controller waits for a device
// status echo before re-publishing a remediation payload: two control
// intervals, or two status poll timeouts, whichever is longer.
func authorityPublishCooldown(cfg Config) time.Duration {
	cooldown := cfg.ControlInterval
	if cfg.StatusPollTimeout > cooldown {
		cooldown = cfg.StatusPollTimeout
	}
	if cooldown <= 0 {
		cooldown = 15 * time.Second
	}
	return 2 * cooldown
}

// recordSuccessfulPublish records the bookkeeping every successful write to
// the control topic shares: the pending-echo state and the publish metrics.
// It is the only place a publish is recorded.
func (c *Controller) recordSuccessfulPublish(payload string, now, observedAt time.Time) {
	c.authorityPendingPayload = payload
	c.authorityPendingSince = now
	c.authorityPendingSeenAt = observedAt
	if c.m != nil {
		c.m.MQTTPublishesTotal.WithLabelValues("write").Inc()
		c.m.RecordLastMQTTPublish(now)
	}
}

// recordCommandedWatts advances the last-command memory after a publish that
// commanded watts: both the ramp baseline and the commanded-slot gauge move
// to the published value.
func (c *Controller) recordCommandedWatts(watts int, at time.Time) {
	c.lastCommandWatts = watts
	c.lastCommandTime = at
	if c.m != nil {
		c.m.CommandedSlotPowerWatts.Set(float64(watts))
	}
}

// recordReassertedCommand records a publish that re-asserted the current
// command unchanged (slot re-assert after suppression): only the publish
// time and the commanded-slot gauge advance.
func (c *Controller) recordReassertedCommand(watts int, at time.Time) {
	c.lastCommandTime = at
	if c.m != nil {
		c.m.CommandedSlotPowerWatts.Set(float64(watts))
	}
}

// noteIdleWithoutPublish records that the controlled slot stands at idle
// with no publish this cycle: the last-command watts become 0 and the
// publish time does not advance. reportGauge preserves the difference
// between the fallback path, which historically leaves the commanded-slot
// gauge untouched here, and the deliberate-idle paths, which sync it.
func (c *Controller) noteIdleWithoutPublish(reportGauge bool) {
	c.lastCommandWatts = 0
	if reportGauge && c.m != nil {
		c.m.CommandedSlotPowerWatts.Set(0)
	}
}
