// Command fxprobe exercises the fx engine end to end against the installed fx
// CLI: the capability claims, the model catalog, the account, a native run and
// a session over the Agent Client Protocol.
//
// It is a manual program, not a test: it performs inference with the
// operator's own fx login and spends whatever that account spends. The
// automated tests in this repository use synthetic fixtures and no live model.
//
//	go run ./examples/fxprobe -model minimax/minimax-m3.1-flash
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/account"
	"github.com/shhac/lib-agent-harness/catalog"
	"github.com/shhac/lib-agent-harness/native"
	"github.com/shhac/lib-agent-harness/session"
)

func main() {
	model := flag.String("model", "", "the fx model ID to use (required)")
	binary := flag.String("binary", "fx", "the fx CLI to run")
	flag.Parse()
	if *model == "" {
		fmt.Fprintln(os.Stderr, "usage: go run ./examples/fxprobe -model MODEL [-binary fx]")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	ctx, deadline := context.WithTimeout(ctx, 10*time.Minute)
	defer deadline()

	provider := harness.Provider{Engine: harness.Fx, CLI: harness.CLI{Binary: *binary}}
	claims(ctx, provider)
	catalogProbe(ctx, provider, *model)
	accountProbe(ctx, provider)
	nativeProbe(ctx, provider, *model)
	sessionProbe(ctx, provider, *model)
}

func section(title string) {
	fmt.Printf("\n%s\n%s\n", title, strings.Repeat("-", len(title)))
}

// claims prints what the library says about the fx engine, so a run of this
// program shows the claims and the evidence side by side.
func claims(_ context.Context, p harness.Provider) {
	section("Capability claims (harness.Support)")
	for _, r := range []struct {
		op      harness.Operation
		feature harness.Feature
	}{
		{harness.Session, harness.Available},
		{harness.Session, harness.Effort},
		{harness.Session, harness.ContextWindow},
		{harness.Session, harness.CacheSplit},
		{harness.Session, harness.Steer},
		{harness.Session, harness.AppendInstructions},
		{harness.Session, harness.ReplaceInstructions},
		{harness.Session, harness.RestrictTools},
		{harness.Run, harness.Available},
		{harness.Run, harness.StructuredOutput},
		{harness.Run, harness.CostReport},
		{harness.Models, harness.Available},
		{harness.Account, harness.Available},
		{harness.Account, harness.Quota},
		{harness.Account, harness.Credits},
		{harness.Complete, harness.Available},
	} {
		c := harness.Support(p.Engine, r.op, r.feature)
		fmt.Printf("  %-8s %-20s %-11s %s\n", r.op, r.feature, c.Availability, c.Reason)
	}
}

func catalogProbe(ctx context.Context, p harness.Provider, model string) {
	section("Model discovery (catalog.Discover, over `fx models --json`)")
	models, err := catalog.Discover(ctx, p)
	if err != nil {
		fmt.Printf("  discover failed: %v\n", err)
		return
	}
	fmt.Printf("  %d models in the catalog\n", len(models))
	for _, m := range models {
		if m.ID != model {
			continue
		}
		efforts := make([]string, 0, len(m.Efforts))
		for _, e := range m.Efforts {
			efforts = append(efforts, e.ID)
		}
		fmt.Printf("  chosen model: %s\n", m.ID)
		fmt.Printf("    name:           %s\n", m.Name)
		fmt.Printf("    efforts:        %s (known: %t)\n", strings.Join(efforts, ", "), m.EffortsKnown)
		fmt.Printf("    context window: %d\n", m.ContextWindow)
		return
	}
	fmt.Printf("  %s is not in the catalog\n", model)
}

func accountProbe(ctx context.Context, p harness.Provider) {
	section("Account inspection (account.Inspect, over `fx status` and `fx credits`)")
	report, err := account.Inspect(ctx, p)
	if err != nil {
		fmt.Printf("  inspect failed: %v\n", err)
		return
	}
	a := report.Account
	fmt.Printf("  logged in:    %v (source: %s, quality: %s)\n", a.LoggedIn != nil && *a.LoggedIn, a.Observation.Source, a.Observation.Quality)
	fmt.Printf("  auth method:  %s\n", a.AuthMethod)
	fmt.Printf("  organization: %s\n", a.Organization)
	if c := report.Credits; c.Known() && c.Balance != nil {
		fmt.Printf("  balance:      %s %s\n", c.Balance.Value, c.Balance.Unit)
	} else {
		fmt.Printf("  balance:      unknown (%s)\n", c.Reason)
	}
	if q := report.Quota; q.Known() {
		fmt.Printf("  quota:        %d windows\n", len(q.Windows))
	} else {
		fmt.Printf("  quota:        unknown (%s)\n", q.Reason)
	}
}

func nativeProbe(ctx context.Context, p harness.Provider, model string) {
	section("Native run (native.Run, over `fx ask --json`)")
	start := time.Now()
	result, err := native.Run(ctx, native.Config{
		Provider: p,
		Model:    model,
		Fx:       native.FxOptions{PermissionMode: "ask"},
	}, native.Request{Prompt: "Reply with exactly: ok", WorkDir: os.TempDir()}, nil)
	if err != nil {
		fmt.Printf("  run failed: %v\n", err)
		return
	}
	fmt.Printf("  report:      %s\n", string(result.Report))
	fmt.Printf("  usage:       in %d, out %d (known: %t)\n", result.Usage.Input, result.Usage.Output, result.Usage.Known)
	if result.Cost.Known {
		fmt.Printf("  cost:        %.4f USD\n", result.Cost.USD)
	} else {
		fmt.Printf("  cost:        not stated by fx\n")
	}
	fmt.Printf("  session id:  %s\n", result.SessionID)
	fmt.Printf("  elapsed:     %s\n", time.Since(start).Round(time.Millisecond))
}

func sessionProbe(ctx context.Context, p harness.Provider, model string) {
	section("Session over the Agent Client Protocol (over `fx acp`)")
	o := session.Options{
		Provider: p,
		Model:    model,
		Effort:   "low",
		Policy:   session.Policy{FxPermission: session.FxAllowWhenAsked},
	}
	start := time.Now()
	s, err := session.Start(ctx, o)
	if err != nil {
		fmt.Printf("  start failed: %v\n", err)
		return
	}
	defer s.Close()
	ref := s.Ref()
	c := s.Capabilities()
	fmt.Printf("  session id:  %s (engine %s)\n", ref.ID, ref.Engine)
	fmt.Printf("  capabilities: start=%s resume=%s steer=%s instructions=%s restrict=%s\n",
		c.Start.Availability, c.Resume.Availability, c.Steer.Availability,
		c.AppendInstructions.Availability, c.RestrictTools.Availability)

	turn, err := s.StartTurn(ctx, session.Input{Text: "Reply with exactly: ok"})
	if err != nil {
		fmt.Printf("  turn failed: %v\n", err)
		return
	}
	var streamed strings.Builder
	var kinds []string
	for e := range turn.Events() {
		kinds = append(kinds, e.Kind)
		switch e.Kind {
		case "text", "text_delta":
			streamed.WriteString(e.Text)
		case "tool_started":
			fmt.Printf("  tool started:   %s\n", e.Tool)
		case "tool_completed":
			fmt.Printf("  tool completed: %s (status: %s)\n", e.Tool, e.Status)
		}
	}
	result, err := turn.Wait(ctx)
	if err != nil {
		fmt.Printf("  turn failed: %v\n", err)
		return
	}
	fmt.Printf("  events:       %s\n", strings.Join(kinds, ", "))
	fmt.Printf("  answer:       %q\n", result.Text)
	fmt.Printf("  streamed:     %q\n", streamed.String())
	fmt.Printf("  usage:        in %d, out %d, reasoning %d (known: %t, final: %t)\n",
		result.Usage.Input, result.Usage.Output, result.Usage.Reasoning, result.Usage.Known, result.Usage.Final)
	if ctx := result.Context; ctx.UsedTokens != nil {
		used, capacity := *ctx.UsedTokens, int64(0)
		if ctx.CapacityTokens != nil {
			capacity = *ctx.CapacityTokens
		}
		fmt.Printf("  context:      model %s, %d/%d tokens (%s, %s)\n",
			ctx.Model, used, capacity, ctx.Observation.Quality, ctx.Observation.Reason)
	}

	// A second turn in the same session, so the conversation carries over.
	next, err := s.StartTurn(ctx, session.Input{Text: "Reply with exactly: still ok"})
	if err != nil {
		fmt.Printf("  second turn failed: %v\n", err)
		return
	}
	for range next.Events() {
	}
	second, err := next.Wait(ctx)
	if err != nil {
		fmt.Printf("  second turn failed: %v\n", err)
		return
	}
	fmt.Printf("  second turn:  %q (in %d, out %d)\n", second.Text, second.Usage.Input, second.Usage.Output)
	fmt.Printf("  elapsed:      %s\n", time.Since(start).Round(time.Millisecond))
}
