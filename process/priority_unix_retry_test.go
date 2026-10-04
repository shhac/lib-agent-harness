//go:build unix

package process

import (
	"runtime"
	"syscall"
	"testing"
)

func TestLowerPriorityWaitsUntilTheLeaderReadsBack(t *testing.T) {
	applied, calls := false, 0
	setPriority = func(which, who, prio int) error {
		calls++
		applied = calls >= 3
		return nil
	}
	getPriority = func(which, who int) (int, error) {
		nice := -10
		if applied {
			nice = backgroundNice
		}
		return rawPriority(nice), nil
	}
	t.Cleanup(func() { setPriority, getPriority = syscall.Setpriority, syscall.Getpriority })
	if err := lowerPriority(123); err != nil || calls != 3 {
		t.Fatalf("lowerPriority = %v after %d calls", err, calls)
	}
	applied = false
	setPriority = func(int, int, int) error { return nil }
	if err := lowerPriority(123); err == nil {
		t.Fatal("a priority that never takes effect was reported as applied")
	}
}

// rawPriority is what getpriority returns for nice on this platform.
func rawPriority(nice int) int {
	if runtime.GOOS == "linux" {
		return 20 - nice
	}
	return nice
}
