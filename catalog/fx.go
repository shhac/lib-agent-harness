package catalog

import (
	"encoding/json"
	"io"
)

// readFxCatalog reads the model list fx prints with `fx models --json`.
//
// fx is a batch command here, not a protocol: it writes one JSON document to
// stdout and exits. The catalog seam still speaks pipes, so this reader takes
// the document as it comes and writes nothing back. The document names the
// models, their display names and the reasoning efforts each one accepts; it
// states no context window and no default selection, so neither is invented.
func readFxCatalog(reader io.Reader, writer io.Writer) ([]Model, error) {
	scanner := lines(reader)
	for scanner.Scan() {
		var reply struct {
			Kind   string `json:"kind"`
			Models []struct {
				ID      string   `json:"id"`
				Name    string   `json:"name"`
				Efforts []string `json:"efforts"`
			} `json:"models"`
		}
		if json.Unmarshal(scanner.Bytes(), &reply) != nil {
			return nil, responseFailure("invalid_response")
		}
		if reply.Kind != "models" {
			continue
		}
		return fxModels(reply.Models)
	}
	return nil, endOfStream(scanner)
}

// fxModels turns fx's model list into catalog entries. An effort list fx
// states is the whole set that model accepts; a model fx lists without one
// says nothing about its efforts, which is not the same as having none.
func fxModels(list []struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Efforts []string `json:"efforts"`
}) ([]Model, error) {
	catalog := newCatalogBuilder()
	for _, entry := range list {
		model := Model{ID: entry.ID, Name: entry.Name}
		if entry.Efforts != nil {
			model.EffortsKnown = true
			model.Efforts = make([]Effort, 0, len(entry.Efforts))
			for _, level := range entry.Efforts {
				if level == "" {
					continue
				}
				model.Efforts = append(model.Efforts, Effort{ID: level})
			}
		}
		if err := catalog.add(model); err != nil {
			return nil, err
		}
	}
	return catalog.models, nil
}
