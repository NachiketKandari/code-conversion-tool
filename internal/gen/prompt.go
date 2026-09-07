package gen

import (
	"fmt"
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

// ContractOf renders the request/response field lists a controller prompt
// consumes (decision 10: Fget32 = inputs, Fadd32 = outputs).
func (s *Service) ContractOf(endpoint string) (request, response string) {
	c := s.ConditionOf(endpoint)
	if c == nil {
		return "", ""
	}
	var req, resp []string
	for _, f := range contractFields(c.FmlOps, ir.FmlGet) {
		req = append(req, fmt.Sprintf("%s %s `json:%q`", f.Name, f.Type, f.JSONTag))
	}
	for _, f := range contractFields(c.FmlOps, ir.FmlAdd) {
		resp = append(resp, fmt.Sprintf("%s %s `json:%q`", f.Name, f.Type, f.JSONTag))
	}
	return strings.Join(req, "\n"), strings.Join(resp, "\n")
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
