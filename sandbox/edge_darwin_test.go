package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/shhac/lib-agent-harness/internal/edgeproxy"
	"github.com/shhac/lib-agent-harness/internal/sandboxbridge"
	"github.com/shhac/lib-agent-harness/internal/testenv"
	"github.com/shhac/lib-agent-harness/process"
)

func TestEdgeProfileSharesCommandPolicy(t *testing.T) {
	l := workbenchLayout{Work: "/work", Home: "/runtime", Tmp: "/runtime/tmp", Read: []string{"/read"}, System: workbenchSystemDirs(), Write: true}
	profile := edgeProfile(l, 12345, "")
	for _, name := range []string{"claude/agents", "claude/commands", "claude/skills", "prompts", "rules"} {
		path := strconv.Quote(filepath.Join(l.Home, name))
		for _, operation := range []string{"file-write*", "file-link"} {
			if !strings.Contains(profile, "(deny "+operation+" (subpath "+path+"))") {
				t.Fatal("runtime instruction loader writable", name, operation)
			}
		}
	}
	if digest := fmt.Sprintf("%x", sha256.Sum256([]byte(profile))); digest != "463a01b22b74a74e1eb79bff1f909764e2a3e71f1395430e0ccc6cbe4134da9f" {
		t.Fatalf("edge template changed: %s", digest)
	}
	shared, added, ok := strings.Cut(profile, seatbeltProfile(l))
	if !ok || shared != "" {
		t.Fatal("shared command policy replaced")
	}
	// The shared policy's own scratch Unix-socket grants stay; the edge adds
	// only the proxy port.
	if strings.Contains(added, "network-bind") || strings.Contains(added, "network-inbound") || strings.Contains(added, "unix-socket") {
		t.Fatal("unsolicited network grant")
	}
	if strings.Count(added, "network-outbound") != 1 || !strings.Contains(added, `localhost:12345`) {
		t.Fatal("proxy grant missing or widened")
	}
	withTools := edgeProfile(l, 12345, "/tool/socket")
	if !strings.Contains(withTools, `(path-literal "/tool/socket")`) {
		t.Fatal("literal socket grant lost")
	}
	l.Loopback = true
	if !strings.Contains(edgeProfile(l, 12345, ""), "Binds and inbound on all local interfaces") {
		t.Fatal("loopback claim changed")
	}
	if edgeSeatbeltVersion != "seatbelt-edge-v4" {
		t.Fatal("revision changed without pin")
	}
}

func TestEdgeReadGrantsKeepSharedSemantics(t *testing.T) {
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	work, home, read := filepath.Join(root, "work"), filepath.Join(root, "runtime"), filepath.Join(root, "read")
	for _, dir := range []string{work, home, read} {
		if err = os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(filepath.Join(read, "credentials.json"), []byte("unrelated caller fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := buildEdge(sandboxbridge.EdgeConfig{Work: work, Runtime: home, Read: []string{read}, Binary: "/usr/bin/true", CLIHome: filepath.Join(root, "login"), ProxyPort: 12345, Write: true})
	if err != nil || plan.Key == "" {
		t.Fatal("caller grant acquired credential-walk semantics", err)
	}
	install := filepath.Join(root, "install")
	if err = os.MkdirAll(install, 0700); err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string]string{filepath.Join(install, "cli"): "#!/bin/sh\n", filepath.Join(install, "auth.json"): "{}"} {
		if err = os.WriteFile(path, []byte(data), 0700); err != nil {
			t.Fatal(err)
		}
	}
	_, err = buildEdge(sandboxbridge.EdgeConfig{Work: work, Runtime: home, Binary: filepath.Join(install, "cli"), CLIHome: filepath.Join(root, "login"), ProxyPort: 12345})
	if err == nil || err.Error() != "edge_install_credentials" {
		t.Fatal("installation credential not specifically refused", err)
	}
}

// Component evidence only: this tests the real shared edge and a fake provider,
// not installed CLI compatibility or an off-machine control. Those are gated
// separately in the installed-CLI CI test and in every session's launch canary.
func TestEdgeRealSeatbeltProviderAndFiles(t *testing.T) {
	testenv.RequireNestedSandbox(t)
	testenv.RequireLoopback(t)
	root := t.TempDir()
	work := filepath.Join(root, "work")
	home := filepath.Join(root, "runtime")
	for _, dir := range []string{work, home, filepath.Join(home, "tmp"), filepath.Join(root, "outside")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(root, "outside", "secret")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "fake-provider-only") }))
	defer server.Close()
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.TLS.Certificates[0].Certificate[0]})
	ca := filepath.Join(home, "ca.pem")
	if err := os.WriteFile(ca, certificate, 0600); err != nil {
		t.Fatal(err)
	}
	p, err := edgeproxy.NewCanary(nil, "example.com", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	o := sandboxbridge.EdgeConfig{Work: work, Runtime: home, Binary: "/usr/bin/curl", CLIHome: filepath.Join(root, "login"), Write: true, Env: []string{"PATH=/usr/bin:/bin", "HTTPS_PROXY=" + p.Endpoint(), "NO_PROXY="}}
	// The listener endpoint belongs only to this launch profile.
	endpoint, err := url.Parse(p.Endpoint())
	if err != nil {
		t.Fatal(err)
	}
	proxyPort, err := strconv.Atoi(endpoint.Port())
	if err != nil {
		t.Fatal(err)
	}
	o.ProxyPort = proxyPort
	plan, err := buildEdge(o)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	args := append(append([]string(nil), plan.Args[:len(plan.Args)-1]...), "/bin/sh", "-c", `/usr/bin/curl --silent --show-error --max-time 4 --cacert "$1" https://example.com/ && printf '\n'; printf 'allowed' > inside; cat inside; if cat "$2" 2>/dev/null; then echo escape-read; else echo denied-read; fi; if /bin/sh -c 'cat "$1"' child "$2" 2>/dev/null; then echo escape-child; else echo denied-child; fi`, "edge", ca, outside)
	cmd, proc, err := process.Command(ctx, plan.Binary, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer proc.Close()
	cmd.Dir = work
	cmd.Env = plan.Env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err = proc.Run(); err != nil {
		t.Fatal("real edge component failed", err)
	}
	text := out.String()
	if !strings.Contains(text, "fake-provider-only") || !strings.Contains(text, "allowed") || !strings.Contains(text, "denied-read") || !strings.Contains(text, "denied-child") || strings.Contains(text, "escape-") {
		t.Fatal("edge component observations missing or escaped")
	}
}
