package completion

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestClaudeModelCatalogReadsOnlyInitializationMetadata(t *testing.T) {
	fixture := `{"type":"control_response","response":{"subtype":"success","request_id":"agent-harness-models","response":{"account":{"token":"not-for-ui"},"commands":["unrelated"],"models":[{"value":"opus","displayName":"Opus","resolvedModel":"version-is-cli-owned","description":"Careful reasoning","supportedEffortLevels":["low","high"]},{"value":"haiku","displayName":"Haiku"}]}}}`
	var requests bytes.Buffer
	models, err := readClaudeModelCatalog(strings.NewReader(fixture), &requests)
	if err != nil || len(models) != 2 || models[0].Name != "Opus" || len(models[0].Efforts) != 2 || len(models[1].Efforts) != 0 {
		t.Fatal(models, err)
	}
	encoded, _ := json.Marshal(models)
	if strings.Contains(string(encoded), "not-for-ui") || strings.Contains(string(encoded), "unrelated") {
		t.Fatal("non-model metadata leaked")
	}
	if strings.Contains(requests.String(), `"type":"user"`) || !strings.Contains(requests.String(), `"subtype":"initialize"`) {
		t.Fatal(requests.String())
	}
}
