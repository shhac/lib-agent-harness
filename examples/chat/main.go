// Command chat demonstrates one native CLI session. Its policy permits native
// tools according to the selected CLI; use completion for tool-free reasoning.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/shhac/lib-agent-harness/session"
)

func main() {
	engine := flag.String("engine", "", "codex or claude (required)")
	model := flag.String("model", "", "installed CLI model ID (required)")
	effort := flag.String("effort", "", "reasoning effort advertised by the CLI")
	home := flag.String("home", "", "optional native CLI home")
	workdir := flag.String("dir", "", "working directory (defaults to current directory)")
	flag.Parse()
	if *engine == "" || *model == "" || flag.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "usage: go run ./examples/chat -engine codex|claude -model MODEL [-effort EFFORT] PROMPT")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	ctx, deadline := context.WithTimeout(ctx, 5*time.Minute)
	defer deadline()
	if err := run(ctx, session.Options{Engine: session.Engine(*engine), Model: *model, Effort: *effort, Home: *home, WorkDir: *workdir}, strings.Join(flag.Args(), " ")); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, options session.Options, prompt string) error {
	s, err := session.Start(ctx, options)
	if err != nil {
		return err
	}
	defer s.Close()
	turn, err := s.StartTurn(ctx, session.Input{Text: prompt})
	if err != nil {
		return err
	}
	streamed := false
	for event := range turn.Events() {
		switch event.Kind {
		case "text_delta":
			streamed = true
			fmt.Print(event.Text)
		case "tool_started":
			fmt.Fprintf(os.Stderr, "\n[tool: %s]\n", event.Tool)
		}
	}
	fmt.Println()
	result, err := turn.Wait(ctx)
	if !streamed && result.Text != "" {
		fmt.Println(result.Text)
	}
	return err
}
