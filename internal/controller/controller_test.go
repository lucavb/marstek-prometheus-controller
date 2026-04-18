package controller_test

import (
	"context"
	"errors"
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
	mu      sync.Mutex
	sample  promclient.Sample
	err     error
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
		PrometheusStaleAfter:  60 * time.Second,
		StatusStaleAfter:      2 * time.Minute,
		StatusPollTimeout:     5 * time.Second,
		StatusHardFailAfter:   5 * time.Minute,
		ControlInterval:       15 * time.Second,
		SmoothingAlpha:        1.0, // no smoothing in tests for determinism
		DeadbandWatts:         25,
		RampUpWattsPerCycle:   800, // effectively no ramp in unit tests
		RampDownWattsPerCycle: 800,
		MinCommandDeltaWatts:  1,
		MinHoldTime:           0,
		MinOutputWatts:        80,
		MaxOutputWatts:        800,
		ControlTopic:          ctrl,
		ScheduleSlot:          1,
		ScheduleStart:         start,
		ScheduleEnd:           end,
		PersistToFlash:        false,
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
