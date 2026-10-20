package catalog

import (
	"strings"
	"testing"

	harness "github.com/shhac/lib-agent-harness"
)

// The document below is trimmed from `fx models --json` as fx 0.0.14 writes it.
const fxCatalogDocument = `{"kind":"models","count":2,"shown_count":2,"more_count":0,"private_models_hidden":false,"ids":["anthropic/claude-sonnet-5.5","openai/gpt-5.4-nano"],"models":[{"id":"anthropic/claude-sonnet-5.5","name":"Claude Sonnet 5.5","efforts":["low","medium","high","xhigh","max"],"fast":false,"ultrafast":false},{"id":"openai/gpt-5.4-nano","name":"GPT-5.4 nano","efforts":[],"fast":false,"ultrafast":false}]}`

func TestFxCatalogReadsTheModelsItStates(t *testing.T) {
	models, err := readFxCatalog(strings.NewReader(fxCatalogDocument+"\n"), &strings.Builder{})
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("models: %+v", models)
	}
	first := models[0]
	if first.ID != "anthropic/claude-sonnet-5.5" || first.Name != "Claude Sonnet 5.5" {
		t.Fatalf("first model: %+v", first)
	}
	if !first.EffortsKnown || len(first.Efforts) != 5 || first.Efforts[0].ID != "low" {
		t.Fatalf("efforts: %+v", first.Efforts)
	}
	second := models[1]
	if !second.EffortsKnown || len(second.Efforts) != 0 {
		t.Fatalf("a model without efforts: %+v", second)
	}
	if first.ContextWindow != 0 || first.IsDefault || first.DefaultEffort != "" {
		t.Fatalf("invented metadata: %+v", first)
	}
}

func TestFxCatalogKeepsAnUnstatedEffortListUnknown(t *testing.T) {
	models, err := readFxCatalog(strings.NewReader(`{"kind":"models","models":[{"id":"m","name":"M"}]}`+"\n"), &strings.Builder{})
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].EffortsKnown {
		t.Fatalf("an unstated effort list: %+v", models)
	}
}

func TestFxCatalogRefusesADocumentItCannotRead(t *testing.T) {
	if _, err := readFxCatalog(strings.NewReader("not json\n"), &strings.Builder{}); err == nil {
		t.Fatal("an unreadable document was accepted")
	}
}

func TestFxCatalogIsOfferedForTheFxEngine(t *testing.T) {
	if c := harness.Support(harness.Fx, harness.Models, harness.Available); !c.Usable() {
		t.Fatalf("fx models: %+v", c)
	}
}
