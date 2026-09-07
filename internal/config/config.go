// Package config loads and validates the tuxgo run configuration
// (.tuxgo.yaml, PRD F7/R1): multi-profile OpenAI-compatible models[] with
// profile routing, run budgets, seam toggles, and artifact paths. Absent
// fields keep the documented defaults; API keys resolve from the environment
// (apiKeyEnv, R6) with a gitignored-file literal (apiKey) as the local-dev
// fallback.
package config

import (
	"bytes"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultPaths mirrors the working-directory layout of
// configs/.tuxgo.example.yaml.
func DefaultPaths() Paths {
	return Paths{
		Tux:    "tux/",
		Target: "../existing-go-service",
		MainGo: "",
		Logs:   "conversion_logs/logs",
		Audit:  "conversion_logs/audit",
		Ledger: "conversion_logs/ledger",
		State:  "conversion_logs/state",
		Staged: "conversion_logs/_staged",
	}
}

// Default returns the stock configuration (profiles per R1, budgets per §4.3,
// retrieval disabled per OQ1).
func Default() *Config {
	return &Config{
		Run: Run{
			Profile:          "isec-vllm",
			Temperature:      0.1,
			MaxContextTokens: 16000,
			MaxPromptTokens:  12000,
			MaxOutputTokens:  4000,
			CharsPerToken:    4,
			LLM:              true,
		},
		Models: []Model{
			{
				Name:      "isec-vllm",
				Provider:  "openai-compatible",
				Model:     "qwen3-vl-30b-a3b",
				APIBase:   "http://10.213.190.86:8002/v1",
				APIKeyEnv: "ISEC_API_KEY",
				Options:   RequestOptions{Temperature: ptr(0.1), MaxTokens: 4000, Stream: true, Timeout: Duration(120 * time.Second), Retries: 3},
			},
			{
				Name:      "local-dev-openrouter",
				Provider:  "openai-compatible",
				Model:     "<openrouter-model-id>", // fill in your local .tuxgo.yaml
				APIBase:   "https://openrouter.ai/api/v1",
				APIKeyEnv: "OPENROUTER_API_KEY",
				Options:   RequestOptions{Temperature: ptr(0.1), MaxTokens: 4000, Stream: true},
			},
		},
		Elision:     Elision{Mode: "safe"},
		Concurrency: Concurrency{Workers: 1},
		ValidateCfg: ValidateCfg{MaxRetries: 3, Compile: "auto"},
		DB:          DB{WithGorm: false},
		Paths:       DefaultPaths(),
	}
}

func ptr(f float64) *float64 { return &f }

// Load reads path (typically .tuxgo.yaml in the working directory), overlays
// it on the defaults (absent sections keep their default values), and
// validates. Unknown keys are errors: a typo'd config must fail loudly.
func Load(path string) (*Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: reading %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("config: parsing %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	return cfg, nil
}

// Config is the root schema of .tuxgo.yaml.
type Config struct {
	Run         Run         `yaml:"run"`
	Models      []Model     `yaml:"models"`
	Retrieval   Retrieval   `yaml:"retrieval"`
	Elision     Elision     `yaml:"elision"`
	Concurrency Concurrency `yaml:"concurrency"`
	ValidateCfg ValidateCfg `yaml:"validate"`
	DB          DB          `yaml:"db"`
	Convert     Convert     `yaml:"convert"`
	Paths       Paths       `yaml:"paths"`
}

// Run carries the generation-wide budget and the default profile name.
// CharsPerToken is the estimator ratio (§4.3): the budgeter approximates
// tokens as characters/CharsPerToken — tune it per model without a code
// change.
type Run struct {
	Profile          string  `yaml:"profile"`
	Temperature      float64 `yaml:"temperature"`
	MaxContextTokens int     `yaml:"maxContextTokens"`
	MaxPromptTokens  int     `yaml:"maxPromptTokens"`
	MaxOutputTokens  int     `yaml:"maxOutputTokens"`
	CharsPerToken    int     `yaml:"charsPerToken"`
	// LLM gates the generation seam (default true). false = deterministic-only
	// run: models/db/interfaces/handler/router generate, pending controller
	// bodies are marked skipped (never failed) for a later LLM-enabled resume.
	LLM bool `yaml:"llm"`
}

// Duration is a yaml time span ("120s", "2m") decoding into time.Duration.
type Duration time.Duration

// UnmarshalYAML implements yaml.v3 strict duration parsing.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("bad duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// MarshalYAML renders the duration in Go notation.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// RequestOptions are the per-profile generation parameters.
type RequestOptions struct {
	Temperature *float64 `yaml:"temperature"` // absent → inherit run.temperature
	MaxTokens   int      `yaml:"maxTokens"`
	Stream      bool     `yaml:"stream"`
	Timeout     Duration `yaml:"timeout"`
	Retries     int      `yaml:"retries"`
}

// Model is one OpenAI-compatible endpoint profile.
type Model struct {
	Name      string         `yaml:"name"`
	Provider  string         `yaml:"provider"`
	Model     string         `yaml:"model"`
	APIBase   string         `yaml:"apiBase"`
	APIKeyEnv string         `yaml:"apiKeyEnv"` // env-var NAME, never the value (R6)
	APIKey    string         `yaml:"apiKey"`    // literal fallback for the gitignored local .tuxgo.yaml only
	Options   RequestOptions `yaml:"requestOptions"`
}

// Route resolves the model profile a run uses: override when non-empty, else
// run.profile. This is the routing seam every LLM-bound command goes through.
func (c *Config) Route(override string) (*Model, error) {
	name := override
	if name == "" {
		name = c.Run.Profile
	}
	for i := range c.Models {
		if c.Models[i].Name == name {
			return &c.Models[i], nil
		}
	}
	names := make([]string, 0, len(c.Models))
	for _, m := range c.Models {
		names = append(names, m.Name)
	}
	return nil, fmt.Errorf("profile %q not found in models[] (have %v)", name, names)
}

// APIKey resolves the profile's key: the env var wins; the literal apiKey
// (local gitignored config only) is the fallback. The source is returned for
// audit logging — never log the key value itself.
func (m *Model) ResolveKey() (key, source string, err error) {
	if m.APIKeyEnv != "" {
		if v := os.Getenv(m.APIKeyEnv); v != "" {
			return v, "env:" + m.APIKeyEnv, nil
		}
		if m.APIKey == "" {
			return "", "", fmt.Errorf("profile %s: env var %s is not set", m.Name, m.APIKeyEnv)
		}
	}
	if m.APIKey != "" {
		return m.APIKey, "literal", nil
	}
	return "", "none", nil
}

// Merged returns the effective request options: profile values where set,
// run-level budget otherwise, stock 120s timeout when neither applies.
func (c *Config) Merged(m *Model) (temperature float64, maxTokens int, stream bool, timeout time.Duration, retries int) {
	temperature = c.Run.Temperature
	if m.Options.Temperature != nil {
		temperature = *m.Options.Temperature
	}
	maxTokens = c.Run.MaxOutputTokens
	if m.Options.MaxTokens > 0 {
		maxTokens = m.Options.MaxTokens
	}
	stream = m.Options.Stream
	timeout = time.Duration(m.Options.Timeout)
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	retries = m.Options.Retries
	return temperature, maxTokens, stream, timeout, retries
}

// Retrieval toggles the deferred retriever seam (MVP: disabled, OQ1).
type Retrieval struct {
	Enabled          bool `yaml:"enabled"`
	MaxContextTokens int  `yaml:"maxContextTokens"`
}

// Elision selects the body-elision mode (G3).
type Elision struct {
	Mode string `yaml:"mode"`
}

// Concurrency is the opt-in DB-unit worker count (pipeline-three-parts 2b).
type Concurrency struct {
	Workers int `yaml:"workers"`
}

// DB carries the generated DB-layer shape options.
type DB struct {
	// WithGorm makes the store carry the legacy *gorm.DB handle alongside
	// sqlx (the nav-example variant: NewXStore(oracle, db)). Default false —
	// the plain sqlx-only store (NewXStore(db)) is the standard shape.
	WithGorm bool `yaml:"withGorm"`
}

// Convert carries the plan/convert commands' default inputs so repeat runs
// need no CLI arguments; explicit CLI flags override these.
type Convert struct {
	// Input is the .pc/.pcf file or directory converted when the CLI passes
	// no positional target.
	Input string `yaml:"input"`
	// Mapping is the user endpoint-mapping YAML used when -mapping is absent.
	Mapping string `yaml:"mapping"`
}

// ValidateCfg configures the bounded gofmt/build/vet/test retry loop (G6)
// and its two tiers (plan-conversion §2): Tier A (parse + gofmt) always runs;
// Tier B (build/vet/test in the target module) runs when paths.mainGo
// resolves — compile: auto follows presence, always errors when the target
// is missing, never skips Tier B even when it could run.
type ValidateCfg struct {
	MaxRetries int    `yaml:"maxRetries"`
	Compile    string `yaml:"compile"`
	Run        bool   `yaml:"run"`
}

// Paths are the run's artifact roots, relative to the config file.
type Paths struct {
	Tux    string `yaml:"tux"`
	Target string `yaml:"target"`
	// MainGo is the target service's main.go (or any file inside the module)
	// — the anchor Tier-B validation walks up from to the go.mod. Empty means
	// the target service is absent on this machine: conversion degrades to
	// syntax-only validation and stages generated code under paths.staged
	// (the two-laptop constraint, plan-conversion §2).
	MainGo string `yaml:"mainGo"`
	Logs   string `yaml:"logs"`
	Audit  string `yaml:"audit"`
	Ledger string `yaml:"ledger"`
	State  string `yaml:"state"`
	Staged string `yaml:"staged"`
}
