package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// costs.go aggregates spend from the archived per-session event logs. Each <session>.event.jsonl
// carries cumulative cost_usd + tokens per turn (pr_opened/push_done events), so the max per file is
// that session's total. This is permanent (survives Temporal's 30-day retention) and cheap — no
// per-workflow queries. Repo + time come from Temporal visibility when the session is still indexed;
// otherwise we fall back to the file's mtime.

type costStore struct {
	logDir string
}

func newCostStore(logDir string) *costStore { return &costStore{logDir: logDir} }

type sessionCost struct {
	SessionID string
	CostUSD   float64
	Tokens    int
	Modified  time.Time
}

// perSession returns the total cost/tokens for every session with an event log.
func (c *costStore) perSession() []sessionCost {
	files, _ := filepath.Glob(filepath.Join(c.logDir, "*.event.jsonl"))
	out := make([]sessionCost, 0, len(files))
	for _, f := range files {
		sid := strings.TrimSuffix(filepath.Base(f), ".event.jsonl")
		sc := sessionCost{SessionID: sid}
		if fi, err := os.Stat(f); err == nil {
			sc.Modified = fi.ModTime()
		}
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		s := bufio.NewScanner(fh)
		s.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for s.Scan() {
			var e struct {
				Cost   float64 `json:"cost_usd"`
				Tokens int     `json:"tokens"`
			}
			if json.Unmarshal(s.Bytes(), &e) != nil {
				continue
			}
			if e.Cost > sc.CostUSD { // cumulative → the max is the session total
				sc.CostUSD = e.Cost
			}
			if e.Tokens > sc.Tokens {
				sc.Tokens = e.Tokens
			}
		}
		_ = fh.Close()
		out = append(out, sc)
	}
	return out
}

type costRepoRow struct {
	Repo     string  `json:"repo"`
	CostUSD  float64 `json:"cost_usd"`
	Tokens   int     `json:"tokens"`
	Sessions int     `json:"sessions"`
}
type costDayRow struct {
	Day      string  `json:"day"`
	CostUSD  float64 `json:"cost_usd"`
	Sessions int     `json:"sessions"`
}
type costSessionRow struct {
	SessionID    string  `json:"session_id"`
	Repo         string  `json:"repo"`
	Time         string  `json:"time"`
	CostUSD      float64 `json:"cost_usd"`
	Tokens       int     `json:"tokens"`
	Provider     string  `json:"provider,omitempty"` // how it ran (durable meta)
	Model        string  `json:"model,omitempty"`
	Subscription bool    `json:"subscription,omitempty"` // cost is notional (flat-rate), not per-token billed
}
type costReport struct {
	TotalUSD    float64 `json:"total_usd"`
	TotalTokens int     `json:"total_tokens"`
	Sessions    int     `json:"sessions"`
	// SubscriptionUSD is the slice of TotalUSD that ran on a Claude subscription — a notional
	// (token-priced) figure, NOT actual per-token billing, since those sessions are flat-rate.
	SubscriptionUSD      float64          `json:"subscription_usd"`
	SubscriptionSessions int              `json:"subscription_sessions"`
	ByRepo               []costRepoRow    `json:"by_repo"`
	ByDay                []costDayRow     `json:"by_day"`
	Top                  []costSessionRow `json:"top"`
}

// buildCostReport joins per-session costs with repo/time metadata (visibility) and how-it-ran meta
// (durable), rolls them up, and separates out subscription spend (which is notional, not billed).
func buildCostReport(costs []sessionCost, meta map[string]sessionMeta, pmeta map[string]sessionMetaRecord) costReport {
	rep := costReport{}
	byRepo := map[string]*costRepoRow{}
	byDay := map[string]*costDayRow{}
	var rows []costSessionRow

	for _, sc := range costs {
		if sc.CostUSD <= 0 {
			continue
		}
		m := meta[sc.SessionID]
		repo := m.Repo
		if repo == "" {
			repo = "(unknown)"
		}
		when := sc.Modified
		if m.Start != "" {
			if t, err := time.Parse(time.RFC3339, m.Start); err == nil {
				when = t
			}
		}
		rep.TotalUSD += sc.CostUSD
		rep.TotalTokens += sc.Tokens
		rep.Sessions++
		pm := pmeta[sc.SessionID]
		if pm.Subscription {
			rep.SubscriptionUSD += sc.CostUSD
			rep.SubscriptionSessions++
		}

		r := byRepo[repo]
		if r == nil {
			r = &costRepoRow{Repo: repo}
			byRepo[repo] = r
		}
		r.CostUSD += sc.CostUSD
		r.Tokens += sc.Tokens
		r.Sessions++

		day := when.UTC().Format("2006-01-02")
		d := byDay[day]
		if d == nil {
			d = &costDayRow{Day: day}
			byDay[day] = d
		}
		d.CostUSD += sc.CostUSD
		d.Sessions++

		rows = append(rows, costSessionRow{SessionID: sc.SessionID, Repo: repo,
			Time: when.UTC().Format(time.RFC3339), CostUSD: sc.CostUSD, Tokens: sc.Tokens,
			Provider: pm.Provider, Model: pm.Model, Subscription: pm.Subscription})
	}

	for _, r := range byRepo {
		rep.ByRepo = append(rep.ByRepo, *r)
	}
	sort.Slice(rep.ByRepo, func(i, j int) bool { return rep.ByRepo[i].CostUSD > rep.ByRepo[j].CostUSD })
	for _, d := range byDay {
		rep.ByDay = append(rep.ByDay, *d)
	}
	sort.Slice(rep.ByDay, func(i, j int) bool { return rep.ByDay[i].Day > rep.ByDay[j].Day }) // newest first
	sort.Slice(rows, func(i, j int) bool { return rows[i].CostUSD > rows[j].CostUSD })
	if len(rows) > 15 {
		rows = rows[:15]
	}
	rep.Top = rows
	return rep
}
