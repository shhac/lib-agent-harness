package sandboxprobe

import "encoding/json"

// InterfacePythonCanary reports socket errnos using an explicitly selected client.
// With names enabled it also records namespace interface names.
func InterfacePythonCanary(client string, attempts []InterfaceAttempt, names bool, diagnosticTail ...string) string {
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
	if names {
		program += `print("namespace-interfaces:"+json.dumps([name for _,name in socket.if_nameindex()]),flush=True)
`
	}
	// Caller-owned diagnostic code stays inside the same Python invocation and
	// therefore the same command namespace. Ordinary standalone bytes are unchanged.
	for _, tail := range diagnosticTail {
		program += tail
	}
	return ShellQuote(client) + " -I -c " + ShellQuote(program) + " " + ShellQuote(string(payload)) + "\n"
}
