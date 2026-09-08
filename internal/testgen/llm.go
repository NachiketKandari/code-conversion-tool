// The LLM gap-fill seam (PRD-2026-09-09 GT-D6): the one place gentest calls
// the AI. It sits inside the per-function worker — after the deterministic
// template proves unable to shape that function (field-mapping controllers)
// — and is gated by budget ceilings, a parse+shape gate, and bounded
// retries that feed gate failures back. The db layer never reaches it (SQL
// is known); handlers never reach it (the controller mock owns mapping).
package testgen

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"

	"github.com/Public/convert-tux-to-go/internal/llm"
)

// fillCtrlMethod fills one field-mapping controller's suite method through
// the LLM seam. Returns the block, the number of chat calls made, and the
// last error when every attempt was rejected.
func fillCtrlMethod(ctx context.Context, u *unit, opts Options) (string, int, error) {
	maxAttempts := opts.MaxRetries + 1
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	calls := 0
	var notes []string
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		prompt := ctrlUserPrompt(u, notes)
		if opts.Budget.MaxPromptTokens > 0 {
			if err := opts.Budget.CheckInput(prompt); err != nil {
				return "", calls, err
			}
		}
		resp, err := opts.Client.Chat(ctx, llm.ChatRequest{
			Messages: []llm.Message{
				{Role: "system", Content: ctrlSystemPrompt(u)},
				{Role: "user", Content: prompt},
			},
		})
		if err != nil {
			return "", calls, fmt.Errorf("llm chat: %w", err)
		}
		calls++
		if opts.Budget.MaxOutputTokens > 0 {
			if err := opts.Budget.CheckOutput(resp.Content); err != nil {
				notes = append(notes, "output over budget: "+err.Error())
				lastErr = err
				continue
			}
		}
		block := extractGoBlock(resp.Content)
		if err := gateCtrlBlock(block, u); err != nil {
			notes = append(notes, fmt.Sprintf("attempt %d rejected: %v", attempt+1, err))
			lastErr = err
			continue
		}
		return block, calls, nil
	}
	return "", calls, lastErr
}

func ctrlSystemPrompt(u *unit) string {
	return `You author Go unit tests for a converted service's controller layer.
Deliverable shape (violations are rejected by automated gates):
- Output ONLY ONE complete suite method: func (suite *` + u.suite + `) Test` + u.fn.Name + `() { ... }
- No package clause, no imports, no other functions, no comments outside the method.
- Table-driven: declare testCases := []struct{ desc string; mockInput []any; expectedError string; expectedOutput <response type> }{...} with a "StoreError" case (mockInput []any{nil, errors.New("store error")}) and a "Success" case.
- Iterate with for _, testCase := range testCases { suite.T().Run(testCase.desc, func(t *testing.T) { ... }) }.
- Mock ONLY with suite.storeMock (a gomock mock): suite.storeMock.EXPECT().<Method>(<args>).Return(testCase.mockInput...) — never invent other mocks.
- Call the controller exactly as the function under test does: suite.` + strings.ToLower(u.sc.name) + `Controller.` + u.fn.Name + `(suite.ctx, &request).
- Reproduce the function's field mapping EXACTLY: the Success expectedOutput must be what the function returns given the mocked rows — copy the mapping from the source, do not guess.
- Use only the suite fields, models types, and imports listed in the prompt (errors, sql, time are available). Never reference packages outside them.
- No commit/rollback, no t.Parallel, no time.Sleep.`
}

func ctrlUserPrompt(u *unit, notes []string) string {
	sc := u.sc
	f := u.ctrl
	var sb strings.Builder
	fmt.Fprintf(&sb, "Service: %s · suite: %s · controller field: suite.%s (interface %s) · store mock field: suite.storeMock (%s)\n\n",
		sc.name, u.suite, strings.ToLower(sc.name)+"Controller", ctrlIfaceName(sc), "db.Mock"+ctrlIfaceName(sc))
	sb.WriteString("Suite fields available: suite.ctx (context.Context), suite.storeMock, suite." + strings.ToLower(sc.name) + "Controller\n\n")
	sb.WriteString("Function under test (verbatim source):\n\n```go\n" + f.Src + "\n```\n\n")
	sb.WriteString("Dependencies to mock (in call order):\n")
	for _, c := range f.StoreCalls {
		fmt.Fprintf(&sb, "  - suite.storeMock.%s(%s) — first arg suite.ctx, remaining args as the source passes them\n", c.Method, strings.Join(append([]string{"suite.ctx"}, c.Args...), ", "))
		if lit := mockReturnLiteral(sc, c.Method); lit != "nil" {
			fmt.Fprintf(&sb, "    Return payload shape for the Success case: %s\n", lit)
		}
	}
	if len(f.StoreCalls) > 0 {
		if df := sc.dbFacts.DB[f.StoreCalls[0].Method]; df != nil && df.RowType != "" {
			fmt.Fprintf(&sb, "\nRow struct shape (db tags → fields):\n")
			for _, fl := range sc.models.Structs[structBase(df.RowType)] {
				if fl.DB != "" {
					fmt.Fprintf(&sb, "  - %s %s (db tag %s)\n", fl.Name, fl.Type, fl.DB)
				}
			}
		}
	}
	fmt.Fprintf(&sb, "\nRequest type: models.%s — assumed request value: %s\n", structBase(f.RequestType), requestLiteral(sc, f.RequestType))
	fmt.Fprintf(&sb, "Response type: %s — assumed Success expectedOutput value: %s\n", f.ResponseType, responseLiteral(sc, f.ResponseType))
	sb.WriteString("\nRules: the error case must exercise the store error path; the Success case must assert the exact mapped output (assert.Equal(t, testCase.expectedOutput, data)); multi-call flows EXPECT every call in order with matching Return payloads.")
	if len(notes) > 0 {
		sb.WriteString("\n\nEarlier attempts failed these gate checks — fix every listed problem:\n")
		for _, n := range notes {
			sb.WriteString("  - " + n + "\n")
		}
	}
	return sb.String()
}

// extractGoBlock takes the first ```go fenced block, else the raw text,
// trimmed of blank padding.
func extractGoBlock(content string) string {
	trimNL := func(s string) string { return strings.Trim(s, "\r\n") }
	if i := strings.Index(content, "```go"); i >= 0 {
		rest := content[i+len("```go"):]
		if j := strings.Index(rest, "```"); j >= 0 {
			return trimNL(rest[:j])
		}
		return trimNL(rest)
	}
	if i := strings.Index(content, "```"); i >= 0 {
		rest := content[i+len("```"):]
		if j := strings.Index(rest, "```"); j >= 0 {
			return trimNL(rest[:j])
		}
	}
	return trimNL(content)
}

// gateCtrlBlock is the contract gate for LLM blocks: the block must parse
// as one method on the right receiver with the right name, and must carry
// the table-driven shape.
func gateCtrlBlock(block string, u *unit) error {
	if !strings.Contains(block, "testCases") {
		return fmt.Errorf("block is not table-driven (no testCases declaration)")
	}
	src := "package gentestgate\n\n" + block
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, "block.go", src, parser.SkipObjectResolution)
	if err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	count := 0
	for _, d := range af.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Recv == nil || len(fd.Recv.List) == 0 {
			continue
		}
		recv := ""
		switch t := fd.Recv.List[0].Type.(type) {
		case *ast.StarExpr:
			if id, ok := t.X.(*ast.Ident); ok {
				recv = id.Name
			}
		case *ast.Ident:
			recv = t.Name
		}
		if recv != u.suite || fd.Name.Name != "Test"+u.fn.Name {
			return fmt.Errorf("want one method func (suite *%s) Test%s(), got func on %s named %s", u.suite, u.fn.Name, recv, fd.Name.Name)
		}
		count++
	}
	if count != 1 {
		return fmt.Errorf("want exactly one suite method, got %d", count)
	}
	return nil
}
