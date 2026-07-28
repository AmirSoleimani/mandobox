package supervisor

import (
	"strings"
	"testing"
)

const validMMDS = `{
  "session_id": "s_0123456789ABCDEFGHJKMNPQRS",
  "network": {"tap":"taps_012345678","host_ip":"172.16.0.1","guest_ip":"172.16.0.2","prefix_len":30,"gateway":"172.16.0.1","dns":"172.31.0.1"},
  "repo": {"slug":"chelodo/foo","clone_url":"https://github.com/chelodo/foo.git","base_branch":"main"},
  "task": {"mode":"initial","prompt":"add a healthcheck"},
  "llm": {"base_url":"http://172.31.0.1:8080","auth_token":"sess-abc"},
  "github": {"token":"ghs_xxx","bot_user":"chelodo-agent[bot]","bot_email":"bot@users.noreply.github.com"},
  "nats": {"url":"nats://203.0.113.20:4222","creds":"jwt"},
  "claude": {"model":"claude-sonnet-5"}
}`

func TestParseBootConfig(t *testing.T) {
	c, err := ParseBootConfig([]byte(validMMDS))
	if err != nil {
		t.Fatalf("ParseBootConfig: %v", err)
	}
	if c.SessionID.String() != "s_0123456789ABCDEFGHJKMNPQRS" {
		t.Errorf("session_id = %s", c.SessionID)
	}
	if c.Branch() != "agent/s_0123456789ABCDEFGHJKMNPQRS" {
		t.Errorf("Branch() = %s", c.Branch())
	}
	if c.Network.GuestIP != "172.16.0.2" || c.Network.PrefixLen != 30 {
		t.Errorf("network = %+v", c.Network)
	}
	if c.LLM.AuthToken != "sess-abc" || c.GitHub.Token != "ghs_xxx" {
		t.Errorf("secrets not parsed: %+v %+v", c.LLM, c.GitHub)
	}
}

func TestParseBootConfigRejectsInvalid(t *testing.T) {
	cases := map[string]string{
		"bad session":  strings.Replace(validMMDS, "s_0123456789ABCDEFGHJKMNPQRS", "nope", 1),
		"bad mode":     strings.Replace(validMMDS, `"mode":"initial"`, `"mode":"sideways"`, 1),
		"missing repo": strings.Replace(validMMDS, "https://github.com/chelodo/foo.git", "", 1),
		"missing llm":  strings.Replace(validMMDS, "sess-abc", "", 1),
		"missing gh":   strings.Replace(validMMDS, "ghs_xxx", "", 1),
	}
	for name, body := range cases {
		if _, err := ParseBootConfig([]byte(body)); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

func TestParseBootConfigResumeMode(t *testing.T) {
	body := strings.Replace(validMMDS, `"mode":"initial","prompt":"add a healthcheck"`,
		`"mode":"resume","instructions":["address review comments"]`, 1)
	c, err := ParseBootConfig([]byte(body))
	if err != nil {
		t.Fatalf("ParseBootConfig resume: %v", err)
	}
	if c.Task.Mode != ModeResume || len(c.Task.Instructions) != 1 {
		t.Fatalf("resume task = %+v", c.Task)
	}
}
