// Package connectors is the chat-connector registry and runtime. Each supported platform is ONE
// Connector — its inbound receive loop (Serve) and its outbound Notifier together. The mando-connectors
// host runs the enabled connectors' Serve loops in a single process; the worker registers the enabled
// connectors' Notifiers. Adding a platform is one Connector here; enabling/configuring it is runtime
// dashboard config (connectors.json for enable/disable + the secret env files for credentials), with no
// per-connector binary and no redeploy.
package connectors

import (
	"context"
	"encoding/json"
	"os"

	"github.com/chelodo/mandobox/internal/control"
)

// Connector is a chat platform's full adapter.
type Connector interface {
	// Kind is the platform id (matches control.Conversation.Kind and the connectors.json key).
	Kind() string
	// Configured reports whether the connector's credentials are present (else it cannot run).
	Configured() bool
	// Serve runs the inbound receive loop (Socket Mode / long-poll / webhook) until ctx is cancelled,
	// starting and steering workflows through d.
	Serve(ctx context.Context, d *Dispatcher) error
	// Notifier is the outbound half — nil when not Configured.
	Notifier() control.Notifier
}

// Registry lists every available connector. Each reads its own credentials from the environment at
// construction, so Configured() reflects whether it is set up on this box.
func Registry() []Connector {
	return []Connector{newSlack(), newTelegram()}
}

// ConnectorConfig is the per-connector runtime state the dashboard manages (connectors.json).
type ConnectorConfig struct {
	Enabled bool `json:"enabled"`
}

// LoadConfig reads connectors.json — {"slack":{"enabled":true},"telegram":{"enabled":false}}. A
// missing/invalid file yields an empty map, so every configured connector defaults to enabled and
// existing setups keep working with no file present.
func LoadConfig(path string) map[string]ConnectorConfig {
	b, err := os.ReadFile(path)
	if err != nil {
		return map[string]ConnectorConfig{}
	}
	var m map[string]ConnectorConfig
	if json.Unmarshal(b, &m) != nil {
		return map[string]ConnectorConfig{}
	}
	return m
}

// Enabled reports whether a connector should run: it must be Configured, and either absent from the
// config (default on — backward compatible) or explicitly enabled. A dashboard-written enabled:false
// turns a configured connector off.
func Enabled(cfg map[string]ConnectorConfig, c Connector) bool {
	if !c.Configured() {
		return false
	}
	if e, ok := cfg[c.Kind()]; ok {
		return e.Enabled
	}
	return true
}
