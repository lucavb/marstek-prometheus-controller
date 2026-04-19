package controller

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/lucavb/marstek-prometheus-controller/internal/marstek"
	"github.com/lucavb/marstek-prometheus-controller/internal/metrics"
)

// MQTTPublisher is the minimal interface the status source needs from mqttclient.
type MQTTPublisher interface {
	Publish(topic, payload string) error
}

// MQTTStatusSource implements StatusSource by listening on the device status
// topic via a previously-registered MQTT subscription. It also implements the
// self-poll (cd=1) and counts incoming messages for the metrics layer.
type MQTTStatusSource struct {
	mu             sync.RWMutex
	latest         marstek.Status
	receivedAt     time.Time
	controlTopic   string
	pollResponseCh chan marstek.Status
	pub            MQTTPublisher
	m              *metrics.Metrics
}

// NewMQTTStatusSource creates a status source. controlTopic is the App→device
// topic used for cd=1 self-polls. pub is used to send the poll.
func NewMQTTStatusSource(controlTopic string, pub MQTTPublisher, m *metrics.Metrics) *MQTTStatusSource {
	return &MQTTStatusSource{
		controlTopic:   controlTopic,
		pollResponseCh: make(chan marstek.Status, 1),
		pub:            pub,
		m:              m,
	}
}

// HandleMessage is the callback registered with mqttclient.Subscribe.
// It parses the incoming payload, updates the cache, and forwards to any
// in-flight Poll waiter.
func (s *MQTTStatusSource) HandleMessage(_, payload string) {
	status := marstek.ParseStatus(payload)
	now := time.Now()

	s.mu.Lock()
	s.latest = status
	s.receivedAt = now
	s.mu.Unlock()

	if s.m != nil {
		s.m.MQTTStatusMessagesTotal.Inc()
		s.m.DeviceLastStatusSecs.Set(0)
		s.m.LastStatusAgeSecs.Set(0)
	}

	// Non-blocking send to poll waiter, if any.
	select {
	case s.pollResponseCh <- status:
	default:
	}
}

// LatestStatus returns the most recent cached status and its arrival time.
func (s *MQTTStatusSource) LatestStatus() (marstek.Status, time.Time) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.latest, s.receivedAt
}

// Poll publishes a cd=1 self-poll request and blocks until any status message
// arrives on the device status topic, or the timeout elapses.
//
// Note: Poll does not correlate responses to the self-poll it just sent. The
// device periodically broadcasts its own status independently of cd=1, and
// any such message that arrives during the wait window will satisfy Poll
// exactly as a direct reply would. The stale-drain at the top of Poll only
// discards a message that was buffered before publishing, not one received
// during the wait. Callers that need strict request/response correlation
// should build it on top of this primitive.
func (s *MQTTStatusSource) Poll(ctx context.Context, timeout time.Duration) (marstek.Status, error) {
	// Drain any stale value in the channel before publishing.
	select {
	case <-s.pollResponseCh:
	default:
	}

	if err := s.pub.Publish(s.controlTopic, marstek.PollPayload); err != nil {
		return marstek.Status{}, fmt.Errorf("statussource: poll publish: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	select {
	case status := <-s.pollResponseCh:
		return status, nil
	case <-ctx.Done():
		return marstek.Status{}, fmt.Errorf("statussource: poll timeout after %s", timeout)
	}
}
