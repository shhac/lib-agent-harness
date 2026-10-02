package session

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/shhac/lib-agent-harness/process"
	"golang.org/x/sys/unix"
)

// The nested helper uses raw clone rather than unshare from a multithreaded Go
// runtime. Between clone and _exit the child makes only raw syscalls; it never
// enters Go's scheduler, allocator or syscall wrappers.
func TestWorkbenchLinuxRawSyscallHelper(t *testing.T) {
	mode := os.Getenv("WORKBENCH_ADVERSARY")
	if mode == "" {
		return
	}
	// Even a launcher regression must not aim raw mount calls at the host.
	for _, kind := range []string{"pid", "mnt"} {
		inside, e := os.Readlink("/proc/self/ns/" + kind)
		host := os.Getenv("WORKBENCH_HOST_" + strings.ToUpper(kind) + "_NS")
		if e != nil || host == "" || inside == host {
			fmt.Println("adversary is not in the requested namespaces")
			os.Exit(1)
		}
	}
	git := ".git"
	x := "/tmp/x"
	for _, flags := range []int{0, unix.MNT_DETACH} {
		if unix.Unmount(git, flags) == nil {
			fmt.Println("escape-unmount")
			os.Exit(1)
		}
	}
	if unix.Mount("", git, "", unix.MS_REMOUNT|unix.MS_BIND, "") == nil {
		fmt.Println("escape-remount")
		os.Exit(1)
	}
	if unix.Mount(".", x, "", unix.MS_BIND, "") == nil {
		fmt.Println("escape-bind")
		os.Exit(1)
	}
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	caps := [2]unix.CapUserData{{Effective: 1 << unix.CAP_SYS_ADMIN, Permitted: 1 << unix.CAP_SYS_ADMIN}}
	if unix.Capset(&header, &caps[0]) == nil {
		fmt.Println("escape-capset")
		os.Exit(1)
	}
	// This direct unshare must fail with controls. In an ordinary Go runtime
	// its failure without controls proves nothing, hence the raw clone below.
	if mode == "controlled" && unix.Unshare(unix.CLONE_NEWUSER|unix.CLONE_NEWNS) == nil {
		fmt.Println("escape-unshare")
		os.Exit(1)
	}
	source := []byte(".\x00")
	target := []byte("/tmp/x\x00")
	gitPath := []byte(".git\x00")
	uidPath := []byte("/proc/self/uid_map\x00")
	uidMap := []byte("0 " + strconv.Itoa(os.Getuid()) + " 1\n")
	sp, tp, gp, up := uintptr(unsafe.Pointer(&source[0])), uintptr(unsafe.Pointer(&target[0])), uintptr(unsafe.Pointer(&gitPath[0])), uintptr(unsafe.Pointer(&uidPath[0]))
	ump := uintptr(unsafe.Pointer(&uidMap[0]))
	uml := uintptr(len(uidMap))
	success := []byte("nested-clone\nnested-bind-refused\n")
	successPtr := uintptr(unsafe.Pointer(&success[0]))
	successLen := uintptr(len(success))
	pid, _, errno := syscall.RawSyscall6(syscall.SYS_CLONE, uintptr(unix.CLONE_NEWUSER|unix.CLONE_NEWNS|syscall.SIGCHLD), 0, 0, 0, 0, 0)
	if errno != 0 {
		if mode != "controlled" {
			fmt.Printf("clone unavailable: %d\n", errno)
			os.Exit(1)
		}
		fmt.Println("controls-refused-clone")
		os.Exit(0)
	}
	if pid == 0 {
		fd, _, e := syscall.RawSyscall6(syscall.SYS_OPENAT, ^uintptr(99), up, unix.O_WRONLY, 0, 0, 0)
		if e != 0 {
			syscall.RawSyscall(syscall.SYS_EXIT, 2, 0, 0)
		}
		_, _, e = syscall.RawSyscall(syscall.SYS_WRITE, fd, ump, uml)
		if e != 0 {
			syscall.RawSyscall(syscall.SYS_EXIT, 3, 0, 0)
		}
		syscall.RawSyscall(syscall.SYS_CLOSE, fd, 0, 0)
		_, _, e = syscall.RawSyscall(syscall.SYS_UMOUNT2, gp, 0, 0)
		if e == 0 {
			syscall.RawSyscall(syscall.SYS_EXIT, 4, 0, 0)
		}
		_, _, e = syscall.RawSyscall(syscall.SYS_UMOUNT2, gp, unix.MNT_DETACH, 0)
		if e == 0 {
			syscall.RawSyscall(syscall.SYS_EXIT, 5, 0, 0)
		}
		_, _, e = syscall.RawSyscall6(syscall.SYS_MOUNT, 0, gp, 0, unix.MS_REMOUNT|unix.MS_BIND, 0, 0)
		if e == 0 {
			syscall.RawSyscall(syscall.SYS_EXIT, 6, 0, 0)
		}
		_, _, e = syscall.RawSyscall6(syscall.SYS_MOUNT, sp, tp, 0, unix.MS_BIND, 0, 0)
		// EINVAL is the kernel's inherited-mount locking witness, not a missing
		// mount utility or a permission error from the Go runtime.
		if e != syscall.EINVAL {
			syscall.RawSyscall(syscall.SYS_EXIT, 7, 0, 0)
		}
		syscall.RawSyscall(syscall.SYS_WRITE, 1, successPtr, successLen)
		syscall.RawSyscall(syscall.SYS_EXIT, 0, 0, 0)
		for {
		} // _exit never returns
	}
	var status syscall.WaitStatus
	if _, e := syscall.Wait4(int(pid), &status, 0, nil); e != nil || !status.Exited() || status.ExitStatus() != 0 {
		fmt.Printf("nested helper: %v %d\n", e, status)
		os.Exit(1)
	}
	runtime.KeepAlive(source)
	runtime.KeepAlive(target)
	runtime.KeepAlive(gitPath)
	runtime.KeepAlive(uidPath)
	runtime.KeepAlive(uidMap)
	runtime.KeepAlive(success)
	if mode == "controlled" {
		fmt.Println("escape-clone")
		os.Exit(1)
	}
	os.Exit(0)
}

func TestWorkbenchLinuxAdversarialMounts(t *testing.T) {
	binary, _ := requireWorkbenchBwrap(t)
	l := linuxTestLayout(t)
	if e := os.Mkdir(filepath.Join(l.Work, ".git"), 0700); e != nil {
		t.Fatal(e)
	}
	l.Write = true
	config := filepath.Join(l.Work, ".git", "config")
	if e := os.WriteFile(config, []byte("unchanged\n"), 0600); e != nil {
		t.Fatal(e)
	}
	executable, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	helperDir := t.TempDir()
	helper := filepath.Join(helperDir, "helper")
	data, e := os.ReadFile(executable)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(helper, data, 0700); e != nil {
		t.Fatal(e)
	}
	l.Read = []string{helperDir}
	for _, controlled := range []bool{true, false} {
		t.Run(strconv.FormatBool(controlled), func(t *testing.T) {
			args, e := bwrapArgs(l)
			if e != nil {
				t.Fatal(e)
			}
			if !controlled {
				filtered := args[:0]
				for i := 0; i < len(args); i++ {
					if args[i] == "--disable-userns" {
						continue
					}
					if args[i] == "--cap-drop" {
						i++
						continue
					}
					filtered = append(filtered, args[i])
				}
				args = filtered
			}
			// /tmp/x must exist before the raw mount; it is on the sandbox's tmpfs.
			args = append(args, "--chdir", l.Work, "--", "/bin/sh", "-c", "mkdir /tmp/x; exec "+workbenchShellQuote(helper)+" -test.run=^TestWorkbenchLinuxRawSyscallHelper$")
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd, p, e := process.Command(ctx, binary, args...)
			if e != nil {
				t.Fatal(e)
			}
			defer p.Close()
			mode := "uncontrolled"
			if controlled {
				mode = "controlled"
			}
			cmd.Env = append(workbenchProbeEnvironment(l), "WORKBENCH_ADVERSARY="+mode)
			for _, kind := range []string{"pid", "mnt"} {
				host, e := os.Readlink("/proc/self/ns/" + kind)
				if e != nil {
					t.Fatal(e)
				}
				cmd.Env = append(cmd.Env, "WORKBENCH_HOST_"+strings.ToUpper(kind)+"_NS="+host)
			}
			out := &workbenchOutput{limit: 4096}
			cmd.Stdout, cmd.Stderr = out, out
			cmd.WaitDelay = time.Second
			if e = p.Run(); e != nil {
				t.Fatalf("raw helper %v: %s", e, out.text())
			}
			witness := "nested-clone\nnested-bind-refused"
			if controlled {
				witness = "controls-refused-clone"
			}
			if !strings.Contains(out.text(), witness) {
				t.Fatal(out.text())
			}
			actual, e := os.ReadFile(config)
			if e != nil || !bytes.Equal(actual, []byte("unchanged\n")) {
				t.Fatalf("git changed: %q %v", actual, e)
			}
		})
	}
}
