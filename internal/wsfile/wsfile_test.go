package wsfile

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestClean(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    string
		problem PathProblem
	}{
		{"a.go", "a.go", PathOK},
		{"dir/./sub//b.txt", "dir/sub/b.txt", PathOK},
		{"dir/../a", "a", PathOK},
		{".git/HEAD", ".git/HEAD", PathOK},
		{"", "", PathInvalid},
		{".", "", PathInvalid},
		{"dir/..", "", PathInvalid},
		{"/etc/passwd", "", PathInvalid},
		{"..", "", PathOutside},
		{"../x", "", PathOutside},
		{"a/../../x", "", PathOutside},
		{`a\b`, "", PathInvalid},
		{`..\x`, "", PathInvalid},
		{"C:/x", "", PathInvalid},
		{"C:x", "", PathInvalid},
		{"file.txt:stream", "", PathInvalid},
		{"a\x00b", "", PathInvalid},
		{"a\nb", "", PathInvalid},
		{"a\u0085b", "", PathInvalid},
		{strings.Repeat("a", 1025), "", PathInvalid},
	} {
		got, problem := Clean(tc.in, 1024)
		if got != tc.want || problem != tc.problem {
			t.Errorf("Clean(%q) = %q, %d; want %q, %d", tc.in, got, problem, tc.want, tc.problem)
		}
	}
}

func TestReservedDevice(t *testing.T) {
	for _, name := range []string{"CON", "con", "nul.txt", "AUX ", "aux.", "dir/PRN/x", "COM1", "lpt9.log", "COM0", "COM¹", "CON .txt", "a/NUL"} {
		if !ReservedDevice(name) {
			t.Errorf("%q was not refused", name)
		}
	}
	for _, name := range []string{"console", "CONFIG", "nulls.txt", "COM", "COM10", "LPTX", "a/aux1", "x.CON", "dir/lpt"} {
		if ReservedDevice(name) {
			t.Errorf("%q was refused", name)
		}
	}
}

func TestReserved(t *testing.T) {
	for _, name := range []string{
		".harness-workbench-0123456789abcdef0123456789abcdef-0123456789abcdef.tmp",
		".harness-workbench-other.tmp",
		".HARNESS-WORKBENCH-x.TMP",
		".harness-workbench-x.tmp. ",
	} {
		if !Reserved(name) {
			t.Errorf("%q is not reserved", name)
		}
	}
	for _, name := range []string{".harness-workbench-.tmp", ".harness-workbench-x.tmpx", "harness-workbench-x.tmp", "x.tmp", ".harness-workbench-x"} {
		if Reserved(name) {
			t.Errorf("%q is reserved", name)
		}
	}
}

func TestText(t *testing.T) {
	if !Text([]byte("héllo\n")) || !Text(nil) {
		t.Fatal("text refused")
	}
	if Text([]byte{0xff, 0xfe}) || Text([]byte("a\x00b")) {
		t.Fatal("binary accepted")
	}
}

func TestReadAtMost(t *testing.T) {
	data, tooLarge, err := ReadAtMost(strings.NewReader(strings.Repeat("x", 100<<10)), 100<<10, nil)
	if err != nil || tooLarge || len(data) != 100<<10 {
		t.Fatalf("at the limit: %d %v %v", len(data), tooLarge, err)
	}
	if _, tooLarge, err = ReadAtMost(strings.NewReader(strings.Repeat("x", 100<<10+1)), 100<<10, nil); err != nil || !tooLarge {
		t.Fatalf("over the limit: %v %v", tooLarge, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	steps := 0
	_, _, err = ReadAtMost(strings.NewReader(strings.Repeat("x", 1<<20)), 2<<20, func() error {
		if steps++; steps == 3 {
			cancel()
		}
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) || steps != 3 {
		t.Fatalf("a stopped read: %v after %d steps", err, steps)
	}
}
