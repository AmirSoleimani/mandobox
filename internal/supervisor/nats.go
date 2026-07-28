package supervisor

import (
	"fmt"
	"os"
	"time"

	"github.com/nats-io/nats.go"
)

// natsCredsPath is where a session NATS creds file is written (tmpfs in the guest).
const natsCredsPath = "/run/fleet-nats.creds"

// NATSTransport is the production Transport over NATS (§4.4).
type NATSTransport struct {
	nc *nats.Conn
}

// DialNATS connects to the control-plane NATS. creds, when non-empty, is a NATS user creds
// file (JWT + seed) written to tmpfs and used for auth; the guest holds only this
// session-scoped credential (Tier-1, §9).
func DialNATS(url, creds string) (*NATSTransport, error) {
	opts := []nats.Option{
		nats.Name("fc-supervisor"),
		nats.Timeout(5 * time.Second),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2 * time.Second),
	}
	if creds != "" {
		if err := os.WriteFile(natsCredsPath, []byte(creds), 0o600); err != nil {
			return nil, fmt.Errorf("write nats creds: %w", err)
		}
		opts = append(opts, nats.UserCredentials(natsCredsPath))
	}
	nc, err := nats.Connect(url, opts...)
	if err != nil {
		return nil, fmt.Errorf("connect nats %s: %w", url, err)
	}
	return &NATSTransport{nc: nc}, nil
}

// Publish sends data to subject.
func (n *NATSTransport) Publish(subject string, data []byte) error {
	return n.nc.Publish(subject, data)
}

// Subscribe registers an async handler for subject.
func (n *NATSTransport) Subscribe(subject string, handler func([]byte)) error {
	_, err := n.nc.Subscribe(subject, func(m *nats.Msg) { handler(m.Data) })
	return err
}

// Flush blocks until buffered messages are sent.
func (n *NATSTransport) Flush() error { return n.nc.FlushTimeout(5 * time.Second) }

// Close drains and closes the connection.
func (n *NATSTransport) Close() error {
	return n.nc.Drain()
}
