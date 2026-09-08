package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultIsValidAndRoutes(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config invalid: %v", err)
	}
	m, err := cfg.Route("")
	if err != nil {
		t.Fatalf("default route failed: %v", err)
	}
	if m.Name != "isec-vllm" || m.Model != "/opt/vllm/models/qwen3-VL-30B-A3B-Instruct-AWQ" {
		t.Errorf("default profile = %+v", m)
	}
	if _, err := cfg.Route("nope"); err == nil {
		t.Error("unknown profile must not route")
	}
}

func TestLoadExampleYAMLStrict(t *testing.T) {
	// The shipped example must parse strictly (KnownFields) — it documents
	// the schema; any drift breaks this test.
	cfg, err := Load(filepath.Join("..", "..", "configs", ".tuxgo.example.yaml"))
	if err != nil {
		t.Fatalf("example config failed to load: %v", err)
	}
	if cfg.Run.Profile != "isec-vllm" || cfg.Run.MaxPromptTokens != 12000 {
		t.Errorf("run section = %+v", cfg.Run)
	}
	if cfg.Run.CharsPerToken != 4 {
		t.Errorf("run.charsPerToken = %d, want default 4", cfg.Run.CharsPerToken)
	}
	if len(cfg.Models) != 2 {
		t.Fatalf("expected 2 profiles, got %d", len(cfg.Models))
	}
	if cfg.Models[1].APIBase != "https://openrouter.ai/api/v1" {
		t.Errorf("openrouter profile = %+v", cfg.Models[1])
	}
	// Timeout parses through the Duration type.
	if d := time.Duration(cfg.Models[0].Options.Timeout); d != 120*time.Second {
		t.Errorf("isec timeout = %v, want 120s", d)
	}
	// Absent sections keep defaults.
	if cfg.Concurrency.Workers != 1 || cfg.ValidateCfg.MaxRetries != 3 {
		t.Errorf("defaults must survive partial yaml: %+v", cfg)
	}
	// New knobs default to the documented behavior: LLM on, sqlx-only store.
	if !cfg.Run.LLM || cfg.DB.WithGorm {
		t.Errorf("run.llm/db.withGorm defaults = %+v / %+v, want true/false", cfg.Run.LLM, cfg.DB.WithGorm)
	}
	if cfg.Paths.State != "conversion_logs/state" {
		t.Errorf("paths.state = %q", cfg.Paths.State)
	}
}

func TestConvertInputsAndLLMToggle(t *testing.T) {
	yamlSrc := `
run:
  llm: false
convert:
  input: tux/SVC_DEMO_LIST.pc
  mapping: nav.mapping.yaml
`
	path := filepath.Join(t.TempDir(), ".tuxgo.yaml")
	if err := os.WriteFile(path, []byte(yamlSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Run.LLM {
		t.Error("run.llm: false must disable the generation seam")
	}
	if cfg.Convert.Input != "tux/SVC_DEMO_LIST.pc" || cfg.Convert.Mapping != "nav.mapping.yaml" {
		t.Errorf("convert inputs = %+v", cfg.Convert)
	}
	// Absent sections still keep their defaults.
	if cfg.Concurrency.Workers != 1 || cfg.Paths.Staged != "conversion_logs/_staged" {
		t.Errorf("defaults must survive partial yaml: %+v", cfg)
	}
}

func TestBatchpyInputKey(t *testing.T) {
	yamlSrc := `
batchpy:
  input: tux/batch/
  outDir: pygen_out
`
	path := filepath.Join(t.TempDir(), ".tuxgo.yaml")
	if err := os.WriteFile(path, []byte(yamlSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Batchpy.Input != "tux/batch/" {
		t.Errorf("batchpy.input = %q, want tux/batch/", cfg.Batchpy.Input)
	}
	if cfg.Batchpy.OutDir != "pygen_out" {
		t.Errorf("batchpy.outDir = %q, want pygen_out", cfg.Batchpy.OutDir)
	}
	// Conventions absent from the yaml keep their defaults.
	if cfg.Batchpy.Entrypoint != "process_daily_batch" || cfg.Batchpy.ChunkSize != 1000 {
		t.Errorf("batchpy defaults must survive partial yaml: %+v", cfg.Batchpy)
	}
}

func TestLoadOverlayAndUnknownKeys(t *testing.T) {
	yamlSrc := `
run:
  profile: local-dev-openrouter
  temperature: 0.2
  maxContextTokens: 8000
  maxPromptTokens: 6000
  maxOutputTokens: 2000
models:
  - name: local-dev-openrouter
    provider: openai-compatible
    model: qwen/qwen3-30b-a3b
    apiBase: https://openrouter.ai/api/v1
    apiKey: sk-or-local-test
    requestOptions:
      temperature: 0.2
      maxTokens: 2000
      stream: false
      timeout: 30s
      retries: 1
elision:
  mode: off
concurrency:
  workers: 2
validate:
  maxRetries: 5
paths:
  tux: corpus/
  logs: logs/
  audit: audit/
  ledger: ledger/
  state: state/
`
	path := filepath.Join(t.TempDir(), ".tuxgo.yaml")
	if err := os.WriteFile(path, []byte(yamlSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if cfg.Run.Profile != "local-dev-openrouter" || cfg.Run.Temperature != 0.2 {
		t.Errorf("run overlay = %+v", cfg.Run)
	}
	// retrieval section absent → default kept.
	if cfg.Retrieval.Enabled {
		t.Error("retrieval must default to disabled")
	}
	m, err := cfg.Route("")
	if err != nil || m.Model != "qwen/qwen3-30b-a3b" {
		t.Fatalf("route = %v, %v", m, err)
	}
	if temp, max, stream, timeout, retries := cfg.Merged(m); temp != 0.2 || max != 2000 || stream || timeout != 30*time.Second || retries != 1 {
		t.Errorf("merged options = %v %v %v %v %v", temp, max, stream, timeout, retries)
	}

	// Unknown keys are errors, never silent.
	if err := os.WriteFile(path, []byte(yamlSrc+"\nunknown_section:\n  x: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "unknown_section") {
		t.Errorf("unknown key must fail load, got %v", err)
	}
}

func TestValidationErrors(t *testing.T) {
	cases := map[string]func(*Config){
		"unknown default profile": func(c *Config) { c.Run.Profile = "ghost" },
		"negative budget":         func(c *Config) { c.Run.MaxPromptTokens = -1 },
		"prompt over window":      func(c *Config) { c.Run.MaxPromptTokens = 32000 },
		"zero chars per token":    func(c *Config) { c.Run.CharsPerToken = 0 },
		"empty models":            func(c *Config) { c.Models = nil },
		"duplicate profile": func(c *Config) {
			c.Models[1].Name = c.Models[0].Name
		},
		"bad provider": func(c *Config) { c.Models[0].Provider = "anthropic" },
		"bad apiBase":  func(c *Config) { c.Models[0].APIBase = "10.0.0.1:8002" },
		"bad elision":  func(c *Config) { c.Elision.Mode = "aggressive" },
		"zero workers": func(c *Config) { c.Concurrency.Workers = 0 },
		"bad compile":  func(c *Config) { c.ValidateCfg.Compile = "sometimes" },
		"always without target": func(c *Config) {
			c.ValidateCfg.Compile = "always"
		},
	}
	for name, mutate := range cases {
		cfg := Default()
		mutate(cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}

func TestAPIKeyResolution(t *testing.T) {
	m := &Model{Name: "p", APIKeyEnv: "TUXGO_TEST_KEY", APIKey: "literal-fallback"}

	// Env wins over literal; the source names the channel, never the value.
	t.Setenv("TUXGO_TEST_KEY", "env-value")
	key, source, err := m.ResolveKey()
	if err != nil || key != "env-value" || source != "env:TUXGO_TEST_KEY" {
		t.Errorf("env precedence = %q %q %v", key, source, err)
	}

	// Literal fallback when the named env var is unset.
	os.Unsetenv("TUXGO_TEST_KEY")
	key, source, err = m.ResolveKey()
	if err != nil || key != "literal-fallback" || source != "literal" {
		t.Errorf("literal fallback = %q %q %v", key, source, err)
	}

	// A named env var that is unset with no literal is an error — a typo'd
	// name must never silently go keyless.
	m.APIKey = ""
	if _, _, err := m.ResolveKey(); err == nil {
		t.Error("unset env without literal must error")
	}

	// Deliberately keyless endpoint: no env name, no literal.
	m2 := &Model{Name: "keyless"}
	if key, source, err := m2.ResolveKey(); key != "" || source != "none" || err != nil {
		t.Errorf("keyless = %q %q %v", key, source, err)
	}
}
