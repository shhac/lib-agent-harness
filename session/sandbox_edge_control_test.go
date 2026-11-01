package session

import (
	"context"
	"errors"
	harness "github.com/shhac/lib-agent-harness"
	"github.com/shhac/lib-agent-harness/internal/sandboxprobe"
	"net"
	"testing"
)

func TestEdgeResolverDoesNotNeedHTTPS(t *testing.T) {
	control, lookup, dial, dns, validate := edgeControl, edgeLookupIPv4, edgeDialControl, edgeLookupDNS, edgeValidateControl
	defer func() {
		edgeControl, edgeLookupIPv4, edgeDialControl, edgeLookupDNS, edgeValidateControl = control, lookup, dial, dns, validate
	}()
	edgeControl = func(context.Context, string) (sandboxprobe.ControlTarget, string) {
		return sandboxprobe.ControlTarget{Addr: "100.100.100.100", Source: "configured"}, ""
	}
	edgeLookupDNS = func(context.Context) error { return nil }
	edgeLookupIPv4 = func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("192.0.2.44")}, nil }
	edgeValidateControl = func(address string) (string, string) {
		if address != "192.0.2.44" {
			t.Fatal(address)
		}
		return address, ""
	}
	dials := 0
	edgeDialControl = func(_ context.Context, address string) (net.Conn, error) {
		dials++
		if address != "192.0.2.44:443" {
			t.Fatal("DNS resolver treated as HTTPS", address)
		}
		a, b := net.Pipe()
		b.Close()
		return a, nil
	}
	o := Options{Provider: harness.Provider{Engine: harness.Codex}, Sandbox: &Sandbox{Edge: &Edge{}}}
	resolver, https, err := edgeNetworkControls(context.Background(), o)
	if err != nil || resolver.Addr != "100.100.100.100" || https != "192.0.2.44" || dials != 1 {
		t.Fatal("separate controls not proved", err)
	}
	edgeDialControl = func(context.Context, string) (net.Conn, error) { return nil, errors.New("unavailable") }
	_, _, err = edgeNetworkControls(context.Background(), o)
	var refused *CapabilityError
	if !errors.As(err, &refused) || refused.Code != CapabilityEdgeControlUnavailable {
		t.Fatal("unavailable control counted as denial", err)
	}
}
