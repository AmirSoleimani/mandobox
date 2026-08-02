// Command nats-sub is a debug subscriber: it prints messages on a subject. Used to observe
// the guest's agent.<session>.log / .event streams during bring-up.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/nats-io/nats.go"
)

func main() {
	url := os.Getenv("NATS_URL")
	if url == "" {
		url = "nats://172.31.0.1:4222"
	}
	subj := "agent.>"
	if len(os.Args) > 1 {
		subj = os.Args[1]
	}
	nc, err := nats.Connect(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	if _, err := nc.Subscribe(subj, func(m *nats.Msg) {
		fmt.Printf("[%s] %s\n", m.Subject, string(m.Data))
	}); err != nil {
		fmt.Fprintln(os.Stderr, "subscribe:", err)
		os.Exit(1)
	}
	_ = nc.Flush()
	fmt.Fprintln(os.Stderr, "subscribed to", subj, "on", url)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch
}
