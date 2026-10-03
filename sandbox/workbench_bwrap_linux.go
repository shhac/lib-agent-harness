package sandbox

import (
	"context"

	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/sandboxhook"
)

func workbenchSystemDirs() []string                   { return bwrapSystemDirs() }
func workbenchSystemContains(system, dir string) bool { return bwrapSystemContains(system, dir) }
func normalizeWorkbenchSystem(o Options, standalone bool) (Options, error) {
	if o.Loopback && !standalone {
		return o, &RefusalError{Operation: "loopback", Code: RefusedNotOffered, Capability: harness.Support(harness.OpenAICompatible, harness.Session, harness.Loopback)}
	}

	if linuxSystemPlacement(o.WorkDir) {
		return o, refusal("work_dir", RefusedWorkDir, "inside the command sandbox's system set")
	}
	if linuxSystemPlacement(o.RuntimeHome) {
		return o, refusal("runtime_home", RefusedRuntimeHome, "inside the command sandbox's system set")
	}
	o.system = workbenchSystemDirs()
	return o, nil
}

// Compute relative nice in the child, avoiding Go thread migration and a
// post-Start race with bwrap's fork. Both shells exec; none is retained.
func linuxBackgroundLaunch(binary string, args []string) (string, []string) {
	return "/bin/sh", append([]string{"-c", workbenchLinuxNice, "workbench-background", binary}, args...)
}

const workbenchLinuxNice = `workbench_nice=$(/usr/bin/ps -o ni= -p $$ | /usr/bin/tr -d ' ')
exec /usr/bin/nice -n "$((10 - workbench_nice))" /bin/sh -c '
workbench_nice=$(/usr/bin/ps -o ni= -p $$ | /usr/bin/tr -d " ")
[ "$workbench_nice" = 10 ] || exit 125
exec "$@"
' workbench-niced "$@"
`

func init() {
	sandboxhook.CommandTrial = func(ctx context.Context, work, home, tmp string) (string, string, error) {
		return checkBwrap(ctx, workbenchLayout{Work: work, Home: home, Tmp: tmp, System: workbenchSystemDirs()})
	}
}
