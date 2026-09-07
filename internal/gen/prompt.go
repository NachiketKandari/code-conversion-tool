package gen

import (
	"fmt"
	"slices"
	"strings"

	"github.com/Public/convert-tux-to-go/internal/budget"
	"github.com/Public/convert-tux-to-go/internal/cproc/ir"
	"github.com/Public/convert-tux-to-go/internal/plan"
	"github.com/Public/convert-tux-to-go/internal/templates"
)

// ConditionOf returns the condition backing an endpoint name.
func (s *Service) ConditionOf(endpoint string) *ir.Condition {
	for _, e := range s.Mapping.Endpoints {
		if e.Name == endpoint {
			return s.conditionOf(e)
		}
	}
	return nil
}

// BranchCalls returns the condition's canonical queries re-based to the
// branch slice plus their resolved DBCall map — the input budget.ReplaceQueries
// needs to rewrite the branch view (plan-conversion §4 step 4).
func (s *Service) BranchCalls(c *ir.Condition, p *plan.Plan) ([]*ir.Query, map[string]budget.DBCall, error) {
	// Collect the branch's queries (explicit references, else line overlap).
	var queries []*ir.Query
	seen := map[string]bool{}
	ids := c.QueryIDs
	if len(ids) == 0 {
		for _, q := range s.Main.Queries {
			if q.StartLine >= c.StartLine && q.StartLine <= c.EndLine {
				ids = append(ids, q.ID)
			}
		}
	}
	// DBCall per referenced site: the canonical plan unit names the method.
	dbUnitByQuery := map[string]plan.Unit{}
	for _, u := range p.Units {
		if u.Kind == plan.KindDBMethod && len(u.QueryIDs) > 0 {
			dbUnitByQuery[u.QueryIDs[0]] = u
		}
	}
	calls := make(map[string]budget.DBCall, len(ids))
	for _, id := range ids {
		orig := s.queries[id]
		if orig == nil {
			return nil, nil, fmt.Errorf("gen: condition %d references unknown query %q", c.Index, id)
		}
		// The canonical unit (dedup target) names the method; the physical
		// SQL region replaced in THIS branch is the referenced site's —
		// a shared query (q5→q3) has its own lines in each branch.
		canon := orig
		if orig.DuplicateOf != "" {
			canon = s.queries[orig.DuplicateOf]
		}
		if seen[orig.ID] {
			continue
		}
		seen[orig.ID] = true
		lq := *orig
		lq.StartLine -= c.StartLine - 1
		lq.EndLine -= c.StartLine - 1
		queries = append(queries, &lq)
		u, ok := dbUnitByQuery[canon.ID]
		if !ok {
			return nil, nil, fmt.Errorf("gen: query %s has no db plan unit", canon.ID)
		}
		pin, _ := s.Pin(canon.ID)
		params, err := s.dbParams(canon, pin)
		if err != nil {
			return nil, nil, err
		}
		args := make([]string, len(params))
		for j, pp := range params {
			args[j] = pp.Name
		}
		calls[orig.ID] = budget.DBCall{Receiver: "s.store", Name: u.Name, CtxName: "c", Args: args}
	}
	return queries, calls, nil
}

// ControllerPromptContext renders the fixed contract a controller prompt
// consumes: the exact method signature (named returns data/err), the
// endpoint's request/response structs, and the row structs its store calls
// return — all verbatim, so the model references parameter and field names
// exactly instead of guessing them (live-smoke finding: ActiveFlg vs
// CActiveFlag, req vs request).
func (s *Service) ControllerPromptContext(endpoint string, p *plan.Plan, storeMethods []string) (string, error) {
	c := s.ConditionOf(endpoint)
	if c == nil {
		return "", fmt.Errorf("gen: no condition for endpoint %s", endpoint)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "func (s *%s) %s(c context.Context, request *models.%s) (data []*models.%s, err error)\n",
		lowerFirst(s.Mapping.Service)+"Controller", endpoint, s.requestType(endpoint), s.responseType(endpoint))
	sb.WriteString("\ntype " + s.requestType(endpoint) + " struct {\n")
	sb.WriteString(indentFields(contractFields(c.FmlOps, ir.FmlGet)))
	sb.WriteString("}\n")
	sb.WriteString("\ntype " + s.responseType(endpoint) + " struct {\n")
	sb.WriteString(indentFields(contractFields(c.FmlOps, ir.FmlAdd)))
	sb.WriteString("}\n")

	rows := map[string]bool{}
	for _, u := range p.Units {
		if u.Kind != plan.KindDBMethod || len(u.QueryIDs) == 0 || !slices.Contains(storeMethods, u.Name) {
			continue
		}
		q := s.Query(u.QueryIDs[0])
		if q == nil || isCountQuery(q) {
			continue
		}
		row := s.RowName(u.QueryIDs[0], u.Name)
		if rows[row] {
			continue
		}
		fields, err := s.rowFields(q)
		if err != nil {
			return "", err
		}
		rows[row] = true
		sb.WriteString("\ntype " + row + " struct {\n")
		sb.WriteString(indentFields(fields))
		sb.WriteString("}\n")
	}

	// External interactions surface as compilable placeholders (PF-4.5): the
	// body calls the stub instead of inventing an outbound call. Endpoints
	// without tpcalls keep prompts unchanged.
	if sigs := s.PlaceholderSignatures(c, p); len(sigs) > 0 {
		sb.WriteString("\nexternal service call placeholders (call these instead of the legacy tpcall; each returns an error — handle it like a store error and return nil, err):\n")
		for _, sig := range sigs {
			sb.WriteString(sig + "\n")
		}
	}
	return sb.String(), nil
}

// indentFields renders struct fields gofmt-shaped.
func indentFields(fields []templates.FieldSpec) string {
	var sb strings.Builder
	for _, f := range fields {
		sb.WriteString("\t" + f.Name + " " + f.Type)
		if tag := f.Tag(); tag != "" {
			sb.WriteString(" `" + tag + "`")
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// RenderControllerMethod wraps an accepted LLM body in the controller
// method template — the template owns the shape, the LLM owns only the
// template-shaped gap.
func (s *Service) RenderControllerMethod(endpoint, body string) (string, error) {
	return render(templates.ControllerMethod, templates.ControllerMethodData{
		StructName:   lowerFirst(s.Mapping.Service) + "Controller",
		Name:         endpoint,
		CtxName:      "c",
		RequestType:  "models." + s.requestType(endpoint),
		ResponseType: "models." + s.responseType(endpoint),
		Body:         strings.TrimRight(body, "\n"),
	})
}
