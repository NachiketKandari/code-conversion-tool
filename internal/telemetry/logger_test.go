package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestContextRunID(t *testing.T) {
	ctx := context.Background()
	if got := RunIDFromContext(ctx); got != "" {
		t.Fatalf("expected empty runID, got %q", got)
	}

	ctxWithID := WithRunID(ctx, "test-run-1234")
	if got := RunIDFromContext(ctxWithID); got != "test-run-1234" {
		t.Fatalf("expected 'test-run-1234', got %q", got)
	}
}

func TestTelemetryLogging(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "tuxgo-telemetry-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tempDir)

	var consoleBuf bytes.Buffer
	runID := "run-abc-999"

	cleanup, err := Init(Config{
		ConsoleWriter: &consoleBuf,
		LogDir:        tempDir,
		RunID:         runID,
		Verbose:       true,
	})
	if err != nil {
		t.Fatalf("Init failed: %v", err)
	}
	defer cleanup()

	ctx := WithRunID(context.Background(), runID)
	Log(ctx).Info("hello world message", "component", "test.component", "count", 42)

	cleanup() // ensure file flush

	// 1. Verify console output
	consoleOut := consoleBuf.String()
	if !strings.Contains(consoleOut, "hello world message") {
		t.Fatalf("console output missing message: %s", consoleOut)
	}
	if !strings.Contains(consoleOut, "run_id=run-abc-999") {
		t.Fatalf("console output missing run_id: %s", consoleOut)
	}

	// 2. Verify JSON file output
	logFile := filepath.Join(tempDir, "run-"+runID+".jsonl")
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("failed reading log file: %v", err)
	}

	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(data), &entry); err != nil {
		t.Fatalf("failed parsing JSON log record: %v\nRaw: %s", err, string(data))
	}

	if entry["msg"] != "hello world message" {
		t.Errorf("expected msg 'hello world message', got %v", entry["msg"])
	}
	if entry["run_id"] != runID {
		t.Errorf("expected run_id %q, got %v", runID, entry["run_id"])
	}
	if entry["component"] != "test.component" {
		t.Errorf("expected component 'test.component', got %v", entry["component"])
	}

	// 3. The human-readable twin .log file mirrors the record with short time.
	logText, err := os.ReadFile(filepath.Join(tempDir, "run-"+runID+".log"))
	if err != nil {
		t.Fatalf("failed reading .log twin: %v", err)
	}
	if !strings.Contains(string(logText), "hello world message") || !strings.Contains(string(logText), "run_id=run-abc-999") {
		t.Errorf(".log twin missing message or run_id: %s", string(logText))
	}
	if strings.Contains(string(logText), "time=2") {
		t.Errorf(".log twin should carry short clock time, got: %s", strings.Split(string(logText), "\n")[0])
	}
}
