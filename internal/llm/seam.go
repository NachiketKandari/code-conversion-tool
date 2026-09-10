package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Public/convert-tux-to-go/internal/audit"
	"github.com/Public/convert-tux-to-go/internal/budget"
	"github.com/Public/convert-tux-to-go/internal/telemetry"
)

// SeamInput describes one bounded-retry LLM seam call. RunSeam owns the
// skeleton every LLM seam shares (attempt clamping, budget ceilings, the
// chat call, exchange archiving, gate-feedback accumulation, the retry
// loop); the caller owns its per-seam policy: message assembly, payload
// extraction, gates, and the error posture (AbortOnChatError).
type SeamInput struct {
	// Unit, Kind, Name identify the call in the audit trail; Template and
	// LLM are optional plan-backed extras. Attempt, Prompt, Response,
	// Errors, and Outcome are owned by the runner.
	Unit     string
	Kind     string
	Name     string
	Template string
	LLM      bool

	// File overrides the audit artifact name per attempt; nil keeps the
	// default "<kind>-<name>-attempt<n>.json" convention.
	File func(attempt int) string

	// Audit archives every attempt when set; nil skips archiving.
	Audit *audit.Recorder

	Client      Client
	Budget      budget.Budget
	MaxRetries  int     // additional attempts beyond the first
	Temperature float64 // passed through verbatim (endpoint defaults are the client's wiring)

	// MaxTokens overrides the endpoint's per-request output cap when > 0
	// (0 = endpoint default). Thinking-mode models spend part of the cap on
	// reasoning that never reaches content, so seams that need code room
	// raise it; the Budget.CheckOutput ceiling on the extracted content is
	// unaffected.
	MaxTokens int

	// Prompt builds this attempt's messages. notes carries the accumulated
	// rejection notes from earlier attempts (empty on the first). The first
	// return value is the prompt text archived in the audit trail.
	Prompt func(notes []string) (archived string, messages []Message)

	// Extract turns the raw response content into the seam payload (fence
	// stripping, JSON selection); nil = identity.
	Extract func(content string) string

	// Gate validates the extracted payload; the returned strings are the
	// rejection reasons archived and accumulated into notes for the next
	// attempt. nil = any in-budget response is accepted.
	Gate func(payload string) []string

	// AbortOnChatError returns the chat error immediately (after archiving)
	// instead of feeding it back and retrying. Transport failures usually
	// mean the endpoint is down, so seams that cannot make progress abort;
	// advisory seams (convert controllers, discover naming) retry.
	AbortOnChatError bool
}

// RunSeam executes one bounded-retry LLM seam call. It returns the accepted
// payload (Extract applied to the raw response), the number of chat calls
// made, the accumulated rejection notes, and the failure error when every
// attempt was rejected or a caller-policy abort fired.
func RunSeam(ctx context.Context, in SeamInput) (payload string, calls int, notes []string, err error) {
	maxAttempts := in.MaxRetries + 1
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	extract := in.Extract
	if extract == nil {
		extract = func(content string) string { return content }
	}
	record := func(attempt int, prompt, response string, errs []string) {
		if in.Audit == nil {
			return
		}
		e := audit.Exchange{
			Unit: in.Unit, Kind: in.Kind, Name: in.Name, Attempt: attempt,
			Template: in.Template, LLM: in.LLM,
			Prompt: prompt, Response: response, Errors: errs, Outcome: "ok",
		}
		if len(errs) > 0 {
			e.Outcome = "failed"
		}
		if in.File != nil {
			e.File = in.File(attempt)
		}
		if _, werr := in.Audit.WriteExchange(e); werr != nil {
			telemetry.Log(ctx).Warn("audit exchange failed", "kind", in.Kind, "name", in.Name, "error", werr)
		}
	}
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		archived, messages := in.Prompt(notes)
		if in.Budget.MaxPromptTokens > 0 {
			if berr := in.Budget.CheckInput(archived); berr != nil {
				return "", calls, notes, berr
			}
		}
		resp, cerr := in.Client.Chat(ctx, ChatRequest{
			Model:       "", // endpoint default (resolved by the client's wiring)
			Messages:    messages,
			Temperature: in.Temperature,
			MaxTokens:   in.MaxTokens,
		})
		calls++
		if cerr != nil {
			lastErr = cerr
			record(attempt, archived, "", []string{cerr.Error()})
			if in.AbortOnChatError {
				return "", calls, notes, fmt.Errorf("llm chat: %w", cerr)
			}
			notes = append(notes, cerr.Error())
			continue
		}
		if in.Budget.MaxOutputTokens > 0 {
			if berr := in.Budget.CheckOutput(resp.Content); berr != nil {
				lastErr = berr
				record(attempt, archived, resp.Content, []string{berr.Error()})
				notes = append(notes, "output over budget: "+berr.Error())
				continue
			}
		}
		payload = extract(resp.Content)
		var gateErrs []string
		if in.Gate != nil {
			gateErrs = in.Gate(payload)
		}
		if len(gateErrs) > 0 {
			lastErr = errors.New(strings.Join(gateErrs, "; "))
			record(attempt, archived, resp.Content, gateErrs)
			notes = append(notes, gateErrs...)
			continue
		}
		record(attempt, archived, resp.Content, nil)
		return payload, calls, notes, nil
	}
	if lastErr == nil {
		lastErr = errors.New("all attempts rejected")
	}
	return "", calls, notes, lastErr
}
