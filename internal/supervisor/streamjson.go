package supervisor

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Usage is the token accounting Claude Code reports on the final result line.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// Result is the terminal object of a `claude -p --output-format stream-json` run — the one
// line with type=="result", carrying cost and usage (PLAN §8.2, §10).
type Result struct {
	Type         string  `json:"type"`
	Subtype      string  `json:"subtype"`
	IsError      bool    `json:"is_error"`
	DurationMs   int     `json:"duration_ms"`
	NumTurns     int     `json:"num_turns"`
	Result       string  `json:"result"`
	SessionID    string  `json:"session_id"` // Claude's own session id (not ours)
	TotalCostUSD float64 `json:"total_cost_usd"`
	Usage        Usage   `json:"usage"`
}

// ErrNoResult means the stream ended without a type=="result" line (agent crashed or
// produced no terminal object).
var ErrNoResult = errors.New("stream ended without a result line")

// ParseStream reads newline-delimited stream-json from r. It calls onLine with each raw line
// (for republishing to agent.<sid>.log) and returns the terminal Result. Lines are read with
// a Reader rather than a Scanner so a large tool-result line never trips the 64KB/8MB token
// ceiling (§8.2).
func ParseStream(r io.Reader, onLine func([]byte)) (Result, error) {
	br := bufio.NewReader(r)
	var result Result
	haveResult := false
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := trimNewline(line)
			if len(trimmed) > 0 {
				if onLine != nil {
					onLine(trimmed)
				}
				if res, ok := parseResultLine(trimmed); ok {
					result = res
					haveResult = true
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return Result{}, fmt.Errorf("read stream: %w", err)
		}
	}
	if !haveResult {
		return Result{}, ErrNoResult
	}
	return result, nil
}

// parseResultLine returns the Result if line is a type=="result" object.
func parseResultLine(line []byte) (Result, bool) {
	// Cheap type probe first to avoid fully decoding every log line.
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &probe); err != nil || probe.Type != "result" {
		return Result{}, false
	}
	var res Result
	if err := json.Unmarshal(line, &res); err != nil {
		return Result{}, false
	}
	return res, true
}

func trimNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
