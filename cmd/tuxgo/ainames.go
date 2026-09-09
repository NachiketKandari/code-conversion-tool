package main

import (
	"context"
	"encoding/json"
	"fmt"
	"go/token"
	"log/slog"
	"strings"

	"github.com/Public/convert-tux-to-go/internal/budget"
	"github.com/Public/convert-tux-to-go/internal/cproc/flow"
	"github.com/Public/convert-tux-to-go/internal/cproc/ir"
	"github.com/Public/convert-tux-to-go/internal/llm"
)

// aiSuggestion is the per-candidate naming proposal (PRD-2026-09-10
// endpoint discovery, user directive: AI-decided draft defaults with a
// deterministic fallback). The draft stays the user's decision — every
// value is advisory and editable. Deterministic marks the no-model
// fallback: names picked from the strongest semantic token source
// available (cursor name → response fields → condition index).
type aiSuggestion struct {
	Name          string
	Route         string
	Methods       map[string]methodPinSuggestion // query ID → pin proposal
	Deterministic bool
}

// methodPinSuggestion proposes a store method name (+ row struct for
// selects). Params are never proposed — the IR derives them from the query
// binds deterministically.
type methodPinSuggestion struct {
	Name string
	Row  string
}

const aiNameSystem = `You name API endpoints and database store methods when converting legacy Tuxedo Pro*C services to idiomatic Go.
Respond ONLY with one JSON object — no prose, no markdown fences:
{"name":"<exported Go method name>","route":"/kebab-or-legacy-route","dbMethods":[{"id":"<query id>","name":"<exported Go method name>","row":"<exported row struct name, empty for non-select queries>"}]}
Rules:
- name/route describe what the endpoint does for its caller; dbMethods names describe what each query reads or writes.
- All names are concise CamelCase Go identifiers (no underscores, no abbreviations you cannot justify from the code).
- One dbMethods entry per query id listed, same id, nothing invented.
- row is filled only for SELECT queries (the row struct the query's columns map to).`

// aiNameEndpoints proposes draft names for every candidate. With a client:
// one LLM call per candidate endpoint carrying the branch source, its FML
// reads/writes, and the involved query SQL. Without one (or per-candidate
// on failure): the deterministic picker. The census and draft emission
// never depend on the LLM.
func aiNameEndpoints(ctx context.Context, log *slog.Logger, client llm.Client, b budget.Budget, f *ir.File, tree *flow.Tree, candidates []flow.Candidate, src []byte) map[string]aiSuggestion {
	out := make(map[string]aiSuggestion, len(candidates))
	queriesByID := make(map[string]*ir.Query, len(f.Queries))
	for _, q := range f.Queries {
		queriesByID[q.ID] = q
	}
	if client == nil {
		for _, c := range candidates {
			out[c.Key] = deterministicSuggestion(c)
		}
		return out
	}
	lines := strings.Split(string(src), "\n")
	for _, c := range candidates {
		cond, err := flow.ConditionFor(tree, f.Conditions, c.Key)
		if err != nil {
			out[c.Key] = deterministicSuggestion(c)
			continue
		}
		sug, err := aiNameOne(ctx, log, client, b, f, cond, queriesByID, lines)
		if err != nil {
			log.Warn("ai naming failed — deterministic names used", "candidate", c.Key, "error", err)
			out[c.Key] = deterministicSuggestion(c)
			continue
		}
		out[c.Key] = sug
	}
	return out
}

// deterministicSuggestion picks draft defaults without any model: the best
// semantic token source wins — a cursor query's name (cur_mf_nav_hist →
// GetMfNavHist), then the first response field (FML_MF_NAV_DATE →
// GetMfNavDate), then the first read, then the condition index.
func deterministicSuggestion(c flow.Candidate) aiSuggestion {
	name := ""
	for _, id := range c.QueryIDs {
		if len(id) > 4 && strings.EqualFold(id[:4], "cur_") {
			name = "Get" + camelName(id[4:])
			break
		}
	}
	if name == "" {
		for _, src := range [][]string{c.Adds, c.Gets} {
			for _, fld := range src {
				fld = strings.ToUpper(fld)
				fld = strings.TrimPrefix(fld, "FML_")
				if camel := camelName(fld); camel != "" {
					name = "Get" + camel
					break
				}
			}
			if name != "" {
				break
			}
		}
	}
	if name == "" {
		name = "Endpoint" + strings.SplitN(strings.TrimPrefix(c.Key, "c"), ".", 2)[0]
	}
	sug := aiSuggestion{Name: name, Route: "/" + kebabName(name), Deterministic: true}
	sug.Methods = map[string]methodPinSuggestion{}
	return sug
}

// camelName turns a delimited C identifier into an exported CamelCase Go
// name ("mf_nav_hist" → "MfNavHist"); digits ride along.
func camelName(s string) string {
	var sb strings.Builder
	upperNext := true
	for _, r := range s {
		if !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9') {
			upperNext = true
			continue
		}
		if upperNext {
			sb.WriteRune(r - 'a' + 'A')
			upperNext = false
			continue
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

// kebabName renders a Go name as a route segment ("GetMfNavHist" →
// "mf-nav-hist").
func kebabName(name string) string {
	var sb strings.Builder
	for i, r := range name {
		if 'A' <= r && r <= 'Z' {
			if i > 0 {
				sb.WriteByte('-')
			}
			sb.WriteRune(r - 'A' + 'a')
			continue
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

func aiNameOne(ctx context.Context, log *slog.Logger, client llm.Client, b budget.Budget, f *ir.File, cond *ir.Condition, queriesByID map[string]*ir.Query, lines []string) (aiSuggestion, error) {
	var sb strings.Builder
	sb.WriteString("Endpoint branch (legacy C):\n\n")
	sb.WriteString(branchSlice(lines, cond.StartLine, cond.EndLine) + "\n\n")
	var gets, adds []string
	for _, op := range cond.FmlOps {
		switch op.Kind {
		case ir.FmlGet:
			gets = append(gets, op.Field)
		case ir.FmlAdd:
			if !op.Error {
				adds = append(adds, op.Field)
			}
		}
	}
	fmt.Fprintf(&sb, "Request fields read: %s\n", strings.Join(gets, ", "))
	fmt.Fprintf(&sb, "Response fields written: %s\n", strings.Join(adds, ", "))
	sb.WriteString("\nQueries (id — kind — SQL):\n")
	for _, id := range cond.QueryIDs {
		q := queriesByID[id]
		if q == nil {
			continue
		}
		fmt.Fprintf(&sb, "%s (%s): %s\n", id, q.Type, oneLine(q.SQL))
	}

	prompt := sb.String()
	if err := b.CheckInput(prompt); err != nil {
		return aiSuggestion{}, err
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		resp, err := client.Chat(ctx, llm.ChatRequest{
			Model:       "",
			Messages:    []llm.Message{{Role: "system", Content: aiNameSystem}, {Role: "user", Content: prompt}},
			Temperature: 0.2,
		})
		if err != nil {
			lastErr = err
			continue
		}
		if err := b.CheckOutput(resp.Content); err != nil {
			lastErr = err
			continue
		}
		sug, perr := parseNameJSON(resp.Content, cond.QueryIDs)
		if perr != nil {
			lastErr = perr
			continue
		}
		log.Debug("ai naming proposal", "condition", cond.Index, "name", sug.Name)
		return sug, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no response")
	}
	return aiSuggestion{}, lastErr
}

// parseNameJSON extracts the JSON object from the model output (fence- or
// prose-tolerant: the first '{' to the last '}') and validates every field —
// identifiers must be valid Go identifiers, routes must start with /, and
// only query ids the branch actually references are accepted.
func parseNameJSON(content string, allowedQueryIDs []string) (aiSuggestion, error) {
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start < 0 || end <= start {
		return aiSuggestion{}, fmt.Errorf("no JSON object in the response")
	}
	var raw struct {
		Name      string `json:"name"`
		Route     string `json:"route"`
		DBMethods []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Row  string `json:"row"`
		} `json:"dbMethods"`
	}
	if err := json.Unmarshal([]byte(content[start:end+1]), &raw); err != nil {
		return aiSuggestion{}, fmt.Errorf("parsing naming JSON: %w", err)
	}
	sug := aiSuggestion{Methods: map[string]methodPinSuggestion{}}
	if token.IsIdentifier(raw.Name) && token.IsExported(raw.Name) {
		sug.Name = raw.Name
	}
	if strings.HasPrefix(raw.Route, "/") && len(raw.Route) > 1 {
		sug.Route = raw.Route
	}
	allowed := map[string]bool{}
	for _, id := range allowedQueryIDs {
		allowed[id] = true
	}
	for _, m := range raw.DBMethods {
		if !allowed[m.ID] || !token.IsIdentifier(m.Name) || !token.IsExported(m.Name) {
			continue
		}
		pin := methodPinSuggestion{Name: m.Name}
		if m.Row != "" && token.IsIdentifier(m.Row) && token.IsExported(m.Row) {
			pin.Row = m.Row
		}
		sug.Methods[m.ID] = pin
	}
	return sug, nil
}

// branchSlice slices the 1-based inclusive line range out of the source.
func branchSlice(lines []string, from, to int) string {
	if from < 1 {
		from = 1
	}
	if to > len(lines) {
		to = len(lines)
	}
	if from > to {
		return ""
	}
	return strings.Join(lines[from-1:to], "\n")
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
