package llm

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Public/convert-tux-to-go/internal/config"
)

func endpoint(url string) Endpoint {
	return Endpoint{
		ProfileName: "fake",
		Model:       "test-model",
		APIBase:     url,
		APIKey:      "test-key",
		Temperature: 0.1,
		MaxTokens:   128,
		Timeout:     5 * time.Second,
		Retries:     1,
	}
}

func TestChatNonStream(t *testing.T) {
	srv := NewFakeServer(FakeResponse{
		Content: "generated go code",
		Usage:   &Usage{PromptTokens: 21, CompletionTokens: 5, TotalTokens: 26},
	})
	defer srv.Close()

	c := New(endpoint(srv.URL))
	resp, err := c.Chat(context.Background(), ChatRequest{
		Model:       "test-model",
		Messages:    []Message{{Role: "user", Content: "convert this fn"}},
		Temperature: 0.1,
		MaxTokens:   128,
	})
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	if resp.Content != "generated go code" || resp.FinishReason != "stop" {
		t.Errorf("response = %+v", resp)
	}
	if resp.Usage.TotalTokens != 26 || resp.Usage.Estimated {
		t.Errorf("usage = %+v", resp.Usage)
	}

	// The request must carry the OpenAI-compatible payload shape.
	req := srv.Requests[0]
	if req["model"] != "test-model" || req["max_tokens"] != float64(128) || req["temperature"] != float64(0.1) {
		t.Errorf("request payload = %v", req)
	}
	msgs, _ := req["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", msgs)
	}
	if got := msgs[0].(map[string]any)["content"]; got != "convert this fn" {
		t.Errorf("message content = %v", got)
	}
	// Auth header present, value never logged by the fake.
	if auth := srv.Headers[0].Get("Authorization"); auth != "Bearer test-key" {
		t.Errorf("authorization header = %q", auth)
	}
}

func TestChatKeylessOmitsAuthHeader(t *testing.T) {
	srv := NewFakeServer()
	defer srv.Close()

	ep := endpoint(srv.URL)
	ep.APIKey = ""
	if _, err := New(ep).Chat(context.Background(), ChatRequest{Model: "test-model"}); err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	if auth := srv.Headers[0].Get("Authorization"); auth != "" {
		t.Errorf("keyless endpoint must not set Authorization, got %q", auth)
	}
}

func TestStreamAssemblesDeltas(t *testing.T) {
	srv := NewFakeServer(FakeResponse{
		Chunks: []string{"func ", "GetNav", "Details() {}"},
		Usage:  &Usage{PromptTokens: 10, CompletionTokens: 7, TotalTokens: 17},
	})
	defer srv.Close()

	c := New(endpoint(srv.URL))
	var seen []string
	resp, err := c.Stream(context.Background(), ChatRequest{
		Model:    "test-model",
		Messages: []Message{{Role: "user", Content: "generate"}},
		Stream:   true,
	}, func(delta string) error {
		seen = append(seen, delta)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream failed: %v", err)
	}
	if resp.Content != "func GetNavDetails() {}" {
		t.Errorf("content = %q", resp.Content)
	}
	if strings.Join(seen, "|") != "func |GetNav|Details() {}" {
		t.Errorf("deltas = %v", seen)
	}
	if resp.Usage.TotalTokens != 17 {
		t.Errorf("usage = %+v", resp.Usage)
	}
	// The wire payload must ask for SSE + usage accounting.
	req := srv.Requests[0]
	if req["stream"] != true {
		t.Errorf("stream flag = %v", req["stream"])
	}
	if _, ok := req["stream_options"]; !ok {
		t.Error("stream_options.include_usage missing")
	}
}

func TestStreamEstimatedUsageWhenAbsent(t *testing.T) {
	srv := NewFakeServer(FakeResponse{Chunks: []string{"abcd", "efgh"}}) // 8 chars → 2 tokens
	defer srv.Close()

	resp, err := New(endpoint(srv.URL)).Stream(context.Background(),
		ChatRequest{Model: "test-model", Messages: []Message{{Role: "user", Content: "12345678"}}},
		nil)
	if err != nil {
		t.Fatalf("Stream failed: %v", err)
	}
	if !resp.Usage.Estimated || resp.Usage.CompletionTokens != 2 || resp.Usage.PromptTokens != 2 {
		t.Errorf("estimated usage = %+v", resp.Usage)
	}
}

func TestChatRetriesTransientErrors(t *testing.T) {
	srv := NewFakeServer(
		FakeResponse{Status: 500},
		FakeResponse{Content: "recovered"},
	)
	defer srv.Close()

	retryBaseDelay = time.Millisecond
	defer func() { retryBaseDelay = 500 * time.Millisecond }()

	resp, err := New(endpoint(srv.URL)).Chat(context.Background(), ChatRequest{Model: "test-model"})
	if err != nil {
		t.Fatalf("Chat with retry failed: %v", err)
	}
	if resp.Content != "recovered" {
		t.Errorf("content = %q", resp.Content)
	}
	if len(srv.Requests) != 2 {
		t.Errorf("expected 2 attempts, got %d", len(srv.Requests))
	}
}

func TestChatRetriesExhausted(t *testing.T) {
	srv := NewFakeServer(FakeResponse{Status: 500}, FakeResponse{Status: 500})
	defer srv.Close()

	retryBaseDelay = time.Millisecond
	defer func() { retryBaseDelay = 500 * time.Millisecond }()

	_, err := New(endpoint(srv.URL)).Chat(context.Background(), ChatRequest{Model: "test-model"})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected exhausted-retries error, got %v", err)
	}
	if len(srv.Requests) != 2 { // 1 + 1 retry
		t.Errorf("attempts = %d, want 2", len(srv.Requests))
	}
}

func TestChatDoesNotRetryClientErrors(t *testing.T) {
	srv := NewFakeServer(FakeResponse{Status: 400})
	defer srv.Close()

	retryBaseDelay = time.Millisecond
	defer func() { retryBaseDelay = 500 * time.Millisecond }()

	if _, err := New(endpoint(srv.URL)).Chat(context.Background(), ChatRequest{Model: "test-model"}); err == nil {
		t.Fatal("expected 400 to fail")
	}
	if len(srv.Requests) != 1 {
		t.Errorf("400 must not retry, attempts = %d", len(srv.Requests))
	}
}

func TestNewFromConfigRouting(t *testing.T) {
	cfgYAML := `
run:
  profile: dev
models:
  - name: dev
    provider: openai-compatible
    model: m1
    apiBase: http://127.0.0.1:1/v1
    apiKeyEnv: TUXGO_NOPE
    requestOptions:
      stream: false
      timeout: 3s
`
	path := t.TempDir() + "/cfg.yaml"
	if err := os.WriteFile(path, []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	// Named env var unset and no literal → routing surfaces the error.
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewFromConfig(context.Background(), cfg, ""); err == nil {
		t.Error("expected unset env key to error")
	}

	// Override routes to the right profile without touching run.profile.
	if _, err := cfg.Route("dev"); err != nil {
		t.Errorf("override route failed: %v", err)
	}
}
