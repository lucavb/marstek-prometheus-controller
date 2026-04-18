// Package mqttclient wraps the paho MQTT client with automatic reconnection,
// a single-pending-publish queue, topic subscription, and connected-state
// tracking suitable for the Marstek controller.
//
// Design constraints from the plan:
//   - Auto-reconnect + connect-retry so transient broker outages are healed silently.
//   - Only one pending publish is queued during a disconnect; if a new one arrives
//     the older pending write is dropped (suppressed) and replaced. This avoids
//     stacking stale commands when the broker comes back.
//   - Publish is fire-and-forget (no QoS 1/2) matching the device protocol.
//   - Subscribe registers a callback for the device status topic.
package mqttclient

import (
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

// MessageHandler is called for every message received on a subscribed topic.
type MessageHandler func(topic, payload string)

// DropHandler is called when a pending publish is dropped because a newer one
// arrived while still disconnected. The argument is a human-readable reason.
type DropHandler func(reason string)

// Options configures the MQTT client.
type Options struct {
	BrokerURL  string
	ClientID   string
	Username   string
	Password   string
	// OnReconnect is called each time the client successfully reconnects.
	OnReconnect func()
	// OnDrop is called when a pending publish is replaced by a newer one.
	OnDrop DropHandler
}

// Client is a thread-safe paho wrapper.
type Client struct {
	opts    Options
	paho    paho.Client
	mu      sync.Mutex
	pending *pendingPublish
	// connected is 1 when the last connection callback fired, 0 otherwise.
	connected atomic.Int32
	reconnects atomic.Int64
}

type pendingPublish struct {
	topic   string
	payload string
}

// New creates and connects a Client. It does not block waiting for the initial
// connection — paho's auto-reconnect handles that asynchronously. Call
// WaitForConnection if you need to block startup until connected.
func New(opts Options) (*Client, error) {
	if opts.BrokerURL == "" {
		return nil, fmt.Errorf("mqttclient: BrokerURL is required")
	}

	c := &Client{opts: opts}

	po := paho.NewClientOptions()
	po.AddBroker(opts.BrokerURL)
	if opts.ClientID != "" {
		po.SetClientID(opts.ClientID)
	}
	if opts.Username != "" {
		po.SetUsername(opts.Username)
	}
	if opts.Password != "" {
		po.SetPassword(opts.Password)
	}

	po.SetAutoReconnect(true)
	po.SetConnectRetry(true)
	po.SetConnectRetryInterval(5 * time.Second)
	po.SetMaxReconnectInterval(60 * time.Second)
	po.SetKeepAlive(30 * time.Second)
	po.SetCleanSession(true)

	po.SetOnConnectHandler(func(_ paho.Client) {
		c.connected.Store(1)
		c.reconnects.Add(1)
		slog.Info("mqtt: connected", "broker", opts.BrokerURL)
		if opts.OnReconnect != nil {
			opts.OnReconnect()
		}
		// Drain any pending publish that queued during disconnect.
		c.mu.Lock()
		p := c.pending
		c.pending = nil
		c.mu.Unlock()
		if p != nil {
			slog.Debug("mqtt: flushing pending publish after reconnect", "topic", p.topic)
			c.paho.Publish(p.topic, 0, false, p.payload)
		}
	})
	po.SetConnectionLostHandler(func(_ paho.Client, err error) {
		c.connected.Store(0)
		slog.Warn("mqtt: connection lost", "err", err)
	})

	c.paho = paho.NewClient(po)
	if tok := c.paho.Connect(); tok.Wait() && tok.Error() != nil {
		// Non-fatal: paho will keep retrying. We log and continue.
		slog.Warn("mqtt: initial connect failed, retrying in background", "err", tok.Error())
	}
	return c, nil
}

// IsConnected returns true when the client has an active broker session.
func (c *Client) IsConnected() bool {
	return c.connected.Load() == 1
}

// ReconnectCount returns the total number of successful (re)connections since
// the client was created.
func (c *Client) ReconnectCount() int64 {
	// Subtract 1 for the initial connection.
	n := c.reconnects.Load()
	if n <= 1 {
		return 0
	}
	return n - 1
}

// Publish sends a message to the given topic with QoS 0 (fire-and-forget).
//
// If the client is currently disconnected the message is held in a single-slot
// pending queue. If another Publish arrives while still disconnected the older
// pending message is dropped (OnDrop is called) and replaced by the newer one.
func (c *Client) Publish(topic, payload string) error {
	if !c.IsConnected() {
		c.mu.Lock()
		if c.pending != nil {
			slog.Debug("mqtt: dropping pending publish (replaced by newer)", "old_topic", c.pending.topic)
			if c.opts.OnDrop != nil {
				c.opts.OnDrop("disconnected")
			}
		}
		c.pending = &pendingPublish{topic: topic, payload: payload}
		c.mu.Unlock()
		return fmt.Errorf("mqttclient: not connected, queued publish to %s", topic)
	}

	tok := c.paho.Publish(topic, 0, false, payload)
	tok.Wait()
	if err := tok.Error(); err != nil {
		return fmt.Errorf("mqttclient: publish to %s: %w", topic, err)
	}
	return nil
}

// Subscribe registers handler to be called for every message on topic.
// This should be called before or immediately after connection; paho re-subscribes
// automatically on reconnect when CleanSession=true via the onConnect handler —
// but we store the subscription so we can re-subscribe inside OnConnect.
func (c *Client) Subscribe(topic string, handler MessageHandler) error {
	wrap := func(_ paho.Client, msg paho.Message) {
		handler(msg.Topic(), string(msg.Payload()))
	}

	// Store for re-subscription on reconnect.
	originalOnConnect := c.opts.OnReconnect
	c.opts.OnReconnect = func() {
		if originalOnConnect != nil {
			originalOnConnect()
		}
		if tok := c.paho.Subscribe(topic, 0, wrap); tok.Wait() && tok.Error() != nil {
			slog.Warn("mqtt: re-subscribe failed", "topic", topic, "err", tok.Error())
		}
	}

	if !c.IsConnected() {
		slog.Debug("mqtt: subscribe deferred (not yet connected)", "topic", topic)
		return nil
	}
	if tok := c.paho.Subscribe(topic, 0, wrap); tok.Wait() && tok.Error() != nil {
		return fmt.Errorf("mqttclient: subscribe %s: %w", topic, tok.Error())
	}
	return nil
}

// Disconnect gracefully disconnects the client with a 500ms quiesce timeout.
func (c *Client) Disconnect() {
	c.paho.Disconnect(500)
	c.connected.Store(0)
}

// WaitForConnection blocks until the client is connected or the deadline
// elapses. Returns true if connected, false on timeout.
func (c *Client) WaitForConnection(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c.IsConnected() {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}
