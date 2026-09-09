package main

import (
	"context"

	"github.com/Public/convert-tux-to-go/internal/audit"
	"github.com/Public/convert-tux-to-go/internal/budget"
	"github.com/Public/convert-tux-to-go/internal/config"
	"github.com/Public/convert-tux-to-go/internal/telemetry"
)

// runWiring is the run-wide collaborator bundle (A5.2): one budget and one
// audit recorder cover every service/file in a run — the budget is
// immutable, the recorder mutex-guarded. Built once per command from the
// run config; the LLM client stays per-purpose (resolveLLMClient) because
// its disable log names the seam.
type runWiring struct {
	cfg    *config.Config
	budget budget.Budget
	audit  *audit.Recorder // nil when the archive folder is unavailable
}

// newWiring resolves the bundle. Audit degradation is visible (WARN), never
// fatal — the audit trail is best-effort by contract (§4.7).
func newWiring(ctx context.Context, cfg *config.Config) *runWiring {
	log := telemetry.Log(ctx)
	w := &runWiring{
		cfg:    cfg,
		budget: budget.New(cfg.Run.MaxPromptTokens, cfg.Run.MaxOutputTokens, cfg.Run.CharsPerToken),
	}
	rec, err := audit.New(auditDir, telemetry.RunIDFromContext(ctx))
	if err != nil {
		log.Warn("audit archive unavailable", "error", err)
		return w
	}
	w.audit = rec
	return w
}

// auditRecorder resolves the run's audit recorder with the standard degrade
// (WARN + nil) for call sites that carry no config — archive helpers.
func auditRecorder(ctx context.Context) *audit.Recorder {
	rec, err := audit.New(auditDir, telemetry.RunIDFromContext(ctx))
	if err != nil {
		telemetry.Log(ctx).Warn("audit archive unavailable", "error", err)
		return nil
	}
	return rec
}
