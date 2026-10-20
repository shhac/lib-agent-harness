package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"time"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/catalog"
	"github.com/shhac/lib-agent-harness/internal/sandboxbridge"
	"github.com/shhac/lib-agent-harness/internal/skills"
)

// normalizeAPI resolves an OpenAI-compatible session's options, refusing every
// field it cannot honour. Only what the loop reads is accepted.
func normalizeAPI(o Options) (Options, error) {
	switch {
	case o.Sandbox != nil:
		return o, refuse(o, "sandbox", RefusedNotOffered, "API sessions use Workbench.Commands for their sandbox; Sandbox is for native sessions")
	case o.Model == "":
		return o, refuse(o, "model", RefusedModelRequired, "an API session requires an explicit model")
	case o.WorkDir != "" && o.Workbench == nil:
		return o, refuse(o, "work_dir", RefusedConflict, "an API session has no workspace: the caller's tools own theirs, and SkillRun.WorkDir says where skill scripts run; leave WorkDir unset")
	case len(o.Env) > 0:
		return o, refuse(o, "env", RefusedConflict, "an API command environment belongs in Workbench.Commands.Env; leave Env unset")
	case policySet(o.Policy):
		return o, refuse(o, "policy", RefusedOtherEnginePolicy, "an API session reads no native policy; leave Policy unset")
	}
	if err := checkCatalogModel(o); err != nil {
		return o, err
	}
	// The entry is evidence for this check only; the session keeps no copy.
	o.CatalogModel = nil
	if code := o.Provider.API.EffortProblem(o.Effort); code != "" {
		return o, refuse(o, "effort", code, "the effort cannot be sent to this endpoint as configured")
	}
	// The loop reads routing on every request; the caller's copy may change.
	if routing := o.Provider.API.OpenRouter; routing != nil {
		frozen := *routing
		o.Provider.API.OpenRouter = &frozen
	}
	var err error
	if o, err = normalizeLimits(o); err != nil {
		return o, err
	}
	if err = checkInstructions(o); err != nil {
		return o, err
	}
	if o, err = normalizeAPIRestriction(o); err != nil {
		return o, err
	}
	if o, err = normalizeRuntimeHome(o); err != nil {
		return o, err
	}
	if o, err = normalizeLoop(o); err != nil {
		return o, err
	}
	if o, err = normalizeAPISkills(o); err != nil {
		return o, err
	}
	return normalizeWorkbench(o)
}

// checkCatalogModel refuses a model whose catalog entry lists its parameters
// without "tools": every API session sends tools, the caller's and any
// workbench's, so it would fail on its first request. An entry that does not
// list them is unknown and goes ahead, as does a session without one.
func checkCatalogModel(o Options) error {
	entry := o.CatalogModel
	if entry == nil {
		return nil
	}
	if entry.ID != o.Model && (entry.Resolved == "" || entry.Resolved != o.Model) {
		return refuse(o, "model", RefusedConflict, "CatalogModel describes another model: its ID or Resolved must equal Model")
	}
	if supported, known := catalog.SupportsTools(*entry); known && !supported {
		return refuse(o, "model", RefusedModelWithoutTools, "the model's catalog entry lists its supported parameters without 'tools', and an API session always sends tools; choose a model that lists tool calling")
	}
	return nil
}

func policySet(p Policy) bool {
	for _, field := range policyFields {
		if field.set(p) {
			return true
		}
	}
	return false
}

func checkInstructions(o Options) error {
	if o.Instructions.Mode != "" && o.Instructions.Mode != Replace && o.Instructions.Mode != Append {
		return refuse(o, "instructions", RefusedInstructionMode, "instruction mode must be replace or append")
	}
	if o.Instructions.Text != "" && o.Instructions.Mode == "" {
		return refuse(o, "instructions", RefusedInstructionModeMissing, "instructions require an explicit replace or append mode")
	}
	return nil
}

func normalizeAPIRestriction(o Options) (Options, error) {
	if o.Restriction == nil {
		return o, refuse(o, "restriction", RefusedNotConfigured, "an API session requires Restriction with a ToolHost naming the caller's tools and handler")
	}
	frozen := *o.Restriction
	host := frozen.Tools
	switch {
	case frozen.Probe != 0:
		return o, refuse(o, "restriction", RefusedConflict, "an API session has no capability probe: its tool surface holds by construction; leave Restriction.Probe unset")
	case host.Dir != "" || host.Bridge.Path != "" || host.Bridge.Args != nil:
		return o, refuse(o, "tools", RefusedConflict, "the library calls an API session's handler directly; leave ToolHost.Dir and ToolHost.Bridge unset")
	}
	if err := host.validateTools(); err != nil {
		return o, toolHostRefusal(o, err)
	}
	frozen.Tools.Tools = freezeTools(host.Tools)
	o.Restriction = &frozen
	return o, nil
}

func normalizeRuntimeHome(o Options) (Options, error) {
	if o.RuntimeHome == "" {
		return o, refuse(o, "runtime_home", RefusedRuntimeHome, "an API session keeps its conversation in a durable private runtime home; set Options.RuntimeHome")
	}
	home, err := filepath.Abs(o.RuntimeHome)
	if err != nil {
		return o, refuse(o, "runtime_home", RefusedRuntimeHome, "invalid runtime home")
	}
	info, err := os.Stat(home)
	if err != nil || !info.IsDir() {
		return o, refuse(o, "runtime_home", RefusedRuntimeHome, "the runtime home must be an existing directory")
	}
	if !sandboxbridge.PrivateStateDir(info) {
		return o, refuse(o, "runtime_home", RefusedRuntimeHome, "the runtime home must be readable only by its owner")
	}
	o.RuntimeHome = home
	return o, nil
}

func normalizeLoop(o Options) (Options, error) {
	l := &o.Loop
	if l.MaxSteps == 0 {
		l.MaxSteps = defaultLoopSteps
	}
	if l.MaxRequestBytes == 0 {
		l.MaxRequestBytes = defaultLoopRequestBytes
	}
	if l.RequestTimeout == 0 {
		l.RequestTimeout = 5 * time.Minute
		if o.Provider.API.Streaming {
			l.RequestTimeout = 30 * time.Minute
		}
	}
	if l.MaxSteps < 1 || l.MaxSteps > maxLoopSteps || l.MaxRequestBytes < 1024 || l.MaxRequestBytes > maxLoopRequestBytes || l.RequestTimeout < 0 || l.MaxOutputTokens < 0 {
		return o, refuse(o, "loop", RefusedLimit, "a loop bound is out of range")
	}
	return o, nil
}

// normalizeAPISkills checks the skill request. Provided skills are composed by
// completion's own mechanism: an index in the system message and its skill
// tools in every request, answered by AnswerSkillCalls through the tool host.
func normalizeAPISkills(o Options) (Options, error) {
	set := o.Skills
	switch {
	case !set.Global.Known():
		return o, refuse(o, "skills", RefusedGlobalSkillsInvalid, "Skills.Global is not a known value")
	case !set.Delivery.Known():
		return o, refuse(o, "skills", RefusedSkillDeliveryInvalid, "Skills.Delivery is not a known value")
	case set.Global == harness.GlobalSkillsInclude:
		return o, &UnsupportedError{Engine: o.Provider.Engine, Operation: "skills", Code: RefusedGlobalSkills, Capability: harness.Support(o.Provider.Engine, harness.Session, harness.IncludeGlobalSkills)}
	}
	if len(set.Provided) == 0 {
		if skillRunSet(o.SkillRun) {
			return o, refuse(o, "skills", RefusedConflict, "SkillRun applies only to provided skills that permit scripts")
		}
		return o, nil
	}
	loaded, err := skills.Load(set.Provided)
	if err != nil {
		return o, refuse(o, "skills", skills.Code(err), "a provided skill could not be loaded")
	}
	for _, tool := range o.Restriction.Tools.Tools {
		if skills.IsTool(tool.Name) {
			return o, refuse(o, "tools", RefusedSkillToolReserved, "load_skill and run_skill_script are reserved for the library's skill tools")
		}
	}
	_, scripted := skills.Names(loaded)
	switch {
	case skillRunSet(o.SkillRun) && len(scripted) == 0:
		return o, refuse(o, "skills", RefusedConflict, "SkillRun applies only to provided skills that permit scripts")
	case len(scripted) > 0 && o.SkillRun.WorkDir == "":
		return o, refuse(o, "skills", skills.CodeWorkDirRequired, "a session offering skill scripts requires SkillRun.WorkDir")
	}
	o.Skills.Provided = slices.Clone(set.Provided)
	o.SkillRun.Env = slices.Clone(o.SkillRun.Env)
	o.skills = &skillPlan{delivery: requestSkills, loaded: loaded}
	return o, nil
}

// apiReference digests what a resume has to match: the engine, the endpoint
// (its base URL and dialect, never a credential), the model, effort,
// instructions, tool server name, skill request and workbench. Ref.Home is the
// runtime home, because the conversation lives there: resuming from another is
// a different session wearing the same name. Ref.WorkDir is the workbench's
// workspace, and empty without one. Like a restricted session's, the digest excludes the
// tools themselves, so a release that edits a tool resumes the stored
// conversation under the new surface. Like EffortParameter and Streaming,
// OpenRouter routing is how requests are sent, not what the conversation is:
// a resume under changed routing continues the same conversation.
func apiReference(o Options, id string) Ref {
	payload, _ := json.Marshal(struct {
		Engine        harness.Engine
		BaseURL       string
		Dialect       harness.Dialect
		Model, Effort string
		Instructions  Instructions
		ToolServer    string
	}{o.Provider.Engine, o.Provider.API.BaseURL, o.Provider.API.Dialect, o.Model, o.Effort, o.Instructions, o.Restriction.Tools.Server})
	if skills := skillsDigest(o); skills != nil {
		payload, _ = json.Marshal(struct {
			Base   json.RawMessage
			Skills any
		}{payload, skills})
	}
	var workDir string
	if workbench := workbenchDigest(o); workbench != nil {
		// Wrapped, like skills, so a session without one keeps its digest.
		payload, _ = json.Marshal(struct {
			Base      json.RawMessage
			Workbench any
		}{payload, workbench})
		workDir = o.WorkDir
	}
	hash := sha256.Sum256(payload)
	return Ref{Engine: o.Provider.Engine, ID: id, Home: o.RuntimeHome, WorkDir: workDir, AccountIdentity: o.AccountIdentity, ConfigHash: hex.EncodeToString(hash[:])}
}
