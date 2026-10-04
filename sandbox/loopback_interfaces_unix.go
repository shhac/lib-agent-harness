//go:build darwin || linux

package sandbox

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
)

// interfaceCanary uses a disposable native socket client, not inference.
// Darwin uses base-system Perl; Linux LocalOnly retains its Python client.
// Each attempt reports its actual errno; failures cannot look like a denial.
func interfaceCanary(client string, attempts []interfaceAttempt) string {
	if runtime.GOOS == "darwin" {
		return interfacePerlCanary(attempts)
	}
	if attempts == nil {
		attempts = []interfaceAttempt{}
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
	return workbenchShellQuote(client) + " -I -c " + workbenchShellQuote(program) + " " + workbenchShellQuote(string(payload)) + "\n"
}

// macOS ships Perl itself; python3 may be an Xcode installation shim.
func interfacePerlCanary(attempts []interfaceAttempt) string {
	program := `use strict; use warnings; use Socket qw(:all);
$|=1;
my $i=0;
while (@ARGV) {
 my ($host,$port,$op)=splice(@ARGV,0,3);
 $port=9 if $op eq "udp" && $port==0;
 my ($error,@addresses)=getaddrinfo($host,$port,{family=>index($host,":")>=0?AF_INET6:AF_INET,socktype=>$op eq "bind"?SOCK_STREAM:SOCK_DGRAM,flags=>AI_NUMERICHOST});
 my $result=999;
 if (!$error && @addresses) {
  my $a=$addresses[0];
  if (socket(my $s,$a->{family},$a->{socktype},$a->{protocol})) {
   my $ok;
   if ($op eq "udp") { $ok=defined(send($s,"canary",0,$a->{addr})); }
   else { $ok=bind($s,$a->{addr}); if ($ok && $op eq "bind") { $ok=listen($s,1); } }
   $result=$ok?0:0+$!;
   close($s);
  } else { $result=0+$!; }
 }
 print "interface-result:$i:$result\n";
 $i++;
}
print "interface-canary-ran\n";
`
	args := []string{"/usr/bin/perl", "-e", workbenchShellQuote(program), "--"}
	for _, a := range attempts {
		args = append(args, workbenchShellQuote(a.Address), fmt.Sprint(a.Port), workbenchShellQuote(a.Operation))
	}
	return strings.Join(args, " ") + "\n"
}
