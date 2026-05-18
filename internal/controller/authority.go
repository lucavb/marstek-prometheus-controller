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
			c.m.AuthorityRemediationTotal.WithLabelValues(kind, "publish_error").Inc()
		}
		return false, false, err
	}
	c.authorityPendingPayload = payload
	c.authorityPendingSince = now
	c.authorityPendingSeenAt = observedAt
	if c.m != nil {
		c.m.MQTTPublishesTotal.WithLabelValues("write").Inc()
		c.m.AuthorityRemediationTotal.WithLabelValues(kind, "published").Inc()
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
	return false, nil
}

func (c *Controller) ensureSurplusFeedIn(now time.Time, devStatus marstek.Status) (bool, error) {
	payload := marstek.BuildSurplusFeedInPayload(true)
	if devStatus.SurplusFeedIn {
		c.loggedSurplusFeedInBlocked = false
		c.clearPendingAuthorityPayload(payload)
		return false, nil
	}
	if !c.cfg.AllowFlashWrites {
		if c.m != nil {
			c.m.AuthorityRemediationTotal.WithLabelValues("surplus_feed_in", "blocked_flash_guard").Inc()
		}
		if !c.loggedSurplusFeedInBlocked {
			c.loggedSurplusFeedInBlocked = true
			slog.Warn("surplus feed-in is disabled; automatic remediation blocked because ALLOW_FLASH_WRITES is false")
		}
		return false, nil
	}
	minInterval := c.cfg.SurplusFeedInRecoveryMinInterval
	if minInterval <= 0 {
		minInterval = 6 * time.Hour
	}
	if !c.lastSurplusFeedInRecoveryAt.IsZero() && now.Sub(c.lastSurplusFeedInRecoveryAt) < minInterval {
		if c.m != nil {
			c.m.AuthorityRemediationTotal.WithLabelValues("surplus_feed_in", "rate_limited").Inc()
		}
		slog.Warn("surplus feed-in remediation rate limited",
			"last_recovery_at", c.lastSurplusFeedInRecoveryAt,
			"min_interval", minInterval)
		return false, nil
	}
	handled, published, err := c.publishAuthorityPayload(now, time.Time{}, payload, "surplus_feed_in",
		"desired_enabled", true,
		"allow_flash_writes", c.cfg.AllowFlashWrites)
	if published {
		c.lastSurplusFeedInRecoveryAt = now
	}
	return handled, err
}

func (c *Controller) maybeEnsureOutputEnabled(now time.Time, devStatus marstek.Status, desiredWatts int, smoothed float64) (bool, error) {
	outputEnablePayload := marstek.BuildOutputEnablePayload(true, true)
	if desiredWatts <= 0 {
		c.outputBlockedCycles = 0
		c.lastOutputEnableAttemptAt = time.Time{}
		c.loggedNuclearRestartBlocked = false
		return false, nil
	}

	idx := c.cfg.ScheduleSlot - 1
	if idx < 0 || idx > 4 {
		idx = 0
	}
	controlledSlot := devStatus.Slots[idx]
	slotArmed := controlledSlot.Enabled && controlledSlot.Watts > 0

	currentOutputWatts := devStatus.Output1Watts + devStatus.Output2Watts
	solarInputWatts := devStatus.Solar1Watts + devStatus.Solar2Watts
	batteryContributionWatts := currentOutputWatts - solarInputWatts
	if batteryContributionWatts > 0 || !slotArmed || c.lastCommandWatts <= 0 {
		c.outputBlockedCycles = 0
		c.lastOutputEnableAttemptAt = time.Time{}
		c.loggedNuclearRestartBlocked = false
		if batteryContributionWatts > 0 || (devStatus.Output1Enabled == 1 && devStatus.Output2Enabled == 1) {
			c.clearPendingAuthorityPayload(outputEnablePayload)
		}
		return false, nil
	}

	c.outputBlockedCycles++
	if handled, err := c.maybeNuclearRestartRecovery(now, devStatus, desiredWatts, smoothed, currentOutputWatts, solarInputWatts, batteryContributionWatts); err != nil {
		return false, err
	} else if handled {
		return true, nil
	}
	if c.outputBlockedCycles < 2 {
		return false, nil
	}

	// fw116 can misreport o1/o2 while discharge is healthy, so output-enable is
	// retried off observed zero-power behavior with a cooldown instead of
	// waiting for a specific status-flag echo.
	handled, _, err := c.publishAuthorityPayload(now, time.Time{}, outputEnablePayload, "output_enable",
		"slot", c.cfg.ScheduleSlot,
		"desired_watts", desiredWatts,
		"slot_watts", controlledSlot.Watts,
		"last_command_watts", c.lastCommandWatts,
		"output_watts", currentOutputWatts,
		"solar_input_watts", solarInputWatts,
		"battery_contribution_watts", batteryContributionWatts,
		"o1", devStatus.Output1Enabled,
		"o2", devStatus.Output2Enabled)
	if handled {
		c.lastOutputEnableAttemptAt = now
	}
	return handled, err
}

func (c *Controller) maybeNuclearRestartRecovery(now time.Time, devStatus marstek.Status, desiredWatts int, smoothed float64, currentOutputWatts, solarInputWatts, batteryContributionWatts int) (bool, error) {
	requiredCycles := c.cfg.NuclearRestartBlockedCycles
	if requiredCycles <= 0 {
		requiredCycles = 6
	}
	if c.outputBlockedCycles < requiredCycles {
		return false, nil
	}
	if c.lastOutputEnableAttemptAt.IsZero() {
		c.recordNuclearRestartOutcome("blocked_evidence_insufficient")
		return false, nil
	}
	if smoothed <= float64(c.cfg.ImportBiasWatts) {
		c.recordNuclearRestartOutcome("blocked_evidence_insufficient")
		return false, nil
	}
	if !c.cfg.NuclearRestartEnabled {
		c.recordNuclearRestartOutcome("disabled")
		if !c.loggedNuclearRestartBlocked {
			c.loggedNuclearRestartBlocked = true
			slog.Warn("nuclear restart recovery disabled despite sustained blocked output",
				"blocked_cycles", c.outputBlockedCycles,
				"desired_watts", desiredWatts,
				"smoothed_grid_watts", smoothed,
				"output_watts", currentOutputWatts,
				"solar_input_watts", solarInputWatts,
				"battery_contribution_watts", batteryContributionWatts,
				"enable_env", "NUCLEAR_RESTART_ENABLED=true",
				"ack_env", "NUCLEAR_RESTART_ACK_WIFI_RECOVERY=true")
		}
		return false, nil
	}
	if !c.cfg.NuclearRestartAckWiFiRecovery {
		c.recordNuclearRestartOutcome("wifi_ack_missing")
		slog.Error("nuclear restart recovery blocked: WiFi recovery acknowledgement missing",
			"blocked_cycles", c.outputBlockedCycles,
			"ack_env", "NUCLEAR_RESTART_ACK_WIFI_RECOVERY=true")
		return false, nil
	}
	minInterval := c.cfg.NuclearRestartMinInterval
	if minInterval > 0 && !c.lastNuclearRestartAt.IsZero() && now.Sub(c.lastNuclearRestartAt) < minInterval {
		c.recordNuclearRestartOutcome("rate_limited")
		slog.Warn("nuclear restart recovery rate limited",
			"last_restart_at", c.lastNuclearRestartAt,
			"min_interval", minInterval)
		return false, nil
	}

	if err := c.pub.Publish(c.cfg.ControlTopic, marstek.RestartPayload); err != nil {
		outcome := "publish_error"
		if classifyMQTTError(err) == "disconnected" {
			outcome = "mqtt_not_connected"
		}
		c.recordNuclearRestartOutcome(outcome)
		if c.m != nil {
			c.m.MQTTPublishErrorsTotal.WithLabelValues(classifyMQTTError(err)).Inc()
		}
		slog.Warn("nuclear restart recovery publish failed",
			"err", err,
			"outcome", outcome)
		return false, err
	}

	blockedCycles := c.outputBlockedCycles
	c.lastNuclearRestartAt = now
	c.outputBlockedCycles = 0
	c.lastOutputEnableAttemptAt = time.Time{}
	c.loggedNuclearRestartBlocked = false
	c.authorityPendingPayload = marstek.RestartPayload
	c.authorityPendingSince = now
	c.authorityPendingSeenAt = time.Time{}
	if c.m != nil {
		c.m.MQTTPublishesTotal.WithLabelValues("write").Inc()
		c.m.RecordLastMQTTPublish(now)
		c.m.NuclearRestartTotal.WithLabelValues("restart_command_published").Inc()
		c.m.LastNuclearRestartTimestampSecs.Set(float64(now.Unix()))
	}
	slog.Error("nuclear restart recovery published device restart",
		"payload", marstek.RestartPayload,
		"blocked_cycles", blockedCycles,
		"desired_watts", desiredWatts,
		"smoothed_grid_watts", smoothed,
		"output_watts", currentOutputWatts,
		"solar_input_watts", solarInputWatts,
		"battery_contribution_watts", batteryContributionWatts)
	return true, nil
}

func (c *Controller) recordNuclearRestartOutcome(outcome string) {
	if c.m == nil || outcome == "" {
		return
	}
	c.m.NuclearRestartTotal.WithLabelValues(outcome).Inc()
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
