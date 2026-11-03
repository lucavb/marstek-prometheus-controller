package controller_test

import (
	"context"
	"testing"
	"time"

	"github.com/lucavb/marstek-prometheus-controller/internal/controller"
	"github.com/lucavb/marstek-prometheus-controller/internal/metrics"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func newTestMetrics() *metrics.Metrics {
	return metrics.New("60323bd14b6e", "B2500", "tcp://broker.test:1883", "test")
}

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

func TestStep_MetricsRecordPublishAndCommandedWatts(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}
	m := newTestMetrics()
	c := controller.New(defaultCfg("topic", "00:00", "23:59"), gridProm(p, nil), pub, st, clk, m)

	p.set(200, 0)
	st.setFresh(statusWith(80, 0, 0, 0, 0, true))
	if err := c.Step(context.Background()); err != nil {
		t.Fatalf("Step() error = %v", err)
	}

	if got := metricValue(t, m.MQTTPublishesTotal.WithLabelValues("write")); got != 1 {
		t.Errorf("mqtt_publishes_total{kind=\"write\"} = %v, want 1", got)
	}
	if got := metricValue(t, m.CommandedSlotPowerWatts); got != 200 {
		t.Errorf("commanded_slot_power_watts = %v, want 200", got)
	}
	if got := metricValue(t, m.LastMQTTPublishTimestampSecs); got != float64(clk.Now().Unix()) {
		t.Errorf("last_mqtt_publish_timestamp_seconds = %v, want %v", got, clk.Now().Unix())
	}
}

func TestStep_MetricsFallbackWithoutPublishKeepsCommandedGauge(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}
	cfg := defaultCfg("topic", "00:00", "23:59")
	m := newTestMetrics()
	c := controller.New(cfg, gridProm(p, nil), pub, st, clk, m)

	p.set(200, 0)
	st.setFresh(withControlledSlot(statusWith(80, 0, 0, 0, 0, true), false, 0))
	_ = c.Step(context.Background())
	if got := metricValue(t, m.CommandedSlotPowerWatts); got != 200 {
		t.Fatalf("commanded_slot_power_watts = %v, want 200 after the discharge publish", got)
	}

	// Prometheus goes stale and the cached device status already shows the
	// controlled slot exactly as the fallback wants it (disabled, matching
	// schedule times): fallback takes the no-publish early return. That path
	// deliberately leaves the commanded-slot gauge untouched (see
	// noteIdleWithoutPublish in slotwrite.go) — pin the asymmetry.
	p.set(200, 2*time.Minute)
	_ = c.Step(context.Background())

	if got := metricValue(t, m.FallbackTotal.WithLabelValues("prometheus_stale")); got != 1 {
		t.Errorf("fallback_total{reason=\"prometheus_stale\"} = %v, want 1", got)
	}
	if got := metricValue(t, m.State); got != float64(metrics.StateFallback) {
		t.Errorf("state = %v, want %v (fallback)", got, metrics.StateFallback)
	}
	if got := metricValue(t, m.MQTTPublishesTotal.WithLabelValues("write")); got != 1 {
		t.Errorf("mqtt_publishes_total{kind=\"write\"} = %v, want 1 (no publish on matching slot)", got)
	}
	if got := metricValue(t, m.CommandedSlotPowerWatts); got != 200 {
		t.Errorf("commanded_slot_power_watts = %v, want 200 (fallback no-publish path must not touch the gauge)", got)
	}
}

func TestStep_MetricsSocFloorIdleZeroesCommandedGauge(t *testing.T) {
	p := &fakeProm{}
	pub := &fakePublisher{}
	st := &fakeStatus{}
	clk := &fakeClock{now: time.Now()}
	cfg := defaultCfg("topic", "00:00", "23:59")
	m := newTestMetrics()
	c := controller.New(cfg, gridProm(p, nil), pub, st, clk, m)

	p.set(200, 0)
	st.setFresh(withControlledSlot(statusWith(80, 0, 0, 0, 0, true), false, 0))
	_ = c.Step(context.Background())
	if got := metricValue(t, m.CommandedSlotPowerWatts); got != 200 {
		t.Fatalf("commanded_slot_power_watts = %v, want 200 after the discharge publish", got)
	}

	// SoC drops below the soft floor ((100−85)+2 = 17%) while the device
	// already reports the controlled slot disabled as desired: the
	// deliberate-idle path publishes nothing but — unlike the fallback
	// early return — syncs the commanded-slot gauge to zero.
	clk.advance(cfg.ControlInterval)
	p.set(200, 0)
	st.setFresh(withControlledSlot(statusWith(10, 0, 0, 0, 0, true), false, 0))
	_ = c.Step(context.Background())

	if got := metricValue(t, m.CommandedSlotPowerWatts); got != 0 {
		t.Errorf("commanded_slot_power_watts = %v, want 0 (deliberate idle must sync the gauge)", got)
	}
	if got := metricValue(t, m.CommandSuppressedTotal.WithLabelValues("soc_floor")); got != 1 {
		t.Errorf("command_suppressed_total{reason=\"soc_floor\"} = %v, want 1", got)
	}
	if got := metricValue(t, m.State); got != float64(metrics.StateIdle) {
		t.Errorf("state = %v, want %v (idle)", got, metrics.StateIdle)
	}
	if got := metricValue(t, m.MQTTPublishesTotal.WithLabelValues("write")); got != 1 {
		t.Errorf("mqtt_publishes_total{kind=\"write\"} = %v, want 1 (no publish on matching slot)", got)
	}
}
