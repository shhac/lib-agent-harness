package harness

import (
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

var updateSupport = flag.Bool("update-support", false, "rewrite testdata/support.golden")

// Every claim Support makes, for every platform, so a change to the table or
// to its platform rules is a reviewed diff rather than a silent one.
func TestSupportSnapshot(t *testing.T) {
	features := []Feature{ProcessInspection, LoopbackPorts, LoopbackLocalOnly}
	for key := range supportTable {
		if !slices.Contains(features, key.feature) {
			features = append(features, key.feature)
		}
	}
	slices.Sort(features)
	engines := append(Engines(), "unrecognized")
	var b strings.Builder
	for _, goos := range []string{"darwin", "linux", "windows", "freebsd"} {
		for _, e := range engines {
			for _, op := range Operations() {
				for _, f := range features {
					c := supportFor(goos, e, op, f)
					b.WriteString(goos + " " + string(e) + " " + string(op) + " " + string(f) + " " + string(c.Availability) + " " + c.Reason + "\n")
				}
			}
		}
	}
	path := filepath.Join("testdata", "support.golden")
	if *updateSupport {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := b.String(); got != string(want) {
		gotLines, wantLines := strings.Split(got, "\n"), strings.Split(string(want), "\n")
		for i := range min(len(gotLines), len(wantLines)) {
			if gotLines[i] != wantLines[i] {
				t.Fatalf("support claims changed (go test -run TestSupportSnapshot -update-support to accept):\nwant %s\n got %s", wantLines[i], gotLines[i])
			}
		}
		t.Fatalf("support claims changed in length: %d lines, want %d", len(gotLines), len(wantLines))
	}
}

// The snapshot's host rows are what Support itself answers here.
func TestSupportIsSupportForThisPlatform(t *testing.T) {
	for key := range supportTable {
		if Support(key.engine, key.operation, key.feature) != supportFor(runtimeGOOS(), key.engine, key.operation, key.feature) {
			t.Fatalf("%+v differs from the host's supportFor", key)
		}
	}
}

func runtimeGOOS() string { return runtime.GOOS }
