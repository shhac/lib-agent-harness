package sandboxprobe

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestResolverParsersAndSelection(t *testing.T) {
	configured := ResolverAddresses("# comment\nnameserver 127.0.0.53\nnameserver 192.0.2.53 # corp\nsearch private.example\nnameserver 2001:db8::53\n")
	if !reflect.DeepEqual(configured, []string{"127.0.0.53", "192.0.2.53", "2001:db8::53"}) {
		t.Fatal(configured)
	}
	scutil := ScutilAddresses("resolver #1\n nameserver[0] : 127.0.0.1\n nameserver[1] : 2001:db8::53\n flags : Scoped, Request A records\nresolver #2\n nameserver[0] : 192.0.2.53\n")
	if !reflect.DeepEqual(scutil, []string{"127.0.0.1", "2001:db8::53", "192.0.2.53"}) {
		t.Fatal(scutil)
	}
	local := []netip.Addr{netip.MustParseAddr("192.0.2.100")}
	for _, tc := range []struct {
		name, caller         string
		configured, upstream []string
		want                 ControlTarget
		reason               string
	}{
		{"caller wins", "192.0.2.4", configured, nil, ControlTarget{"192.0.2.4", "caller"}, ""},
		{"private resolver", "", []string{"10.20.30.40"}, nil, ControlTarget{"10.20.30.40", "configured"}, ""},
		{"configured", "", configured, nil, ControlTarget{"192.0.2.53", "configured"}, ""},
		{"scoped ipv6", "", scutil, nil, ControlTarget{"2001:db8::53", "configured"}, ""},
		{"systemd upstream", "", []string{"127.0.0.53"}, []string{"10.20.30.40"}, ControlTarget{"10.20.30.40", "systemd_upstream"}, ""},
		{"missing upstream", "", []string{"127.0.0.53"}, nil, ControlTarget{}, NoOffMachineResolver},
		{"local upstream", "", []string{"127.0.0.53"}, []string{"192.0.2.100", "::1"}, ControlTarget{}, NoOffMachineResolver},
		{"empty", "", nil, nil, ControlTarget{}, NoOffMachineResolver},
		{"caller local never falls back", "192.0.2.100", configured, nil, ControlTarget{"", "caller"}, ControlOnThisMachine},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := selectControl(tc.caller, tc.configured, tc.upstream, local)
			if got != tc.want || reason != tc.reason {
				t.Fatalf("%+v %s", got, reason)
			}
		})
	}
	if _, reason := validateControl("::ffff:192.0.2.100", local); reason != ControlOnThisMachine {
		t.Fatal(reason)
	}
}

func TestDNSControlEvidence(t *testing.T) {
	for _, kind := range []string{"match", "mismatch", "not reply", "short", "udp silence", "tcp refusal", "slow", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			if kind == "cancel" {
				cancel()
			}
			var wg sync.WaitGroup
			var mu sync.Mutex
			var networks []string
			dial := func(ctx context.Context, network, address string) (net.Conn, error) {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				mu.Lock()
				networks = append(networks, network)
				mu.Unlock()
				if address != "fixture:53" {
					t.Errorf("destination changed: %s", address)
				}
				if network == "tcp" && kind == "tcp refusal" {
					return nil, errors.New("synthetic refusal")
				}
				c, s := net.Pipe()
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer s.Close()
					if network == "tcp" {
						return
					}
					query := make([]byte, 512)
					n, err := s.Read(query)
					if err != nil {
						return
					}
					if n != 17 || !reflect.DeepEqual(query[2:n], []byte{0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 2, 0, 1}) {
						t.Errorf("unexpected DNS query shape")
					}
					if kind == "udp silence" || kind == "slow" {
						<-ctx.Done()
						return
					}
					reply := append([]byte(nil), query[:n]...)
					reply[2] = 0x80
					if kind == "mismatch" {
						reply[0] ^= 0xff
					}
					if kind == "not reply" {
						reply[2] = 0
					}
					if kind == "short" {
						reply = reply[:2]
					}
					_, _ = s.Write(reply)
				}()
				return c, nil
			}
			start := time.Now()
			reason := proveDNSControl(ctx, "fixture:53", dial)
			cancel()
			wg.Wait()
			want := ""
			switch kind {
			case "mismatch", "not reply", "short":
				want = UDPUnanswered
			case "tcp refusal":
				want = TCPUnanswered
			case "udp silence", "slow", "cancel":
				want = ControlDeadline
			}
			if reason != want {
				t.Fatalf("%s want %s", reason, want)
			}
			if time.Since(start) > time.Second {
				t.Fatal("control hung")
			}
			if kind == "match" && !reflect.DeepEqual(networks, []string{"udp", "tcp"}) {
				t.Fatal("missing outside positive")
			}
			// Reasons are fixed vocabulary; no query or response data can leak.
			if reason != "" && reason != want {
				t.Fatal("non-structural diagnostic")
			}
		})
	}
}

func TestConfiguredControlFallbackWithinDeadline(t *testing.T) {
	targets, reason := selectControls("", []string{"2001:db8::53", "192.0.2.53", "192.0.2.53"}, nil, nil)
	if reason != "" || len(targets) != 2 {
		t.Fatal(targets, reason)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	calls := 0
	got, reason := proveControlCandidates(ctx, targets, func(ctx context.Context, target ControlTarget) string {
		calls++
		if target.Addr == "2001:db8::53" {
			<-ctx.Done()
			return ControlDeadline
		}
		return ""
	})
	if reason != "" || got.Addr != "192.0.2.53" || calls != 2 {
		t.Fatal(got, reason, calls)
	}
	pinned, _ := selectControls("192.0.2.54", []string{"192.0.2.53"}, nil, nil)
	calls = 0
	got, reason = proveControlCandidates(ctx, pinned, func(context.Context, ControlTarget) string { calls++; return TCPUnanswered })
	if calls != 1 || reason != TCPUnanswered || got.Source != "caller" || got.Addr != "192.0.2.54" {
		t.Fatal("caller fell back", got, reason, calls)
	}
}

func TestControlMetadataSanitization(t *testing.T) {
	for _, target := range []ControlTarget{{"raw query contents", "caller"}, {"192.0.2.53", "raw source"}, {"127.0.0.1", "configured"}} {
		if SanitizeControl(target) != (ControlTarget{}) {
			t.Fatal("untrusted metadata escaped")
		}
	}
	if got := SanitizeControl(ControlTarget{"::ffff:192.0.2.53", "systemd_upstream"}); got != (ControlTarget{"192.0.2.53", "systemd_upstream"}) {
		t.Fatal(got)
	}
}
