package completion

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/shhac/lib-agent-harness/process"
)

// ModelOption contains only public model metadata, never account details.
type ModelOption struct {
	ID            string        `json:"id"`
	Name          string        `json:"name"`
	Description   string        `json:"description,omitempty"`
	DefaultEffort string        `json:"default_effort"`
	Efforts       []ModelEffort `json:"efforts"`
	IsDefault     bool          `json:"is_default"`
}
type ModelEffort struct {
	ID          string `json:"id"`
	Description string `json:"description,omitempty"`
}

type modelTransport func(context.Context, Config, func(io.Reader, io.Writer) error) error

// DiscoverModels reads Codex's account-aware catalog without starting a thread
// or an inference request. Unsupported engines return no invented options.
func DiscoverModels(ctx context.Context, cfg Config) ([]ModelOption, error) {
	return discoverModels(ctx, cfg, runModelTransport)
}

func discoverModels(ctx context.Context, cfg Config, run modelTransport) ([]ModelOption, error) {
	if cfg.Engine != "codex" && cfg.Engine != "claude" {
		return nil, errors.New("model discovery is available for local CLIs; keep the saved model or use advanced settings")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var models []ModelOption
	err := run(ctx, cfg, func(reader io.Reader, writer io.Writer) error {
		var err error
		if cfg.Engine == "claude" {
			models, err = readClaudeModelCatalog(reader, writer)
		} else {
			models, err = readModelCatalog(reader, writer)
		}
		return err
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// The child may report credentials or arbitrary user configuration in errors.
		return nil, errors.New("could not discover models from the CLI; check the selected login and CLI installation, then retry (saved settings are unchanged)")
	}
	return models, nil
}

// catalogInvocation selects the engine's catalog command once: its binary, its
// arguments, and its login environment. Only the selected engine's invocation
// is built, so discovering Claude's models never resolves a Codex home.
func catalogInvocation(cfg Config) (bin string, args, env []string, err error) {
	if cfg.Engine == "claude" {
		return claudeCatalogInvocation(cfg)
	}
	return codexCatalogInvocation(cfg)
}

func codexCatalogInvocation(cfg Config) (string, []string, []string, error) {
	bin, err := resolveCatalogBinary(cfg.CodexBin, "codex")
	if err != nil {
		return "", nil, nil, err
	}
	env, err := CodexEnvironment(cfg.CodexHome)
	if err != nil {
		return "", nil, nil, err
	}
	args := []string{"app-server", "--listen", "stdio://", "-c", "analytics.enabled=false", "-c", "check_for_update_on_startup=false"}
	// No thread is created. Also disable tool-discovery features during startup.
	for _, feature := range disabledCodexFeatures {
		args = append(args, "-c", "features."+feature+"=false")
	}
	return bin, args, env, nil
}

func claudeCatalogInvocation(cfg Config) (string, []string, []string, error) {
	bin, err := resolveCatalogBinary(cfg.ClaudeBin, "claude")
	if err != nil {
		return "", nil, nil, err
	}
	env, err := ClaudeEnvironment(cfg.ClaudeHome)
	if err != nil {
		return "", nil, nil, err
	}
	return bin, append(claudeBaseArgs(), "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"), env, nil
}

func resolveCatalogBinary(configured, engine string) (string, error) {
	bin := configured
	if bin == "" {
		bin = engine
	}
	bin, err := exec.LookPath(bin)
	if err != nil {
		return "", err
	}
	return filepath.Abs(bin)
}

func runModelTransport(ctx context.Context, cfg Config, exchange func(io.Reader, io.Writer) error) error {
	bin, args, env, err := catalogInvocation(cfg)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "agent-harness-model-catalog-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir, cmd.Env, cmd.Stderr = dir, env, io.Discard
	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	defer stdinReader.Close()
	defer stdinWriter.Close()
	defer stdoutReader.Close()
	defer stdoutWriter.Close()
	stopClosing := context.AfterFunc(ctx, func() {
		stdinWriter.CloseWithError(ctx.Err())
		stdoutReader.CloseWithError(ctx.Err())
	})
	defer stopClosing()
	cmd.Stdin, cmd.Stdout = stdinReader, stdoutWriter
	child, err := process.New(cmd)
	if err != nil {
		return err
	}
	defer child.Close()
	cmd.Cancel = func() error { child.Stop(); return nil }
	cmd.WaitDelay = time.Second
	done := make(chan error, 1)
	go func() {
		err := child.Run()
		stdoutWriter.CloseWithError(err)
		stdinReader.CloseWithError(err)
		done <- err
	}()
	err = exchange(stdoutReader, stdinWriter)
	cancel()
	stdinWriter.Close()
	stdoutReader.Close()
	<-done
	return err
}

// readModelCatalog follows initialize -> initialized -> model/list, waiting for
// each reply before sending the next request. Both pagination and output are bounded.
func readModelCatalog(reader io.Reader, writer io.Writer) ([]ModelOption, error) {
	scanner := bufio.NewScanner(io.LimitReader(reader, 4<<20))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	encoder := json.NewEncoder(writer)
	request := func(id int, method string, params any) error {
		return encoder.Encode(map[string]any{"id": id, "method": method, "params": params})
	}
	reply := func(id int) (json.RawMessage, error) {
		for scanner.Scan() {
			var response struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if json.Unmarshal(scanner.Bytes(), &response) != nil {
				return nil, errors.New("invalid catalog response")
			}
			if response.ID != id {
				continue
			}
			if len(response.Error) != 0 && string(response.Error) != "null" {
				return nil, errors.New("catalog request failed")
			}
			if len(response.Result) == 0 || string(response.Result) == "null" {
				return nil, errors.New("missing catalog response")
			}
			return response.Result, nil
		}
		return nil, errors.New("catalog response missing or exceeds limit")
	}
	if err := request(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "lib-agent-harness", "version": "1"}}); err != nil {
		return nil, err
	}
	if _, err := reply(1); err != nil {
		return nil, err
	}
	if err := encoder.Encode(map[string]string{"method": "initialized"}); err != nil {
		return nil, err
	}
	models := make([]ModelOption, 0)
	seen := make(map[string]bool)
	cursors := make(map[string]bool)
	cursor := ""
	for page := 0; page < 10; page++ {
		params := map[string]any{"limit": 100, "includeHidden": false}
		if cursor != "" {
			params["cursor"] = cursor
		}
		if err := request(page+2, "model/list", params); err != nil {
			return nil, err
		}
		raw, err := reply(page + 2)
		if err != nil {
			return nil, err
		}
		var result struct {
			Data []struct {
				Model                  string `json:"model"`
				DisplayName            string `json:"displayName"`
				Description            string `json:"description"`
				DefaultReasoningEffort string `json:"defaultReasoningEffort"`
				Hidden                 bool   `json:"hidden"`
				IsDefault              bool   `json:"isDefault"`
				Efforts                []struct {
					ID          string `json:"reasoningEffort"`
					Description string `json:"description"`
				} `json:"supportedReasoningEfforts"`
			} `json:"data"`
			NextCursor string `json:"nextCursor"`
		}
		if json.Unmarshal(raw, &result) != nil || result.Data == nil {
			return nil, errors.New("invalid model catalog")
		}
		for _, m := range result.Data {
			if m.Hidden || m.Model == "" || seen[m.Model] {
				continue
			}
			seen[m.Model] = true
			option := ModelOption{ID: m.Model, Name: m.DisplayName, Description: m.Description, DefaultEffort: m.DefaultReasoningEffort, IsDefault: m.IsDefault, Efforts: make([]ModelEffort, 0)}
			if option.Name == "" {
				option.Name = option.ID
			}
			for _, effort := range m.Efforts {
				if effort.ID != "" {
					option.Efforts = append(option.Efforts, ModelEffort{ID: effort.ID, Description: effort.Description})
				}
			}
			models = append(models, option)
		}
		if result.NextCursor == "" {
			return models, nil
		}
		if cursors[result.NextCursor] {
			return nil, errors.New("catalog pagination repeated")
		}
		cursors[result.NextCursor] = true
		cursor = result.NextCursor
	}
	return nil, errors.New("model catalog exceeds page limit")
}
