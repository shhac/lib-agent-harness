package sandbox

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

func judgeInspectionMarker(output, marker string) error {
	if marker != "" && strings.Contains(output, marker) {
		return &ProofError{Code: CapabilitySandboxNotEnforced, Step: ProofStepProcessInspection}
	}
	return nil
}

func judgeProcessInspection(output string, background bool) error {
	fail := func(code string) error { return &ProofError{Code: code, Step: ProofStepProcessInspection} }
	if inspectionControl(output) != "" {
		return fail(CapabilitySandboxNotEnforced)
	}
	for _, line := range strings.Split(output, "\n") {
		if line == "membership-unavailable" {
			return fail(CapabilityProcessInspectionUnavailable)
		}
	}
	var ps, stat []string
	group := ""
	expectedNice := ""
	command, private, negative, finished := false, false, false, false
	start := ""
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "expected-nice":
			if len(fields) == 2 {
				expectedNice = fields[1]
			}
		case "expected-group":
			if len(fields) == 2 {
				group = fields[1]
			}
		case "positive":
			ps = fields
		case "stat":
			stat = fields
		case "command":
			command = len(fields) >= 8 && strings.Contains(line, "/bin/sleep 30")
			if len(fields) >= 6 {
				start = strings.Join(fields[1:6], " ")
			}
		case "private-namespace":
			private = true
		case "negative-invisible", "negative-collision":
			negative = true
		case "inspection-ran":
			finished = true
		}
	}
	nice, niceErr := strconv.Atoi(expectedNice)
	if niceErr != nil || nice < -15 || nice > 19 || (background && nice != 15) {
		return fail(CapabilityProcessInspectionUnavailable)
	}
	if !command || !private || !negative || !finished || len(ps) != 5 || len(stat) != 6 {
		return fail(CapabilityProcessInspectionUnavailable)
	}
	pid, e := strconv.Atoi(ps[1])
	pgid, e2 := strconv.Atoi(ps[2])
	birth, e3 := strconv.ParseUint(stat[5], 10, 64)
	_, e4 := time.Parse("Mon Jan 2 15:04:05 2006", start)
	if e != nil || e2 != nil || e3 != nil || e4 != nil || pid <= 0 || pgid <= 0 || birth == 0 || group != ps[2] || ps[1] != stat[1] || ps[2] != stat[2] || ps[3] != stat[3] || ps[3] != strconv.Itoa(nice) || len(stat[4]) != 1 || !strings.Contains("RSDTtIW", stat[4]) || !strings.HasPrefix(ps[4], stat[4]) {
		return fail(CapabilityProcessInspectionUnavailable)
	}
	return nil
}

func judgeInspectionHost(output string) error {
	var host, stat []string
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "host":
			host = fields
		case "stat":
			stat = fields
		}
	}
	if len(host) == 5 && len(stat) == 6 && host[1] == stat[1] && host[2] == stat[2] && host[3] == stat[3] && host[4] == stat[5] {
		return nil
	}
	return &ProofError{Code: CapabilityProcessInspectionUnavailable, Step: ProofStepProcessInspection}
}

// Match emitted controls, not their source text in our own shell argv.
func inspectionControl(output string) string {
	for _, line := range strings.Split(output, "\n") {
		switch line {
		case "foreign-process", "shared-namespace", "negative-visible-refused", "marker-exposure":
			return line
		}
	}
	return ""
}

func inspectionDiagnostic(output, marker string) string {
	if marker != "" {
		if strings.Contains(output, marker) {
			output = "marker-exposure\n" + output
		}
		output = strings.ReplaceAll(output, marker, "[fixture-marker]")
	}
	if len(output) > 64<<10 {
		output = output[:64<<10] + "\n[truncated]"
	}
	return output
}

// Rooted NSpid plus a distinct shell/host namespace proves the proc boundary.
// In real procfs, the first NSpid ID always equals its directory PID. Non-rooted
// checks only establish readable, well-formed status; they cannot independently
// prove membership or distinguish a host process. Status avoids ptrace-gated ns links.
func inspectionStatusCheck(path, pid string, rooted bool) string {
	condition := "$2!=pid"
	if rooted {
		condition = "NF!=2 || " + condition
	}
	return fmt.Sprintf("/usr/bin/awk -v pid=%s '$1==\"NSpid:\" {found=1; if(%s) exit 1} END {if(!found) exit 1}' %s", pid, condition, path)
}

func judgeInspectionTranscript(ctx context.Context, output, marker string) error {
	if err := judgeInspectionMarker(output, marker); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return &ProofError{Code: CapabilityProbeTimeout, Step: ProofStepProcessInspection}
	}
	return nil
}
