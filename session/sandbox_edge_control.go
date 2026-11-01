package session

import (
	"context"
	"github.com/shhac/lib-agent-harness/internal/sandboxprobe"
	"net"
	"time"
)

var edgeLookupIPv4 = func(ctx context.Context, host string) ([]net.IP, error) {
	return net.DefaultResolver.LookupIP(ctx, "ip4", host)
}
var edgeDialControl = func(ctx context.Context, address string) (net.Conn, error) {
	return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "tcp", address)
}
var edgeLookupDNS = func(ctx context.Context) error {
	_, err := net.DefaultResolver.LookupHost(ctx, "example.com")
	return err
}
var edgeValidateControl = sandboxprobe.ValidateControl

// A resolver proves TCP/UDP 53, not HTTPS. Each port needs a positive witness.
func edgeNetworkControls(ctx context.Context, o Options) (sandboxprobe.ControlTarget, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	dns, reason := edgeControl(ctx, "")
	if reason != "" || net.ParseIP(dns.Addr).To4() == nil {
		return dns, "", edgeFailure(o, CapabilityEdgeControlUnavailable)
	}
	if edgeLookupDNS(ctx) != nil {
		return dns, "", edgeFailure(o, CapabilityEdgeControlUnavailable)
	}
	for _, destination := range edgeDestinations(o) {
		if destination.Port != 443 {
			continue
		}
		ips, err := edgeLookupIPv4(ctx, destination.Host)
		if err != nil {
			continue
		}
		for _, ip := range ips {
			address, reason := edgeValidateControl(ip.String())
			if reason != "" || ip.To4() == nil {
				continue
			}
			conn, err := edgeDialControl(ctx, net.JoinHostPort(address, "443"))
			if err != nil {
				continue
			}
			conn.Close()
			return dns, address, nil
		}
	}
	return dns, "", edgeFailure(o, CapabilityEdgeControlUnavailable)
}
