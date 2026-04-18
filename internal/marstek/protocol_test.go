package marstek_test

import (
	"strings"
	"testing"

	"github.com/lucavb/marstek-prometheus-controller/internal/marstek"
)

// realDevicePayload is a verbatim status payload captured from the live
// HMJ-2 device (firmware vv=110) during plan validation on 2026-04-18.
const realDevicePayload = "p1=1,p2=1,w1=375,w2=380,pe=51,vv=110,sv=9,cs=0,cd=0,am=0,o1=1,o2=1,do=90,lv=240,cj=0,kn=1142,g1=120,g2=118,b1=0,b2=0,md=0,d1=1,e1=0:0,f1=23:59,h1=240,d2=0,e2=0:0,f2=23:59,h2=80,d3=0,e3=0:0,f3=23:59,h3=80,sg=0,sp=80,st=0,tl=24,th=25,tc=0,tf=0,fc=202310231502,id=5,a0=51,a1=0,a2=0,l0=2,l1=0,c0=255,c1=4,bc=4392,bs=3135,pt=4920,it=3229,m0=0,m1=0,m2=0,m3=238,d4=0,e4=0:0,f4=23:59,h4=80,d5=0,e5=0:0,f5=23:59,h5=80,lmo=2045,lmi=1483,lmf=0,uv=107,sm=0,bn=0,ct_t=7,tc_dis=1"

func TestTopics(t *testing.T) {
	ctrl := marstek.ControlTopic("HMJ-2", "60323bd14b6e")
	want := "hame_energy/HMJ-2/App/60323bd14b6e/ctrl"
	if ctrl != want {
		t.Errorf("ControlTopic = %q, want %q", ctrl, want)
	}

	status := marstek.StatusTopic("HMJ-2", "60323bd14b6e")
	wantS := "hame_energy/HMJ-2/device/60323bd14b6e/ctrl"
	if status != wantS {
		t.Errorf("StatusTopic = %q, want %q", status, wantS)
	}
}

func TestBuildTimedDischargePayload_Volatile(t *testing.T) {
	slots := [5]marstek.Slot{
		{Enabled: true, Start: "00:00", End: "23:59", Watts: 240},
		{Enabled: false, Start: "00:00", End: "23:59", Watts: 80},
		{Enabled: false, Start: "00:00", End: "23:59", Watts: 80},
		{Enabled: false, Start: "00:00", End: "23:59", Watts: 80},
		{Enabled: false, Start: "00:00", End: "23:59", Watts: 80},
	}
	payload := marstek.BuildTimedDischargePayload(slots, false)

	want := "cd=20,md=0,a1=1,b1=00:00,e1=23:59,v1=240,a2=0,b2=00:00,e2=23:59,v2=80,a3=0,b3=00:00,e3=23:59,v3=80,a4=0,b4=00:00,e4=23:59,v4=80,a5=0,b5=00:00,e5=23:59,v5=80"
	if payload != want {
		t.Errorf("BuildTimedDischargePayload (volatile) =\n  %q\nwant\n  %q", payload, want)
	}
}

func TestBuildTimedDischargePayload_Flash(t *testing.T) {
	slots := [5]marstek.Slot{
		{Enabled: true, Start: "00:00", End: "23:59", Watts: 300},
		{}, {}, {}, {},
	}
	payload := marstek.BuildTimedDischargePayload(slots, true)
	if !strings.HasPrefix(payload, "cd=7,") {
		t.Errorf("flash payload should start with cd=7, got %q", payload)
	}
}

func TestBuildTimedDischargePayload_WattsClamp(t *testing.T) {
	slots := [5]marstek.Slot{
		{Enabled: true, Start: "00:00", End: "23:59", Watts: 999},
		{Enabled: false, Start: "00:00", End: "23:59", Watts: -10},
		{}, {}, {},
	}
	payload := marstek.BuildTimedDischargePayload(slots, false)
	if !strings.Contains(payload, ",v1=800,") {
		t.Errorf("expected v1 clamped to 800, got %q", payload)
	}
	if !strings.Contains(payload, ",v2=0,") {
		t.Errorf("expected v2 clamped to 0, got %q", payload)
	}
}

func TestBuildTimedDischargePayload_EmptySlots(t *testing.T) {
	// Zero-value Slot should produce enabled=0, start=00:00, end=23:59, watts=0
	var slots [5]marstek.Slot
	payload := marstek.BuildTimedDischargePayload(slots, false)
	want := "cd=20,md=0,a1=0,b1=00:00,e1=23:59,v1=0,a2=0,b2=00:00,e2=23:59,v2=0,a3=0,b3=00:00,e3=23:59,v3=0,a4=0,b4=00:00,e4=23:59,v4=0,a5=0,b5=00:00,e5=23:59,v5=0"
	if payload != want {
		t.Errorf("BuildTimedDischargePayload (empty) =\n  %q\nwant\n  %q", payload, want)
	}
}

func TestParseStatus_RealPayload(t *testing.T) {
	s := marstek.ParseStatus(realDevicePayload)

	tests := []struct {
		name string
		got  int
		want int
	}{
		{"SOCPercent", s.SOCPercent, 51},
		{"RemainingWh", s.RemainingWh, 1142},
		{"DoDPercent", s.DoDPercent, 90},
		{"ChargingMode", s.ChargingMode, 0},
		{"Output1Enabled", s.Output1Enabled, 1},
		{"Output2Enabled", s.Output2Enabled, 1},
		{"Output1Watts", s.Output1Watts, 120},
		{"Output2Watts", s.Output2Watts, 118},
		{"OutputThresholdWatts", s.OutputThresholdWatts, 240},
		{"Solar1Watts", s.Solar1Watts, 375},
		{"Solar2Watts", s.Solar2Watts, 380},
		{"FirmwareMajor", s.FirmwareMajor, 110},
		{"FirmwareSub", s.FirmwareSub, 9},
		{"Bootloader", s.Bootloader, 107},
		{"RatedOutputWatts", s.RatedOutputWatts, 2045},
		{"TempMinC", s.TempMinC, 24},
		{"TempMaxC", s.TempMaxC, 25},
		{"DischargeSettingsCode", s.DischargeSettingsCode, 0},
		{"Slot1Watts", s.Slots[0].Watts, 240},
		{"Slot2Watts", s.Slots[1].Watts, 80},
		{"Slot4Watts", s.Slots[3].Watts, 80},
		{"Slot5Watts", s.Slots[4].Watts, 80},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got %d, want %d", tt.got, tt.want)
			}
		})
	}

	if s.SurplusFeedIn {
		t.Error("SurplusFeedIn should be false (tc_dis=1)")
	}
	if !s.Slots[0].Enabled {
		t.Error("Slot 1 should be enabled (d1=1)")
	}
	if s.Slots[1].Enabled {
		t.Error("Slot 2 should be disabled (d2=0)")
	}
}

func TestParseStatus_SlotTimes(t *testing.T) {
	s := marstek.ParseStatus(realDevicePayload)
	// Device returns ragged times like "0:0" for midnight
	if s.Slots[0].Start != "0:0" {
		t.Errorf("Slot1.Start = %q, want %q", s.Slots[0].Start, "0:0")
	}
	if s.Slots[0].End != "23:59" {
		t.Errorf("Slot1.End = %q, want %q", s.Slots[0].End, "23:59")
	}
}

func TestSlotsAsWriteSlots_NormalisesTime(t *testing.T) {
	s := marstek.ParseStatus(realDevicePayload)
	ws := marstek.SlotsAsWriteSlots(s)
	if ws[0].Start != "00:00" {
		t.Errorf("write slot 1 Start = %q, want %q", ws[0].Start, "00:00")
	}
	if ws[0].End != "23:59" {
		t.Errorf("write slot 1 End = %q, want %q", ws[0].End, "23:59")
	}
	if !ws[0].Enabled {
		t.Error("write slot 1 should be enabled")
	}
	if ws[0].Watts != 240 {
		t.Errorf("write slot 1 Watts = %d, want 240", ws[0].Watts)
	}
}

func TestParseStatus_EmptyPayload(t *testing.T) {
	s := marstek.ParseStatus("")
	if s.SOCPercent != 0 {
		t.Errorf("empty payload SOC = %d, want 0", s.SOCPercent)
	}
}

func TestParseStatus_PartialPayload(t *testing.T) {
	s := marstek.ParseStatus("pe=42,vv=110")
	if s.SOCPercent != 42 {
		t.Errorf("SOC = %d, want 42", s.SOCPercent)
	}
	if s.FirmwareMajor != 110 {
		t.Errorf("FirmwareMajor = %d, want 110", s.FirmwareMajor)
	}
	if s.Output1Watts != 0 {
		t.Errorf("Output1Watts should default to 0, got %d", s.Output1Watts)
	}
}

func TestRoundTrip_WritePreservesOtherSlots(t *testing.T) {
	// Parse real status, convert to write slots, change slot 1 power, build payload.
	s := marstek.ParseStatus(realDevicePayload)
	ws := marstek.SlotsAsWriteSlots(s)
	ws[0].Watts = 300

	payload := marstek.BuildTimedDischargePayload(ws, false)

	// Slot 1 should have new power
	if !strings.Contains(payload, ",v1=300,") {
		t.Errorf("expected v1=300 in payload, got %q", payload)
	}
	// Slot 2–5 should be preserved (80 W, disabled)
	for _, n := range []int{2, 3, 4, 5} {
		needle := ",v" + string(rune('0'+n)) + "=80,"
		if !strings.Contains(payload, needle) && !strings.HasSuffix(payload, ",v"+string(rune('0'+n))+"=80") {
			t.Errorf("expected v%d=80 preserved in payload, got %q", n, payload)
		}
	}
}
