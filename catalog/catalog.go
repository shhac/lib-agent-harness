// Package catalog lists the models an engine offers, with their reasoning
// efforts and defaults, without starting a conversation or an inference
// request. It reads each engine's own catalog: Codex's app-server model list,
// Claude's and Grok's initialization metadata, and an OpenAI-compatible
// endpoint's model list. Nothing is invented: a catalog that cannot be read is
// an error, never a fallback list.
package catalog

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/shhac/lib-agent-harness"
)

// Model is public model metadata, never account details.
type Model struct {
	// ID is what the engine accepts as its model setting. It may be an alias
	// that follows the engine's newest model of a family, such as Claude's
	// "opus" or "opus[1m]"; Resolved says what it selects today.
	ID string `json:"id"`
	// Resolved is the concrete model ID selects at discovery time, when the
	// engine states it; empty when it did not. Pin Resolved to keep a model
	// fixed, or keep ID to follow the engine's upgrades.
	Resolved    string `json:"resolved,omitempty"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// DefaultEffort is the effort the engine applies when none is chosen; empty
	// when it did not say.
	DefaultEffort string   `json:"default_effort,omitempty"`
	Efforts       []Effort `json:"efforts"`
	// EffortsKnown says the engine stated which efforts the model accepts. An
	// empty Efforts is then "none"; without it, Efforts is nil and means only
	// that the engine did not say.
	EffortsKnown bool `json:"efforts_known"`
	// IsDefault marks the engine's own default selection.
	IsDefault bool `json:"is_default"`
	// ContextWindow is the stated window in tokens; zero means not stated.
	ContextWindow int64 `json:"context_window,omitempty"`
}

// Effort is one reasoning effort a model accepts.
type Effort struct {
	ID          string `json:"id"`
	Description string `json:"description,omitempty"`
	Default     bool   `json:"default,omitempty"`
}

const (
	defaultTimeout = 15 * time.Second
	// maxModels bounds every catalog. A longer one fails rather than being
	// silently truncated.
	maxModels = 2000
)

// discoverer carries the seams tests replace: the subprocess runner and the
// HTTP round trip.
type discoverer struct {
	timeout   time.Duration
	run       cliRunner
	transport http.RoundTripper
}

var production = discoverer{timeout: defaultTimeout, run: runCLI}

// Discover lists the provider's models. It is available where
// harness.Support(p.Engine, harness.Models, harness.Available) is usable. It is
// bounded to 15 seconds within ctx. Failures are *Error, carrying only fixed
// codes; cancellation of ctx returns ctx.Err().
func Discover(ctx context.Context, p harness.Provider) ([]Model, error) {
	return production.discover(ctx, p)
}

func (d discoverer) discover(ctx context.Context, p harness.Provider) ([]Model, error) {
	engine := p.Engine
	if !harness.Support(engine, harness.Models, harness.Available).Usable() {
		return nil, refusal(engine, "unsupported_engine")
	}
	if code := p.Problem(); code != "" {
		return nil, refusal(engine, code)
	}
	if harness.LoginStoreLocked(engine) {
		return nil, refusal(engine, harness.CodeKeychainUnavailable)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bounded, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	models, err := d.list(bounded, p)
	if err == nil {
		return models, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, classify(engine, bounded, err)
}

func (d discoverer) list(ctx context.Context, p harness.Provider) ([]Model, error) {
	if p.Engine == harness.OpenAICompatible {
		return d.listAPI(ctx, p.API)
	}
	command, read, err := cliCatalog(p)
	if err != nil {
		return nil, err
	}
	var models []Model
	err = d.run(ctx, command, func(reader io.Reader, writer io.Writer) error {
		var err error
		models, err = read(reader, writer)
		return err
	})
	if err == nil && p.Engine == harness.Codex {
		d.addCodexWindows(ctx, command, models)
	}
	return models, err
}

// catalogBuilder collects one engine's catalog: first mention of an ID wins,
// entries without one are skipped, and the size is bounded.
type catalogBuilder struct {
	models []Model
	seen   map[string]bool
}

func newCatalogBuilder() *catalogBuilder {
	return &catalogBuilder{models: make([]Model, 0), seen: map[string]bool{}}
}

func (b *catalogBuilder) add(model Model) error {
	if model.ID == "" || b.seen[model.ID] {
		return nil
	}
	if len(b.models) == maxModels {
		return responseFailure("catalog_limit")
	}
	if model.Name == "" {
		model.Name = model.ID
	}
	b.seen[model.ID] = true
	b.models = append(b.models, model)
	return nil
}
