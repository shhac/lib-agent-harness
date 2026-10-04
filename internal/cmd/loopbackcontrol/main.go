// loopbackcontrol pins CI to a responding configured resolver, never a public default.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/shhac/lib-agent-harness/internal/sandboxprobe"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	target, reason := sandboxprobe.DiscoverAndProveControl(ctx, "")
	if reason != "" {
		fmt.Fprintln(os.Stderr, reason)
		os.Exit(1)
	}
	fmt.Println(target.Addr)
}
