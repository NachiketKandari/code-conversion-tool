package flow

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/Public/convert-tux-to-go/internal/cproc/ir"
)

// Candidate is one discovered API candidate (PRD-2026-09-10 endpoint
// discovery): a block enclosing request reads and non-error response
// writes. Key is the stable draft reference — "c<n>" for a top-level
// condition, "c<n>.<k>" for the k-th qualifying nested branch under it.
type Candidate struct {
	Key             string   `json:"key"`
	ParentCondition int      `json:"parent_condition"`
	Cond            string   `json:"cond,omitempty"`
	StartLine       int      `json:"start_line"`
	EndLine         int      `json:"end_line"`
	Gets            []string `json:"gets,omitempty"`
	Adds            []string `json:"adds,omitempty"`
	ErrorAdds       []string `json:"error_adds,omitempty"`
	QueryIDs        []string `json:"query_ids,omitempty"`
	Redundant       bool     `json:"redundant,omitempty"`
}

// Discover walks the tree and returns the API candidates sorted by line:
// top-level branches matched against the condition inventory (key c<n>),
// plus qualifying nested branches (key c<n>.<k>). An if whose adds are all
// error emissions fails the rubric — it is a guard (the read collapses into
// the request struct), never an endpoint.
func Discover(tree *Tree, conditions []ir.Condition) []Candidate {
	var out []Candidate
	// walkChildren examines branch children under a qualifying parent key.
	var walkChildren func(n *Node, parentKey string, parentIdx int, parentCand *Node)
	walkChildren = func(n *Node, parentKey string, parentIdx int, parentCand *Node) {
		k := 0
		for _, c := range n.Children {
			if c.Kind != KindBranch {
				walkChildren(c, parentKey, parentIdx, parentCand)
				continue
			}
			census := fmlCensus(c)
			if len(census.gets) == 0 || len(census.adds) == 0 {
				// Guard or logic-only — not a candidate, but its subtree can
				// hold candidates under the same parent.
				walkChildren(c, parentKey, parentIdx, parentCand)
				continue
			}
			k++
			cand := Candidate{
				Key:             fmt.Sprintf("%s.%d", parentKey, k),
				ParentCondition: parentIdx,
				Cond:            c.Cond,
				StartLine:       c.Line,
				EndLine:         c.EndLine,
				Gets:            census.gets,
				Adds:            census.adds,
				ErrorAdds:       census.errorAdds,
				QueryIDs:        c.QueryIDs,
			}
			if parentCand != nil {
				pc := fmlCensus(parentCand)
				cand.Redundant = subset(census.gets, pc.gets) && subset(census.adds, pc.adds)
			}
			out = append(out, cand)
			walkChildren(c, cand.Key, parentIdx, c)
		}
	}
	for _, root := range tree.Root {
		if root.Kind != KindBranch {
			walkChildren(root, "", 0, nil)
			continue
		}
		census := fmlCensus(root)
		idx := conditionIndexOf(root, conditions)
		if len(census.gets) == 0 || len(census.adds) == 0 || idx == 0 {
			walkChildren(root, "", 0, nil)
			continue
		}
		out = append(out, Candidate{
			Key:             "c" + strconv.Itoa(idx),
			ParentCondition: idx,
			Cond:            root.Cond,
			StartLine:       root.Line,
			EndLine:         root.EndLine,
			Gets:            census.gets,
			Adds:            census.adds,
			ErrorAdds:       census.errorAdds,
			QueryIDs:        root.QueryIDs,
		})
		walkChildren(root, "c"+strconv.Itoa(idx), idx, root)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartLine < out[j].StartLine })
	return out
}

// conditionIndexOf matches a root branch node to its 1-based condition
// inventory index by line containment, preferring the smallest containing
// span (an `} else if` header line sits inside the previous condition's
// extent too).
func conditionIndexOf(n *Node, conditions []ir.Condition) int {
	best := 0
	bestSize := 1 << 30
	for i := range conditions {
		c := &conditions[i]
		if n.Line >= c.StartLine && n.Line <= c.EndLine {
			if size := c.EndLine - c.StartLine; size < bestSize {
				best, bestSize = c.Index, size
			}
		}
	}
	return best
}

func subset(child, parent []string) bool {
	if len(parent) == 0 {
		return false
	}
	pm := map[string]bool{}
	for _, s := range parent {
		pm[s] = true
	}
	for _, s := range child {
		if !pm[s] {
			return false
		}
	}
	return true
}

// fmlCensus splits a node's FML ops into distinct request reads, distinct
// non-error response writes, and the error emissions (DIS-D2).
func fmlCensus(n *Node) struct {
	gets      []string
	adds      []string
	errorAdds []string
} {
	seenGet := map[string]bool{}
	seenAdd := map[string]bool{}
	seenErr := map[string]bool{}
	var out struct {
		gets      []string
		adds      []string
		errorAdds []string
	}
	for _, op := range n.FmlOps {
		switch {
		case op.Kind == ir.FmlGet:
			if !seenGet[op.Field] {
				seenGet[op.Field] = true
				out.gets = append(out.gets, op.Field)
			}
		case op.Kind == ir.FmlAdd && isErrorAdd(op, n):
			if !seenErr[op.Field] {
				seenErr[op.Field] = true
				out.errorAdds = append(out.errorAdds, op.Field)
			}
		case op.Kind == ir.FmlAdd:
			if !seenAdd[op.Field] {
				seenAdd[op.Field] = true
				out.adds = append(out.adds, op.Field)
			}
		}
	}
	return out
}

// isErrorAdd reports an error-emission add: an ERR field or an add into a
// buffer with a known input/send role (the "fadd err = returning error"
// idiom). Unknown-role buffers stay conservative — a plain add is a
// response write unless proven otherwise.
func isErrorAdd(op ir.FmlOp, n *Node) bool {
	if strings.Contains(strings.ToUpper(op.Field), "ERR") {
		return true
	}
	if role, ok := n.BufRoles[op.Buffer]; ok && (role == "input" || role == "send") {
		return true
	}
	return false
}

// Census summarizes a condition's FML ops with the discovery error-add rule
// (DIS-D2, ERR-field convention — buffer roles need the flow node, so this
// inventory-level helper is the approximation the draft comments use). It
// works for any condition, qualifying or not.
func Census(c *ir.Condition) (gets, adds, errs []string) {
	seen := map[string]map[string]bool{"/g": {}, "/a": {}, "/e": {}}
	add := func(k, s string) []string {
		if !seen[k][s] {
			seen[k][s] = true
			switch k {
			case "/g":
				gets = append(gets, s)
			case "/a":
				adds = append(adds, s)
			default:
				errs = append(errs, s)
			}
		}
		return nil
	}
	for _, op := range c.FmlOps {
		switch {
		case op.Kind == ir.FmlGet:
			add("/g", op.Field)
		case op.Kind == ir.FmlAdd && strings.Contains(strings.ToUpper(op.Field), "ERR"):
			add("/e", op.Field)
		case op.Kind == ir.FmlAdd:
			add("/a", op.Field)
		}
	}
	return gets, adds, errs
}

// ConditionFor re-derives the candidate a draft reference names (DIS-D3):
// "c<n>" resolves to the top-level branch matching condition n's span,
// "c<n>.<k>" to its k-th qualifying nested branch. The returned condition
// is synthesized (Index 0 — never collides with the 1-based inventory) with
// Error-flagged FML ops so contract derivation can exclude error emissions.
func ConditionFor(tree *Tree, conditions []ir.Condition, ref string) (*ir.Condition, error) {
	segments := strings.Split(ref, ".")
	if len(segments) == 0 || !strings.HasPrefix(segments[0], "c") {
		return nil, fmt.Errorf("flow: malformed candidate reference %q (want c<n> or c<n>.<k>)", ref)
	}
	n, err := strconv.Atoi(strings.TrimPrefix(segments[0], "c"))
	if err != nil || n < 1 || n > len(conditions) {
		return nil, fmt.Errorf("flow: candidate reference %q — condition inventory has %d condition(s)", ref, len(conditions))
	}
	c := &conditions[n-1]
	current := rootFor(tree, c)
	if current == nil {
		return nil, fmt.Errorf("flow: candidate reference %q — no branch node matches condition %d's span", ref, n)
	}
	for _, seg := range segments[1:] {
		k, err := strconv.Atoi(seg)
		if err != nil || k < 1 {
			return nil, fmt.Errorf("flow: malformed candidate reference %q", ref)
		}
		found := kthQualifyingChild(current, k)
		if found == nil {
			return nil, fmt.Errorf("flow: candidate reference %q — branch at line %d has fewer qualifying nested branch(es)", ref, current.Line)
		}
		current = found
	}
	return synthCondition(current), nil
}

// rootFor matches a condition's span to its root branch node.
func rootFor(tree *Tree, c *ir.Condition) *Node {
	for _, root := range tree.Root {
		if root.Kind == KindBranch && root.Line >= c.StartLine && root.Line <= c.EndLine {
			return root
		}
	}
	return nil
}

// kthQualifyingChild replays Discover's key assignment: the k-th qualifying
// branch in the same depth-first order the walk used for keys.
func kthQualifyingChild(n *Node, k int) *Node {
	count := 0
	var walk func(n *Node) *Node
	walk = func(n *Node) *Node {
		for _, c := range n.Children {
			if c.Kind != KindBranch {
				if f := walk(c); f != nil {
					return f
				}
				continue
			}
			census := fmlCensus(c)
			if len(census.gets) == 0 || len(census.adds) == 0 {
				if f := walk(c); f != nil {
					return f
				}
				continue
			}
			count++
			if count == k {
				return c
			}
		}
		return nil
	}
	return walk(n)
}

// synthCondition converts a branch node into the condition-shaped value the
// plan/gen machinery consumes. Index 0 marks it synthesized (DIS-D4).
func synthCondition(n *Node) *ir.Condition {
	c := &ir.Condition{
		Index:     0,
		Kind:      n.Sub,
		Expr:      n.Cond,
		StartLine: n.Line,
		EndLine:   n.EndLine,
		QueryIDs:  n.QueryIDs,
	}
	if n.Predicate != nil {
		e := *n.Predicate
		c.Predicate = &e
	}
	for _, op := range n.FmlOps {
		o := op
		if isErrorAdd(o, n) {
			o.Error = true
		}
		c.FmlOps = append(c.FmlOps, o)
	}
	return c
}
