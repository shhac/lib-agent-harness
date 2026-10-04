package sandboxprobe

import "testing"

// The session's non-Darwin canary must remain byte-identical.
func TestLoopbackCanaryLegacyBytes(t *testing.T) {
	want := `command -v nc >/dev/null 2>&1 || echo no-network-client
nc -z -w 3 127.0.0.1 1 >/dev/null 2>&1 && echo loopback
nc -z -w 3 192.0.2.1 443 >/dev/null 2>&1 && echo outside
nc -l 127.0.0.1 2 >/dev/null 2>&1 &
sleep 1
nc -z -w 3 127.0.0.1 2 >/dev/null 2>&1 && echo bound
echo canary-ran`
	if got := LoopbackCanary(1, "192.0.2.1", 443, 2); got != want {
		t.Fatal("legacy canary changed")
	}
}
