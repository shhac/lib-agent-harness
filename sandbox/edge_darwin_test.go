package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
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
	"github.com/shhac/lib-agent-harness/internal/nativecli"
	"github.com/shhac/lib-agent-harness/internal/sandboxbridge"
	"github.com/shhac/lib-agent-harness/internal/testenv"
	"github.com/shhac/lib-agent-harness/process"
)

func TestEdgeProfileSharesCommandPolicy(t *testing.T) {
	l := workbenchLayout{Work: "/work", Home: "/runtime", Tmp: "/runtime/tmp", Read: []string{"/read"}, System: workbenchSystemDirs(), Write: true}
	profile := edgeProfile(l, 12345, "")
	for _, name := range []string{"claude/agents", "claude/commands", "claude/skills", "prompts", "rules", "skills"} {
		path := strconv.Quote(filepath.Join(l.Home, name))
		for _, operation := range []string{"file-write*", "file-link"} {
			if !strings.Contains(profile, "(deny "+operation+" (subpath "+path+"))") {
				t.Fatal("runtime instruction loader writable", name, operation)
			}
		}
	}
	if digest := fmt.Sprintf("%x", sha256.Sum256([]byte(profile))); digest != "8149eea812c66ca9947ef251db2b14ae48ee483c571324dde44ecdcffda9af16" {
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
	if edgeSeatbeltVersion != "seatbelt-edge-v5" {
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
// separately in the installed-CLI test and in every session's launch canary.
// The tunnel carries plain HTTP: macOS curl's LibreSSL aborts at start-up when
// it cannot read /private/etc/ssl/openssl.cnf, which the edge does not grant,
// and provider TLS belongs to the CLI, not to this component.
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "fake-provider-only") }))
	defer server.Close()
	p, err := edgeproxy.NewCanary(nil, "example.com", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	endpoint, err := url.Parse(p.Endpoint())
	if err != nil {
		t.Fatal(err)
	}
	proxyPort, err := strconv.Atoi(endpoint.Port())
	if err != nil {
		t.Fatal(err)
	}
	// The listener endpoint belongs only to this launch profile.
	o := sandboxbridge.EdgeConfig{Work: work, Runtime: home, Binary: "/usr/bin/perl", CLIHome: filepath.Join(root, "login"), Write: true, ProxyPort: proxyPort, Env: []string{"PATH=/usr/bin:/bin", "EDGE_PROXY=" + endpoint.Host, "EDGE_PROXY_AUTH=" + endpoint.User.String()}}
	plan, err := buildEdge(o)
	if err != nil {
		t.Fatal(err)
	}
	tunnel := `use IO::Socket::INET; use MIME::Base64;
my ($h,$p)=split(/:/,$ENV{EDGE_PROXY});
my $s=IO::Socket::INET->new(PeerAddr=>$h,PeerPort=>$p,Proto=>'tcp',Timeout=>4) or die "proxy";
my $auth=encode_base64($ENV{EDGE_PROXY_AUTH},'');
print $s "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\nProxy-Authorization: Basic $auth\r\n\r\n";
my $status=<$s>; die "refused" unless $status =~ m{^HTTP/1[.]1 200 };
while (my $line=<$s>) { last if $line eq "\r\n"; }
print $s "GET / HTTP/1.0\r\nHost: example.com\r\n\r\n";
local $/; my $body=<$s>; print "$body\n";`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	args := append(append([]string(nil), plan.Args[:len(plan.Args)-1]...), "/bin/sh", "-c", `/usr/bin/perl -e "$1"; printf 'allowed' > inside; cat inside; echo; if cat "$2" 2>/dev/null; then echo escape-read; else echo denied-read; fi; if /bin/sh -c 'cat "$1"' child "$2" 2>/dev/null; then echo escape-child; else echo denied-child; fi`, "edge", tunnel, outside)
	cmd, proc, err := process.Command(ctx, plan.Binary, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer proc.Close()
	cmd.Dir = work
	cmd.Env = plan.Env
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	if err = proc.Run(); err != nil {
		t.Fatal("real edge component failed", err, errOut.String())
	}
	text := out.String()
	if !strings.Contains(text, "fake-provider-only") || !strings.Contains(text, "allowed") || !strings.Contains(text, "denied-read") || !strings.Contains(text, "denied-child") || strings.Contains(text, "escape-") {
		t.Fatal("edge component observations missing or escaped", text, errOut.String())
	}
}

func TestEdgePreferencesNameOnlyTheirDomains(t *testing.T) {
	if edgePreferenceProfile(nil) != "" {
		t.Fatal("preference access granted without a domain")
	}
	profile := edgePreferenceProfile([]string{"com.openai.codex"})
	if !strings.Contains(profile, `(allow user-preference-read (preference-domain "com.openai.codex"))`) || strings.Contains(profile, "(allow user-preference-read)") {
		t.Fatal("preference read not confined to its domain", profile)
	}
}

func TestEdgeEngineProfileAddsOnlyWhatTheEngineDeclares(t *testing.T) {
	l := workbenchLayout{Work: "/work", Home: "/runtime", Tmp: "/runtime/tmp", System: workbenchSystemDirs()}
	if got, ok := edgeEngineProfile(l, sandboxbridge.EdgeConfig{}, nativecli.EdgeInstallation{Root: "/install"}, "/install/codex"); !ok || got != "" {
		t.Fatal("an engine declaring nothing changed its edge profile", got)
	}
	c := sandboxbridge.EdgeConfig{ProtectRuntime: []string{".gemini/config/skills"}, ProtectWork: []string{".gemini"}, ReadFiles: []string{"/private/etc/ssl/cert.pem"}, Terminals: true}
	profile, ok := edgeEngineProfile(l, c, nativecli.EdgeInstallation{Root: "/owner/.local/bin", Standalone: true}, "/owner/.local/bin/agy")
	if !ok {
		t.Fatal("engine profile refused")
	}
	for _, want := range []string{
		`(allow file-read* (literal "/owner/.local/bin/agy"))`,
		`(allow process-exec (literal "/owner/.local/bin/agy"))`,
		`(allow file-read* (literal "/private/etc/ssl/cert.pem"))`,
		`(deny file-write* (subpath "/runtime/.gemini/config/skills"))`,
		`(deny file-link (subpath "/work/.gemini"))`,
		`(extension "com.apple.sandbox.pty")`,
	} {
		if !strings.Contains(profile, want) {
			t.Fatal("missing", want)
		}
	}
	if strings.Contains(profile, `(subpath "/owner/.local/bin")`) {
		t.Fatal("standalone installation folder granted")
	}
	if strings.Contains(profile, "network") {
		t.Fatal("engine profile widened the network")
	}
}

// edgeInstallFixture is a private work and runtime pair beside a folder of
// installed tools, under one resolved temporary root.
func edgeInstallFixture(t *testing.T) (root, bin, work, home string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bin = filepath.Join(root, "bin")
	work, home = filepath.Join(root, "work"), filepath.Join(root, "runtime")
	for _, dir := range []string{bin, work, home} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	return root, bin, work, home
}

func writeNative(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("\xcf\xfa\xed\xfe native"), 0700); err != nil {
		t.Fatal(err)
	}
}

func TestEdgeStandaloneInstallationGrantsOnlyTheFile(t *testing.T) {
	root, bin, work, home := edgeInstallFixture(t)
	// ~/.local/bin/agy -> ~/.local/share/agy/1.4.12/agy-arm64, beside other tools.
	native := filepath.Join(root, "share", "agy", "1.4.12", "agy-arm64")
	writeNative(t, native)
	writeNative(t, filepath.Join(filepath.Dir(native), "unrelated-tool"))
	alias := filepath.Join(bin, "agy")
	if err := os.Symlink(native, alias); err != nil {
		t.Fatal(err)
	}
	config := sandboxbridge.EdgeConfig{Work: work, Runtime: home, Binary: alias, CLIHome: filepath.Join(root, "login"), ProxyPort: 1234, Standalone: true}
	plan, err := buildEdge(config)
	if err != nil {
		t.Fatal("standalone alias refused", err)
	}
	profile := plan.Args[1]
	if !strings.Contains(profile, "(literal "+strconv.Quote(native)+")") || strings.Contains(profile, "(subpath "+strconv.Quote(filepath.Dir(native))+")") || strings.Contains(profile, "(subpath "+strconv.Quote(bin)+")") {
		t.Fatal("symlinked standalone binary became a folder grant")
	}
	launcher := filepath.Join(root, "launcher", "agy")
	if err := os.MkdirAll(filepath.Dir(launcher), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\nexec agy-arm64 \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	config.Binary = launcher
	if _, err = buildEdge(config); !errors.Is(err, nativecli.ErrEdgeStandalone) {
		t.Fatal("launcher script granted rather than refused", err)
	}
	config.Binary, config.CLIHome = alias, filepath.Dir(native)
	if _, err = buildEdge(config); err == nil || err.Error() != "edge_install_credentials" {
		t.Fatal("standalone binary inside the login home admitted", err)
	}
}

func TestEdgeRefusesAnExecutableTheSessionCanWrite(t *testing.T) {
	_, bin, work, home := edgeInstallFixture(t)
	writeNative(t, filepath.Join(bin, "agy"))
	for name, binary := range map[string]string{
		"workspace":       filepath.Join(work, "tools", "agy"),
		"runtime home":    filepath.Join(home, "bin", "agy"),
		"runtime scratch": filepath.Join(home, "tmp", "agy"),
	} {
		writeNative(t, binary)
		alias := filepath.Join(bin, "alias-"+strings.ReplaceAll(name, " ", "-"))
		if err := os.Symlink(binary, alias); err != nil {
			t.Fatal(err)
		}
		for _, standalone := range []bool{true, false} {
			for _, invoked := range []string{binary, alias} {
				c := sandboxbridge.EdgeConfig{Work: work, Runtime: home, Binary: invoked, CLIHome: filepath.Join(bin, "login"), ProxyPort: 1234, Write: true, Standalone: standalone}
				if _, err := buildEdge(c); err == nil || err.Error() != "edge_install_writable" {
					t.Fatalf("%s (standalone %v, via %s): executable the session can replace admitted: %v", name, standalone, invoked, err)
				}
			}
		}
	}
	c := sandboxbridge.EdgeConfig{Work: work, Runtime: home, Binary: filepath.Join(bin, "agy"), CLIHome: filepath.Join(bin, "login"), ProxyPort: 1234, Standalone: true, Bridge: filepath.Join(work, "bridge")}
	writeNative(t, c.Bridge)
	if _, err := buildEdge(c); err == nil || err.Error() != "edge_install_writable" {
		t.Fatal("tool bridge inside the workspace admitted", err)
	}
	c.Bridge = ""
	if _, err := buildEdge(c); err != nil {
		t.Fatal("installation outside the writable roots refused", err)
	}
}

func TestEdgeWritableRuntimeFollowsItsProtectedTree(t *testing.T) {
	l := workbenchLayout{Work: "/work", Home: "/runtime", Tmp: "/runtime/tmp", System: workbenchSystemDirs()}
	c := sandboxbridge.EdgeConfig{ProtectRuntime: []string{".gemini"}, WritableRuntime: []string{".gemini/state/", ".gemini/state.db"}}
	profile, ok := edgeEngineProfile(l, c, nativecli.EdgeInstallation{}, "")
	if !ok {
		t.Fatal("refused")
	}
	deny := strings.Index(profile, `(deny file-write* (subpath "/runtime/.gemini"))`)
	dir := strings.Index(profile, `(allow file-write* (subpath "/runtime/.gemini/state"))`)
	file := strings.Index(profile, `(allow file-write* (regex #"^/runtime/[.]gemini/state[.]db[^/]*$"))`)
	if deny < 0 || dir < deny || file < deny {
		t.Fatal("state exceptions missing or before the protection they open", profile)
	}
	if strings.Contains(profile, "allow file-link") {
		t.Fatal("links admitted inside the protected tree")
	}
	if _, ok = edgeEngineProfile(l, sandboxbridge.EdgeConfig{WritableRuntime: []string{`.gemini/odd"name`}}, nativecli.EdgeInstallation{}, ""); ok {
		t.Fatal("unspellable path admitted")
	}
}
