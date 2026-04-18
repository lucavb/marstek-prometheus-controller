package controller

import (
	"context"
	"time"

	"github.com/lucavb/marstek-prometheus-controller/internal/marstek"
	"github.com/lucavb/marstek-prometheus-controller/internal/promclient"
)

// PromReader returns the latest grid power sample from Prometheus.
type PromReader interface {
	Query(ctx context.Context) (promclient.Sample, error)
}

// Publisher publishes an MQTT payload to a topic. Returns an error if
// disconnected or the broker rejected the message.
type Publisher interface {
	Publish(topic, payload string) error
}

// StatusSource provides the most recent device status and the time it was
// received. It also exposes a method to issue a manual poll.
type StatusSource interface {
	// LatestStatus returns the most recently received device status and the
	// wall-clock time it arrived. If no status has been received, the zero
	// value of marstek.Status is returned and receivedAt is zero.
	LatestStatus() (status marstek.Status, receivedAt time.Time)

	// Poll publishes a cd=1 request and waits up to timeout for a response.
	// Returns the received status, or an error on timeout.
	Poll(ctx context.Context, timeout time.Duration) (marstek.Status, error)
}

// Clock abstracts time so tests can run deterministically.
type Clock interface {
	Now() time.Time
}

// RealClock implements Clock using the system clock.
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }
