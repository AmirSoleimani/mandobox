package supervisor

import (
	"strings"
	"testing"
)

func TestParseStream(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"abc"}`,
		`{"type":"assistant","message":{"content":"working"}}`,
		`{"type":"result","subtype":"success","is_error":false,"duration_ms":1200,"num_turns":3,"result":"done","session_id":"abc","total_cost_usd":0.0421,"usage":{"input_tokens":1000,"output_tokens":250,"cache_read_input_tokens":500}}`,
	}, "\n") + "\n"

	var lines int
	res, err := ParseStream(strings.NewReader(stream), func([]byte) { lines++ })
	if err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	if lines != 3 {
		t.Errorf("onLine called %d times, want 3", lines)
	}
	if res.IsError || res.Subtype != "success" {
		t.Errorf("result = %+v", res)
	}
	if res.TotalCostUSD != 0.0421 {
		t.Errorf("cost = %v, want 0.0421", res.TotalCostUSD)
	}
	if res.Usage.InputTokens != 1000 || res.Usage.OutputTokens != 250 || res.Usage.CacheReadInputTokens != 500 {
		t.Errorf("usage = %+v", res.Usage)
	}
}

func TestParseStreamNoResult(t *testing.T) {
	stream := `{"type":"system"}` + "\n" + `{"type":"assistant"}` + "\n"
	if _, err := ParseStream(strings.NewReader(stream), nil); err != ErrNoResult {
		t.Fatalf("err = %v, want ErrNoResult", err)
	}
}

func TestParseStreamHandlesLargeLine(t *testing.T) {
	// A tool result far larger than the default scanner token ceiling must not break parsing.
	big := strings.Repeat("x", 2<<20) // 2 MiB
	stream := `{"type":"assistant","message":{"content":"` + big + `"}}` + "\n" +
		`{"type":"result","subtype":"success","is_error":false,"result":"ok"}` + "\n"
	res, err := ParseStream(strings.NewReader(stream), nil)
	if err != nil {
		t.Fatalf("ParseStream large: %v", err)
	}
	if res.Subtype != "success" {
		t.Fatalf("result = %+v", res)
	}
}
