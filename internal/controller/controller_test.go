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

func defaultCfg(ctrl, start, end string) controller.Config {
	return controller.Config{
		PrometheusStaleAfter:              60 * time.Second,
		StatusStaleAfter:                  2 * time.Minute,
		StatusPollTimeout:                 5 * time.Second,
		StatusHardFailAfter:               5 * time.Minute,
		ControlInterval:                   15 * time.Second,
		SmoothingAlpha:                    1.0,
		DeadbandWatts:                     25,
		RampUpWattsPerCycle:               800,
		RampDownWattsPerCycle:             800,
		MinCommandDeltaWatts:              1,
		MinCommandDeltaWattsExporting:     0,
		MinHoldTime:                       0,
		MinOutputWatts:                    80,
		MaxOutputWatts:                    800,
		ControlTopic:                      ctrl,
		ScheduleSlot:                      1,
		ScheduleStart:                     start,
		ScheduleEnd:                       end,
		PersistToFlash:                    false,
		NearFullIdleEnabled:               true,
		NearFullIdleEnterPercent:          100,
		NearFullIdleConsecutiveSamples:    2,
		NearFullIdleGridImportExitWatts:   50,
		NearFullIdleGridImportExitSamples: 4,
		SurplusFeedInRecoveryMinInterval:  time.Hour,
		BatterySoCFloorMarginPercent:      2,
		BatterySoCHysteresisPercent:       5,
		BatterySoCFloorFallbackPercent:    15,
	}
}

func statusWith(socPct, solarW1, solarW2, outputW1, outputW2 int, feedIn bool) marstek.Status {
	tcDis := 1
	if feedIn {
		tcDis = 0
	}
	return marstek.ParseStatus(fmt.Sprintf(
		"p1=1,p2=1,w1=%d,w2=%d,pe=%d,vv=116,sv=6,cs=0,cd=0,am=0,o1=1,o2=1,do=85,"+
			"lv=240,cj=0,kn=2240,g1=%d,g2=%d,b1=0,b2=0,md=0,"+
			"d1=0,e1=0:0,f1=23:59,h1=240,d2=0,e2=0:0,f2=23:59,h2=80,"+
			"d3=0,e3=0:0,f3=23:59,h3=80,d4=0,e4=0:0,f4=23:59,h4=80,"+
			"d5=0,e5=0:0,f5=23:59,h5=80,lmo=2045,lmi=1483,lmf=0,uv=107,sm=0,bn=0,ct_t=7,tc_dis=%d",
		solarW1, solarW2, socPct, outputW1, outputW2, tcDis,
	))
}

func passThroughStatus(socPct, solarW1, solarW2, outputW1, outputW2 int, feedIn bool) marstek.Status {
	s := statusWith(socPct, solarW1, solarW2, outputW1, outputW2, feedIn)
	s.Solar1Mode = 2
	s.Solar2Mode = 2
	return s
}

func withControlledSlot(status marstek.Status, enabled bool, watts int) marstek.Status {
	status.Slots[0] = marstek.ReadSlot{
		Enabled: enabled,
		Start:   "00:00",
		End:     "23:59",
		Watts:   watts,
	}
	return status
}

func TestStep_NormalImportPublishesDischarge(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	p.set(200, 0)
	st.setFresh(statusWith(80, 0, 0, 0, 0, true))
	c := controller.New(defaultCfg("topic", "00:00", "23:59"), p, pub, st, clk, nil)

	if err := c.Step(context.Background()); err != nil {
		t.Fatalf("Step() error = %v", err)
	}
	if got := pub.last(); !strings.Contains(got, ",a1=1,") || !strings.Contains(got, ",v1=200,") {
		t.Fatalf("expected 200 W discharge payload, got %q", got)
	}
	if !c.Ready() {
		t.Fatal("controller should be ready")
	}
}

func TestStep_ExportFastPathStopsDischarge(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}
	cfg := defaultCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)

	p.set(300, 0)
	st.setFresh(statusWith(80, 0, 0, 0, 0, true))
	_ = c.Step(context.Background())
	clk.advance(cfg.ControlInterval)

	p.set(-500, 0)
	st.setFresh(withControlledSlot(statusWith(80, 0, 0, 300, 0, true), true, 300))
	if err := c.Step(context.Background()); err != nil {
		t.Fatalf("Step() error = %v", err)
	}
	if got := pub.last(); !strings.Contains(got, ",a1=0,") || !strings.Contains(got, ",v1=0,") {
		t.Fatalf("expected export fast-path idle payload, got %q", got)
	}
}

func TestStep_PrometheusStaleFallsBackToZero(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	p.set(200, 2*time.Minute)
	st.setFresh(withControlledSlot(statusWith(80, 0, 0, 200, 0, true), true, 200))
	c := controller.New(defaultCfg("topic", "00:00", "23:59"), p, pub, st, clk, nil)

	_ = c.Step(context.Background())
	if got := pub.last(); !strings.Contains(got, ",a1=0,") || !strings.Contains(got, ",v1=0,") {
		t.Fatalf("expected fallback zero payload, got %q", got)
	}
}

func TestStep_StatusHardFailFallsBackToZero(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{pollErr: errors.New("timeout")}
	clk := &fakeClock{now: time.Now()}
	cfg := defaultCfg("topic", "00:00", "23:59")
	st.status = withControlledSlot(statusWith(80, 0, 0, 200, 0, true), true, 200)
	st.receivedAt = clk.now.Add(-10 * time.Minute)
	p.set(200, 0)

	c := controller.New(cfg, p, pub, st, clk, nil)
	_ = c.Step(context.Background())
	if got := pub.last(); !strings.Contains(got, ",a1=0,") || !strings.Contains(got, ",v1=0,") {
		t.Fatalf("expected fallback zero payload, got %q", got)
	}
}

func TestStep_SoCFloorDisablesControlledSlot(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	p.set(200, 0)
	st.setFresh(withControlledSlot(statusWith(10, 0, 0, 200, 0, true), true, 200))
	c := controller.New(defaultCfg("topic", "00:00", "23:59"), p, pub, st, clk, nil)

	_ = c.Step(context.Background())
	if got := pub.last(); !strings.Contains(got, ",a1=0,") || !strings.Contains(got, ",v1=0,") {
		t.Fatalf("expected soc floor idle payload, got %q", got)
	}
}

func TestStep_AuthorityRemediatesChargingModeBeforeControl(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	status := statusWith(80, 0, 0, 0, 0, true)
	status.ChargingMode = 1
	p.set(200, 0)
	st.setFresh(status)
	c := controller.New(defaultCfg("topic", "00:00", "23:59"), p, pub, st, clk, nil)

	_ = c.Step(context.Background())
	if got := pub.last(); got != "cd=17,md=0" {
		t.Fatalf("expected charging-mode remediation, got %q", got)
	}
}

func TestStep_AuthorityEnablesSurplusFeedInWithFlashGuard(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}
	cfg := defaultCfg("topic", "00:00", "23:59")
	cfg.AllowFlashWrites = true

	p.set(200, 0)
	st.setFresh(statusWith(80, 0, 0, 0, 0, false))
	c := controller.New(cfg, p, pub, st, clk, nil)

	_ = c.Step(context.Background())
	if got := pub.last(); got != "cd=31,touchuan_disa=0" {
		t.Fatalf("expected surplus feed-in remediation, got %q", got)
	}
}

func TestStep_AuthorityDoesNotEnableSurplusFeedInWithoutFlashGuard(t *testing.T) {
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}

	p.set(200, 0)
	st.setFresh(statusWith(80, 0, 0, 0, 0, false))
	c := controller.New(defaultCfg("topic", "00:00", "23:59"), p, pub, st, clk, nil)

	_ = c.Step(context.Background())
	if pub.countContaining("cd=31") != 0 {
		t.Fatalf("flash guard should block surplus feed-in writes")
	}
	if !strings.Contains(buf.String(), "surplus feed-in is disabled") {
		t.Fatalf("expected surplus feed-in warning, got %s", buf.String())
	}
}

func TestStep_AuthorityRateLimitsSurplusFeedInRecovery(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}
	cfg := defaultCfg("topic", "00:00", "23:59")
	cfg.AllowFlashWrites = true
	cfg.SurplusFeedInRecoveryMinInterval = time.Hour

	p.set(200, 0)
	st.setFresh(statusWith(80, 0, 0, 0, 0, false))
	c := controller.New(cfg, p, pub, st, clk, nil)

	_ = c.Step(context.Background())
	clk.advance(cfg.ControlInterval)
	st.setFresh(statusWith(80, 0, 0, 0, 0, false))
	_ = c.Step(context.Background())
	if got := pub.countContaining("cd=31,touchuan_disa=0"); got != 1 {
		t.Fatalf("expected one rate-limited surplus remediation, got %d", got)
	}
}

func TestStep_AuthorityUnblocksOutputsAfterSustainedZeroOutput(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}
	cfg := defaultCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)

	blocked := withControlledSlot(statusWith(80, 0, 0, 0, 0, true), true, 200)
	blocked.Output1Enabled = 0
	blocked.Output2Enabled = 0
	p.set(200, 0)
	st.setFresh(blocked)
	_ = c.Step(context.Background())
	clk.advance(cfg.ControlInterval)
	st.setFresh(blocked)
	_ = c.Step(context.Background())
	clk.advance(cfg.ControlInterval)
	st.setFresh(blocked)
	_ = c.Step(context.Background())

	if got := pub.last(); got != "cd=18,md=3" {
		t.Fatalf("expected output-enable remediation, got %q", got)
	}
}

func TestStep_OutputEnableRemediationDoesNotStarveHigherSlotCommand(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}
	cfg := defaultCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)

	// First publish establishes the stale low command that the live incident got
	// stuck on.
	p.set(300, 0)
	st.setFresh(statusWith(80, 0, 0, 0, 0, true))
	if err := c.Step(context.Background()); err != nil {
		t.Fatalf("Step() initial low command error = %v", err)
	}
	if got := pub.last(); !strings.Contains(got, ",a1=1,") || !strings.Contains(got, ",v1=300,") {
		t.Fatalf("expected initial 300 W slot publish, got %q", got)
	}

	// The device still reports zero battery contribution and the old 300 W slot,
	// but demand has risen sharply. The controller must advance the slot command
	// to the new target instead of getting stuck in output-enable remediation.
	clk.advance(cfg.ControlInterval)
	blockedAt300 := withControlledSlot(statusWith(80, 0, 0, 0, 0, true), true, 300)
	blockedAt300.Output1Enabled = 0
	blockedAt300.Output2Enabled = 0
	p.set(800, 0)
	st.setFresh(blockedAt300)
	if err := c.Step(context.Background()); err != nil {
		t.Fatalf("Step() higher demand error = %v", err)
	}
	if got := pub.last(); !strings.Contains(got, ",a1=1,") || !strings.Contains(got, ",v1=800,") {
		t.Fatalf("expected higher 800 W slot publish, got %q", got)
	}
	if got := pub.countContaining("cd=18,md=3"); got != 0 {
		t.Fatalf("output-enable remediation should not fire before desired slot is applied, got %d publishes", got)
	}

	// Once the device status reflects the desired 800 W slot and still reports
	// zero battery contribution, remediation should continue to work normally.
	blockedAt800 := withControlledSlot(statusWith(80, 0, 0, 0, 0, true), true, 800)
	blockedAt800.Output1Enabled = 0
	blockedAt800.Output2Enabled = 0
	for range 2 {
		clk.advance(cfg.ControlInterval)
		p.set(800, 0)
		st.setFresh(blockedAt800)
		if err := c.Step(context.Background()); err != nil {
			t.Fatalf("Step() blocked-at-target error = %v", err)
		}
	}
	if got := pub.last(); got != "cd=18,md=3" {
		t.Fatalf("expected output-enable remediation after target slot was applied, got %q", got)
	}
}

func TestStep_NuclearRestartDisabledByDefault(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}
	cfg := defaultCfg("topic", "00:00", "23:59")
	cfg.NuclearRestartBlockedCycles = 3
	c := controller.New(cfg, p, pub, st, clk, nil)

	blocked := withControlledSlot(statusWith(80, 0, 0, 0, 0, true), true, 200)
	blocked.Output1Enabled = 0
	blocked.Output2Enabled = 0
	p.set(200, 0)
	for i := 0; i < 8; i++ {
		st.setFresh(blocked)
		if err := c.Step(context.Background()); err != nil {
			t.Fatalf("Step() error = %v", err)
		}
		clk.advance(cfg.ControlInterval)
	}
	if got := pub.countContaining("cd=10"); got != 0 {
		t.Fatalf("nuclear restart must be disabled by default, got %d restart publishes", got)
	}
}

func TestStep_NuclearRestartPublishesAfterSustainedBlockedOutput(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}
	cfg := defaultCfg("topic", "00:00", "23:59")
	cfg.NuclearRestartEnabled = true
	cfg.NuclearRestartAckWiFiRecovery = true
	cfg.NuclearRestartBlockedCycles = 4
	c := controller.New(cfg, p, pub, st, clk, nil)

	blocked := withControlledSlot(statusWith(80, 0, 0, 0, 0, true), true, 200)
	blocked.Output1Enabled = 0
	blocked.Output2Enabled = 0
	p.set(200, 0)
	for i := 0; i < 5; i++ {
		st.setFresh(blocked)
		if err := c.Step(context.Background()); err != nil {
			t.Fatalf("Step() error = %v", err)
		}
		clk.advance(cfg.ControlInterval)
	}
	if got := pub.last(); got != "cd=10" {
		t.Fatalf("expected nuclear restart payload, got %q", got)
	}
	if got := pub.countContaining("cd=18,md=3"); got == 0 {
		t.Fatalf("expected output-enable remediation before nuclear restart")
	}
}

func TestStep_NuclearRestartRequiresOutputEnableAttempt(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}
	cfg := defaultCfg("topic", "00:00", "23:59")
	cfg.NuclearRestartEnabled = true
	cfg.NuclearRestartAckWiFiRecovery = true
	cfg.NuclearRestartBlockedCycles = 1
	c := controller.New(cfg, p, pub, st, clk, nil)

	blocked := withControlledSlot(statusWith(80, 0, 0, 0, 0, true), true, 200)
	blocked.Output1Enabled = 0
	blocked.Output2Enabled = 0
	p.set(200, 0)
	for i := 0; i < 2; i++ {
		st.setFresh(blocked)
		if err := c.Step(context.Background()); err != nil {
			t.Fatalf("Step() error = %v", err)
		}
		clk.advance(cfg.ControlInterval)
	}
	if got := pub.countContaining("cd=10"); got != 0 {
		t.Fatalf("restart must wait until output-enable has been attempted, got %d restart publishes", got)
	}
}

func TestStep_NuclearRestartDoesNotRunDuringExport(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}
	cfg := defaultCfg("topic", "00:00", "23:59")
	cfg.NuclearRestartEnabled = true
	cfg.NuclearRestartAckWiFiRecovery = true
	cfg.NuclearRestartBlockedCycles = 3
	c := controller.New(cfg, p, pub, st, clk, nil)

	blocked := withControlledSlot(statusWith(80, 0, 0, 0, 0, true), true, 200)
	blocked.Output1Enabled = 0
	blocked.Output2Enabled = 0
	p.set(-100, 0)
	for i := 0; i < 6; i++ {
		st.setFresh(blocked)
		if err := c.Step(context.Background()); err != nil {
			t.Fatalf("Step() error = %v", err)
		}
		clk.advance(cfg.ControlInterval)
	}
	if got := pub.countContaining("cd=10"); got != 0 {
		t.Fatalf("restart must not run during export, got %d restart publishes", got)
	}
}

func TestStep_NuclearRestartDoesNotRunDuringTopChargeIdle(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}
	cfg := defaultCfg("topic", "00:00", "23:59")
	cfg.NuclearRestartEnabled = true
	cfg.NuclearRestartAckWiFiRecovery = true
	cfg.NuclearRestartBlockedCycles = 2
	c := controller.New(cfg, p, pub, st, clk, nil)

	for i := 0; i < cfg.NearFullIdleConsecutiveSamples+4; i++ {
		p.set(0, 0)
		status := passThroughStatus(100, 300, 300, 120, 120, true)
		if i > 0 {
			status = withControlledSlot(status, true, 240)
		}
		st.setFresh(status)
		if err := c.Step(context.Background()); err != nil {
			t.Fatalf("Step() error = %v", err)
		}
		clk.advance(cfg.ControlInterval)
	}
	if got := pub.countContaining("cd=10"); got != 0 {
		t.Fatalf("restart must not run during top-charge idle, got %d restart publishes", got)
	}
}

func TestStep_NuclearRestartDoesNotRunBelowSoCFloor(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}
	cfg := defaultCfg("topic", "00:00", "23:59")
	cfg.NuclearRestartEnabled = true
	cfg.NuclearRestartAckWiFiRecovery = true
	cfg.NuclearRestartBlockedCycles = 2
	c := controller.New(cfg, p, pub, st, clk, nil)

	p.set(200, 0)
	for i := 0; i < 6; i++ {
		st.setFresh(withControlledSlot(statusWith(10, 0, 0, 0, 0, true), true, 200))
		if err := c.Step(context.Background()); err != nil {
			t.Fatalf("Step() error = %v", err)
		}
		clk.advance(cfg.ControlInterval)
	}
	if got := pub.countContaining("cd=10"); got != 0 {
		t.Fatalf("restart must not run below SoC floor, got %d restart publishes", got)
	}
}

func TestStep_NuclearRestartRateLimited(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}
	cfg := defaultCfg("topic", "00:00", "23:59")
	cfg.NuclearRestartEnabled = true
	cfg.NuclearRestartAckWiFiRecovery = true
	cfg.NuclearRestartBlockedCycles = 3
	cfg.NuclearRestartMinInterval = time.Hour
	c := controller.New(cfg, p, pub, st, clk, nil)

	blocked := withControlledSlot(statusWith(80, 0, 0, 0, 0, true), true, 200)
	blocked.Output1Enabled = 0
	blocked.Output2Enabled = 0
	p.set(200, 0)
	for i := 0; i < 10; i++ {
		st.setFresh(blocked)
		if err := c.Step(context.Background()); err != nil {
			t.Fatalf("Step() error = %v", err)
		}
		clk.advance(cfg.ControlInterval)
	}
	if got := pub.countContaining("cd=10"); got != 1 {
		t.Fatalf("expected exactly one rate-limited restart, got %d", got)
	}
}

func TestStep_TopChargeIdleEntersOnlyAtFullWithSurplusEvidence(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}
	cfg := defaultCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)

	for i := 0; i < cfg.NearFullIdleConsecutiveSamples; i++ {
		p.set(0, 0)
		status := passThroughStatus(100, 300, 300, 120, 120, true)
		if i > 0 {
			status = withControlledSlot(status, true, 240)
		}
		st.setFresh(status)
		_ = c.Step(context.Background())
		clk.advance(cfg.ControlInterval)
	}
	if got := pub.last(); !strings.Contains(got, ",a1=0,") || !strings.Contains(got, ",v1=0,") {
		t.Fatalf("expected top-charge idle payload, got %q", got)
	}
}

func TestStep_TopChargeIdleDoesNotEnterBelowFull(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}
	cfg := defaultCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)

	for i := 0; i < cfg.NearFullIdleConsecutiveSamples+2; i++ {
		p.set(0, 0)
		st.setFresh(passThroughStatus(99, 300, 300, 120, 120, true))
		_ = c.Step(context.Background())
		clk.advance(cfg.ControlInterval)
	}
	if got := pub.last(); strings.Contains(got, ",a1=0,") && strings.Contains(got, ",v1=0,") {
		t.Fatalf("must not enter top-charge idle below full SoC, got %q", got)
	}
}

func TestStep_TopChargeIdleDoesNotEnterOnMeaningfulImport(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}
	cfg := defaultCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)

	for i := 0; i < cfg.NearFullIdleConsecutiveSamples+2; i++ {
		p.set(100, 0)
		st.setFresh(passThroughStatus(100, 300, 300, 0, 0, true))
		_ = c.Step(context.Background())
		clk.advance(cfg.ControlInterval)
	}
	if got := pub.last(); strings.Contains(got, ",a1=0,") && strings.Contains(got, ",v1=0,") {
		t.Fatalf("must not enter top-charge idle during meaningful import, got %q", got)
	}
}

func TestStep_TopChargeIdleDoesNotFlapOnSmallGridSwings(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}
	cfg := defaultCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)

	for i := 0; i < cfg.NearFullIdleConsecutiveSamples; i++ {
		p.set(0, 0)
		status := passThroughStatus(100, 300, 300, 120, 120, true)
		if i > 0 {
			status = withControlledSlot(status, true, 240)
		}
		st.setFresh(status)
		_ = c.Step(context.Background())
		clk.advance(cfg.ControlInterval)
	}
	countAfterEntry := pub.count()
	for _, grid := range []float64{-20, 20, 45, -10, 30, 0, 50} {
		p.set(grid, 0)
		st.setFresh(withControlledSlot(passThroughStatus(100, 300, 300, 0, 0, true), false, 0))
		_ = c.Step(context.Background())
		clk.advance(cfg.ControlInterval)
	}
	if pub.count() != countAfterEntry {
		t.Fatalf("top-charge idle should stay quiet on small swings, got %d publishes want %d", pub.count(), countAfterEntry)
	}
}

func TestStep_TopChargeIdleExitsOnSustainedImport(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}
	cfg := defaultCfg("topic", "00:00", "23:59")
	cfg.NearFullIdleGridImportExitSamples = 3
	c := controller.New(cfg, p, pub, st, clk, nil)

	for i := 0; i < cfg.NearFullIdleConsecutiveSamples; i++ {
		p.set(0, 0)
		status := passThroughStatus(100, 300, 300, 120, 120, true)
		if i > 0 {
			status = withControlledSlot(status, true, 240)
		}
		st.setFresh(status)
		_ = c.Step(context.Background())
		clk.advance(cfg.ControlInterval)
	}
	countAfterEntry := pub.count()
	for i := 0; i < cfg.NearFullIdleGridImportExitSamples; i++ {
		p.set(150, 0)
		st.setFresh(withControlledSlot(passThroughStatus(100, 150, 150, 0, 0, true), false, 0))
		_ = c.Step(context.Background())
		clk.advance(cfg.ControlInterval)
	}
	if pub.count() <= countAfterEntry {
		t.Fatalf("expected discharge publish after sustained import")
	}
	if got := pub.last(); !strings.Contains(got, ",a1=1,") || !strings.Contains(got, ",v1=150,") {
		t.Fatalf("expected resumed discharge after sustained import, got %q", got)
	}
}

func TestStep_TopChargeIdleExitsWhenSoCDropsBelowFull(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}
	cfg := defaultCfg("topic", "00:00", "23:59")
	c := controller.New(cfg, p, pub, st, clk, nil)

	for i := 0; i < cfg.NearFullIdleConsecutiveSamples; i++ {
		p.set(0, 0)
		status := passThroughStatus(100, 300, 300, 120, 120, true)
		if i > 0 {
			status = withControlledSlot(status, true, 240)
		}
		st.setFresh(status)
		_ = c.Step(context.Background())
		clk.advance(cfg.ControlInterval)
	}
	for i := 0; i < cfg.NearFullIdleConsecutiveSamples; i++ {
		p.set(120, 0)
		st.setFresh(withControlledSlot(passThroughStatus(99, 150, 150, 0, 0, true), false, 0))
		_ = c.Step(context.Background())
		clk.advance(cfg.ControlInterval)
	}
	if got := pub.last(); !strings.Contains(got, ",a1=1,") {
		t.Fatalf("expected normal control after SoC exit, got %q", got)
	}
}

func TestStep_TopChargeIdleKillSwitchDisablesEntry(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}
	cfg := defaultCfg("topic", "00:00", "23:59")
	cfg.NearFullIdleEnabled = false
	c := controller.New(cfg, p, pub, st, clk, nil)

	for i := 0; i < 4; i++ {
		p.set(0, 0)
		st.setFresh(passThroughStatus(100, 300, 300, 120, 120, true))
		_ = c.Step(context.Background())
		clk.advance(cfg.ControlInterval)
	}
	if got := pub.last(); strings.Contains(got, ",a1=0,") && strings.Contains(got, ",v1=0,") {
		t.Fatalf("top-charge idle must not activate when disabled, got %q", got)
	}
}
