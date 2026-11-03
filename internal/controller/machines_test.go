package controller

import (
	"testing"
	"time"

	"github.com/lucavb/marstek-prometheus-controller/internal/marstek"
	"github.com/lucavb/marstek-prometheus-controller/internal/metrics"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// metricValue reads the current value of a gauge or counter without touching
// the registry, so tests stay independent of client_golang's testutil (which
// would add a transitive dependency).
func metricValue(t *testing.T, metric prometheus.Metric) float64 {
	t.Helper()
	var m dto.Metric
	if err := metric.Write(&m); err != nil {
		t.Fatalf("read metric: %v", err)
	}
	switch {
	case m.Gauge != nil:
		return m.Gauge.GetValue()
	case m.Counter != nil:
		return m.Counter.GetValue()
	default:
		t.Fatal("metric is neither gauge nor counter")
		return 0
	}
}

func TestEscalationResetClearsEvidence(t *testing.T) {
	var e escalationMachine
	e.noteBlockedCycle()
	e.noteBlockedCycle()
	e.recordEnableAttempt(time.Unix(1000, 0))
	e.shouldWarnRestartDisabled()

	if e.blockedCycles != 2 || !e.enableAttempted() {
		t.Fatal("expected blocked evidence to be recorded")
	}

	e.reset()

	if e.blockedCycles != 0 {
		t.Error("reset must clear blocked cycles")
	}
	if e.enableAttempted() {
		t.Error("reset must clear the enable attempt")
	}
	if !e.shouldWarnRestartDisabled() {
		t.Error("reset must re-arm the once-per-regime warning")
	}
}

func TestEscalationRecordNuclearRestartResetsEvidence(t *testing.T) {
	now := time.Unix(2000, 0)
	var e escalationMachine
	e.noteBlockedCycle()
	e.recordEnableAttempt(now)

	e.recordNuclearRestart(now)

	if !e.lastRestartAt.Equal(now) {
		t.Errorf("lastRestartAt = %v, want %v", e.lastRestartAt, now)
	}
	if e.blockedCycles != 0 || e.enableAttempted() {
		t.Error("recordNuclearRestart must clear the blocked evidence")
	}
}

func TestEscalationRestartRateLimited(t *testing.T) {
	tests := []struct {
		name          string
		lastRestartAt time.Time
		minInterval   time.Duration
		now           time.Time
		want          bool
	}{
		{"never restarted", time.Time{}, time.Hour, time.Unix(5000, 0), false},
		{"within interval", time.Unix(4000, 0), time.Hour, time.Unix(5000, 0), true},
		{"past interval", time.Unix(4000, 0), time.Hour, time.Unix(8000, 0), false},
		{"zero interval disables rate limit", time.Unix(4000, 0), 0, time.Unix(4001, 0), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := escalationMachine{lastRestartAt: tt.lastRestartAt}
			if got := e.restartRateLimited(tt.now, tt.minInterval); got != tt.want {
				t.Errorf("restartRateLimited() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTopChargeIdleResetClearsDebounce(t *testing.T) {
	m := topChargeIdleMachine{
		active:                 true,
		enterSamples:           3,
		socExitSamples:         2,
		importSamples:          1,
		rawImportSamples:       4,
		passThroughLostSamples: 2,
	}

	if !m.reset("soc_floor") {
		t.Fatal("reset must report the regime was active")
	}
	if m.active {
		t.Error("reset must disengage the regime")
	}
	if m.enterSamples != 0 || m.socExitSamples != 0 || m.importSamples != 0 ||
		m.rawImportSamples != 0 || m.passThroughLostSamples != 0 {
		t.Error("reset must clear every debounce counter")
	}
	if m.reset("again") {
		t.Error("reset on an inactive regime must report false")
	}
}

func TestTopChargeIdleFailureCooldown(t *testing.T) {
	tests := []struct {
		name              string
		controlInterval   time.Duration
		statusPollTimeout time.Duration
		want              time.Duration
	}{
		{"floored to one minute", 15 * time.Second, 5 * time.Second, time.Minute},
		{"two control intervals above floor", 45 * time.Second, 5 * time.Second, 90 * time.Second},
		{"status poll timeout dominates", 10 * time.Second, 40 * time.Second, 80 * time.Second},
		{"zero values fall back to 15s base", 0, 0, time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := topChargeIdleMachine{cfg: Config{
				ControlInterval:   tt.controlInterval,
				StatusPollTimeout: tt.statusPollTimeout,
			}}
			if got := m.failureCooldown(); got != tt.want {
				t.Errorf("failureCooldown() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTopChargeIdleBlockUntil(t *testing.T) {
	m := topChargeIdleMachine{cfg: Config{ControlInterval: 45 * time.Second}}
	now := time.Unix(1000, 0)
	got := m.blockUntil(now)
	if !got.Equal(now.Add(90 * time.Second)) {
		t.Errorf("blockUntil() = %v, want %v", got, now.Add(90*time.Second))
	}
}

// machineStatus builds the device status the top-charge idle machine reads:
// full battery, feed-in on, firmware pass-through flowing house-sized solar
// and output power.
func machineStatus(socPct int, feedIn, passThrough bool) marstek.Status {
	s := marstek.Status{
		SOCPercent:    socPct,
		SurplusFeedIn: feedIn,
		Solar1Watts:   300,
		Solar2Watts:   300,
		Output1Watts:  120,
		Output2Watts:  120,
	}
	if passThrough {
		s.Solar1Mode = 2
		s.Solar2Mode = 2
	}
	return s
}

func idleMachineCfg() Config {
	return Config{
		NearFullIdleEnabled:               true,
		EnforceSurplusFeedIn:              true,
		NearFullIdleEnterPercent:          100,
		NearFullIdleConsecutiveSamples:    2,
		NearFullIdleGridImportExitWatts:   50,
		NearFullIdleGridImportExitSamples: 2,
		ControlInterval:                   15 * time.Second,
		StatusPollTimeout:                 5 * time.Second,
		MinOutputWatts:                    80,
	}
}

func TestTopChargeIdleUpdateReentersAfterCooldownExpires(t *testing.T) {
	cfg := idleMachineCfg()
	m := topChargeIdleMachine{cfg: cfg}
	now := time.Unix(10_000, 0)
	full := machineStatus(100, true, true)

	if m.update(now, full, 0, 0) {
		t.Fatal("must not engage on the first eligible sample")
	}
	now = now.Add(cfg.ControlInterval)
	if !m.update(now, full, 0, 0) {
		t.Fatal("regime must engage once the entry debounce completes")
	}

	broken := machineStatus(100, true, false)
	broken.Solar1Watts, broken.Solar2Watts = 0, 0
	broken.Output1Watts, broken.Output2Watts = 0, 0
	now = now.Add(cfg.ControlInterval)
	if !m.update(now, broken, 0, 0) {
		t.Fatal("regime must stay engaged on the first unhealthy sample")
	}
	exitAt := now.Add(cfg.ControlInterval)
	now = exitAt
	if m.update(now, broken, 0, 0) {
		t.Fatal("regime must exit after sustained pass-through loss")
	}
	if m.active {
		t.Fatal("exit must disengage the regime")
	}

	for i := 0; i < 3; i++ {
		now = now.Add(cfg.ControlInterval)
		if m.update(now, full, 0, 0) {
			t.Fatalf("cooldown must block re-entry, cycle %d past exit", i+1)
		}
	}

	// The gate must open again once the deadline passes, allowing a full
	// entry debounce from scratch. A gate that never re-opens fails here.
	now = exitAt.Add(m.failureCooldown())
	if m.update(now, full, 0, 0) {
		t.Fatal("first post-cooldown sample must not engage yet")
	}
	now = now.Add(cfg.ControlInterval)
	if !m.update(now, full, 0, 0) {
		t.Fatal("regime must re-enter after the cooldown expires")
	}
}

func TestTopChargeIdleUpdateIgnoresImportExitsWhenSamplesZero(t *testing.T) {
	cfg := idleMachineCfg()
	cfg.NearFullIdleGridImportExitSamples = 0
	m := topChargeIdleMachine{cfg: cfg, active: true}
	now := time.Unix(10_000, 0)

	for i := 0; i < 5; i++ {
		now = now.Add(cfg.ControlInterval)
		if !m.update(now, machineStatus(100, true, true), 200, 200) {
			t.Fatalf("regime must stay engaged with import exits disabled, exited at cycle %d", i+1)
		}
	}
	if !m.active {
		t.Error("regime must still be engaged after sustained import")
	}
}

func TestTopChargeIdleUpdateExitsOnSmoothedImportOnly(t *testing.T) {
	mm := metrics.New("60323bd14b6e", "B2500", "tcp://broker.test:1883", "test")
	cfg := idleMachineCfg()
	m := topChargeIdleMachine{cfg: cfg, m: mm, active: true}
	now := time.Unix(10_000, 0)

	// Raw power is at zero, so only the smoothed signal shows import: the
	// exit can only fire via the smoothed-only "grid_import" reason.
	for i := 0; i+1 < cfg.NearFullIdleGridImportExitSamples; i++ {
		now = now.Add(cfg.ControlInterval)
		if !m.update(now, machineStatus(100, true, true), 0, 200) {
			t.Fatalf("regime must stay engaged while the import exit debounces, exited at cycle %d", i+1)
		}
	}
	now = now.Add(cfg.ControlInterval)
	if m.update(now, machineStatus(100, true, true), 0, 200) {
		t.Fatal("regime must exit once smoothed import persists past the debounce window")
	}
	if got := metricValue(t, mm.TopChargeIdleExitReasonTotal.WithLabelValues("grid_import")); got != 1 {
		t.Errorf("top_charge_idle_exit_reason_total{reason=\"grid_import\"} = %v, want 1", got)
	}
	if got := metricValue(t, mm.TopChargeIdleExited); got != 1 {
		t.Errorf("top_charge_idle_exited_total = %v, want 1", got)
	}
	if m.blockedUntil.IsZero() {
		t.Error("grid_import exit must start the re-entry cooldown")
	}
}

func TestTopChargeIdleUpdateDisengagesWhenDeviceFeedInDisabled(t *testing.T) {
	m := topChargeIdleMachine{cfg: idleMachineCfg(), active: true, passThroughLostSamples: 3}
	now := time.Unix(10_000, 0)

	if m.update(now, machineStatus(100, false, true), 0, 0) {
		t.Fatal("regime must disengage when the device reports surplus feed-in disabled")
	}
	if m.active {
		t.Error("feed-in loss must disengage the regime")
	}
	if m.passThroughLostSamples != 0 {
		t.Error("disengagement must clear the debounce counters")
	}
}
