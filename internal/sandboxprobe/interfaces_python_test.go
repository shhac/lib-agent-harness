package sandboxprobe

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestInterfacePythonCanaryBytesPinned(t *testing.T) {
	want, err := os.ReadFile("testdata/interface-python-canary.txt")
	if err != nil {
		t.Fatal(err)
	}
	attempts := []InterfaceAttempt{{Address: "192.0.2.1", Operation: "bind"}, {Address: "fe80::1%en0", Operation: "udp", Port: 9}}
	if InterfacePythonCanary("/usr/bin/python3", attempts, false) != string(want) {
		t.Fatal("standalone Linux canary bytes changed")
	}
	if legacyLinuxPythonCanary("/usr/bin/python3", attempts) != string(want) {
		t.Fatal("pin does not match the retained pre-move generator")
	}
	if !strings.Contains(InterfacePythonCanary("/usr/bin/python3", attempts, true), "socket.if_nameindex()") {
		t.Fatal("namespace names missing")
	}
}

// Frozen from sandbox/loopback_interfaces_unix.go before the relocation. Do
// not regenerate this from InterfacePythonCanary: it is the migration oracle.
// The old workbenchShellQuote delegated to this same single-quote expansion.
func legacyLinuxPythonCanary(client string, attempts []InterfaceAttempt) string {
	if attempts == nil {
		attempts = []InterfaceAttempt{}
	}
	payload, _ := json.Marshal(attempts)
	program := `import json,socket,sys
for i,a in enumerate(json.loads(sys.argv[1])):
 s=None
 try:
  host=a["Address"]
  if a["NamespaceScope"]: host=host.split("%",1)[0]
  family=socket.AF_INET6 if ":" in host else socket.AF_INET
  port=a["Port"]
  if a["Operation"]=="udp" and port==0: port=9
  address=socket.getaddrinfo(host,port,family,socket.SOCK_DGRAM,0,socket.AI_NUMERICHOST)[0][4]
  if a["NamespaceScope"]: address=(address[0],address[1],address[2],socket.if_nametoindex("lo"))
  s=socket.socket(family,socket.SOCK_STREAM if a["Operation"]=="bind" else socket.SOCK_DGRAM)
  s.settimeout(0.3)
  if a["Operation"]=="udp": s.sendto(b"canary",address)
  else:
   s.bind(address)
   if a["Operation"]=="bind": s.listen(1)
  result=0
 except OSError as e: result=e.errno if e.errno is not None else 999
 except Exception: result=999
 finally:
  if s is not None: s.close()
 print("interface-result:%d:%d"%(i,result),flush=True)
print("interface-canary-ran",flush=True)
`
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	return quote(client) + " -I -c " + quote(program) + " " + quote(string(payload)) + "\n"
}

func TestLinuxPythonMovePreservesPreMoveBytes(t *testing.T) {
	for _, a := range [][]InterfaceAttempt{nil, {}, InterfaceAttempts([]string{"192.0.2.1", "fe80::1%eth0"}, true, true), NamespaceInterfaceAttempts([]string{"fe80::2%eth0"})} {
		if got, want := InterfacePythonCanary("/usr/bin/python3", a, false), legacyLinuxPythonCanary("/usr/bin/python3", a); got != want {
			t.Fatal("rename changed Linux standalone command")
		}
	}
}
