package session

import harness "github.com/shhac/lib-agent-harness"

func workbenchSystemDirs() []string                   { return bwrapSystemDirs() }
func workbenchSystemContains(system, dir string) bool { return bwrapSystemContains(system, dir) }
func normalizeWorkbenchSystem(o Options) (Options, error) {
	if o.Workbench.Commands.Loopback && !o.Workbench.standaloneCommands {
		return o, &UnsupportedError{Engine: o.Provider.Engine, Operation: "loopback", Code: RefusedNotOffered, Capability: harness.Support(harness.OpenAICompatible, harness.Session, harness.Loopback)}
	}
	if linuxSystemPlacement(o.WorkDir) {
		return o, refuse(o, "work_dir", RefusedWorkDir, "inside the command sandbox's system set")
	}
	if linuxSystemPlacement(o.RuntimeHome) {
		return o, refuse(o, "runtime_home", RefusedRuntimeHome, "inside the command sandbox's system set")
	}
	o.Workbench.system = workbenchSystemDirs()
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
