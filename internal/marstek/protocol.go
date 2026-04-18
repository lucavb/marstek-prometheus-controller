// Package marstek contains pure protocol helpers for the hame_energy MQTT
// protocol used by Marstek B2500 (HMJ-2 and siblings). No I/O lives here.
//
// Key protocol facts (confirmed on firmware vv=110):
//
//   - Poll command (sent to device):  cd=1
//   - Status response (from device):  flat k=v,k=v,… string
//   - Timed-discharge write keys differ from read keys:
//     Write: a<N> (enabled), b<N> (start HH:MM), e<N> (end HH:MM), v<N> (watts)
//     Read:  d<N> (enabled), e<N> (start, may be ragged "0:0"), f<N> (end), h<N> (watts)
//   - All 5 slots must be included in every timed-discharge write.
//   - cd=20 → volatile (no flash wear). cd=7 → persistent flash.
//   - Writes produce no response on the status topic.
package marstek

import (
	"fmt"
	"strconv"
	"strings"
)

// ControlTopic returns the topic the controller publishes commands to.
// Direction: App → device.
func ControlTopic(deviceType, deviceID string) string {
	return fmt.Sprintf("hame_energy/%s/App/%s/ctrl", deviceType, deviceID)
}

// StatusTopic returns the topic the device publishes status on.
// Direction: device → broker (broadcast to all subscribers).
func StatusTopic(deviceType, deviceID string) string {
	return fmt.Sprintf("hame_energy/%s/device/%s/ctrl", deviceType, deviceID)
}

// PollPayload is the payload to publish to ControlTopic to request a status response.
const PollPayload = "cd=1"

// CDVolatile is the command code for a volatile (no-flash) timed-discharge write.
const CDVolatile = 20

// CDPersist is the command code for a persistent (flash) timed-discharge write.
const CDPersist = 7

// Slot represents one timed-discharge period as used in write commands.
// Index is 1-based (1–5).
type Slot struct {
	Enabled bool
	Start   string // HH:MM e.g. "00:00"
	End     string // HH:MM e.g. "23:59"
	Watts   int    // 0–800
}

// BuildTimedDischargePayload builds the full MQTT payload for a timed-discharge
// write command. All 5 slots must be provided; partial writes clear other slots.
//
// flash=false → cd=20 (volatile, no flash wear — default for control loops).
// flash=true  → cd=7  (persistent, use only for manual one-shot configuration).
func BuildTimedDischargePayload(slots [5]Slot, flash bool) string {
	cd := CDVolatile
	if flash {
		cd = CDPersist
	}

	var b strings.Builder
	fmt.Fprintf(&b, "cd=%d,md=0", cd)
	for i, s := range slots {
		n := i + 1
		enabled := 0
		if s.Enabled {
			enabled = 1
		}
		watts := s.Watts
		if watts < 0 {
			watts = 0
		}
		if watts > 800 {
			watts = 800
		}
		start := s.Start
		if start == "" {
			start = "00:00"
		}
		end := s.End
		if end == "" {
			end = "23:59"
		}
		fmt.Fprintf(&b, ",a%d=%d,b%d=%s,e%d=%s,v%d=%d",
			n, enabled,
			n, start,
			n, end,
			n, watts)
	}
	return b.String()
}

// Status holds the parsed fields from a device status payload (cd=1 response).
// Field names correspond directly to the protocol keys; see the plan for the
// full key mapping. Only fields relevant to the controller are populated.
type Status struct {
	// Battery
	SOCPercent   int // pe
	RemainingWh  int // kn
	DoDPercent   int // do
	ChargingMode int // cs: 0=simultaneous, 1=charge-then-discharge
	AdaptiveMode int // md: 0=off, 1=on (am in some firmware; md in read payload)

	// Outputs (each port)
	Output1Enabled int // o1
	Output2Enabled int // o2
	Output1Watts   int // g1
	Output2Watts   int // g2

	// Schedule slots (read-side keys: d/e/f/h)
	Slots [5]ReadSlot

	// Active threshold (mirrors active slot's power on fw vv=110)
	OutputThresholdWatts int // lv

	// Solar inputs
	Solar1Watts int // w1
	Solar2Watts int // w2

	// Device info
	FirmwareMajor    int  // vv
	FirmwareSub      int  // sv
	Bootloader       int  // uv
	RatedOutputWatts int  // lmo
	SurplusFeedIn    bool // tc_dis: 0=enabled, 1=disabled

	// Temperature
	TempMinC int // tl
	TempMaxC int // th

	// DischargeSettingsCode is the device's current cd= value in the status
	// payload. This is NOT the write-side command code — it shadows the concept.
	DischargeSettingsCode int // cd (in read payload)
}

// ReadSlot is one timed-discharge period as returned by the device (cd=1).
// Fields use the read-side key names (d/e/f/h), NOT the write-side (a/b/e/v).
type ReadSlot struct {
	Enabled bool   // d<N>
	Start   string // e<N> — may be ragged ("0:0")
	End     string // f<N>
	Watts   int    // h<N>
}

// ParseStatus parses a flat "k=v,k=v,…" payload from a device status message.
// Unknown keys are silently ignored; partially-formed payloads are handled
// gracefully (zero-values for missing fields).
func ParseStatus(payload string) Status {
	m := parseKV(payload)
	var s Status

	s.SOCPercent = intField(m, "pe")
	s.RemainingWh = intField(m, "kn")
	s.DoDPercent = intField(m, "do")
	s.ChargingMode = intField(m, "cs")
	s.AdaptiveMode = intField(m, "md")
	s.Output1Enabled = intField(m, "o1")
	s.Output2Enabled = intField(m, "o2")
	s.Output1Watts = intField(m, "g1")
	s.Output2Watts = intField(m, "g2")
	s.OutputThresholdWatts = intField(m, "lv")
	s.Solar1Watts = intField(m, "w1")
	s.Solar2Watts = intField(m, "w2")
	s.FirmwareMajor = intField(m, "vv")
	s.FirmwareSub = intField(m, "sv")
	s.Bootloader = intField(m, "uv")
	s.RatedOutputWatts = intField(m, "lmo")
	s.TempMinC = intField(m, "tl")
	s.TempMaxC = intField(m, "th")
	s.DischargeSettingsCode = intField(m, "cd")

	// tc_dis=0 means surplus feed-in is ENABLED (inverted flag in protocol).
	s.SurplusFeedIn = intField(m, "tc_dis") == 0

	for i := 0; i < 5; i++ {
		n := i + 1
		s.Slots[i] = ReadSlot{
			Enabled: intField(m, fmt.Sprintf("d%d", n)) == 1,
			Start:   m[fmt.Sprintf("e%d", n)],
			End:     m[fmt.Sprintf("f%d", n)],
			Watts:   intField(m, fmt.Sprintf("h%d", n)),
		}
	}

	return s
}

// SlotsAsWriteSlots converts the 5 ReadSlots from a parsed status into the
// Slot type used by BuildTimedDischargePayload, normalising ragged time strings
// to "HH:MM" format (e.g. "0:0" → "00:00").
func SlotsAsWriteSlots(s Status) [5]Slot {
	var out [5]Slot
	for i, rs := range s.Slots {
		out[i] = Slot{
			Enabled: rs.Enabled,
			Start:   normaliseTime(rs.Start),
			End:     normaliseTime(rs.End),
			Watts:   rs.Watts,
		}
	}
	return out
}

// normaliseTime pads single-digit hour/minute components to two digits.
// "0:0" → "00:00", "8:5" → "08:05", "23:59" → "23:59".
func normaliseTime(t string) string {
	if t == "" {
		return "00:00"
	}
	parts := strings.SplitN(t, ":", 2)
	if len(parts) != 2 {
		return t
	}
	// Pad with leading zero only when the component is a single digit.
	var h, m string
	if len(parts[0]) == 1 {
		h = "0" + parts[0]
	} else {
		h = parts[0]
	}
	if len(parts[1]) == 1 {
		m = "0" + parts[1]
	} else {
		m = parts[1]
	}
	return h + ":" + m
}

// parseKV splits a "k=v,k=v,…" string into a map. Each token is split on the
// first "=" only so that time values like "e1=0:0" are handled correctly.
func parseKV(payload string) map[string]string {
	result := make(map[string]string)
	for _, token := range strings.Split(payload, ",") {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		k, v, ok := strings.Cut(token, "=")
		if !ok {
			continue
		}
		result[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return result
}

func intField(m map[string]string, key string) int {
	v, ok := m[key]
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0
	}
	return n
}
