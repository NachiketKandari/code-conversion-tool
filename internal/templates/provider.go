package templates

import (
	"fmt"
	"strings"
	"sync"
	"text/template"
)

// Provider is the template seam (pipeline-three-parts Part 3): the pipeline
// consumes this interface; the embedded set is the MVP default.
type Provider interface {
	// Render executes the template id against data and returns the result.
	Render(id ID, data any) (string, error)
}

// EmbeddedProvider renders the go:embed'ed, versioned template set distilled
// from the golden nav example (PRD §4.8.6).
type EmbeddedProvider struct {
	mu    sync.Mutex
	cache map[ID]*template.Template
}

// NewEmbeddedProvider returns the default embedded template provider.
func NewEmbeddedProvider() *EmbeddedProvider { return &EmbeddedProvider{} }

// Render implements Provider.
func (p *EmbeddedProvider) Render(id ID, data any) (string, error) {
	t, err := p.get(id)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	if err := t.Execute(&sb, data); err != nil {
		return "", fmt.Errorf("templates: execute %s: %w", id, err)
	}
	return sb.String(), nil
}

func (p *EmbeddedProvider) get(id ID) (*template.Template, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cache == nil {
		p.cache = make(map[ID]*template.Template)
	}
	if t, ok := p.cache[id]; ok {
		return t, nil
	}
	t, err := load(id)
	if err != nil {
		return nil, err
	}
	p.cache[id] = t
	return t, nil
}
