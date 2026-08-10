package control

import (
	"testing"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

// TestActivitiesRegisterCleanly guards a trap that unit tests miss: worker.RegisterActivity(struct)
// reflects over EVERY exported method of *Activities and panics at startup on any that isn't a valid
// Temporal activity signature (result+error, or just error). An exported helper method — e.g. a
// Register* for chat connectors — has no such signature and would crash the worker before a single
// PRWorkflow runs. The other tests mock individual methods (env.OnActivity) and never register the
// whole struct, so they can't catch it. This registers it exactly as cmd/mando-worker does.
func TestActivitiesRegisterCleanly(t *testing.T) {
	c, err := client.NewLazyClient(client.Options{HostPort: "127.0.0.1:7233"})
	if err != nil {
		t.Fatalf("lazy client: %v", err)
	}
	defer c.Close()

	w := worker.New(c, TaskQueue, worker.Options{})
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("worker.RegisterActivity(&Activities{}) panicked — an exported method is not a "+
				"valid Temporal activity signature (add connectors via the Notifiers map, not a method): %v", r)
		}
	}()
	w.RegisterActivity(&Activities{})
}
