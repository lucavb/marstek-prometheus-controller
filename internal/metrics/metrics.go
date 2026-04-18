// Package metrics defines the controller's self-contained Prometheus exporter.
// It uses a private registry (not DefaultRegisterer) so only the controller's
// own series plus Go/process collectors are scraped — no noise from paho or
// other transitive library registrations.
//
// All series carry a constant label device_id=<MARSTEK_DEVICE_ID> so future
// multi-instance deployments won't collide.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

const ns = "marstek_controller"

// Metrics holds all registered Prometheus instruments and the registry they
// were registered on. The registry is exported so httpserver can serve it.
type Metrics struct {
	Registry *prometheus.Registry

	// Controller state (gauges)
	GridPowerWatts         prometheus.Gauge
	SmoothedGridPowerWatts prometheus.Gauge
	TargetSlotPowerWatts   prometheus.Gauge
	CommandedSlotPowerWatts prometheus.Gauge
	SlotIndex              prometheus.Gauge
	MinOutputWatts         prometheus.Gauge
	MaxOutputWatts         prometheus.Gauge
	State                  prometheus.Gauge // 0 starting, 1 idle, 2 discharging, 3 holding, 4 fallback

	// Dependency health (gauges)
	MQTTConnected                      prometheus.Gauge
	PrometheusUp                       prometheus.Gauge
	LastPrometheusSuccessTimestampSecs prometheus.Gauge
	LastMQTTPublishTimestampSecs       prometheus.Gauge
	LastStatusAgeSecs                  prometheus.Gauge

	// Activity (counters)
	PrometheusQueriesTotal  prometheus.Counter
	PrometheusErrorsTotal   *prometheus.CounterVec
	MQTTPublishesTotal      *prometheus.CounterVec // label: kind (write|self_poll)
	MQTTPublishErrorsTotal  *prometheus.CounterVec // label: reason
	MQTTReconnectsTotal     prometheus.Counter
	MQTTStatusMessagesTotal prometheus.Counter
	SelfPollsTotal          prometheus.Counter
	ControlCyclesTotal      prometheus.Counter
	CommandSuppressedTotal  *prometheus.CounterVec // label: reason
	FallbackTotal           *prometheus.CounterVec // label: reason

	// Performance (histogram)
	ControlLoopDurationSecs prometheus.Histogram
}

// New creates a Metrics instance with a fresh private registry, all instruments
// registered, and Go/process collectors attached. The info gauge is set once
// with the provided constant labels.
func New(deviceID, deviceType, brokerURL, version string) *Metrics {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	constLabels := prometheus.Labels{"device_id": deviceID}

	newGauge := func(name, help string) prometheus.Gauge {
		g := prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace:   ns,
			Name:        name,
			Help:        help,
			ConstLabels: constLabels,
		})
		reg.MustRegister(g)
		return g
	}

	newCounter := func(name, help string) prometheus.Counter {
		c := prometheus.NewCounter(prometheus.CounterOpts{
			Namespace:   ns,
			Name:        name,
			Help:        help,
			ConstLabels: constLabels,
		})
		reg.MustRegister(c)
		return c
	}

	newCounterVec := func(name, help string, labels []string) *prometheus.CounterVec {
		c := prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace:   ns,
			Name:        name,
			Help:        help,
			ConstLabels: constLabels,
		}, labels)
		reg.MustRegister(c)
		return c
	}

	// info gauge — value always 1, all meaningful data in labels.
	info := prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: ns,
		Name:      "info",
		Help:      "Controller build and configuration info (value always 1).",
		ConstLabels: prometheus.Labels{
			"device_id":   deviceID,
			"device_type": deviceType,
			"broker":      brokerURL,
			"version":     version,
		},
	})
	reg.MustRegister(info)
	info.Set(1)

	m := &Metrics{
		Registry: reg,

		GridPowerWatts:          newGauge("grid_power_watts", "Last Prometheus grid power sample (W). Positive = import, negative = export."),
		SmoothedGridPowerWatts:  newGauge("smoothed_grid_power_watts", "EMA-smoothed grid power signal driving control decisions (W)."),
		TargetSlotPowerWatts:    newGauge("target_slot_power_watts", "Computed target before ramp/hold limits are applied (W)."),
		CommandedSlotPowerWatts: newGauge("commanded_slot_power_watts", "Last value actually published to the MQTT schedule slot (W)."),
		SlotIndex:               newGauge("slot_index", "Timed-discharge slot being driven (1–5)."),
		MinOutputWatts:          newGauge("min_output_watts", "Lower clamp on non-zero commanded slot power (W)."),
		MaxOutputWatts:          newGauge("max_output_watts", "Effective upper clamp on commanded slot power (W)."),
		State:                   newGauge("state", "Controller state: 0=starting 1=idle 2=discharging 3=holding 4=fallback."),

		MQTTConnected:                      newGauge("mqtt_connected", "1 if the MQTT client has an active broker session, 0 otherwise."),
		PrometheusUp:                       newGauge("prometheus_up", "1 if the last Prometheus query succeeded within the staleness window, 0 otherwise."),
		LastPrometheusSuccessTimestampSecs: newGauge("last_prometheus_success_timestamp_seconds", "Unix timestamp of the last successful Prometheus query."),
		LastMQTTPublishTimestampSecs:       newGauge("last_mqtt_publish_timestamp_seconds", "Unix timestamp of the last successful MQTT publish."),
		LastStatusAgeSecs:                  newGauge("last_status_age_seconds", "Seconds since the last device status message was received."),

		PrometheusQueriesTotal:  newCounter("prometheus_queries_total", "Total number of Prometheus instant queries executed."),
		PrometheusErrorsTotal:   newCounterVec("prometheus_errors_total", "Prometheus query errors by reason.", []string{"reason"}),
		MQTTPublishesTotal:      newCounterVec("mqtt_publishes_total", "Successful MQTT publishes by kind (write, self_poll).", []string{"kind"}),
		MQTTPublishErrorsTotal:  newCounterVec("mqtt_publish_errors_total", "Failed MQTT publishes by reason.", []string{"reason"}),
		MQTTReconnectsTotal:     newCounter("mqtt_reconnects_total", "Total number of MQTT reconnections (excludes the initial connection)."),
		MQTTStatusMessagesTotal: newCounter("mqtt_status_messages_total", "Total device status messages received via MQTT subscription."),
		SelfPollsTotal:          newCounter("self_polls_total", "Times the controller published a cd=1 poll because device status was stale."),
		ControlCyclesTotal:      newCounter("control_cycles_total", "Total number of control loop iterations executed."),
		CommandSuppressedTotal:  newCounterVec("command_suppressed_total", "Commands not published, by suppression reason.", []string{"reason"}),
		FallbackTotal:           newCounterVec("fallback_total", "Fallback events by reason.", []string{"reason"}),

		ControlLoopDurationSecs: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace:   ns,
			Name:        "control_loop_duration_seconds",
			Help:        "Wall-clock duration of one control loop iteration.",
			ConstLabels: constLabels,
			Buckets:     prometheus.DefBuckets,
		}),
	}
	reg.MustRegister(m.ControlLoopDurationSecs)
	return m
}

// SetState sets the state gauge to the numeric representation of s.
func (m *Metrics) SetState(s State) {
	m.State.Set(float64(s))
}

// RecordLastPrometheusSuccess sets the success timestamp to now and marks up=1.
func (m *Metrics) RecordLastPrometheusSuccess(now time.Time) {
	m.LastPrometheusSuccessTimestampSecs.Set(float64(now.Unix()))
	m.PrometheusUp.Set(1)
}

// RecordLastMQTTPublish sets the publish timestamp to now.
func (m *Metrics) RecordLastMQTTPublish(now time.Time) {
	m.LastMQTTPublishTimestampSecs.Set(float64(now.Unix()))
}

// State is the controller's operating state.
type State float64

const (
	StateStarting   State = 0
	StateIdle       State = 1
	StateDischarging State = 2
	StateHolding    State = 3
	StateFallback   State = 4
)
