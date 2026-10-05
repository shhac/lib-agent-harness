package sandboxprobe

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/shhac/lib-agent-harness/internal/testenv"
)

// No external resolver: the private control dial hook targets local fixtures.
// The public entry point must continue refusing the same loopback destination.
func TestDNSControlLocalFixture(t *testing.T) {
	testenv.RequireLoopback(t)
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenPacket("udp", tcp.Addr().String())
	if err != nil {
		tcp.Close()
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		conn, err := tcp.Accept()
		if err == nil {
			conn.Close()
		}
	}()
	go func() {
		defer wg.Done()
		buf := make([]byte, 512)
		n, addr, err := udp.ReadFrom(buf)
		if err == nil && n >= 12 {
			buf[2] |= 0x80
			_, _ = udp.WriteTo(buf[:n], addr)
		}
	}()
	defer func() { tcp.Close(); udp.Close(); wg.Wait() }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if reason := proveDNSControl(ctx, tcp.Addr().String(), (&net.Dialer{}).DialContext); reason != "" {
		t.Fatal(reason)
	}
	if reason := ProveDNSControl(ctx, ControlTarget{Addr: "127.0.0.1", Source: "caller"}); reason != ControlOnThisMachine {
		t.Fatal("public control bypassed loopback guard", reason)
	}
}
