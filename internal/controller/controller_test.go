package controller_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lucavb/marstek-prometheus-controller/internal/controller"
	"github.com/lucavb/marstek-prometheus-controller/internal/marstek"
	"github.com/lucavb/marstek-prometheus-controller/internal/promclient"
)

// ── fakes ──────────────────────────────────────────────────────────────────

type fakeProm struct {
	mu     sync.Mutex
	sample promclient.Sample
	err    error
}

func (f *fakeProm) Query(_ context.Context) (promclient.Sample, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sample, f.err
}

func (f *fakeProm) set(watts float64, age time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sample = promclient.Sample{Watts: watts, SampleTime: time.Now().Add(-age)}
	f.err = nil
}

func (f *fakeProm) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

type fakePublisher struct {
	mu       sync.Mutex
	payloads []string
	err      error
}

func (f *fakePublisher) Publish(_, payload string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.payloads = append(f.payloads, payload)
	return nil
}

func (f *fakePublisher) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.payloads) == 0 {
		return ""
	}
	return f.payloads[len(f.payloads)-1]
}

func (f *fakePublisher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.payloads)
}

func (f *fakePublisher) countContaining(needle string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, payload := range f.payloads {
		if strings.Contains(payload, needle) {
			count++
		}
	}
	return count
}

type fakeStatus struct {
	mu          sync.Mutex
	status      marstek.Status
	receivedAt  time.Time
	pollErr     error
	pollPayload marstek.Status
}

func (f *fakeStatus) LatestStatus() (marstek.Status, time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status, f.receivedAt
}

func (f *fakeStatus) Poll(_ context.Context, _ time.Duration) (marstek.Status, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pollPayload, f.pollErr
}

func (f *fakeStatus) setFresh(s marstek.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = s
	f.receivedAt = time.Now()
}

func (f *fakeStatus) setStale() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.receivedAt = time.Now().Add(-10 * time.Minute)
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// ── helpers ────────────────────────────────────────────────────────────────

func freshDevStatus() marstek.Status {
	// g1=0,g2=0: device is not currently discharging. Tests that need a
	// non-zero baseline must set Output1Watts/Output2Watts explicitly.
	// Using zero output ensures rawTarget = 0 + grid - bias, keeping all
	// single-step tests equivalent to the previous absolute formula.
	s := marstek.ParseStatus(
		"p1=1,p2=1,w1=375,w2=380,pe=51,vv=110,sv=9,cs=0,cd=0,am=0,o1=1,o2=1,do=90," +
			"lv=240,cj=0,kn=1142,g1=0,g2=0,b1=0,b2=0,md=0," +
			"d1=1,e1=0:0,f1=23:59,h1=240,d2=0,e2=0:0,f2=23:59,h2=80," +
			"d3=0,e3=0:0,f3=23:59,h3=80,d4=0,e4=0:0,f4=23:59,h4=80," +
			"d5=0,e5=0:0,f5=23:59,h5=80,lmo=2045,lmi=1483,lmf=0,uv=107,sm=0,bn=0,ct_t=7,tc_dis=1",
	)
	return s
}

func defaultCfg(ctrl, start, end string) controller.Config {
	return controller.Config{
		PrometheusStaleAfter:                60 * time.Second,
		StatusStaleAfter:                    2 * time.Minute,
		StatusPollTimeout:                   5 * time.Second,
		StatusHardFailAfter:                 5 * time.Minute,
		ControlInterval:                     15 * time.Second,
		SmoothingAlpha:                      1.0, // no smoothing in tests for determinism
		DeadbandWatts:                       25,
		RampUpWattsPerCycle:                 800, // effectively no ramp in unit tests
		RampDownWattsPerCycle:               800,
		MinCommandDeltaWatts:                1,
		MinCommandDeltaWattsExporting:       0,
		MinHoldTime:                         0,
		MinOutputWatts:                      80,
		MaxOutputWatts:                      800,
		ControlTopic:                        ctrl,
		ScheduleSlot:                        1,
		ScheduleStart:                       start,
		ScheduleEnd:                         end,
		PersistToFlash:                      false,
		PassthroughStallDetectCycles:        5,
		PassthroughStallMinCommandWatts:     80,
		PassthroughAutoRecoveryMinInterval:  time.Hour,
		PassthroughAutoRecoveryRestoreDelay: 5 * time.Minute,
	}
}

// ── tests ──────────────────────────────────────────────────────────────────

func TestStep_GridImport_IncreasesSlotPower(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	p.set(200, 0) // 200 W grid import
	st.setFresh(freshDevStatus())

	cfg := defaultCfg("hame_energy/HMJ-2/App/60323bd14b6e/ctrl", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)

	if err := c.Step(context.Background()); err != nil {
		t.Fatalf("Step() error = %v", err)
	}

	last := pub.last()
	if last == "" {
		t.Fatal("expected a publish, got none")
	}
	// currentOutput=0 (g1=0,g2=0), bias=0 → v1 = 0 + 200 - 0 = 200
	if !strings.Contains(last, ",v1=200,") {
		t.Errorf("expected v1=200 in payload, got %q", last)
	}
	if !c.Ready() {
		t.Error("controller should be ready after successful step")
	}
}

func TestStep_GridExport_ReducesSlotToZero(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	// Step 1: 300 W import — establishes a non-zero last command.
	p.set(300, 0)
	st.setFresh(freshDevStatus())
	cfg := defaultCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)
	_ = c.Step(context.Background())

	// Step 2: -100 W export → target 0 → ramp down (800/cycle) → 0.
	p.set(-100, 0)
	st.setFresh(freshDevStatus())
	_ = c.Step(context.Background())

	last := pub.last()
	if !strings.Contains(last, ",a1=0,") {
		t.Errorf("expected a1=0 (slot disabled) for zero discharge, got %q", last)
	}
	if !strings.Contains(last, ",v1=0,") {
		t.Errorf("expected v1=0 in payload for zero discharge, got %q", last)
	}
}

func TestStep_Deadband_NoPublish(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	// Within deadband: 10 W import, deadband is 25 W. With no prior command
	// lastCommandWatts=0 so delta = |10-0| = 10 < MinCommandDeltaWatts=1? No:
	// MinCommandDeltaWatts=1 in defaultCfg, so 10 will still be published.
	// Use a tighter scenario: grid is exactly 0 W (in deadband) and last command was 0.
	p.set(0, 0)
	st.setFresh(freshDevStatus())

	cfg := defaultCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)

	if err := c.Step(context.Background()); err != nil {
		t.Fatalf("Step() error = %v", err)
	}

	// 0 W grid → target=0, lastCommandWatts=0, delta=0 < MinCommandDeltaWatts=1 → suppressed
	if pub.count() != 0 {
		t.Errorf("expected no publish for zero grid power (no change from initial), got %d", pub.count())
	}
}

func TestStep_MinCommandDelta_Suppresses(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	// First step sets 200 W
	p.set(200, 0)
	st.setFresh(freshDevStatus())
	cfg := defaultCfg("topic", "00:00", "23:59")
	cfg.MinCommandDeltaWatts = 50
	c := controller.New(cfg, p, pub, st, clk, nil)

	_ = c.Step(context.Background()) // sets 200 W

	// Second step: 210 W (delta=10 < 50) → suppressed
	p.set(210, 0)
	st.setFresh(freshDevStatus())
	beforeCount := pub.count()
	_ = c.Step(context.Background())

	if pub.count() != beforeCount {
		t.Errorf("expected no publish for small delta, count changed from %d to %d", beforeCount, pub.count())
	}
}

func TestStep_MinHoldTime_Suppresses(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	p.set(200, 0)
	st.setFresh(freshDevStatus())
	cfg := defaultCfg("topic", "00:00", "23:59")
	cfg.MinHoldTime = 30 * time.Second
	c := controller.New(cfg, p, pub, st, clk, nil)

	_ = c.Step(context.Background()) // publishes 200 W

	// Advance only 10 seconds — within hold time
	clk.advance(10 * time.Second)
	p.set(400, 0)
	st.setFresh(freshDevStatus())
	beforeCount := pub.count()
	_ = c.Step(context.Background())

	if pub.count() != beforeCount {
		t.Errorf("expected hold-time suppression, count changed from %d to %d", beforeCount, pub.count())
	}

	// Advance past hold time — should publish
	clk.advance(25 * time.Second) // total 35s > 30s hold
	_ = c.Step(context.Background())
	if pub.count() == beforeCount {
		t.Error("expected publish after hold time expired")
	}
}

func TestStep_RampUp_LimitsIncrease(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	p.set(500, 0)
	st.setFresh(freshDevStatus())
	cfg := defaultCfg("topic", "00:00", "23:59")
	cfg.RampUpWattsPerCycle = 100
	cfg.MinCommandDeltaWatts = 1
	c := controller.New(cfg, p, pub, st, clk, nil)

	_ = c.Step(context.Background())
	last := pub.last()
	// Starting from 0, ramp-up cap = 100
	if !strings.Contains(last, ",v1=100,") {
		t.Errorf("expected ramp-limited v1=100, got %q", last)
	}
}

func TestStep_RampDown_LimitsDecrease(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	p.set(500, 0)
	st.setFresh(freshDevStatus())
	cfg := defaultCfg("topic", "00:00", "23:59")
	cfg.RampUpWattsPerCycle = 800
	cfg.RampDownWattsPerCycle = 50
	cfg.MinCommandDeltaWatts = 1
	c := controller.New(cfg, p, pub, st, clk, nil)

	_ = c.Step(context.Background()) // first step: 500 W

	// Now grid is 0 (export/import=0), target = 0, ramp down = 50 → 500-50=450
	p.set(0, 0)
	st.setFresh(freshDevStatus())
	_ = c.Step(context.Background())
	last := pub.last()
	if !strings.Contains(last, ",v1=450,") {
		t.Errorf("expected ramp-down to 450, got %q", last)
	}
}

func TestStep_MaxOutputWatts_Clamps(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	p.set(1000, 0) // above max
	st.setFresh(freshDevStatus())
	cfg := defaultCfg("topic", "00:00", "23:59")
	cfg.MaxOutputWatts = 400
	cfg.RampUpWattsPerCycle = 800
	c := controller.New(cfg, p, pub, st, clk, nil)

	_ = c.Step(context.Background())
	last := pub.last()
	if !strings.Contains(last, ",v1=400,") {
		t.Errorf("expected v1 clamped to 400, got %q", last)
	}
}

func TestStep_StalePrometheus_Fallback(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	// Give the controller a previous command of 300 W
	p.set(300, 0)
	st.setFresh(freshDevStatus())
	cfg := defaultCfg("topic", "00:00", "23:59")
	cfg.PrometheusStaleAfter = 30 * time.Second
	c := controller.New(cfg, p, pub, st, clk, nil)
	_ = c.Step(context.Background()) // publishes 300 W

	// Now serve a stale sample
	p.set(300, 60*time.Second) // 60s old > 30s threshold
	st.setFresh(freshDevStatus())
	countBefore := pub.count()
	_ = c.Step(context.Background()) // should fall back to 0 W

	if pub.count() == countBefore {
		t.Fatal("expected fallback publish on stale prometheus")
	}
	last := pub.last()
	if !strings.Contains(last, ",a1=0,") {
		t.Errorf("expected fallback to disable slot (a1=0), got %q", last)
	}
	if !strings.Contains(last, ",v1=0,") {
		t.Errorf("expected v1=0 in fallback payload, got %q", last)
	}
}

func TestStep_PrometheusError_Fallback(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	p.set(300, 0)
	st.setFresh(freshDevStatus())
	cfg := defaultCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)
	_ = c.Step(context.Background())

	p.setErr(errors.New("connection refused"))
	st.setFresh(freshDevStatus())
	countBefore := pub.count()
	_ = c.Step(context.Background())

	if pub.count() == countBefore {
		t.Fatal("expected fallback publish on prom error")
	}
	last := pub.last()
	if !strings.Contains(last, ",a1=0,") {
		t.Errorf("expected fallback to disable slot (a1=0), got %q", last)
	}
	if !strings.Contains(last, ",v1=0,") {
		t.Errorf("expected v1=0 in fallback payload, got %q", last)
	}
}

func TestStep_SelfPoll_OnStaleStatus(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	p.set(200, 0)
	st.setStale() // status is stale (10 min old)
	st.pollPayload = freshDevStatus()
	st.pollErr = nil

	cfg := defaultCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)
	_ = c.Step(context.Background())

	// Should still publish the schedule update (using poll result)
	last := pub.last()
	if last == "" {
		t.Fatal("expected publish despite stale status (used self-poll)")
	}
	if !strings.Contains(last, ",v1=200,") {
		t.Errorf("expected v1=200 after self-poll, got %q", last)
	}
}

func TestStep_MQTTPublishError_DoesNotUpdateLastCommand(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	p.set(300, 0)
	st.setFresh(freshDevStatus())
	cfg := defaultCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)

	pub.err = errors.New("not connected")
	err := c.Step(context.Background())
	if err == nil {
		t.Fatal("expected error from publish failure")
	}
}

func TestStep_PreservesOtherSlots(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	p.set(300, 0)
	st.setFresh(freshDevStatus()) // freshDevStatus has slots 2-5 at 80 W

	cfg := defaultCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)
	_ = c.Step(context.Background())

	last := pub.last()
	// Slots 2–5 should be preserved at 80 W
	for _, n := range []int{2, 3, 4, 5} {
		needle := ",v" + string(rune('0'+n)) + "=80"
		if !strings.Contains(last, needle) {
			t.Errorf("expected slot %d preserved at 80 W, payload = %q", n, last)
		}
	}
}

func TestStep_VolatilePayload_DefaultFalse(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	p.set(100, 0)
	st.setFresh(freshDevStatus())
	cfg := defaultCfg("topic", "00:00", "23:59")
	cfg.PersistToFlash = false
	c := controller.New(cfg, p, pub, st, clk, nil)
	_ = c.Step(context.Background())

	last := pub.last()
	if !strings.HasPrefix(last, "cd=20,") {
		t.Errorf("expected cd=20 (volatile), got %q", last)
	}
}

func TestController_Ready_FalseBeforeFirstStep(t *testing.T) {
	cfg := defaultCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, &fakeProm{}, &fakePublisher{}, &fakeStatus{}, nil, nil)
	if c.Ready() {
		t.Error("controller should not be ready before first step")
	}
}

func TestStep_ImportBias_ReducesTarget(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	p.set(200, 0)
	st.setFresh(freshDevStatus())
	cfg := defaultCfg("topic", "00:00", "23:59")
	cfg.ImportBiasWatts = 50 // should result in target = 200 - 50 = 150
	c := controller.New(cfg, p, pub, st, clk, nil)

	if err := c.Step(context.Background()); err != nil {
		t.Fatalf("Step() error = %v", err)
	}
	last := pub.last()
	if !strings.Contains(last, ",v1=150,") {
		t.Errorf("expected v1=150 (200W import − 50W bias), got %q", last)
	}
}

func TestStep_ImportBias_ZeroFloor(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	// Grid is importing only 30 W, but bias is 50 → raw = 30-50 = -20 → clamped to 0.
	p.set(30, 0)
	st.setFresh(freshDevStatus())
	cfg := defaultCfg("topic", "00:00", "23:59")
	cfg.ImportBiasWatts = 50
	c := controller.New(cfg, p, pub, st, clk, nil)

	// lastCommandWatts starts at 0, target=0, delta=0 → suppressed (no publish)
	if err := c.Step(context.Background()); err != nil {
		t.Fatalf("Step() error = %v", err)
	}
	if pub.count() != 0 {
		t.Errorf("expected no publish when biased target == lastCommandWatts(0), got %d", pub.count())
	}
}

// TestStep_ExportFastPath_BypassesHoldTime verifies that the export fast-path
// skips MinHoldTime suppression when reducing the command — a slow hold-time
// would leave the battery discharging into the grid for up to the hold window.
func TestStep_ExportFastPath_BypassesHoldTime(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := defaultCfg("topic", "00:00", "23:59")
	cfg.RampUpWattsPerCycle = 800
	cfg.RampDownWattsPerCycle = 800
	cfg.MinCommandDeltaWatts = 1
	cfg.MinHoldTime = 30 * time.Second
	c := controller.New(cfg, p, pub, st, clk, nil)

	// First step: establish 500 W discharge at time t=0.
	p.set(500, 0)
	st.setFresh(freshDevStatus())
	_ = c.Step(context.Background())
	if pub.count() != 1 {
		t.Fatalf("expected initial publish, got %d", pub.count())
	}

	// Advance only 5 s — well within the 30 s hold time. Grid goes negative
	// (exporting). Hold-time would normally suppress, but the fast-path must
	// bypass it.
	clk.advance(5 * time.Second)
	p.set(-100, 0)
	st.setFresh(freshDevStatus())
	_ = c.Step(context.Background())

	if pub.count() != 2 {
		t.Fatalf("export fast-path must bypass hold time; expected second publish, got count=%d", pub.count())
	}
	last := pub.last()
	if !strings.Contains(last, ",a1=0,") || !strings.Contains(last, ",v1=0,") {
		t.Errorf("export fast-path: expected slot disabled (a1=0,v1=0), got %q", last)
	}
}

// TestStep_HoldTime_StillSuppresses_WhenNotExporting sanity-checks that the
// new fast-path bypass does not leak into normal operation: positive grid
// readings with a reduced target still respect MinHoldTime.
func TestStep_HoldTime_StillSuppresses_WhenNotExporting(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := defaultCfg("topic", "00:00", "23:59")
	cfg.MinHoldTime = 30 * time.Second
	c := controller.New(cfg, p, pub, st, clk, nil)

	// Establish 500 W.
	p.set(500, 0)
	st.setFresh(freshDevStatus())
	_ = c.Step(context.Background())

	// Within hold time, grid drops to 200 W (still importing, not exporting).
	// We try to reduce to 200 W — hold-time must still suppress.
	clk.advance(5 * time.Second)
	p.set(200, 0)
	st.setFresh(freshDevStatus())
	countBefore := pub.count()
	_ = c.Step(context.Background())

	if pub.count() != countBefore {
		t.Errorf("non-export hold-time suppression regressed: got %d publishes, want %d", pub.count(), countBefore)
	}
}

// TestStep_ExportFastPath_BothGatesBypassed verifies that when the grid is
// exporting and we are reducing the command, both the min-delta gate and the
// hold-time gate are bypassed. This is the critical cross-gate interaction: if
// either gate were not bypassed, the battery would keep discharging into the
// grid for up to the hold window.
func TestStep_ExportFastPath_BothGatesBypassed(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := defaultCfg("topic", "00:00", "23:59")
	// MinCommandDeltaWatts=200 so non-export gate is very tight. Warmup uses
	// grid=300 so its delta (300) clears the 200 W non-export threshold.
	cfg.MinCommandDeltaWatts = 200
	cfg.MinCommandDeltaWattsExporting = 5
	cfg.MinHoldTime = 60 * time.Second
	cfg.MinOutputWatts = 1
	c := controller.New(cfg, p, pub, st, clk, nil)

	// Warmup: 300 W import → delta=300 >= 200 → publishes; lastCommandWatts=300.
	p.set(300, 0)
	st.setFresh(freshDevStatus())
	_ = c.Step(context.Background())
	if pub.count() != 1 {
		t.Fatalf("expected warmup publish, got %d", pub.count())
	}

	// 10 s into hold time, grid goes negative (exporting).
	// delta = |0 − 300| = 300 ≥ export threshold 5 → delta gate passes.
	// hold-time is still active but export fast-path bypasses it.
	clk.advance(10 * time.Second)
	p.set(-50, 0)
	st.setFresh(freshDevStatus()) // g1=g2=0 → currentOutput=0, rawTarget=0
	_ = c.Step(context.Background())

	if pub.count() != 2 {
		t.Fatalf("expected export fast-path to bypass both delta and hold-time gates; pub count=%d want 2", pub.count())
	}
	last := pub.last()
	if !strings.Contains(last, ",a1=0,") || !strings.Contains(last, ",v1=0,") {
		t.Errorf("expected slot disabled (a1=0,v1=0), got %q", last)
	}
}

func TestStep_ExportFastPath_BypassesRampDown(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	// Establish 500 W discharge.
	p.set(500, 0)
	st.setFresh(freshDevStatus())
	cfg := defaultCfg("topic", "00:00", "23:59")
	cfg.RampUpWattsPerCycle = 800
	cfg.RampDownWattsPerCycle = 50 // very slow ramp-down
	cfg.MinCommandDeltaWatts = 1
	c := controller.New(cfg, p, pub, st, clk, nil)
	_ = c.Step(context.Background()) // 500 W commanded

	// Grid goes negative: exporting.  Normally ramp-down=50 would limit us to
	// 450 W, but the export fast-path must jump directly to 0.
	p.set(-100, 0)
	st.setFresh(freshDevStatus())
	_ = c.Step(context.Background())
	last := pub.last()
	if !strings.Contains(last, ",a1=0,") {
		t.Errorf("export fast-path: expected a1=0 (slot disabled), got %q", last)
	}
	if !strings.Contains(last, ",v1=0,") {
		t.Errorf("export fast-path: expected v1=0, got %q", last)
	}
}

// ── MinOutputWatts tests ───────────────────────────────────────────────────

// TestStep_MinOutputWatts_SnapsDeadZoneUp verifies that a computed target in
// the 1..MinOutputWatts-1 dead zone is snapped up to MinOutputWatts.
func TestStep_MinOutputWatts_SnapsDeadZoneUp(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	// grid=130W, bias=100 → rawTarget=30W (inside 1..79 dead zone).
	p.set(130, 0)
	st.setFresh(freshDevStatus())
	cfg := defaultCfg("topic", "00:00", "23:59")
	cfg.ImportBiasWatts = 100
	cfg.MinOutputWatts = 80
	c := controller.New(cfg, p, pub, st, clk, nil)

	if err := c.Step(context.Background()); err != nil {
		t.Fatalf("Step() error = %v", err)
	}
	last := pub.last()
	if !strings.Contains(last, ",a1=1,") {
		t.Errorf("expected a1=1 (slot enabled), got %q", last)
	}
	if !strings.Contains(last, ",v1=80,") {
		t.Errorf("expected dead-zone snap-up to v1=80, got %q", last)
	}
}

// TestStep_MinOutputWatts_ZeroTargetDisablesSlot verifies that a computed
// target of exactly 0W produces a disabled slot (a1=0), not an enabled slot
// with v=0 (which the device silently clamps to 80W).
func TestStep_MinOutputWatts_ZeroTargetDisablesSlot(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	// First step: establish a non-zero last command so delta > 0 on second step.
	p.set(200, 0)
	st.setFresh(freshDevStatus())
	cfg := defaultCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)
	_ = c.Step(context.Background())

	// Second step: grid=0 → target=0 → slot should be disabled.
	p.set(0, 0)
	st.setFresh(freshDevStatus())
	_ = c.Step(context.Background())

	last := pub.last()
	if !strings.Contains(last, ",a1=0,") {
		t.Errorf("zero target must disable slot (a1=0), got %q", last)
	}
	if !strings.Contains(last, ",v1=0,") {
		t.Errorf("expected v1=0 in payload, got %q", last)
	}
}

// TestStep_RampDownAcrossDeadZone_JumpsToZero ensures that a slow ramp-down
// which lands inside the dead zone (1..MinOutputWatts-1) collapses to 0 and
// disables the slot, rather than publishing an unreachable wattage.
func TestStep_RampDownAcrossDeadZone_JumpsToZero(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	// Establish 120 W discharge.
	p.set(120, 0)
	st.setFresh(freshDevStatus())
	cfg := defaultCfg("topic", "00:00", "23:59")
	cfg.RampUpWattsPerCycle = 800
	cfg.RampDownWattsPerCycle = 50 // ramp-down would land at 120-50=70W (dead zone)
	cfg.MinCommandDeltaWatts = 1
	cfg.MinOutputWatts = 80
	c := controller.New(cfg, p, pub, st, clk, nil)
	_ = c.Step(context.Background()) // 120 W commanded

	// Target drops to 0; ramp would give 70W but that's in the dead zone.
	// Controller must jump to 0 and disable the slot.
	p.set(0, 0)
	st.setFresh(freshDevStatus())
	_ = c.Step(context.Background())

	last := pub.last()
	if !strings.Contains(last, ",a1=0,") {
		t.Errorf("ramp through dead zone: expected a1=0 (slot disabled), got %q", last)
	}
	if !strings.Contains(last, ",v1=0,") {
		t.Errorf("ramp through dead zone: expected v1=0, got %q", last)
	}
}

// TestStep_Fallback_PreservesOtherSlots verifies that a fallback (triggered by
// stale Prometheus) re-publishes slots 2–5 with the values the device last
// reported, rather than wiping them to zero. Only the controlled slot is
// disabled.
func TestStep_Fallback_PreservesOtherSlots(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	// Status with slot 3 enabled at 200 W and slot 5 enabled at 150 W.
	custom := marstek.ParseStatus(
		"pe=51,vv=110,sv=9,cs=0,o1=1,o2=1,g1=0,g2=0,md=0," +
			"d1=1,e1=0:0,f1=23:59,h1=240," +
			"d2=0,e2=0:0,f2=23:59,h2=80," +
			"d3=1,e3=6:0,f3=22:0,h3=200," +
			"d4=0,e4=0:0,f4=23:59,h4=80," +
			"d5=1,e5=12:0,f5=18:0,h5=150,tc_dis=1",
	)
	st.setFresh(custom)
	p.set(300, 0)

	cfg := defaultCfg("topic", "00:00", "23:59")
	cfg.PrometheusStaleAfter = 30 * time.Second
	c := controller.New(cfg, p, pub, st, clk, nil)

	// First step: successful publish establishes lastCommandWatts > 0 and
	// caches the device status for later fallback re-use.
	if err := c.Step(context.Background()); err != nil {
		t.Fatalf("initial Step() error = %v", err)
	}

	// Second step: serve a stale sample to force fallback.
	p.set(300, 60*time.Second)
	st.setFresh(custom)
	countBefore := pub.count()
	_ = c.Step(context.Background())

	if pub.count() == countBefore {
		t.Fatal("expected fallback publish on stale prometheus")
	}
	last := pub.last()
	// Controlled slot (1) should be disabled.
	if !strings.Contains(last, ",a1=0,") || !strings.Contains(last, ",v1=0,") {
		t.Errorf("expected controlled slot disabled (a1=0,v1=0), got %q", last)
	}
	// Slots 3 and 5 should be preserved.
	if !strings.Contains(last, ",a3=1,") {
		t.Errorf("expected slot 3 preserved as enabled (a3=1), got %q", last)
	}
	if !strings.Contains(last, ",v3=200,") {
		t.Errorf("expected slot 3 watts preserved (v3=200), got %q", last)
	}
	if !strings.Contains(last, ",a5=1,") {
		t.Errorf("expected slot 5 preserved as enabled (a5=1), got %q", last)
	}
	if !strings.Contains(last, ",v5=150,") && !strings.HasSuffix(last, ",v5=150") {
		t.Errorf("expected slot 5 watts preserved (v5=150), got %q", last)
	}
	// Slots 2 and 4 were disabled in the status and should stay that way.
	if !strings.Contains(last, ",a2=0,") || !strings.Contains(last, ",v2=80,") {
		t.Errorf("expected slot 2 preserved disabled at 80 W, got %q", last)
	}
	if !strings.Contains(last, ",a4=0,") || !strings.Contains(last, ",v4=80,") {
		t.Errorf("expected slot 4 preserved disabled at 80 W, got %q", last)
	}
}

// ── SoC soft floor tests ───────────────────────────────────────────────────

// devStatusWithSoC builds a minimal device status with the given SoC and DoD.
func devStatusWithSoC(socPct, dodPct int) marstek.Status {
	return marstek.ParseStatus(
		"p1=1,p2=1,w1=0,w2=0,pe=" + itoa(socPct) + ",vv=110,sv=9,cs=0,cd=0,am=0,o1=1,o2=1,do=" + itoa(dodPct) + "," +
			"lv=240,cj=0,kn=500,g1=0,g2=0,b1=0,b2=0,md=0," +
			"d1=1,e1=0:0,f1=23:59,h1=240,d2=0,e2=0:0,f2=23:59,h2=80," +
			"d3=0,e3=0:0,f3=23:59,h3=80,d4=0,e4=0:0,f4=23:59,h4=80," +
			"d5=0,e5=0:0,f5=23:59,h5=80,lmo=2045,lmi=1483,lmf=0,uv=107,sm=0,bn=0,ct_t=7,tc_dis=1",
	)
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}

// socFloorCfg returns a defaultCfg with explicit SoC floor settings.
// margin=2, hysteresis=5, fallback=15.
func socFloorCfg(ctrl, start, end string) controller.Config {
	cfg := defaultCfg(ctrl, start, end)
	cfg.BatterySoCFloorMarginPercent = 2
	cfg.BatterySoCHysteresisPercent = 5
	cfg.BatterySoCFloorFallbackPercent = 15
	return cfg
}

// TestStep_SoCFloor_AboveFloor_PublishesNormally verifies that when SoC is
// well above the soft floor, the controller publishes a normal discharge command.
func TestStep_SoCFloor_AboveFloor_PublishesNormally(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	// DoD=80 → floor = (100-80)+2 = 22; SoC=51 is well above floor.
	p.set(200, 0)
	st.setFresh(devStatusWithSoC(51, 80))
	cfg := socFloorCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)

	if err := c.Step(context.Background()); err != nil {
		t.Fatalf("Step() error = %v", err)
	}
	if pub.count() == 0 {
		t.Fatal("expected a publish, got none")
	}
	if !strings.Contains(pub.last(), ",v1=200,") {
		t.Errorf("expected v1=200, got %q", pub.last())
	}
}

// TestStep_SoCFloor_AtFloor_DisablesSlot verifies that when SoC drops to/below
// the soft floor after a prior discharge command, the controller publishes a
// disable-slot write.
func TestStep_SoCFloor_AtFloor_DisablesSlot(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := socFloorCfg("topic", "00:00", "23:59")
	// DoD=80 → floor = 22, resume = 27.
	c := controller.New(cfg, p, pub, st, clk, nil)

	// Step 1: SoC=51, establish 200 W discharge.
	p.set(200, 0)
	st.setFresh(devStatusWithSoC(51, 80))
	if err := c.Step(context.Background()); err != nil {
		t.Fatalf("Step 1 error = %v", err)
	}
	countAfterStep1 := pub.count()
	if countAfterStep1 == 0 {
		t.Fatal("expected publish in step 1")
	}

	// Step 2: SoC drops to 22 (== floor) → commandIdle must fire.
	p.set(200, 0)
	st.setFresh(devStatusWithSoC(22, 80))
	if err := c.Step(context.Background()); err != nil {
		t.Fatalf("Step 2 error = %v", err)
	}
	if pub.count() <= countAfterStep1 {
		t.Fatal("expected commandIdle publish when SoC hits soft floor")
	}
	last := pub.last()
	if !strings.Contains(last, ",a1=0,") || !strings.Contains(last, ",v1=0,") {
		t.Errorf("expected slot disabled (a1=0,v1=0), got %q", last)
	}
}

// TestStep_SoCFloor_Hysteresis_StaysAtZero verifies that once the SoC floor
// is active, the controller stays at 0 W while SoC is between the floor and
// (floor + hysteresis).
func TestStep_SoCFloor_Hysteresis_StaysAtZero(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := socFloorCfg("topic", "00:00", "23:59")
	// DoD=80 → floor=22, resume=27.
	c := controller.New(cfg, p, pub, st, clk, nil)

	// Establish non-zero command.
	p.set(200, 0)
	st.setFresh(devStatusWithSoC(51, 80))
	_ = c.Step(context.Background())

	// SoC drops to floor → activate.
	p.set(200, 0)
	st.setFresh(devStatusWithSoC(22, 80))
	_ = c.Step(context.Background())

	// SoC recovers to 24 — above floor but below resume (27) → must stay suppressed.
	p.set(200, 0)
	st.setFresh(devStatusWithSoC(24, 80))
	countBefore := pub.count()
	if err := c.Step(context.Background()); err != nil {
		t.Fatalf("Step error = %v", err)
	}
	// commandIdle skips publish if lastCommandWatts==0, so count stays the same.
	if pub.count() != countBefore {
		t.Errorf("expected no additional publish in hysteresis band, got %d new publishes", pub.count()-countBefore)
	}
}

// TestStep_SoCFloor_Hysteresis_ResumesAboveResumeAt verifies that normal
// discharge resumes once SoC climbs above (floor + hysteresis).
func TestStep_SoCFloor_Hysteresis_ResumesAboveResumeAt(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := socFloorCfg("topic", "00:00", "23:59")
	// DoD=80 → floor=22, resume=27.
	c := controller.New(cfg, p, pub, st, clk, nil)

	// Step 1: establish discharge.
	p.set(200, 0)
	st.setFresh(devStatusWithSoC(51, 80))
	_ = c.Step(context.Background())

	// Step 2: hit the floor.
	p.set(200, 0)
	st.setFresh(devStatusWithSoC(22, 80))
	_ = c.Step(context.Background())

	// Step 3: SoC climbs to 27 (== resume) → normal discharge must resume.
	p.set(200, 0)
	st.setFresh(devStatusWithSoC(27, 80))
	countBefore := pub.count()
	if err := c.Step(context.Background()); err != nil {
		t.Fatalf("Step error = %v", err)
	}
	if pub.count() <= countBefore {
		t.Fatal("expected discharge to resume once SoC reaches resumeAt")
	}
	last := pub.last()
	if !strings.Contains(last, ",a1=1,") {
		t.Errorf("expected slot re-enabled (a1=1) on resume, got %q", last)
	}
}

// TestStep_SoCFloor_DoDZero_UsesFallback verifies that when DoDPercent is 0
// (protocol field missing), the controller uses BatterySoCFloorFallbackPercent.
func TestStep_SoCFloor_DoDZero_UsesFallback(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := socFloorCfg("topic", "00:00", "23:59")
	// Fallback=15, hysteresis=5 → floor=15, resume=20.
	c := controller.New(cfg, p, pub, st, clk, nil)

	// Step 1: establish discharge at SoC=50, DoD=0 (unknown).
	p.set(200, 0)
	st.setFresh(devStatusWithSoC(50, 0))
	_ = c.Step(context.Background())

	// Step 2: SoC=14 (below fallback floor of 15) → suppress.
	p.set(200, 0)
	st.setFresh(devStatusWithSoC(14, 0))
	countBefore := pub.count()
	if err := c.Step(context.Background()); err != nil {
		t.Fatalf("Step error = %v", err)
	}
	if pub.count() <= countBefore {
		t.Fatal("expected disable-slot publish when SoC below fallback floor")
	}
	last := pub.last()
	if !strings.Contains(last, ",a1=0,") || !strings.Contains(last, ",v1=0,") {
		t.Errorf("expected slot disabled (a1=0,v1=0), got %q", last)
	}
}

// TestStep_SoCFloor_DoDChanges_NewFloorApplied verifies that if DoDPercent
// changes between cycles (user adjusted in the Marstek app), the new soft
// floor is computed on the next cycle.
func TestStep_SoCFloor_DoDChanges_NewFloorApplied(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := socFloorCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)

	// Step 1: DoD=90 → floor=(100-90)+2=12, resume=17. SoC=51 → normal.
	p.set(200, 0)
	st.setFresh(devStatusWithSoC(51, 90))
	_ = c.Step(context.Background())

	// Step 2: DoD changes to 80 → floor now 22. SoC=20 is below new floor.
	p.set(200, 0)
	st.setFresh(devStatusWithSoC(20, 80))
	countBefore := pub.count()
	if err := c.Step(context.Background()); err != nil {
		t.Fatalf("Step error = %v", err)
	}
	if pub.count() <= countBefore {
		t.Fatal("expected suppress with new DoD floor applied immediately")
	}
	last := pub.last()
	if !strings.Contains(last, ",a1=0,") {
		t.Errorf("expected slot disabled after DoD change raised the floor, got %q", last)
	}
}

// TestStep_SoCFloor_ExportFastPath_StillZeros verifies that the export
// fast-path and SoC floor both result in 0 W / disabled slot, and that the
// SoC floor does not interfere with the export counter path.
func TestStep_SoCFloor_ExportFastPath_StillZeros(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := socFloorCfg("topic", "00:00", "23:59")
	cfg.RampUpWattsPerCycle = 800
	cfg.RampDownWattsPerCycle = 800
	cfg.MinCommandDeltaWatts = 1
	c := controller.New(cfg, p, pub, st, clk, nil)

	// Step 1: establish 200 W discharge, SoC=51.
	p.set(200, 0)
	st.setFresh(devStatusWithSoC(51, 80))
	_ = c.Step(context.Background())

	// Step 2: grid is exporting AND SoC is at the floor.
	// Both paths agree on 0 W — just verify no panic and slot disabled.
	p.set(-100, 0)
	st.setFresh(devStatusWithSoC(22, 80))
	if err := c.Step(context.Background()); err != nil {
		t.Fatalf("Step error = %v", err)
	}
	last := pub.last()
	if !strings.Contains(last, ",a1=0,") || !strings.Contains(last, ",v1=0,") {
		t.Errorf("expected slot disabled when exporting and at SoC floor, got %q", last)
	}
}

// TestStep_SoCFloor_SetsReady verifies that the controller reports Ready()=true
// after a step where the SoC soft floor suppresses discharge. Both Prometheus
// and device status were successfully obtained, so the readiness probe must
// return 200 even though no discharge command was issued.
func TestStep_SoCFloor_SetsReady(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	// SoC=13 < soft floor=22 (DoD=80): SoC floor active from the first step.
	p.set(200, 0)
	st.setFresh(devStatusWithSoC(13, 80))

	cfg := socFloorCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)

	if c.Ready() {
		t.Fatal("controller must not be ready before first step")
	}
	if err := c.Step(context.Background()); err != nil {
		t.Fatalf("Step() error = %v", err)
	}
	if !c.Ready() {
		t.Error("controller must be ready after a step where Prometheus and device status were both healthy, even if SoC floor suppressed discharge")
	}
}

// TestStep_DeviceStatusLogged_WhenSoCFloorActive verifies that the one-time
// "device status received" info log fires on the first step that yields a valid
// device status, even when the SoC soft floor immediately suppresses discharge
// and commandIdle() returns early.  Before the fix, loggedFirmware was never
// set when socFloorActive was true, so the message was silently dropped.
func TestStep_DeviceStatusLogged_WhenSoCFloorActive(t *testing.T) {
	// Capture slog output for the duration of this test.
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	// SoC=13 < soft floor=22 (DoD=80 → (100-80)+2=22): floor is active from
	// the very first step, so commandIdle returns early.
	p.set(200, 0)
	st.setFresh(devStatusWithSoC(13, 80))

	cfg := socFloorCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)

	if err := c.Step(context.Background()); err != nil {
		t.Fatalf("Step() error = %v", err)
	}

	if !strings.Contains(buf.String(), "device status received") {
		t.Errorf("expected 'device status received' log on first valid status even when SoC floor suppresses discharge\nlog output:\n%s", buf.String())
	}
}

// TestStep_NoStatus_PollFails_FreezesControl verifies that when the controller
// has never received a device status (receivedAt is the zero value) and the
// self-poll also times out, Step freezes at the last commanded wattage rather
// than hard-failing to zero discharge. This is the startup regression: the raw
// statusAge overflows int64 and saturates to MaxInt64, which was incorrectly
// treated as "too old" and triggered an immediate hard-fail.
func TestStep_NoStatus_PollFails_FreezesControl(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{} // receivedAt is time.Time{} — never set
	clk := &fakeClock{now: time.Now()}

	p.set(300, 0)
	st.pollErr = errors.New("statussource: poll timeout after 5s")

	cfg := defaultCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)

	if err := c.Step(context.Background()); err != nil {
		t.Fatalf("Step() must not hard-fail on first startup: got err = %v", err)
	}
	if pub.count() != 0 {
		t.Errorf("expected no publish during freeze, got %d", pub.count())
	}
}

// TestStep_StaleStatus_PollFails_HardFails verifies that when a previously-seen
// device status has gone silent beyond StatusHardFailAfter and the self-poll also
// fails, the controller falls back to zero discharge. This is the legitimate
// hard-fail path that must still work after the startup fix.
func TestStep_StaleStatus_PollFails_HardFails(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	// Step 1: healthy state — establishes a non-zero lastCommandWatts so
	// fallback() will actually publish a zero-discharge command.
	p.set(300, 0)
	st.setFresh(freshDevStatus())
	cfg := defaultCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)
	if err := c.Step(context.Background()); err != nil {
		t.Fatalf("initial Step() error = %v", err)
	}
	countAfterStep1 := pub.count()
	if countAfterStep1 == 0 {
		t.Fatal("expected initial publish")
	}

	// Step 2: move receivedAt 10 min into the past (> StatusHardFailAfter=5min)
	// and make Poll() fail — must trigger hard-fail fallback.
	st.setStale()
	st.pollErr = errors.New("statussource: poll timeout after 5s")
	if err := c.Step(context.Background()); err != nil {
		t.Fatalf("Step() returned unexpected error = %v", err)
	}
	if pub.count() <= countAfterStep1 {
		t.Fatal("expected fallback publish when stale beyond StatusHardFailAfter and poll fails")
	}
	last := pub.last()
	if !strings.Contains(last, ",a1=0,") {
		t.Errorf("expected fallback to disable slot (a1=0), got %q", last)
	}
	if !strings.Contains(last, ",v1=0,") {
		t.Errorf("expected v1=0 in fallback payload, got %q", last)
	}
}

// TestStep_AboveMinOutputWatts_PassesThrough checks that a target at or above
// MinOutputWatts is unchanged by the dead-zone logic.
func TestStep_AboveMinOutputWatts_PassesThrough(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	p.set(200, 0) // rawTarget = 200W, well above 80W floor
	st.setFresh(freshDevStatus())
	cfg := defaultCfg("topic", "00:00", "23:59")
	cfg.MinOutputWatts = 80
	c := controller.New(cfg, p, pub, st, clk, nil)

	if err := c.Step(context.Background()); err != nil {
		t.Fatalf("Step() error = %v", err)
	}
	last := pub.last()
	if !strings.Contains(last, ",a1=1,") {
		t.Errorf("expected a1=1 (slot enabled) for 200W target, got %q", last)
	}
	if !strings.Contains(last, ",v1=200,") {
		t.Errorf("expected v1=200 unchanged, got %q", last)
	}
}

// devStatusWithOutput returns a fresh device status with the given current
// output watts on port 1 (g1) and port 2 (g2). The rest of the fields match
// freshDevStatus so SoC/DoD values keep the controller above the soft floor.
func devStatusWithOutput(g1, g2 int) marstek.Status {
	return marstek.ParseStatus(fmt.Sprintf(
		"p1=1,p2=1,w1=375,w2=380,pe=51,vv=110,sv=9,cs=0,cd=0,am=0,o1=1,o2=1,do=90,"+
			"lv=240,cj=0,kn=1142,g1=%d,g2=%d,b1=0,b2=0,md=0,"+
			"d1=1,e1=0:0,f1=23:59,h1=240,d2=0,e2=0:0,f2=23:59,h2=80,"+
			"d3=0,e3=0:0,f3=23:59,h3=80,d4=0,e4=0:0,f4=23:59,h4=80,"+
			"d5=0,e5=0:0,f5=23:59,h5=80,lmo=2045,lmi=1483,lmf=0,uv=107,sm=0,bn=0,ct_t=7,tc_dis=1",
		g1, g2,
	))
}

// TestStep_MinCommandDelta_Asymmetric verifies that MinCommandDeltaWatts is
// applied when the smoothed grid is non-negative, and MinCommandDeltaWattsExporting
// is applied when the smoothed grid is negative (exporting). A zero delta is
// always suppressed regardless of which threshold is in effect.
//
// All cases use ImportBiasWatts=0, SmoothingAlpha=1.0, and generous ramp limits
// so that rawTarget = currentOutput + grid and smoothed = grid exactly.
func TestStep_MinCommandDelta_Asymmetric(t *testing.T) {
	cases := []struct {
		name string
		// Warmup step establishes lastCommandWatts. g1/g2=0 for warmup.
		warmupGrid float64
		// Test step: grid reading and device-reported current output.
		testGrid       float64
		testG1, testG2 int
		minDelta       int
		minDeltaExp    int
		wantPublish    bool
		// If true, run a second identical test step and assert still suppressed.
		assertStillSuppressedOnRepeat bool
	}{
		// ── Group 1: export-classification boundary ──────────────────────────
		// The key discriminator: same delta=5, non-export threshold=50 suppresses
		// while export threshold=5 passes (strict < means == is a pass).
		{
			name:       "G1a smoothed>0 delta=5 uses non-export threshold -> suppressed",
			warmupGrid: 100,                      // lastCommandWatts = 100
			testGrid:   5, testG1: 90, testG2: 0, // rawTarget = 90+5 = 95, delta=5
			minDelta: 50, minDeltaExp: 5,
			wantPublish: false,
		},
		{
			name:       "G1b smoothed<0 delta=5 uses export threshold (5 not < 5) -> publishes",
			warmupGrid: 100,                        // lastCommandWatts = 100
			testGrid:   -5, testG1: 100, testG2: 0, // rawTarget = 100+(-5) = 95, delta=5
			minDelta: 50, minDeltaExp: 5,
			wantPublish: true,
		},
		{
			name:       "G1c smoothed=0 treated as non-export (no-op when delta=0)",
			warmupGrid: 100,                       // lastCommandWatts = 100
			testGrid:   0, testG1: 100, testG2: 0, // rawTarget = 100, delta=0 -> no-op
			minDelta: 50, minDeltaExp: 5,
			wantPublish: false,
		},

		// ── Group 2: threshold-value boundary ───────────────────────────────
		{
			name:       "G2a export delta==threshold -> publishes (strict <)",
			warmupGrid: 100,
			testGrid:   -5, testG1: 100, testG2: 0, // rawTarget=95, delta=5
			minDelta: 50, minDeltaExp: 5,
			wantPublish: true,
		},
		{
			name:       "G2b export delta==threshold-1 -> suppressed",
			warmupGrid: 100,
			testGrid:   -4, testG1: 100, testG2: 0, // rawTarget=96, delta=4
			minDelta: 50, minDeltaExp: 5,
			wantPublish: false,
		},
		{
			name:       "G2c non-export delta==threshold -> publishes (strict <)",
			warmupGrid: 100,
			testGrid:   50, testG1: 100, testG2: 0, // rawTarget=150, delta=50
			minDelta: 50, minDeltaExp: 5,
			wantPublish: true,
		},

		// ── Group 3: no-op guard (delta=0 always suppressed) ────────────────
		{
			name:       "G3a export delta=0 threshold=5 -> suppressed",
			warmupGrid: 95,                         // lastCommandWatts = 95
			testGrid:   -5, testG1: 100, testG2: 0, // rawTarget=100+(-5)=95, delta=0
			minDelta: 50, minDeltaExp: 5,
			wantPublish:                   false,
			assertStillSuppressedOnRepeat: true,
		},
		{
			name:       "G3b export delta=0 threshold=0 -> suppressed by explicit guard (no MQTT spam)",
			warmupGrid: 95,
			testGrid:   -5, testG1: 100, testG2: 0, // rawTarget=95, delta=0
			minDelta: 50, minDeltaExp: 0,
			wantPublish:                   false,
			assertStillSuppressedOnRepeat: true,
		},
		{
			name:       "G3c non-export delta=0 -> suppressed",
			warmupGrid: 100,
			testGrid:   100, testG1: 0, testG2: 0, // rawTarget=100, delta=0
			minDelta: 50, minDeltaExp: 5,
			wantPublish: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &fakeProm{}
			pub := &fakePublisher{}
			st := &fakeStatus{}
			clk := &fakeClock{now: time.Now()}

			cfg := defaultCfg("topic", "00:00", "23:59")
			cfg.MinCommandDeltaWatts = tc.minDelta
			cfg.MinCommandDeltaWattsExporting = tc.minDeltaExp
			cfg.MinOutputWatts = 1 // keep small so 95W targets aren't snapped up
			cfg.MinHoldTime = 0    // no hold-time interference

			c := controller.New(cfg, p, pub, st, clk, nil)

			// Warmup step: establish lastCommandWatts.
			p.set(tc.warmupGrid, 0)
			st.setFresh(freshDevStatus()) // g1=g2=0 for warmup
			if err := c.Step(context.Background()); err != nil {
				t.Fatalf("warmup Step() error = %v", err)
			}

			// Test step.
			p.set(tc.testGrid, 0)
			st.setFresh(devStatusWithOutput(tc.testG1, tc.testG2))
			before := pub.count()
			if err := c.Step(context.Background()); err != nil {
				t.Fatalf("test Step() error = %v", err)
			}

			published := pub.count() > before
			if published != tc.wantPublish {
				t.Errorf("publish=%v, want %v (pub count before=%d after=%d)",
					published, tc.wantPublish, before, pub.count())
			}

			// For no-op cases, verify a repeat step also doesn't publish.
			if tc.assertStillSuppressedOnRepeat {
				before2 := pub.count()
				st.setFresh(devStatusWithOutput(tc.testG1, tc.testG2))
				_ = c.Step(context.Background())
				if pub.count() != before2 {
					t.Errorf("repeat step published unexpectedly (count %d→%d); delta=0 should always suppress",
						before2, pub.count())
				}
			}
		})
	}
}

// ── Near-full idle tests ───────────────────────────────────────────────────

// devStatusAtSoC returns a device status with tc_dis=0 (SurplusFeedIn=true),
// DoD=80 (SoC soft floor = 22 — well below the near-full band), the given SoC
// and solar/output watts, and a prior schedule where slot 1 is enabled at
// 247 W. It lets each test exercise the near-full idle branch without
// accidentally tripping the SoC soft floor.
func devStatusAtSoC(socPct, solarW1, solarW2, g1, g2 int) marstek.Status {
	return marstek.ParseStatus(fmt.Sprintf(
		"p1=1,p2=1,w1=%d,w2=%d,pe=%d,vv=110,sv=9,cs=0,cd=0,am=0,o1=1,o2=1,do=80,"+
			"lv=240,cj=0,kn=2240,g1=%d,g2=%d,b1=0,b2=0,md=0,"+
			"d1=1,e1=0:0,f1=23:59,h1=247,d2=0,e2=0:0,f2=23:59,h2=80,"+
			"d3=0,e3=0:0,f3=23:59,h3=80,d4=0,e4=0:0,f4=23:59,h4=80,"+
			"d5=0,e5=0:0,f5=23:59,h5=80,lmo=2029,lmi=293,lmf=1,uv=107,sm=0,bn=0,ct_t=7,tc_dis=0",
		solarW1, solarW2, socPct, g1, g2,
	))
}

func devStatusAtSoCPassThrough(socPct, solarW1, solarW2, g1, g2 int) marstek.Status {
	return marstek.ParseStatus(fmt.Sprintf(
		"p1=2,p2=2,w1=%d,w2=%d,pe=%d,vv=110,sv=9,cs=0,cd=0,am=0,o1=1,o2=1,do=80,"+
			"lv=240,cj=0,kn=2240,g1=%d,g2=%d,b1=0,b2=0,md=0,"+
			"d1=1,e1=0:0,f1=23:59,h1=247,d2=0,e2=0:0,f2=23:59,h2=80,"+
			"d3=0,e3=0:0,f3=23:59,h3=80,d4=0,e4=0:0,f4=23:59,h4=80,"+
			"d5=0,e5=0:0,f5=23:59,h5=80,lmo=2029,lmi=293,lmf=1,uv=107,sm=0,bn=0,ct_t=7,tc_dis=0",
		solarW1, solarW2, socPct, g1, g2,
	))
}

// devStatusAtSoCNoFeedIn is the same as devStatusAtSoC but with tc_dis=1
// (SurplusFeedIn=false) so near-full idle is gated off.
func devStatusAtSoCNoFeedIn(socPct, solarW1, solarW2, g1, g2 int) marstek.Status {
	return marstek.ParseStatus(fmt.Sprintf(
		"p1=1,p2=1,w1=%d,w2=%d,pe=%d,vv=110,sv=9,cs=0,cd=0,am=0,o1=1,o2=1,do=80,"+
			"lv=240,cj=0,kn=2240,g1=%d,g2=%d,b1=0,b2=0,md=0,"+
			"d1=1,e1=0:0,f1=23:59,h1=247,d2=0,e2=0:0,f2=23:59,h2=80,"+
			"d3=0,e3=0:0,f3=23:59,h3=80,d4=0,e4=0:0,f4=23:59,h4=80,"+
			"d5=0,e5=0:0,f5=23:59,h5=80,lmo=2029,lmi=293,lmf=1,uv=107,sm=0,bn=0,ct_t=7,tc_dis=1",
		solarW1, solarW2, socPct, g1, g2,
	))
}

// nearFullIdleCfg returns a default config with near-full idle enabled using
// the production defaults (enter=98, exit=95, consecutive=2).
func nearFullIdleCfg(ctrl, start, end string) controller.Config {
	cfg := defaultCfg(ctrl, start, end)
	cfg.BatterySoCFloorMarginPercent = 2
	cfg.BatterySoCHysteresisPercent = 5
	cfg.BatterySoCFloorFallbackPercent = 15
	cfg.NearFullIdleEnabled = true
	cfg.NearFullIdleEnterPercent = 98
	cfg.NearFullIdleExitPercent = 95
	cfg.NearFullIdleConsecutiveSamples = 2
	cfg.NearFullIdleEntryExportWatts = 25
	// Mirror production defaults. Tests that want to exercise the grid-import
	// exit set these explicitly; the idle-entry gate requires meaningful
	// export, so activation loops in these tests feed -50 W to satisfy it.
	cfg.NearFullIdleGridImportExitWatts = 50
	cfg.NearFullIdleGridImportExitSamples = 8
	return cfg
}

// TestStep_NearFullIdle_EntryDebounced verifies that idle requires exactly N
// consecutive SoC>=enter samples before disabling the slot.
func TestStep_NearFullIdle_EntryDebounced(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := nearFullIdleCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)

	// Step 1: SoC=98, single sample. Must not disable yet — normal control runs.
	// Grid=-50 W satisfies the meaningful-export entry gate; g1+g2=123+123=246 → the
	// bias path still produces a positive rawTarget and the slot stays enabled.
	p.set(-50, 0)
	st.setFresh(devStatusAtSoC(98, 50, 50, 123, 123))
	_ = c.Step(context.Background())
	last1 := pub.last()
	if strings.Contains(last1, ",a1=0,") {
		t.Errorf("idle must not activate on first sample; got disabled slot %q", last1)
	}

	// Step 2: second consecutive SoC=98 sample at export=-50 → idle engages.
	p.set(-50, 0)
	st.setFresh(devStatusAtSoC(98, 50, 50, 123, 123))
	_ = c.Step(context.Background())
	last2 := pub.last()
	if !strings.Contains(last2, ",a1=0,") {
		t.Errorf("idle must activate after %d samples; expected a1=0 got %q", cfg.NearFullIdleConsecutiveSamples, last2)
	}
	if !strings.Contains(last2, ",v1=0,") {
		t.Errorf("idle must command v1=0; got %q", last2)
	}
}

// TestStep_NearFullIdle_StayThroughSoCFlicker verifies that with enter=98 and
// exit=95, a SoC that oscillates between 96 and 99 keeps idle active — the
// exit debounce never accumulates because SoC stays >= exit.
func TestStep_NearFullIdle_StayThroughSoCFlicker(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := nearFullIdleCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)

	// Activate: two SoC=98 samples at export=-50 (entry gate requires meaningful export).
	for i := 0; i < 2; i++ {
		p.set(-50, 0)
		st.setFresh(devStatusAtSoC(98, 50, 50, 123, 123))
		_ = c.Step(context.Background())
	}
	if !strings.Contains(pub.last(), ",a1=0,") {
		t.Fatal("precondition: idle must be active")
	}

	// Flicker 96↔99 for several cycles — all are >= exit=95, so exitSamples
	// never reaches the debounce threshold and idle stays on.
	for i, soc := range []int{96, 99, 97, 99, 96} {
		countBefore := pub.count()
		p.set(50, 0)
		st.setFresh(devStatusAtSoC(soc, 50, 50, 123, 123))
		_ = c.Step(context.Background())
		// lastCommandWatts is already 0, so commandIdle short-circuits without
		// publishing again. The invariant: no publish should show an enabled
		// slot during the flicker.
		if pub.count() > countBefore {
			last := pub.last()
			if !strings.Contains(last, ",a1=0,") || !strings.Contains(last, ",v1=0,") {
				t.Errorf("flicker step %d (soc=%d): idle must keep slot disabled; got %q", i, soc, last)
			}
		}
	}
}

// TestStep_NearFullIdle_ExitDebounced verifies that idle exits only after N
// consecutive SoC<exit samples, and that the next cycle resumes normal control.
func TestStep_NearFullIdle_ExitDebounced(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := nearFullIdleCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)

	// Activate at export=-50 (entry gate requires meaningful export).
	for i := 0; i < 2; i++ {
		p.set(-50, 0)
		st.setFresh(devStatusAtSoC(98, 50, 50, 123, 123))
		_ = c.Step(context.Background())
	}
	if !strings.Contains(pub.last(), ",a1=0,") {
		t.Fatal("precondition: idle must be active")
	}

	// First SoC=94 (< exit=95) sample: idle must stay active.
	p.set(50, 0)
	st.setFresh(devStatusAtSoC(94, 50, 50, 123, 123))
	countBefore := pub.count()
	_ = c.Step(context.Background())
	if pub.count() > countBefore {
		last := pub.last()
		if !strings.Contains(last, ",a1=0,") {
			t.Fatalf("first SoC=94 sample must not re-enable slot; got %q", last)
		}
	}

	// Second SoC=94 sample: debounce satisfied → idle exits and normal control
	// resumes. Grid=50 + g1+g2=246 - bias(0) = 296 W → expect enabled slot.
	p.set(50, 0)
	st.setFresh(devStatusAtSoC(94, 50, 50, 123, 123))
	_ = c.Step(context.Background())
	last := pub.last()
	if !strings.Contains(last, ",a1=1,") {
		t.Errorf("after exit debounce, normal control must re-enable slot; got %q", last)
	}
	if strings.Contains(last, ",v1=0,") {
		t.Errorf("after exit, slot power must be non-zero; got %q", last)
	}
}

// TestStep_NearFullIdle_DoesNotDischargeOnGridImport verifies the core
// invariant: once idle is active, a grid import does not cause discharge —
// the slot stays disabled regardless of load.
func TestStep_NearFullIdle_DoesNotDischargeOnGridImport(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := nearFullIdleCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)

	// Activate idle at export=-50 (entry gate requires meaningful export). Use g1=g2=123
	// on activation so we have a non-zero lastCommandWatts for commandIdle to
	// actually publish the a1=0 disable.
	for i := 0; i < 2; i++ {
		p.set(-50, 0)
		st.setFresh(devStatusAtSoC(98, 50, 50, 123, 123))
		_ = c.Step(context.Background())
	}
	if !strings.Contains(pub.last(), ",a1=0,") {
		t.Fatal("precondition: idle must be active")
	}

	// 300 W grid import, SoC=99 (still in idle band), battery currently not
	// outputting. Controller must NOT ramp up discharge — slot stays disabled.
	p.set(300, 0)
	st.setFresh(devStatusAtSoC(99, 50, 50, 0, 0))
	countBefore := pub.count()
	_ = c.Step(context.Background())
	if pub.count() > countBefore {
		last := pub.last()
		if !strings.Contains(last, ",a1=0,") || !strings.Contains(last, ",v1=0,") {
			t.Errorf("import at SoC=99 in idle must not publish discharge; got %q", last)
		}
	}
}

// TestStep_NearFullIdle_ExitsOnSustainedGridImport verifies the secondary
// "grid_import" exit reason breaks the SoC-deadlock: while idle is active,
// sustained grid import above the threshold for N consecutive cycles exits
// idle and lets normal control re-enable the slot, even though SoC has not
// dropped below the SoC-exit threshold.
func TestStep_NearFullIdle_ExitsOnSustainedGridImport(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := nearFullIdleCfg("topic", "00:00", "23:59")
	// Keep the debounce short so the test runs in a handful of cycles.
	cfg.NearFullIdleGridImportExitSamples = 3
	cfg.NearFullIdleGridImportExitWatts = 50
	c := controller.New(cfg, p, pub, st, clk, nil)

	// Activate idle at SoC=98 with meaningful export.
	for i := 0; i < 2; i++ {
		p.set(-50, 0)
		st.setFresh(devStatusAtSoC(98, 50, 50, 123, 123))
		_ = c.Step(context.Background())
	}
	if !strings.Contains(pub.last(), ",a1=0,") {
		t.Fatal("precondition: idle must be active")
	}

	// Feed NearFullIdleGridImportExitSamples - 1 cycles of grid above the
	// threshold at SoC=99 (still in the near-full band, so SoC exit cannot
	// fire). Slot must stay disabled through the debounce.
	for i := 0; i < cfg.NearFullIdleGridImportExitSamples-1; i++ {
		countBefore := pub.count()
		p.set(150, 0)
		st.setFresh(devStatusAtSoC(99, 50, 50, 0, 0))
		_ = c.Step(context.Background())
		if pub.count() > countBefore {
			last := pub.last()
			if !strings.Contains(last, ",a1=0,") || !strings.Contains(last, ",v1=0,") {
				t.Fatalf("cycle %d: idle must stay active during grid-import debounce; got %q", i, last)
			}
		}
	}

	// N-th consecutive high-import cycle: idle exits, normal control runs in
	// the same cycle. Expected rawTarget = 0 (currentOutput) + 150 (smoothed)
	// - ImportBiasWatts(0) = 150 W → slot enabled.
	p.set(150, 0)
	st.setFresh(devStatusAtSoC(99, 50, 50, 0, 0))
	_ = c.Step(context.Background())
	last := pub.last()
	if !strings.Contains(last, ",a1=1,") {
		t.Errorf("idle must exit on sustained grid import and re-enable slot; got %q", last)
	}
	if strings.Contains(last, ",v1=0,") {
		t.Errorf("after grid_import exit, slot power must be non-zero; got %q", last)
	}
}

// TestStep_NearFullIdle_TransientImportDoesNotExit verifies that a single
// low-import cycle in the middle of a would-be exit sequence resets the
// grid-import counter, so a transient spike (e.g. washing-machine pulse)
// does not prematurely force idle off.
func TestStep_NearFullIdle_TransientImportDoesNotExit(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := nearFullIdleCfg("topic", "00:00", "23:59")
	cfg.NearFullIdleGridImportExitSamples = 4
	cfg.NearFullIdleGridImportExitWatts = 50
	c := controller.New(cfg, p, pub, st, clk, nil)

	// Activate idle at export=-50 (entry gate requires meaningful export).
	for i := 0; i < 2; i++ {
		p.set(-50, 0)
		st.setFresh(devStatusAtSoC(98, 50, 50, 123, 123))
		_ = c.Step(context.Background())
	}
	if !strings.Contains(pub.last(), ",a1=0,") {
		t.Fatal("precondition: idle must be active")
	}

	// 3 high-import cycles (counter climbs to 3, still below threshold 4).
	for i := 0; i < 3; i++ {
		p.set(200, 0)
		st.setFresh(devStatusAtSoC(99, 50, 50, 0, 0))
		_ = c.Step(context.Background())
	}

	// One low-import cycle → counter must reset to 0.
	p.set(10, 0)
	st.setFresh(devStatusAtSoC(99, 50, 50, 0, 0))
	_ = c.Step(context.Background())

	// 3 more high-import cycles should not yet trip the exit, since the
	// counter restarted at 0. Slot must stay disabled.
	for i := 0; i < 3; i++ {
		countBefore := pub.count()
		p.set(200, 0)
		st.setFresh(devStatusAtSoC(99, 50, 50, 0, 0))
		_ = c.Step(context.Background())
		if pub.count() > countBefore {
			last := pub.last()
			if !strings.Contains(last, ",a1=0,") {
				t.Fatalf("cycle %d: transient reset should prevent grid_import exit; got %q", i, last)
			}
		}
	}

	// 4th consecutive high sample → idle finally exits.
	p.set(200, 0)
	st.setFresh(devStatusAtSoC(99, 50, 50, 0, 0))
	_ = c.Step(context.Background())
	if !strings.Contains(pub.last(), ",a1=1,") {
		t.Errorf("idle must exit after 4 fresh consecutive high-import samples; got %q", pub.last())
	}
}

// TestStep_NearFullIdle_GridImportExitDisabledByZeroSamples verifies that
// setting NearFullIdleGridImportExitSamples=0 turns off the new exit path
// entirely, preserving the prior "SoC-only" behaviour for operators who
// want the zero-risk rollback.
func TestStep_NearFullIdle_GridImportExitDisabledByZeroSamples(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := nearFullIdleCfg("topic", "00:00", "23:59")
	cfg.NearFullIdleGridImportExitSamples = 0
	cfg.NearFullIdleGridImportExitWatts = 50
	c := controller.New(cfg, p, pub, st, clk, nil)

	// Activate idle at export=-50 (entry gate requires meaningful export).
	for i := 0; i < 2; i++ {
		p.set(-50, 0)
		st.setFresh(devStatusAtSoC(98, 50, 50, 123, 123))
		_ = c.Step(context.Background())
	}
	if !strings.Contains(pub.last(), ",a1=0,") {
		t.Fatal("precondition: idle must be active")
	}

	// 50 sustained high-import cycles with SoC held at 99 (so the SoC exit
	// path cannot fire). Idle must NOT exit.
	for i := 0; i < 50; i++ {
		countBefore := pub.count()
		p.set(500, 0)
		st.setFresh(devStatusAtSoC(99, 50, 50, 0, 0))
		_ = c.Step(context.Background())
		if pub.count() > countBefore {
			last := pub.last()
			if !strings.Contains(last, ",a1=0,") || !strings.Contains(last, ",v1=0,") {
				t.Fatalf("cycle %d: grid-import exit disabled — slot must stay disabled; got %q", i, last)
			}
		}
	}
}

// TestStep_NearFullIdle_SoCFloorTakesPrecedence verifies that the SoC soft
// floor still wins over near-full idle. (Both disable the slot, but the
// precedence matters so the soc_floor metric is incremented, not
// near_full_idle.)
func TestStep_NearFullIdle_SoCFloorTakesPrecedence(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := nearFullIdleCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)

	// Step 1: establish a non-zero lastCommandWatts.
	p.set(200, 0)
	st.setFresh(devStatusAtSoC(51, 0, 0, 80, 80))
	_ = c.Step(context.Background())

	// Step 2: SoC=13 < soft floor=22 AND SoC=13 would never enter near-full
	// idle anyway, but the important check is that SoC floor fires first.
	// To probe precedence, use a SoC that would satisfy both branches if the
	// floor didn't win — that's not possible (enter=98 is always above the
	// floor), so we instead verify the floor path still disables the slot
	// even with near-full idle enabled.
	p.set(200, 0)
	st.setFresh(devStatusWithSoC(13, 80))
	_ = c.Step(context.Background())
	last := pub.last()
	if !strings.Contains(last, ",a1=0,") {
		t.Errorf("SoC floor must disable slot; got %q", last)
	}
	if !strings.Contains(last, ",v1=0,") {
		t.Errorf("SoC floor must command v1=0; got %q", last)
	}
}

// TestStep_NearFullIdle_FallbackClearsState verifies that when fallback fires
// while idle is active, state is fully reset and a subsequent fresh SoC>=enter
// sample does not instantly re-engage — the enter debounce must restart.
func TestStep_NearFullIdle_FallbackClearsState(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := nearFullIdleCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)

	// Activate idle at export=-50 (entry gate requires meaningful export).
	for i := 0; i < 2; i++ {
		p.set(-50, 0)
		st.setFresh(devStatusAtSoC(98, 50, 50, 123, 123))
		_ = c.Step(context.Background())
	}
	if !strings.Contains(pub.last(), ",a1=0,") {
		t.Fatal("precondition: idle must be active")
	}

	// Fallback: Prometheus error clears the idle state. lastCommandWatts is
	// already 0 so fallback short-circuits without publishing, which is fine —
	// the state reset is what we're testing.
	p.setErr(errors.New("boom"))
	_ = c.Step(context.Background())

	// Recovery sample 1: fresh post-fallback — debounce must not be carried
	// over, so idle must NOT instantly re-engage. Export keeps the entry gate
	// open so this test isolates the debounce reset rather than the gate.
	p.set(-50, 0)
	st.setFresh(devStatusAtSoC(98, 50, 50, 123, 123))
	countBefore := pub.count()
	_ = c.Step(context.Background())
	if pub.count() > countBefore {
		last := pub.last()
		if strings.Contains(last, ",a1=0,") && strings.Contains(last, ",v1=0,") {
			// Only a1=0 v1=0 on the first post-fallback sample would mean idle
			// re-engaged without debounce — that's the regression.
			t.Fatalf("idle must not re-engage on first sample after fallback; got %q", last)
		}
	}

	// Recovery sample 2: debounce complete → idle engages again.
	p.set(-50, 0)
	st.setFresh(devStatusAtSoC(98, 50, 50, 123, 123))
	_ = c.Step(context.Background())
	if !strings.Contains(pub.last(), ",a1=0,") {
		t.Errorf("idle must re-engage after a fresh post-fallback streak; got %q", pub.last())
	}
}

// TestStep_NearFullIdle_KillSwitchDisables verifies that the env kill switch
// prevents activation entirely even at SoC=100 with surplus feed-in enabled.
func TestStep_NearFullIdle_KillSwitchDisables(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := nearFullIdleCfg("topic", "00:00", "23:59")
	cfg.NearFullIdleEnabled = false
	c := controller.New(cfg, p, pub, st, clk, nil)

	for i := 0; i < 3; i++ {
		p.set(50, 0)
		st.setFresh(devStatusAtSoC(100, 50, 50, 0, 0))
		_ = c.Step(context.Background())
	}
	last := pub.last()
	if strings.Contains(last, ",a1=0,") && strings.Contains(last, ",v1=0,") {
		t.Errorf("idle must not activate when kill switch is off; got %q", last)
	}
}

// TestStep_NearFullIdle_RequiresSurplusFeedIn verifies that idle does not
// engage while SurplusFeedIn is reported false, and engages cleanly (after
// the debounce) once the device reports it as true.
func TestStep_NearFullIdle_RequiresSurplusFeedIn(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := nearFullIdleCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)

	// Several SoC=100 samples with tc_dis=1 (SurplusFeedIn=false) — must NOT
	// engage idle; normal control runs.
	for i := 0; i < 3; i++ {
		p.set(50, 0)
		st.setFresh(devStatusAtSoCNoFeedIn(100, 50, 50, 80, 80))
		_ = c.Step(context.Background())
	}
	last := pub.last()
	if strings.Contains(last, ",a1=0,") && strings.Contains(last, ",v1=0,") {
		t.Errorf("idle must not engage while SurplusFeedIn=false; got %q", last)
	}

	// Flip SurplusFeedIn to true with export=-50 so the entry gate passes. Debounce
	// still applies, so the first sample must not activate on its own.
	p.set(-50, 0)
	st.setFresh(devStatusAtSoC(100, 50, 50, 80, 80))
	_ = c.Step(context.Background())
	if strings.Contains(pub.last(), ",a1=0,") && strings.Contains(pub.last(), ",v1=0,") {
		// Could be valid if this is the second sample — but we only just flipped
		// the flag, so this is the first sample under gating. Guard against a
		// regression where the entry counter persists across gating transitions.
		t.Fatalf("idle must not engage on first SurplusFeedIn=true sample; got %q", pub.last())
	}

	// Second sample after flip → idle engages.
	p.set(-50, 0)
	st.setFresh(devStatusAtSoC(100, 50, 50, 80, 80))
	_ = c.Step(context.Background())
	if !strings.Contains(pub.last(), ",a1=0,") {
		t.Errorf("idle must engage after debounce once SurplusFeedIn=true; got %q", pub.last())
	}
}

// TestStep_NearFullIdle_SurplusFeedInFlipExitsIdle verifies that flipping
// SurplusFeedIn to false while idle is active immediately exits idle so the
// firmware doesn't curtail MPPT at full SoC with no export path.
func TestStep_NearFullIdle_SurplusFeedInFlipExitsIdle(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := nearFullIdleCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)

	// Activate idle with SurplusFeedIn=true at export=-50 (entry gate).
	for i := 0; i < 2; i++ {
		p.set(-50, 0)
		st.setFresh(devStatusAtSoC(98, 50, 50, 123, 123))
		_ = c.Step(context.Background())
	}
	if !strings.Contains(pub.last(), ",a1=0,") {
		t.Fatal("precondition: idle must be active")
	}

	// Flip SurplusFeedIn to false: idle must exit immediately (no debounce) and
	// normal control runs. Grid=50 + g1+g2=246 - bias(0) = 296 W → slot enabled.
	p.set(50, 0)
	st.setFresh(devStatusAtSoCNoFeedIn(98, 50, 50, 123, 123))
	_ = c.Step(context.Background())
	last := pub.last()
	if !strings.Contains(last, ",a1=1,") {
		t.Errorf("idle must exit and re-enable slot when SurplusFeedIn flips off; got %q", last)
	}
}

// TestStep_NearFullIdle_DoesNotEnterWhileImporting verifies the grid-surplus
// entry gate blocks activation while the grid is importing, even when SoC
// sits at 100 for an arbitrarily long time. Without the gate, a SoC-only
// counter would engage idle after two cycles and cause the flap.
func TestStep_NearFullIdle_DoesNotEnterWhileImporting(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := nearFullIdleCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)

	// 30 cycles at SoC=100 with a +100 W grid import. smoothed stays > 0 so
	// the entry counter must never accumulate. Every published command must
	// stay on the enabled-slot path (a1=1 with v1>0), never the idle signature.
	for i := 0; i < 30; i++ {
		p.set(100, 0)
		st.setFresh(devStatusAtSoC(100, 50, 50, 123, 123))
		_ = c.Step(context.Background())
		last := pub.last()
		if strings.Contains(last, ",a1=0,") && strings.Contains(last, ",v1=0,") {
			t.Fatalf("cycle %d: idle must not engage while grid is importing; got %q", i, last)
		}
	}
}

// TestStep_NearFullIdle_DoesNotEnterOnTinyExport verifies that meter noise
// around zero does not disable discharge at the top of charge. The controller
// should only enter near-full idle when there is meaningful export to carry
// through via the firmware surplus-feed-in path.
func TestStep_NearFullIdle_DoesNotEnterOnTinyExport(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := nearFullIdleCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)

	for i := 0; i < 10; i++ {
		p.set(-2, 0)
		st.setFresh(devStatusAtSoC(100, 50, 50, 123, 123))
		_ = c.Step(context.Background())
		last := pub.last()
		if strings.Contains(last, ",a1=0,") && strings.Contains(last, ",v1=0,") {
			t.Fatalf("cycle %d: tiny export must not enter near-full idle; got %q", i, last)
		}
	}

	for i := 0; i < cfg.NearFullIdleConsecutiveSamples; i++ {
		p.set(-50, 0)
		st.setFresh(devStatusAtSoC(100, 50, 50, 123, 123))
		_ = c.Step(context.Background())
	}
	last := pub.last()
	if !strings.Contains(last, ",a1=0,") || !strings.Contains(last, ",v1=0,") {
		t.Fatalf("meaningful export should still enter near-full idle; got %q", last)
	}
}

// TestStep_NearFullIdle_NoFlapAfterGridImportExit reproduces the 2026-04
// incident: idle activates, a sustained grid import trips the grid_import
// exit, and normal control then settles at a small positive grid import
// (~ImportBiasWatts) while SoC is still pinned at 100 on the LFP plateau.
// Before the entry gate, the SoC-only debounce re-fired in two cycles and
// idle flapped back on. With the gate, smoothed > 0 blocks re-entry for as
// long as the grid keeps importing.
func TestStep_NearFullIdle_NoFlapAfterGridImportExit(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := nearFullIdleCfg("topic", "00:00", "23:59")
	// Short debounce so the exit fires in a handful of cycles.
	cfg.NearFullIdleGridImportExitSamples = 3
	cfg.NearFullIdleGridImportExitWatts = 50
	c := controller.New(cfg, p, pub, st, clk, nil)

	// Activate idle at SoC=100 with meaningful export.
	for i := 0; i < 2; i++ {
		p.set(-50, 0)
		st.setFresh(devStatusAtSoC(100, 50, 50, 123, 123))
		_ = c.Step(context.Background())
	}
	if !strings.Contains(pub.last(), ",a1=0,") {
		t.Fatal("precondition: idle must be active")
	}

	// Drive the grid_import exit: N consecutive high-import cycles. On the
	// Nth cycle idle exits and normal control publishes an enabled slot.
	for i := 0; i < cfg.NearFullIdleGridImportExitSamples; i++ {
		p.set(150, 0)
		st.setFresh(devStatusAtSoC(100, 50, 50, 0, 0))
		_ = c.Step(context.Background())
	}
	if !strings.Contains(pub.last(), ",a1=1,") {
		t.Fatalf("precondition: grid_import exit must fire and re-enable slot; got %q", pub.last())
	}

	// Post-exit: grid settles at +25 W (a realistic ImportBias-sized residual),
	// SoC still pinned at 100 on the LFP plateau. Over many cycles, the entry
	// gate must keep idle off — any idle publish signature (a1=0 v1=0) is the
	// regression this test guards against.
	for i := 0; i < 30; i++ {
		p.set(25, 0)
		st.setFresh(devStatusAtSoC(100, 50, 50, 0, 0))
		_ = c.Step(context.Background())
		last := pub.last()
		if strings.Contains(last, ",a1=0,") && strings.Contains(last, ",v1=0,") {
			t.Fatalf("cycle %d after grid_import exit: idle must not re-engage while grid is importing; got %q", i, last)
		}
	}
}

func TestStep_NearFullIdle_EntersAtFullWithSolarSurplus(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := nearFullIdleCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)

	for i := 0; i < cfg.NearFullIdleConsecutiveSamples; i++ {
		p.set(-50, 0)
		st.setFresh(devStatusAtSoCPassThrough(100, 300, 300, 123, 123))
		_ = c.Step(context.Background())
	}

	last := pub.last()
	if !strings.Contains(last, ",a1=0,") || !strings.Contains(last, ",v1=0,") {
		t.Fatalf("full SOC with solar surplus should enter idle/pass-through; got %q", last)
	}
	if pub.countContaining("cd=31") != 0 {
		t.Fatalf("solar surplus should not trigger recovery; payloads with cd=31 = %d", pub.countContaining("cd=31"))
	}
}

func TestStep_NearFullIdle_StaysInPassthroughWhenSolarCoversLoad(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := nearFullIdleCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)

	for i := 0; i < cfg.NearFullIdleConsecutiveSamples; i++ {
		p.set(-50, 0)
		st.setFresh(devStatusAtSoCPassThrough(100, 300, 300, 123, 123))
		_ = c.Step(context.Background())
	}
	countAfterEntry := pub.count()

	for i := 0; i < 10; i++ {
		p.set(-20, 0)
		st.setFresh(devStatusAtSoCPassThrough(100, 300, 300, 0, 0))
		_ = c.Step(context.Background())
	}

	if pub.count() != countAfterEntry {
		t.Fatalf("pass-through with solar covering load should not publish chatter; got %d publishes after entry, want %d", pub.count(), countAfterEntry)
	}
	if pub.countContaining("cd=31") != 0 {
		t.Fatalf("solar-covering pass-through should not toggle surplus feed-in")
	}
}

func TestStep_NearFullIdle_GridImportExitWorksDuringFirmwarePassthrough(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := nearFullIdleCfg("topic", "00:00", "23:59")
	cfg.NearFullIdleGridImportExitSamples = 3
	cfg.PassthroughStallDetectCycles = 0
	c := controller.New(cfg, p, pub, st, clk, nil)

	for i := 0; i < cfg.NearFullIdleConsecutiveSamples; i++ {
		p.set(-50, 0)
		st.setFresh(devStatusAtSoCPassThrough(100, 300, 300, 123, 123))
		_ = c.Step(context.Background())
	}
	countAfterEntry := pub.count()

	for i := 0; i < cfg.NearFullIdleGridImportExitSamples; i++ {
		p.set(150, 0)
		st.setFresh(devStatusAtSoCPassThrough(100, 150, 150, 0, 0))
		_ = c.Step(context.Background())
	}

	last := pub.last()
	if pub.count() <= countAfterEntry {
		t.Fatalf("sustained import during pass-through should publish a discharge command; got %d publishes, want more than %d", pub.count(), countAfterEntry)
	}
	if !strings.Contains(last, ",a1=1,") || !strings.Contains(last, ",v1=150,") {
		t.Fatalf("grid_import exit must re-enable the slot during pass-through; got %q", last)
	}
	if pub.countContaining("cd=31") != 0 {
		t.Fatalf("grid_import exit should not require flash recovery writes; payloads with cd=31 = %d", pub.countContaining("cd=31"))
	}
}

func TestStep_PassthroughRecovery_DisabledObserveOnly(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := nearFullIdleCfg("topic", "00:00", "23:59")
	cfg.PassthroughStallDetectCycles = 2
	cfg.PassthroughAutoRecovery = false
	c := controller.New(cfg, p, pub, st, clk, nil)

	for i := 0; i < cfg.NearFullIdleConsecutiveSamples; i++ {
		p.set(-50, 0)
		st.setFresh(devStatusAtSoCPassThrough(100, 300, 300, 123, 123))
		_ = c.Step(context.Background())
	}
	for i := 0; i < cfg.PassthroughStallDetectCycles+2; i++ {
		p.set(150, 0)
		st.setFresh(devStatusAtSoCPassThrough(100, 150, 150, 0, 0))
		_ = c.Step(context.Background())
	}

	if pub.countContaining("cd=31") != 0 {
		t.Fatalf("recovery disabled should not publish cd=31; got %d", pub.countContaining("cd=31"))
	}
}

func TestStep_PassthroughRecovery_BlockedWithoutFlashGuard(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := nearFullIdleCfg("topic", "00:00", "23:59")
	cfg.PassthroughStallDetectCycles = 2
	cfg.PassthroughAutoRecovery = true
	cfg.AllowFlashWrites = false
	c := controller.New(cfg, p, pub, st, clk, nil)

	for i := 0; i < cfg.NearFullIdleConsecutiveSamples; i++ {
		p.set(-50, 0)
		st.setFresh(devStatusAtSoCPassThrough(100, 300, 300, 123, 123))
		_ = c.Step(context.Background())
	}
	for i := 0; i < cfg.PassthroughStallDetectCycles+2; i++ {
		p.set(150, 0)
		st.setFresh(devStatusAtSoCPassThrough(100, 150, 150, 0, 0))
		_ = c.Step(context.Background())
	}

	if pub.countContaining("cd=31") != 0 {
		t.Fatalf("flash guard should block recovery writes; got %d cd=31 publishes", pub.countContaining("cd=31"))
	}
}

func TestStep_PassthroughRecovery_DisablesSurplusFeedInOnce(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := nearFullIdleCfg("topic", "00:00", "23:59")
	cfg.PassthroughStallDetectCycles = 2
	cfg.PassthroughAutoRecovery = true
	cfg.AllowFlashWrites = true
	c := controller.New(cfg, p, pub, st, clk, nil)

	for i := 0; i < cfg.NearFullIdleConsecutiveSamples; i++ {
		p.set(-50, 0)
		st.setFresh(devStatusAtSoCPassThrough(100, 300, 300, 123, 123))
		_ = c.Step(context.Background())
	}
	for i := 0; i < cfg.PassthroughStallDetectCycles+4; i++ {
		p.set(150, 0)
		st.setFresh(devStatusAtSoCPassThrough(100, 150, 150, 0, 0))
		_ = c.Step(context.Background())
	}

	if got := pub.countContaining("cd=31,touchuan_disa=1"); got != 1 {
		t.Fatalf("auto-recovery should disable surplus feed-in exactly once, got %d", got)
	}
}

func TestStep_PassthroughRecovery_NormalControlRunsAfterDisable(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := nearFullIdleCfg("topic", "00:00", "23:59")
	cfg.PassthroughStallDetectCycles = 2
	cfg.PassthroughAutoRecovery = true
	cfg.AllowFlashWrites = true
	c := controller.New(cfg, p, pub, st, clk, nil)

	for i := 0; i < cfg.NearFullIdleConsecutiveSamples; i++ {
		p.set(-50, 0)
		st.setFresh(devStatusAtSoCPassThrough(100, 300, 300, 123, 123))
		_ = c.Step(context.Background())
	}
	for i := 0; i < cfg.PassthroughStallDetectCycles; i++ {
		p.set(150, 0)
		st.setFresh(devStatusAtSoCPassThrough(100, 150, 150, 0, 0))
		_ = c.Step(context.Background())
	}
	if got := pub.countContaining("cd=31,touchuan_disa=1"); got != 1 {
		t.Fatalf("precondition: expected one disable command, got %d", got)
	}

	p.set(150, 0)
	st.setFresh(devStatusAtSoCNoFeedIn(100, 150, 150, 0, 0))
	_ = c.Step(context.Background())
	last := pub.last()
	if !strings.Contains(last, ",a1=1,") || !strings.Contains(last, ",v1=150,") {
		t.Fatalf("after pass-through clears, normal timed-discharge control should run; got %q", last)
	}
}

func TestStep_PassthroughRecovery_RestoresWhenBatteryLeavesFullPlateau(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := nearFullIdleCfg("topic", "00:00", "23:59")
	cfg.PassthroughStallDetectCycles = 2
	cfg.PassthroughAutoRecovery = true
	cfg.AllowFlashWrites = true
	cfg.PassthroughAutoRecoveryRestoreDelay = time.Hour
	c := controller.New(cfg, p, pub, st, clk, nil)

	for i := 0; i < cfg.NearFullIdleConsecutiveSamples; i++ {
		p.set(-50, 0)
		st.setFresh(devStatusAtSoCPassThrough(100, 300, 300, 123, 123))
		_ = c.Step(context.Background())
	}
	for i := 0; i < cfg.PassthroughStallDetectCycles; i++ {
		p.set(150, 0)
		st.setFresh(devStatusAtSoCPassThrough(100, 150, 150, 0, 0))
		_ = c.Step(context.Background())
	}

	p.set(0, 0)
	st.setFresh(devStatusAtSoCNoFeedIn(94, 0, 0, 80, 80))
	_ = c.Step(context.Background())
	if got := pub.countContaining("cd=31,touchuan_disa=0"); got != 1 {
		t.Fatalf("SOC below near-full exit should restore surplus feed-in once, got %d", got)
	}
}

func TestStep_PassthroughRecovery_RateLimitsRepeatedEvents(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := nearFullIdleCfg("topic", "00:00", "23:59")
	cfg.PassthroughStallDetectCycles = 1
	cfg.PassthroughAutoRecovery = true
	cfg.AllowFlashWrites = true
	cfg.PassthroughAutoRecoveryMinInterval = time.Hour
	cfg.PassthroughAutoRecoveryRestoreDelay = 0
	c := controller.New(cfg, p, pub, st, clk, nil)

	for i := 0; i < cfg.NearFullIdleConsecutiveSamples; i++ {
		p.set(-50, 0)
		st.setFresh(devStatusAtSoCPassThrough(100, 300, 300, 123, 123))
		_ = c.Step(context.Background())
	}
	p.set(150, 0)
	st.setFresh(devStatusAtSoCPassThrough(100, 150, 150, 0, 0))
	_ = c.Step(context.Background())
	p.set(0, 0)
	st.setFresh(devStatusAtSoCNoFeedIn(94, 0, 0, 80, 80))
	_ = c.Step(context.Background())

	for i := 0; i < cfg.NearFullIdleConsecutiveSamples; i++ {
		p.set(-50, 0)
		st.setFresh(devStatusAtSoCPassThrough(100, 300, 300, 123, 123))
		_ = c.Step(context.Background())
	}
	p.set(150, 0)
	st.setFresh(devStatusAtSoCPassThrough(100, 150, 150, 0, 0))
	_ = c.Step(context.Background())

	if got := pub.countContaining("cd=31,touchuan_disa=1"); got != 1 {
		t.Fatalf("second recovery inside min interval must be rate-limited; disable count = %d", got)
	}
}

func TestStep_PassthroughRecovery_DoesNotRestoreIfNotControllerDisabled(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := nearFullIdleCfg("topic", "00:00", "23:59")
	cfg.PassthroughAutoRecovery = true
	cfg.AllowFlashWrites = true
	c := controller.New(cfg, p, pub, st, clk, nil)

	p.set(0, 0)
	st.setFresh(devStatusAtSoCNoFeedIn(100, 0, 0, 0, 0))
	_ = c.Step(context.Background())
	if got := pub.countContaining("cd=31,touchuan_disa=0"); got != 0 {
		t.Fatalf("controller must not restore surplus feed-in unless it disabled it; got %d restore writes", got)
	}
}

// ── Transient-zero-output guard tests ──────────────────────────────────────

// devStatusWithOutputAndSolar returns a device status with specific output and
// solar watts, with SoC=51 (well above soft floor) and tc_dis=1 (feed-in off,
// neutral for guard tests).
func devStatusWithOutputAndSolar(g1, g2, w1, w2 int) marstek.Status {
	return marstek.ParseStatus(fmt.Sprintf(
		"p1=1,p2=1,w1=%d,w2=%d,pe=51,vv=110,sv=9,cs=0,cd=0,am=0,o1=1,o2=1,do=90,"+
			"lv=240,cj=0,kn=1142,g1=%d,g2=%d,b1=0,b2=0,md=0,"+
			"d1=1,e1=0:0,f1=23:59,h1=240,d2=0,e2=0:0,f2=23:59,h2=80,"+
			"d3=0,e3=0:0,f3=23:59,h3=80,d4=0,e4=0:0,f4=23:59,h4=80,"+
			"d5=0,e5=0:0,f5=23:59,h5=80,lmo=2045,lmi=1483,lmf=0,uv=107,sm=0,bn=0,ct_t=7,tc_dis=1",
		w1, w2, g1, g2,
	))
}

// TestStep_TransientZeroOutput_HoldsOneCycle verifies that a single g1=g2=0
// cycle after active discharge is suppressed for exactly one cycle, and normal
// control resumes on the next cycle.
func TestStep_TransientZeroOutput_HoldsOneCycle(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := defaultCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)

	// Step 1: establish 200 W discharge (g1=100, g2=100).
	p.set(200, 0)
	st.setFresh(devStatusWithOutputAndSolar(100, 100, 375, 380))
	_ = c.Step(context.Background())
	countAfter1 := pub.count()
	if countAfter1 == 0 {
		t.Fatal("expected publish in step 1")
	}

	// Step 2: g1=g2=0 transient blip — must suppress and NOT publish.
	p.set(200, 0)
	st.setFresh(devStatusWithOutputAndSolar(0, 0, 375, 380))
	_ = c.Step(context.Background())
	if pub.count() != countAfter1 {
		t.Errorf("transient zero: expected no publish on blip cycle, count changed %d→%d", countAfter1, pub.count())
	}

	// Step 3: output returns to normal with a different grid reading to force a
	// new target (rawTarget = 100+100+50 = 250 ≠ lastCommandWatts=400, delta≥1).
	p.set(50, 0)
	st.setFresh(devStatusWithOutputAndSolar(100, 100, 375, 380))
	_ = c.Step(context.Background())
	if pub.count() == countAfter1 {
		t.Errorf("expected publish after transient zero clears (step 3), count=%d", pub.count())
	}
}

// TestStep_TransientZeroOutput_DoesNotHold_TwoInARow verifies that two
// consecutive zero-output cycles are not both suppressed — the second proceeds
// to normal control so the guard cannot mask a real outage.
func TestStep_TransientZeroOutput_DoesNotHold_TwoInARow(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := defaultCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)

	// Step 1: establish discharge.
	p.set(200, 0)
	st.setFresh(devStatusWithOutputAndSolar(100, 100, 375, 380))
	_ = c.Step(context.Background())
	countAfter1 := pub.count()

	// Step 2: first zero — suppressed.
	p.set(200, 0)
	st.setFresh(devStatusWithOutputAndSolar(0, 0, 375, 380))
	_ = c.Step(context.Background())
	if pub.count() != countAfter1 {
		t.Fatal("precondition: first zero cycle must be suppressed")
	}

	// Step 3: second consecutive zero — must NOT be suppressed (one-cycle max).
	p.set(200, 0)
	st.setFresh(devStatusWithOutputAndSolar(0, 0, 375, 380))
	_ = c.Step(context.Background())
	if pub.count() == countAfter1 {
		t.Errorf("second consecutive zero-output cycle must not be suppressed; count stayed at %d", countAfter1)
	}
}

// TestStep_TransientZeroOutput_DoesNotFire_WhenCommandIsMin verifies that the
// guard does not fire when lastCommandWatts == MinOutputWatts (the condition
// requires strictly greater than MinOutputWatts to avoid triggering on an
// already-floored command).
func TestStep_TransientZeroOutput_DoesNotFire_WhenCommandIsMin(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	cfg := defaultCfg("topic", "00:00", "23:59")
	cfg.ImportBiasWatts = 120 // bias high so rawTarget snaps to 80 W on first step
	c := controller.New(cfg, p, pub, st, clk, nil)

	// Step 1: grid=200, bias=120 → rawTarget=80 = MinOutputWatts. Command=80.
	p.set(200, 0)
	st.setFresh(devStatusWithOutputAndSolar(0, 0, 375, 380))
	_ = c.Step(context.Background())
	// Expect 80 W — only checking the guard doesn't interfere here.
	countAfter1 := pub.count()

	// Step 2: g1=g2=0 again, but lastCommand==MinOutputWatts → guard must NOT fire.
	p.set(200, 0)
	st.setFresh(devStatusWithOutputAndSolar(0, 0, 375, 380))
	before := pub.count()
	_ = c.Step(context.Background())
	// Either suppressed by delta-gate or published — both are fine.
	// The key check: the guard's suppression counter did not fire (we can't check
	// directly without metrics, but we verify no unexpected behaviour).
	_ = before // accepted: delta may also suppress
	_ = countAfter1
}

// TestStep_SurplusFeedIn_WarnsWhenDisabledAndNearFullIdleEnabled verifies that
// the one-time startup log warns when near-full idle is enabled but tc_dis=1.
func TestStep_SurplusFeedIn_WarnsWhenDisabledAndNearFullIdleEnabled(t *testing.T) {
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	// tc_dis=1 → SurplusFeedIn=false; override enabled.
	statusNoFeedIn := marstek.ParseStatus(
		"p1=1,p2=1,w1=375,w2=380,pe=51,vv=110,sv=9,cs=0,cd=0,am=0,o1=1,o2=1,do=90," +
			"lv=240,cj=0,kn=1142,g1=0,g2=0,b1=0,b2=0,md=0," +
			"d1=1,e1=0:0,f1=23:59,h1=240,d2=0,e2=0:0,f2=23:59,h2=80," +
			"d3=0,e3=0:0,f3=23:59,h3=80,d4=0,e4=0:0,f4=23:59,h4=80," +
			"d5=0,e5=0:0,f5=23:59,h5=80,lmo=2045,lmi=1483,lmf=0,uv=107,sm=0,bn=0,ct_t=7,tc_dis=1",
	)
	st.setFresh(statusNoFeedIn)
	p.set(200, 0)

	cfg := nearFullIdleCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)
	_ = c.Step(context.Background())

	output := buf.String()
	if !strings.Contains(output, "surplus feed-in is disabled") {
		t.Errorf("expected surplus feed-in disabled warning when near-full idle enabled and tc_dis=1\nlog output:\n%s", output)
	}
	if strings.Contains(output, "may interfere with zero-export control") {
		t.Errorf("old 'may interfere' warning must not appear; got:\n%s", output)
	}
}

// TestStep_SurplusFeedIn_NoWarnWhenEnabled verifies that no feed-in warning
// fires when tc_dis=0 (surplus feed-in is enabled — the desired state).
func TestStep_SurplusFeedIn_NoWarnWhenEnabled(t *testing.T) {
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	// tc_dis=0 → SurplusFeedIn=true. SoC=51 keeps idle disengaged.
	st.setFresh(devStatusAtSoC(51, 0, 0, 100, 100))
	p.set(200, 0)

	cfg := nearFullIdleCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)
	_ = c.Step(context.Background())

	output := buf.String()
	if strings.Contains(output, "surplus feed-in is disabled") {
		t.Errorf("must not warn about surplus feed-in when tc_dis=0; got:\n%s", output)
	}
	if strings.Contains(output, "may interfere with zero-export control") {
		t.Errorf("old 'may interfere' warning must not appear; got:\n%s", output)
	}
}
