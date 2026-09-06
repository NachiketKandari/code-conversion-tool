package config

import (
	"fmt"
	"net/url"
	"strings"
)

// Validate enforces the schema invariants: exactly one default profile,
// unique names, OpenAI-compatible endpoints with resolvable URLs, positive
// budgets, and known enum values. Errors name the offending field so a
// mis-edited yaml fails at load, never mid-run.
func (c *Config) Validate() error {
	if c.Run.Profile == "" {
		return fmt.Errorf("run.profile must not be empty")
	}
	for _, bad := range []struct {
		name string
		v    int
	}{
		{"run.maxContextTokens", c.Run.MaxContextTokens},
		{"run.maxPromptTokens", c.Run.MaxPromptTokens},
		{"run.maxOutputTokens", c.Run.MaxOutputTokens},
	} {
		if bad.v <= 0 {
			return fmt.Errorf("%s must be positive, got %d", bad.name, bad.v)
		}
	}
	if c.Run.MaxPromptTokens > c.Run.MaxContextTokens {
		return fmt.Errorf("run.maxPromptTokens (%d) exceeds the context window (%d)", c.Run.MaxPromptTokens, c.Run.MaxContextTokens)
	}
	if c.Run.Temperature < 0 || c.Run.Temperature > 2 {
		return fmt.Errorf("run.temperature %v out of range [0,2]", c.Run.Temperature)
	}

	if len(c.Models) == 0 {
		return fmt.Errorf("models[] must not be empty")
	}
	seen := map[string]bool{}
	for i := range c.Models {
		m := &c.Models[i]
		if m.Name == "" {
			return fmt.Errorf("models[%d].name must not be empty", i)
		}
		if seen[m.Name] {
			return fmt.Errorf("duplicate model profile %q", m.Name)
		}
		seen[m.Name] = true
		if m.Provider != "openai-compatible" {
			return fmt.Errorf("models[%d].provider %q: only \"openai-compatible\" is supported (R1)", i, m.Provider)
		}
		if m.Model == "" {
			return fmt.Errorf("models[%d].model must not be empty", i)
		}
		u, err := url.Parse(m.APIBase)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("models[%d].apiBase %q is not a valid http(s) URL", i, m.APIBase)
		}
		// No apiKeyEnv/apiKey is a deliberate keyless endpoint; a *named*
		// env var that is unset with no literal fallback errors at
		// Model.APIKey() time so a typo'd name never silently goes keyless.
		if m.Options.Temperature != nil && (*m.Options.Temperature < 0 || *m.Options.Temperature > 2) {
			return fmt.Errorf("models[%d].requestOptions.temperature %v out of range [0,2]", i, *m.Options.Temperature)
		}
		if m.Options.MaxTokens < 0 {
			return fmt.Errorf("models[%d].requestOptions.maxTokens must not be negative", i)
		}
		if m.Options.Retries < 0 {
			return fmt.Errorf("models[%d].requestOptions.retries must not be negative", i)
		}
	}

	// The default profile must route to an existing entry.
	if _, err := c.Route(""); err != nil {
		return fmt.Errorf("run.profile: %w", err)
	}

	switch c.Elision.Mode {
	case "safe", "off":
	default:
		return fmt.Errorf("elision.mode %q: only \"safe\" or \"off\"", c.Elision.Mode)
	}
	if c.Concurrency.Workers < 1 {
		return fmt.Errorf("concurrency.workers must be >= 1, got %d", c.Concurrency.Workers)
	}
	if c.ValidateCfg.MaxRetries < 0 {
		return fmt.Errorf("validate.maxRetries must not be negative, got %d", c.ValidateCfg.MaxRetries)
	}

	for _, p := range []struct{ name, v string }{
		{"paths.tux", c.Paths.Tux},
		{"paths.logs", c.Paths.Logs},
		{"paths.audit", c.Paths.Audit},
		{"paths.state", c.Paths.State},
	} {
		if strings.TrimSpace(p.v) == "" {
			return fmt.Errorf("%s must not be empty", p.name)
		}
	}
	return nil
}
