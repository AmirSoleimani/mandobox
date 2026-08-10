package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseAuditLine(t *testing.T) {
	line := "2026-08-06T09:12:00Z\tsha=abc123\tclaude-code=2.1.220\tcodex=latest"
	e := parseAuditLine(line)
	if e.Timestamp != "2026-08-06T09:12:00Z" || e.SHA != "abc123" || e.Claude != "2.1.220" || e.Codex != "latest" {
		t.Fatalf("bad parse: %+v", e)
	}
}

func TestParseAuditLineMissingCodex(t *testing.T) {
	e := parseAuditLine("2026-08-06T09:12:00Z\tsha=deadbeef\tclaude-code=2.1.0\tcodex=")
	if e.SHA != "deadbeef" || e.Claude != "2.1.0" || e.Codex != "" {
		t.Fatalf("bad parse: %+v", e)
	}
}

func TestParseEnvFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "tools.env")
	if err := os.WriteFile(p, []byte("# comment\n\nCLAUDE_CODE_VERSION=2.1.220\nCODEX_VERSION=latest\n  SPACED = x \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := parseEnvFile(p)
	if m["CLAUDE_CODE_VERSION"] != "2.1.220" || m["CODEX_VERSION"] != "latest" || m["SPACED"] != "x" {
		t.Fatalf("bad env parse: %+v", m)
	}
}

func TestValidateConfig(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		ok   bool
	}{
		{"valid mapping", "agents_allowed: [claude]\ndefaults:\n  model: claude-sonnet-5\n", true},
		{"empty", "", false},
		{"bare scalar", "just-a-string", false},
		{"bare list", "- a\n- b\n", false},
		{"malformed", "key: [unclosed\n", false},
		{"comment only", "# nothing here\n", false}, // parses to nil map → rejected
	}
	for _, c := range cases {
		err := validateConfig(c.raw)
		if (err == nil) != c.ok {
			t.Errorf("%s: got err=%v, wanted ok=%v", c.name, err, c.ok)
		}
	}
}

func TestConfigWriteReadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	cs := newConfigStore(filepath.Join(dir, "mandobox.yml"))

	// Absent config reads cleanly as "not present".
	v, err := cs.read()
	if err != nil || v.Exists {
		t.Fatalf("absent read: exists=%v err=%v", v.Exists, err)
	}

	raw := "agents_allowed:\n  - claude\ndefaults:\n  model: claude-sonnet-5\n"
	if err := cs.write(raw); err != nil {
		t.Fatalf("write: %v", err)
	}
	v, err = cs.read()
	if err != nil || !v.Exists || v.Raw != raw {
		t.Fatalf("read after write mismatch: exists=%v err=%v raw=%q", v.Exists, err, v.Raw)
	}
	if v.Parsed == nil || v.Parsed["defaults"] == nil {
		t.Fatalf("summary parse missing: %+v", v.Parsed)
	}

	// A second write backs up the previous content.
	if err := cs.write("agents_allowed:\n  - claude\n  - codex\n"); err != nil {
		t.Fatalf("second write: %v", err)
	}
	if bak, err := os.ReadFile(cs.path + ".bak"); err != nil || string(bak) != raw {
		t.Fatalf("backup not preserved: err=%v", err)
	}

	// A malformed write is rejected and leaves the good file intact.
	if err := cs.write("key: [unclosed"); err == nil {
		t.Fatal("expected malformed write to fail")
	}
	v, _ = cs.read()
	if v.Parsed["agents_allowed"] == nil {
		t.Fatal("good config was clobbered by a rejected write")
	}
}

func TestUpsertEnvLine(t *testing.T) {
	// Replace an existing key, preserve the others.
	body := "ANTHROPIC_API_KEY=old\nLITELLM_MASTER_KEY=keep\n"
	got := upsertEnvLine(body, "ANTHROPIC_API_KEY", "new")
	if got != "ANTHROPIC_API_KEY=new\nLITELLM_MASTER_KEY=keep\n" {
		t.Fatalf("replace: %q", got)
	}
	// Append a missing key without duplicating trailing blank lines.
	got = upsertEnvLine("EXISTING=1\n\n", "NEWKEY", "v")
	if got != "EXISTING=1\nNEWKEY=v\n" {
		t.Fatalf("append: %q", got)
	}
	// Empty file → single line.
	if got = upsertEnvLine("", "K", "v"); got != "K=v\n" {
		t.Fatalf("empty: %q", got)
	}
	// A value containing '=' is preserved intact.
	if got = upsertEnvLine("K=a=b\n", "K", "x=y"); got != "K=x=y\n" {
		t.Fatalf("equals: %q", got)
	}
}

func TestFingerprint(t *testing.T) {
	a, b := fingerprint("secret-one"), fingerprint("secret-two")
	if a == b || len(a) != 12 {
		t.Fatalf("fingerprint not distinct/short: %q %q", a, b)
	}
	if a != fingerprint("secret-one") {
		t.Fatal("fingerprint not deterministic")
	}
}

func TestReadSecretValue(t *testing.T) {
	dir := t.TempDir()
	envp := filepath.Join(dir, "x.env")
	if err := os.WriteFile(envp, []byte("# c\nSLACK_BOT_TOKEN=xoxb-abc\nOTHER=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, ok := readSecretValue(secretTarget{Path: envp, Key: "SLACK_BOT_TOKEN"}); !ok || v != "xoxb-abc" {
		t.Fatalf("env read: %q %v", v, ok)
	}
	if _, ok := readSecretValue(secretTarget{Path: envp, Key: "MISSING"}); ok {
		t.Fatal("missing key should not be found")
	}
	filep := filepath.Join(dir, "token.json")
	if err := os.WriteFile(filep, []byte("  {\"t\":1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, ok := readSecretValue(secretTarget{Path: filep}); !ok || v != `{"t":1}` {
		t.Fatalf("file read: %q %v", v, ok)
	}
}

func TestEnrichVMsOrphan(t *testing.T) {
	now := time.Unix(10_000, 0)
	vms := []vmRecord{
		{Session: "s_live", StartedAt: 10_000 - 300},   // 300s old, running → live
		{Session: "s_orphan", StartedAt: 10_000 - 300}, // 300s old, not running → orphan
		{Session: "s_new", StartedAt: 10_000 - 10},     // 10s old, not running → within grace, not orphan
	}
	running := map[string]bool{"s_live": true}
	got := enrichVMs(vms, running, now)
	if got[0].Orphan || !got[0].SessionKnown {
		t.Fatalf("live flagged orphan: %+v", got[0])
	}
	if !got[1].Orphan {
		t.Fatalf("orphan not flagged: %+v", got[1])
	}
	if got[2].Orphan {
		t.Fatalf("young VM flagged orphan despite grace: %+v", got[2])
	}
	if got[0].UptimeSec != 300 {
		t.Fatalf("uptime wrong: %d", got[0].UptimeSec)
	}
	// Temporal unreachable → no orphan claims.
	none := enrichVMs(vms, nil, now)
	for _, v := range none {
		if v.Orphan || v.SessionKnown {
			t.Fatalf("orphan claimed with nil running set: %+v", v)
		}
	}
}

func TestConfigHelpDoc(t *testing.T) {
	h := configHelpDoc()
	if len(h.Keys) == 0 || len(h.Snippets) == 0 || h.Example == "" {
		t.Fatal("config help incomplete")
	}
}

func TestRestartLabels(t *testing.T) {
	got := restartLabels([][]string{{"mando-worker", "fleet-worker"}, {"slack-gateway"}})
	if len(got) != 2 || got[0] != "mando-worker" || got[1] != "slack-gateway" {
		t.Fatalf("labels: %v", got)
	}
}

func TestPreambleStore(t *testing.T) {
	dir := t.TempDir()
	auto := filepath.Join(dir, "preamble-autonomous.md")
	collab := filepath.Join(dir, "preamble-collaborate.md")
	// worker-materialized defaults
	if err := os.WriteFile(auto+".default", []byte("BUILTIN-AUTO"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(collab+".default", []byte("BUILTIN-COLLAB"), 0o644); err != nil {
		t.Fatal(err)
	}
	ps := newPreambleStore(auto, collab)

	// No override → shows default, has_override=false.
	v := ps.view()
	if len(v) != 2 || v[0].HasOverride || v[0].Default != "BUILTIN-AUTO" {
		t.Fatalf("initial view wrong: %+v", v[0])
	}

	// Write an override → active.
	if err := ps.write("autonomous", "CUSTOM"); err != nil {
		t.Fatal(err)
	}
	v = ps.view()
	if !v[0].HasOverride || v[0].Override != "CUSTOM" || v[0].Default != "BUILTIN-AUTO" {
		t.Fatalf("after override: %+v", v[0])
	}

	// Clear via empty write → back to default (has_override=false).
	if err := ps.write("autonomous", ""); err != nil {
		t.Fatal(err)
	}
	if ps.view()[0].HasOverride {
		t.Fatal("empty write should clear the override")
	}

	// Unknown name → error.
	if err := ps.write("bogus", "x"); err == nil {
		t.Fatal("expected error for unknown preamble")
	}
}

func TestActivityTime(t *testing.T) {
	// Closed session ranks by CloseTime; open by StartTime.
	closed := Session{Status: "Completed", StartTime: "2026-08-01T00:00:00Z", CloseTime: "2026-08-06T10:00:00Z"}
	open := Session{Status: "Running", StartTime: "2026-08-05T00:00:00Z"}
	if activityTime(closed) != "2026-08-06T10:00:00Z" {
		t.Errorf("closed should rank by CloseTime, got %q", activityTime(closed))
	}
	if activityTime(open) != "2026-08-05T00:00:00Z" {
		t.Errorf("open should rank by StartTime, got %q", activityTime(open))
	}
	// A session that started long ago but just closed must outrank a session that started recently but
	// closed earlier — the bug that made just-closed sessions disappear.
	oldStartJustClosed := Session{Status: "Completed", StartTime: "2026-08-01T00:00:00Z", CloseTime: "2026-08-06T12:00:00Z"}
	recentStartEarlierClose := Session{Status: "Completed", StartTime: "2026-08-06T09:00:00Z", CloseTime: "2026-08-06T09:30:00Z"}
	if !(activityTime(oldStartJustClosed) > activityTime(recentStartEarlierClose)) {
		t.Error("a just-closed session must outrank one that closed earlier, regardless of start time")
	}
}

func TestParseStreamLine(t *testing.T) {
	seq := 0
	// assistant message with a thinking block + a Bash tool_use → two events.
	line := `{"type":"assistant","timestamp":"2026-08-07T09:00:00Z","message":{"content":[` +
		`{"type":"thinking","thinking":"let me run the tests"},` +
		`{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}]}}`
	evs := parseStreamLine([]byte(line), &seq)
	if len(evs) != 2 {
		t.Fatalf("want 2 events, got %d: %+v", len(evs), evs)
	}
	if evs[0].Kind != "thinking" || evs[0].Text == "" {
		t.Errorf("thinking event wrong: %+v", evs[0])
	}
	if evs[1].Kind != "tool" || evs[1].Tool != "Bash" || evs[1].Text != "go test ./..." {
		t.Errorf("tool event wrong: %+v", evs[1])
	}
	if evs[0].Seq == evs[1].Seq {
		t.Error("events must have distinct seq")
	}

	// result line → a single result event.
	seq = 0
	r := parseStreamLine([]byte(`{"type":"result","total_cost_usd":0.12,"duration_ms":4500}`), &seq)
	if len(r) != 1 || r[0].Kind != "result" || !strings.Contains(r[0].Text, "$0.12") {
		t.Fatalf("result event wrong: %+v", r)
	}

	// blank / malformed lines produce nothing.
	if len(parseStreamLine([]byte("  "), &seq)) != 0 || len(parseStreamLine([]byte("not json"), &seq)) != 0 {
		t.Error("blank/malformed lines should yield no events")
	}
}

func TestSummarizeToolInput(t *testing.T) {
	cases := []struct{ name, input, want string }{
		{"Read", `{"file_path":"main.go"}`, "main.go"},
		{"Bash", `{"command":"ls -la"}`, "ls -la"},
		{"Grep", `{"pattern":"TODO","path":"internal/"}`, "TODO in internal/"},
	}
	for _, c := range cases {
		if got := summarizeToolInput(c.name, json.RawMessage(c.input)); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestActivityLogPathRejectsTraversal(t *testing.T) {
	a := newActivityStore("/var/lib/fleet/logs")
	if _, ok := a.logPath("s_ABC123"); !ok {
		t.Error("valid session id should be accepted")
	}
	for _, bad := range []string{"../etc/passwd", "s_../x", "s_ABC/../y", "", "s_ABC.log"} {
		if _, ok := a.logPath(bad); ok {
			t.Errorf("path %q should be rejected", bad)
		}
	}
}

func TestMergeState(t *testing.T) {
	sess := Session{WorkflowID: "wf1", Status: "Running"}
	mergeState(&sess, &State{Phase: "reviewing", PRNumber: 12, PRURL: "http://x/12", VMState: "running", ReviewRound: 2, CumulativeCostUSD: 1.5, HeadBranch: "add-thing", Repo: "org/repo"})
	if !sess.Live || sess.Phase != "reviewing" || sess.PRNumber != 12 || sess.Branch != "add-thing" || sess.Repo != "org/repo" || sess.CostUSD != 1.5 {
		t.Fatalf("merge lost fields: %+v", sess)
	}
}
