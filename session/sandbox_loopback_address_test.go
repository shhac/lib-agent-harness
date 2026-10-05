package session

import (
	"net"
	"strings"
)

func claudeInterfaceAddressClass(address string) string {
	host, _, _ := strings.Cut(address, "%")
	ip := net.ParseIP(host)
	if ip == nil {
		return "unknown"
	}
	family := "IPv6"
	if ip.To4() != nil {
		family = "IPv4"
	}
	switch {
	case ip.IsUnspecified():
		return family + " wildcard"
	case ip.IsLinkLocalUnicast():
		return family + " link-local"
	case ip.IsPrivate():
		return family + " private"
	case ip.IsLoopback():
		return family + " loopback"
	default:
		return family + " other"
	}
}
