package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Public/convert-tux-to-go/internal/audit"
	"github.com/Public/convert-tux-to-go/internal/budget"
)

// stubClient is a scripted Client for seam tests: each pop of the script
// returns one chat outcome; the last entry repeats.
type stubClient struct {
	responses []chatOutcome
	calls     int
	lastReq   ChatRequest
}

type chatOutcome struct {
	content string
	err     error
}

func (s *stubClient) Chat(_ context.Context, req ChatRequest) (Response, error) {
	if s.calls < len(s.responses) {
		o := s.responses[s.calls]
		s.calls++
		s.lastReq = req
		if o.err != nil {
			return Response{}, o.err
		}
		return Response{Content: o.content}, nil
	}
	o := s.responses[len(s.responses)-1]
	s.calls++
	s.lastReq = req
	if o.err != nil {
		return Response{}, o.err
	}
	return Response{Content: o.content}, nil
}

func (s *stubClient) Stream(ctx context.Context, req ChatRequest, onDelta func(string) error) (Response, error) {
	return s.Chat(ctx, req)
}

func newRecorder(t *testing.T) *audit.Recorder {
	t.Helper()
	rec, err := audit.New(t.TempDir(), "test-run")
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	return rec
}

// mustReadExchange reads one archived exchange from the recorder's folder,
// failing the test when it is missing or unparsable.
func mustReadExchange(t *testing.T, rec *audit.Recorder, name string) audit.Exchange {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(rec.Dir(), name))
	if err != nil {
		t.Fatalf("read exchange %s: %v", name, err)
	}
	var e audit.Exchange
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatalf("parse exchange %s: %v", name, err)
	}
	return e
}

func seamInput(c Client, rec *audit.Recorder) SeamInput {
	return SeamInput{
		Unit: "u01", Kind: "testseam", Name: "demo", Audit: rec,
		Client: c, MaxRetries: 2,
		Prompt: func(notes []string) (string, []Message) {
			return "prompt", []Message{{Role: "system", Content: "sys"}, {Role: "user", Content: "prompt"}}
		},
	}
}

func TestRunSeamSuccessFirstAttempt(t *testing.T) {
	c := &stubClient{responses: []chatOutcome{{content: "```go\nbody\n```"}}}
	rec := newRecorder(t)
	in := seamInput(c, rec)
	in.Extract = func(content string) string { return ExtractFenced(content, "go") }
	payload, calls, notes, err := RunSeam(context.Background(), in)
	if err != nil {
		t.Fatalf("RunSeam: %v", err)
	}
	if payload != "body" || calls != 1 || len(notes) != 0 {
		t.Fatalf("payload=%q calls=%d notes=%v", payload, calls, notes)
	}
	e := mustReadExchange(t, rec, "testseam-demo-attempt0.json")
	if e.Outcome != "ok" || e.Unit != "u01" || e.Prompt != "prompt" || e.Response != "```go\nbody\n```" {
		t.Fatalf("archived exchange: %+v", e)
	}
	if c.lastReq.Temperature != 0 {
		t.Fatalf("temperature not passed through: %v", c.lastReq.Temperature)
	}
}

func TestRunSeamGateRetryFeedsNotesAndSucceeds(t *testing.T) {
	c := &stubClient{responses: []chatOutcome{
		{content: "bad"},
		{content: "good"},
	}}
	rec := newRecorder(t)
	in := seamInput(c, rec)
	in.Gate = func(payload string) []string {
		if payload == "bad" {
			return []string{"gate: not table-driven"}
		}
		return nil
	}
	seen := ""
	in.Prompt = func(notes []string) (string, []Message) {
		seen = strings.Join(notes, "|")
		return "prompt", []Message{{Role: "user", Content: "p"}}
	}
	payload, calls, notes, err := RunSeam(context.Background(), in)
	if err != nil {
		t.Fatalf("RunSeam: %v", err)
	}
	if payload != "good" || calls != 2 {
		t.Fatalf("payload=%q calls=%d", payload, calls)
	}
	if seen != "gate: not table-driven" {
		t.Fatalf("retry prompt did not carry the gate note: %q", seen)
	}
	if len(notes) != 1 || notes[0] != "gate: not table-driven" {
		t.Fatalf("notes=%v", notes)
	}
}

func TestRunSeamExhaustionReturnsJoinedGateError(t *testing.T) {
	c := &stubClient{responses: []chatOutcome{{content: "bad"}}}
	in := seamInput(c, nil)
	in.MaxRetries = 1
	in.Gate = func(string) []string { return []string{"e1", "e2"} }
	_, calls, notes, err := RunSeam(context.Background(), in)
	if err == nil || err.Error() != "e1; e2" {
		t.Fatalf("err=%v", err)
	}
	if calls != 2 || len(notes) != 4 {
		t.Fatalf("calls=%d notes=%v", calls, notes)
	}
}

func TestRunSeamChatErrorAbortPolicy(t *testing.T) {
	boom := errors.New("connection refused")
	c := &stubClient{responses: []chatOutcome{{err: boom}}}
	rec := newRecorder(t)
	in := seamInput(c, rec)
	in.AbortOnChatError = true
	_, calls, _, err := RunSeam(context.Background(), in)
	if err == nil || !strings.Contains(err.Error(), "llm chat: connection refused") {
		t.Fatalf("err=%v", err)
	}
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
	e := mustReadExchange(t, rec, "testseam-demo-attempt0.json")
	if e.Outcome != "failed" || len(e.Errors) != 1 || e.Errors[0] != "connection refused" {
		t.Fatalf("archived exchange: %+v", e)
	}
}

func TestRunSeamChatErrorRetryPolicy(t *testing.T) {
	c := &stubClient{responses: []chatOutcome{
		{err: errors.New("transient")},
		{content: "ok"},
	}}
	in := seamInput(c, nil)
	payload, calls, notes, err := RunSeam(context.Background(), in)
	if err != nil || payload != "ok" || calls != 2 {
		t.Fatalf("payload=%q calls=%d err=%v", payload, calls, err)
	}
	if len(notes) != 1 || notes[0] != "transient" {
		t.Fatalf("notes=%v", notes)
	}
}

func TestRunSeamBudgetCeilings(t *testing.T) {
	long := strings.Repeat("x", 100)
	in := seamInput(&stubClient{responses: []chatOutcome{{content: long}}}, nil)
	in.Budget = budget.New(1000, 10, 4) // output ceiling trips
	_, _, notes, err := RunSeam(context.Background(), in)
	if err == nil {
		t.Fatal("over-budget output must fail after retries")
	}
	if len(notes) == 0 || !strings.HasPrefix(notes[0], "output over budget: ") {
		t.Fatalf("notes=%v", notes)
	}

	in = seamInput(&stubClient{responses: []chatOutcome{{content: "x"}}}, nil)
	in.Budget = budget.New(1, 100, 4) // prompt ceiling trips before any call
	in.Prompt = func([]string) (string, []Message) { return long, nil }
	payload, calls, _, err := RunSeam(context.Background(), in)
	if payload != "" || calls != 0 {
		t.Fatalf("payload=%q calls=%d", payload, calls)
	}
	if err == nil || !strings.Contains(err.Error(), "exceeds the 1-token ceiling") {
		t.Fatalf("err=%v", err)
	}
}

func TestRunSeamMaxRetriesClampAndFileOverride(t *testing.T) {
	c := &stubClient{responses: []chatOutcome{{content: "ok"}}}
	rec := newRecorder(t)
	in := seamInput(c, rec)
	in.MaxRetries = -5
	in.File = func(attempt int) string { return fmt.Sprintf("unit-u01-attempt%d.json", attempt) }
	if _, _, _, err := RunSeam(context.Background(), in); err != nil {
		t.Fatalf("RunSeam: %v", err)
	}
	if e := mustReadExchange(t, rec, "unit-u01-attempt0.json"); e.Unit != "u01" {
		t.Fatalf("custom file name not archived: %+v", e)
	}
}

func TestExtractFenced(t *testing.T) {
	cases := []struct {
		name, content, lang, want string
	}{
		{"tagged fence", "prose\n```python\nprint(1)\n```\ntail", "python", "print(1)"},
		{"bare fence fallback", "```\nx\n```", "python", "x"},
		{"no fence", "  raw\n", "go", "  raw"},
		{"padding only trimmed", "```go\n\n  indented keeps spaces\n\n```", "go", "  indented keeps spaces"},
		{"unclosed fence", "```go\npartial", "go", "partial"},
	}
	for _, tc := range cases {
		if got := ExtractFenced(tc.content, tc.lang); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestJSONObject(t *testing.T) {
	if got := JSONObject("noise {\"a\":1} noise"); got != `{"a":1}` {
		t.Errorf("got %q", got)
	}
	if got := JSONObject("no braces"); got != "" {
		t.Errorf("got %q", got)
	}
}
