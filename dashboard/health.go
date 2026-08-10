package main

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// health.go answers "is my box set up and running?" — the onboarding/operability question. It checks
// the fleet services, endpoint reachability, the active image, disk, VM capacity, and secret presence,
// returning a flat list of green/red checks grouped for the UI.

type healthCheck struct {
	Name   string `json:"name"`
	Group  string `json:"group"` // services | reachability | resources | config
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

type healthStore struct {
	temporalAddr string
	litellmAddr  string
	natsAddr     string
	fleetURL     string // https://host:9443 — we TCP-check the port (mTLS handshake not needed for liveness)
	diskPath     string
	vms          *vmStore
	tools        *toolStore
	secrets      *secretStore
}

func newHealthStore(temporalAddr, litellmAddr, natsAddr, fleetURL, diskPath string, vms *vmStore, tools *toolStore, secrets *secretStore) *healthStore {
	return &healthStore{
		temporalAddr: temporalAddr, litellmAddr: litellmAddr, natsAddr: natsAddr,
		fleetURL: fleetURL, diskPath: diskPath, vms: vms, tools: tools, secrets: secrets,
	}
}

// serviceUnits maps a logical service to candidate unit names (the box may run pre-rename fleet-*).
var serviceUnits = []struct {
	label      string
	candidates []string
}{
	{"worker", []string{"mando-worker", "fleet-worker"}},
	{"agent", []string{"mando-agent", "fleet-agent"}},
	{"egress gateway", []string{"mando-gateway", "fleet-gateway"}},
	{"temporal", []string{"temporal"}},
	{"litellm", []string{"litellm"}},
	{"nats", []string{"nats"}},
	{"nats-bridge", []string{"nats-bridge"}},
	{"webhook-rx", []string{"webhook-rx"}},
	{"slack-gateway", []string{"slack-gateway"}},
	{"dashboard", []string{"mando-dashboard"}},
}

func (h *healthStore) report(ctx context.Context) []healthCheck {
	var out []healthCheck

	for _, s := range serviceUnits {
		out = append(out, serviceCheck(ctx, s.label, s.candidates))
	}

	out = append(out,
		tcpCheck("Temporal", h.temporalAddr),
		tcpCheck("LiteLLM", h.litellmAddr),
		tcpCheck("NATS", h.natsAddr),
		tcpCheck("fleet-agent API", hostPort(h.fleetURL)),
	)

	// image
	tv, _ := h.tools.view()
	imgOK := tv.CurrentSHA != ""
	out = append(out, healthCheck{Name: "golden image", Group: "resources", OK: imgOK,
		Detail: firstNonEmptyDetail(imgOK, "active "+shortSHA(tv.CurrentSHA)+" · claude "+tv.Claude+" · codex "+tv.Codex, "no active image — build one on Tools")})

	// VMs
	vms, verr := h.vms.list(ctx)
	out = append(out, healthCheck{Name: "microVMs", Group: "resources", OK: verr == nil,
		Detail: firstNonEmptyDetail(verr == nil, fmt.Sprintf("%d running", len(vms)), "agent unreachable")})

	// disk
	out = append(out, diskCheck(h.diskPath))

	// secrets present
	set, total := 0, 0
	for _, s := range h.secrets.view() {
		total++
		if s.Present {
			set++
		}
	}
	out = append(out, healthCheck{Name: "secrets", Group: "config", OK: set > 0,
		Detail: fmt.Sprintf("%d of %d set", set, total)})

	return out
}

func serviceCheck(ctx context.Context, label string, candidates []string) healthCheck {
	for _, u := range candidates {
		if !unitLoaded(ctx, u) {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		out, _ := exec.CommandContext(cctx, "systemctl", "is-active", u).Output()
		cancel()
		state := strings.TrimSpace(string(out))
		return healthCheck{Name: label, Group: "services", OK: state == "active", Detail: u + ": " + state}
	}
	return healthCheck{Name: label, Group: "services", OK: false, Detail: "not installed"}
}

func tcpCheck(label, addr string) healthCheck {
	if addr == "" {
		return healthCheck{Name: label, Group: "reachability", OK: false, Detail: "no address"}
	}
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return healthCheck{Name: label, Group: "reachability", OK: false, Detail: addr + " unreachable"}
	}
	_ = c.Close()
	return healthCheck{Name: label, Group: "reachability", OK: true, Detail: addr + " reachable"}
}

// diskCheck shells out to df (portable across linux/darwin) rather than syscall.Statfs, whose struct
// fields differ per OS and would break the local build/test.
func diskCheck(path string) healthCheck {
	c := healthCheck{Name: "disk", Group: "resources"}
	out, err := exec.Command("df", "-Pk", path).Output()
	if err != nil {
		c.Detail = "df failed"
		return c
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		c.Detail = "unparseable"
		return c
	}
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 4 {
		c.Detail = "unparseable"
		return c
	}
	totalKB, _ := strconv.ParseInt(f[1], 10, 64)
	availKB, _ := strconv.ParseInt(f[3], 10, 64)
	pctFree := 0
	if totalKB > 0 {
		pctFree = int(availKB * 100 / totalKB)
	}
	c.OK = pctFree > 10
	c.Name = "disk (" + path + ")"
	c.Detail = fmt.Sprintf("%d%% free · %s available", pctFree, humanBytes(availKB*1024))
	return c
}

func humanBytes(b int64) string {
	const u = 1024
	if b < u {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(u), 0
	for n := b / u; n >= u; n /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// hostPort extracts host:port from a URL like https://127.0.0.1:9443 (for a TCP liveness dial).
func hostPort(u string) string {
	u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
	if i := strings.IndexByte(u, '/'); i >= 0 {
		u = u[:i]
	}
	return u
}

func shortSHA(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func firstNonEmptyDetail(ok bool, okDetail, badDetail string) string {
	if ok {
		return okDetail
	}
	return badDetail
}
