package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/apihttp"
)

const (
	apiBodyLimit = 4 << 20
	// maxParameters and maxParameterBytes bound one entry's
	// supported_parameters. A longer list is not truncated: that entry's
	// parameters are then unknown.
	maxParameters     = 128
	maxParameterBytes = 64
)

// listAPI reads GET {BaseURL}/models under the same rules as completion's
// Chat Completions requests: the caller's credential only, no ambient proxy,
// no redirects, a bounded body, and no provider prose in errors. One page is
// read; pagination is not followed.
func (d discoverer) listAPI(ctx context.Context, api harness.API) ([]Model, error) {
	endpoint, code := api.Endpoint("models")
	if code != "" {
		return nil, preflightFailure(code)
	}
	token, err := apihttp.Credential(ctx, api)
	if err != nil {
		return nil, err
	}
	data, err := apihttp.Do(ctx, apihttp.Request{Method: http.MethodGet, URL: endpoint, Token: token, Transport: d.transport, Limit: apiBodyLimit})
	if err != nil {
		return nil, err
	}
	if apihttp.Echoes(string(data), token) {
		return nil, apihttp.ResponseFailure("credential_echoed")
	}
	models, err := parseAPIModels(data)
	if err != nil {
		return nil, err
	}
	for _, model := range models {
		if apihttp.Echoes(model.ID+model.Name+model.Description, token) {
			return nil, apihttp.ResponseFailure("credential_echoed")
		}
		for _, parameter := range model.Parameters {
			if apihttp.Echoes(parameter, token) {
				return nil, apihttp.ResponseFailure("credential_echoed")
			}
		}
	}
	return models, nil
}

// parseAPIModels reads the OpenAI list shape, {"data":[{"id":...}]}. Name,
// description and context window are common gateway extensions, taken only
// when they have the expected type: context_window as Vercel AI Gateway
// reports it, context_length as OpenRouter does. So are OpenRouter's
// supported_parameters, read by apiParameters. No endpoint lists efforts.
func parseAPIModels(data []byte) ([]Model, error) {
	var list struct {
		Data []struct {
			ID                  string          `json:"id"`
			Name                json.RawMessage `json:"name"`
			Description         json.RawMessage `json:"description"`
			ContextWindow       json.RawMessage `json:"context_window"`
			ContextLength       json.RawMessage `json:"context_length"`
			SupportedParameters json.RawMessage `json:"supported_parameters"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if decoder.Decode(&list) != nil || decoder.Decode(new(any)) != io.EOF || list.Data == nil {
		return nil, apihttp.ResponseFailure("invalid_catalog")
	}
	catalog := newCatalogBuilder()
	for _, entry := range list.Data {
		model := Model{ID: entry.ID, Name: optionalString(entry.Name), Description: optionalString(entry.Description)}
		model.ContextWindow = tokenCount(entry.ContextWindow)
		if model.ContextWindow == 0 {
			model.ContextWindow = tokenCount(entry.ContextLength)
		}
		model.Parameters, model.ParametersKnown = apiParameters(entry.SupportedParameters)
		if err := catalog.add(model); err != nil {
			return nil, err
		}
	}
	return catalog.models, nil
}

// apiParameters accepts only a JSON array of at most maxParameters non-empty
// strings of at most maxParameterBytes each, dropping repeats and keeping
// order. An empty array is known and lists none. Anything else, null or
// missing included, is unknown for this entry alone.
func apiParameters(raw json.RawMessage) ([]string, bool) {
	var values []json.RawMessage
	if json.Unmarshal(raw, &values) != nil || values == nil || len(values) > maxParameters {
		return nil, false
	}
	parameters := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		var parameter string
		if json.Unmarshal(value, &parameter) != nil || parameter == "" || len(parameter) > maxParameterBytes {
			return nil, false
		}
		if !seen[parameter] {
			seen[parameter] = true
			parameters = append(parameters, parameter)
		}
	}
	return parameters, true
}

func optionalString(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

// tokenCount accepts only a positive integer JSON number; anything else is
// "not stated".
func tokenCount(raw json.RawMessage) int64 {
	count, err := strconv.ParseInt(string(bytes.TrimSpace(raw)), 10, 64)
	if err != nil || count <= 0 {
		return 0
	}
	return count
}
