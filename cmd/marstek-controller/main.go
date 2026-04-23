// Command marstek-controller is a Go daemon that keeps grid power near zero by
// adjusting the power of one Marstek B2500 timed-discharge slot over MQTT.
//
// It reads electricity_power_watts from Prometheus (or a configurable PromQL
// query), subscribes to the device status topic for live battery state, and
// publishes cd=20 timed-discharge writes whenever the smoothed grid-power
// signal deviates outside the configured deadband.
//
// See the README for full configuration documentation.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // embed IANA timezone database so named zones work in minimal containers

	"github.com/lucavb/marstek-prometheus-controller/internal/config"
	"github.com/lucavb/marstek-prometheus-controller/internal/controller"
	"github.com/lucavb/marstek-prometheus-controller/internal/httpserver"
	"github.com/lucavb/marstek-prometheus-controller/internal/marstek"
	"github.com/lucavb/marstek-prometheus-controller/internal/metrics"
	"github.com/lucavb/marstek-prometheus-controller/internal/mqttclient"
	"github.com/lucavb/marstek-prometheus-controller/internal/promclient"
	"github.com/lucavb/marstek-prometheus-controller/internal/schedule"
)

// version is set at build time via -ldflags "-X main.version=<tag>".
var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	setupLogger(cfg.LogLevel, cfg.LogFormat)
	slog.Info("marstek-controller starting",
		"version", version,
		"device_type", cfg.MarstekDeviceType,
		"device_id", cfg.MarstekDeviceID,
		"broker", cfg.MQTTBrokerURL,
		"prometheus", cfg.PrometheusBaseURL,
		"slot", cfg.ScheduleSlot,
		"schedule", cfg.ScheduleStart+"–"+cfg.ScheduleEnd,
	)

	m := metrics.New(cfg.MarstekDeviceID, cfg.MarstekDeviceType, cfg.MQTTBrokerURL, version)

	// Prometheus client.
	prom := promclient.New(cfg.PrometheusBaseURL, cfg.PrometheusQuery, cfg.PrometheusTimeout)

	// MQTT client.
	ctrlTopic := marstek.ControlTopic(cfg.MarstekDeviceType, cfg.MarstekDeviceID)
	statusTopic := marstek.StatusTopic(cfg.MarstekDeviceType, cfg.MarstekDeviceID)

	mqttOpts := mqttclient.Options{
		BrokerURL: cfg.MQTTBrokerURL,
		ClientID:  cfg.MQTTClientID,
		Username:  cfg.MQTTUsername,
		Password:  cfg.MQTTPassword,
		OnReconnect: func() {
			m.MQTTConnected.Set(1)
		},
		OnDisconnect: func() {
			m.MQTTConnected.Set(0)
		},
		OnDrop: func(reason string) {
			m.CommandSuppressedTotal.WithLabelValues(reason).Inc()
		},
	}

	mqtt, err := mqttclient.New(mqttOpts)
	if err != nil {
		return fmt.Errorf("mqtt: %w", err)
	}
	defer mqtt.Disconnect()

	// Status source: subscribes to device → broker topic.
	statusSrc := controller.NewMQTTStatusSource(ctrlTopic, mqtt, m)

	if err := mqtt.Subscribe(statusTopic, statusSrc.HandleMessage); err != nil {
		slog.Warn("initial subscribe failed (will retry on connect)", "err", err)
	}

	slog.Info("waiting for MQTT connection", "timeout", "10s")
	if !mqtt.WaitForConnection(10 * time.Second) {
		slog.Warn("MQTT not connected after 10s, proceeding anyway (auto-reconnect active)")
	}
	m.MQTTConnected.Set(boolToFloat(mqtt.IsConnected()))

	// Controller.
	ctrlCfg := controller.Config{
		PrometheusStaleAfter:          cfg.PrometheusStaleAfter,
		StatusStaleAfter:              cfg.MQTTStatusStaleAfter,
		StatusPollTimeout:             cfg.MQTTStatusPollTimeout,
		StatusHardFailAfter:           cfg.MQTTStatusHardFailAfter,
		DeviceID:                      cfg.MarstekDeviceID,
		ControlInterval:               cfg.ControlInterval,
		SmoothingAlpha:                cfg.SmoothingAlpha,
		DeadbandWatts:                 cfg.DeadbandWatts,
		ImportBiasWatts:               cfg.ImportBiasWatts,
		RampUpWattsPerCycle:           cfg.RampUpWattsPerCycle,
		RampDownWattsPerCycle:         cfg.RampDownWattsPerCycle,
		MinCommandDeltaWatts:          cfg.MinCommandDeltaWatts,
		MinCommandDeltaWattsExporting: cfg.MinCommandDeltaWattsExporting,
		MinHoldTime:                   cfg.MinHoldTime,
		MinOutputWatts:                cfg.MinOutputWatts,
		MaxOutputWatts:                cfg.MaxOutputWatts,
		ControlTopic:                  ctrlTopic,
		ScheduleSlot:                  cfg.ScheduleSlot,
		ScheduleStart:                 cfg.ScheduleStart,
		ScheduleEnd:                   cfg.ScheduleEnd,
		PersistToFlash:                cfg.PersistToFlash,

		BatterySoCFloorMarginPercent:   cfg.BatterySoCFloorMarginPercent,
		BatterySoCHysteresisPercent:    cfg.BatterySoCHysteresisPercent,
		BatterySoCFloorFallbackPercent: cfg.BatterySoCFloorFallbackPercent,

		FullBatteryOverrideEnabled:         cfg.FullBatteryOverrideEnabled,
		FullBatterySoCEnterPercent:         cfg.FullBatterySoCEnterPercent,
		FullBatterySoCExitPercent:          cfg.FullBatterySoCExitPercent,
		FullBatteryEnterConsecutiveSamples: cfg.FullBatteryEnterConsecutiveSamples,
	}

	ctrl := controller.New(ctrlCfg, prom, mqtt, statusSrc, nil, m)

	// HTTP server.
	srv := httpserver.New(cfg.HTTPListenAddr, m.Registry, ctrl)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Start MQTT connected-state poller.
	go pollMQTTState(ctx, mqtt, m)

	// Start periodic device restart scheduler only when explicitly opted in.
	// When DEVICE_RESTART_SCHEDULE is empty this block is unreachable — no
	// goroutine is spawned, no timer is created, no metrics are emitted.
	if cfg.DeviceRestartSchedule != "" {
		sched, err := schedule.Parse(cfg.DeviceRestartSchedule)
		if err != nil {
			// Defence in depth: config.validate() already caught this.
			return fmt.Errorf("restart schedule: %w", err)
		}
		loc := cfg.DeviceRestartLocation
		next := sched.Next(time.Now().In(loc))
		slog.Info("periodic device restart enabled",
			"spec", cfg.DeviceRestartSchedule,
			"timezone", loc.String(),
			"next_fire_local", next.Format(time.RFC3339),
			"next_fire_utc", next.UTC().Format(time.RFC3339))
		m.DeviceRestartInfo.WithLabelValues(cfg.DeviceRestartSchedule, loc.String()).Set(1)
		m.NextDeviceRestartTimestampSecs.Set(float64(next.Unix()))
		go runRestartScheduler(ctx, sched, loc, mqtt, ctrlTopic, m)
	}

	// Start HTTP server in background.
	httpErrCh := make(chan error, 1)
	go func() {
		slog.Info("http server listening", "addr", cfg.HTTPListenAddr)
		httpErrCh <- srv.ListenAndServe()
	}()

	// Run the control loop (blocks until ctx is cancelled).
	ctrlErr := ctrl.Run(ctx)

	// Graceful shutdown.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("http shutdown error", "err", err)
	}

	// Give ListenAndServe a moment to return after Shutdown completes so any
	// error is logged rather than dropped. Shutdown's own 5s cap bounds this.
	select {
	case err := <-httpErrCh:
		if err != nil {
			slog.Warn("http server error", "err", err)
		}
	case <-time.After(1 * time.Second):
	}

	if ctrlErr != nil && !errors.Is(ctrlErr, context.Canceled) {
		return ctrlErr
	}
	slog.Info("marstek-controller stopped cleanly")
	return nil
}

// runRestartScheduler fires a cd=10 (SOFTWARE_RESTART) command at each
// scheduled instant. It is only started when DEVICE_RESTART_SCHEDULE is set.
//
// Safety: the mqttclient.Client.Publish queues a pending publish when
// disconnected and silently replaces any prior pending one. The IsConnected()
// precheck is therefore load-bearing — it prevents a restart command from
// stomping a queued control-loop cd=20 publish during an MQTT outage.
func runRestartScheduler(
	ctx context.Context,
	sched *schedule.Schedule,
	loc *time.Location,
	pub *mqttclient.Client,
	ctrlTopic string,
	m *metrics.Metrics,
) {
	for {
		next := sched.Next(time.Now().In(loc))
		if next.IsZero() {
			slog.Error("restart scheduler: no valid next fire time; scheduler stopped")
			return
		}
		m.NextDeviceRestartTimestampSecs.Set(float64(next.Unix()))

		delay := time.Until(next)
		timer := time.NewTimer(delay)

		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		if !pub.IsConnected() {
			slog.Warn("restart scheduler: MQTT not connected, skipping restart",
				"scheduled_utc", next.UTC().Format(time.RFC3339))
			m.DeviceRestartsTotal.WithLabelValues("skipped_not_connected").Inc()
			continue
		}

		if err := pub.Publish(ctrlTopic, marstek.RestartPayload); err != nil {
			slog.Warn("restart scheduler: publish error",
				"scheduled_utc", next.UTC().Format(time.RFC3339),
				"err", err)
			m.DeviceRestartsTotal.WithLabelValues("publish_error").Inc()
			continue
		}

		slog.Warn("restart scheduler: device restart commanded",
			"scheduled_utc", next.UTC().Format(time.RFC3339))
		m.DeviceRestartsTotal.WithLabelValues("sent").Inc()
		m.LastDeviceRestartTimestampSecs.Set(float64(time.Now().Unix()))
	}
}

func pollMQTTState(ctx context.Context, mqtt *mqttclient.Client, m *metrics.Metrics) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	var lastReconnects int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.MQTTConnected.Set(boolToFloat(mqtt.IsConnected()))
			current := mqtt.ReconnectCount()
			if delta := current - lastReconnects; delta > 0 {
				m.MQTTReconnectsTotal.Add(float64(delta))
				lastReconnects = current
			}
		}
	}
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func setupLogger(level, format string) {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	// ReplaceAttr converts the level value to lowercase so Loki pipelines can
	// filter on level="info" / level="warn" / level="error" / level="debug"
	// without a transformation stage.
	opts := &slog.HandlerOptions{
		Level: lvl,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.LevelKey {
				l, ok := a.Value.Any().(slog.Level)
				if !ok {
					return a
				}
				switch {
				case l < slog.LevelInfo:
					a.Value = slog.StringValue("debug")
				case l < slog.LevelWarn:
					a.Value = slog.StringValue("info")
				case l < slog.LevelError:
					a.Value = slog.StringValue("warn")
				default:
					a.Value = slog.StringValue("error")
				}
			}
			return a
		},
	}

	var handler slog.Handler
	if format == "json" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	slog.SetDefault(slog.New(handler))
}
