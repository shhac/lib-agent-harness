package session

import (
	"github.com/shhac/lib-agent-harness/internal/testenv"
	"golang.org/x/sys/windows"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"
)

func TestWorkbenchWindowsNewFileModeDoesNotSetReadOnly(t *testing.T) {
	w, work, _ := testWorkspace(t)
	w.id = newID()
	w.mode = 0400
	r := call(t, w, workbenchWriteFile, map[string]any{"path": "nested/new", "content": "old"})
	if r.IsError {
		t.Fatal(r)
	}
	f, err := os.OpenFile(filepath.Join(work, "nested", "new"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	r = call(t, w, workbenchEditFile, map[string]any{"path": "nested/new", "old": "old", "new": "new"})
	if r.IsError {
		t.Fatal(r)
	}
}

func TestWorkbenchWindowsWritesInheritDirectoryACL(t *testing.T) {
	w, work, _ := testWorkspace(t)
	w.id = newID()
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + user.User.Sid.String() + ")(A;OICI;GR;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(work, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"new", "nested/new"} {
		r := call(t, w, workbenchWriteFile, map[string]any{"path": path, "content": "content"})
		if r.IsError {
			t.Fatal(r)
		}
		child, err := windows.GetNamedSecurityInfo(filepath.Join(work, filepath.FromSlash(path)), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		acl, _, err := child.DACL()
		if err != nil || acl == nil {
			t.Fatalf("no inherited DACL: %v", err)
		}
		owner, world := false, false
		for i := uint32(0); i < uint32(acl.AceCount); i++ {
			var ace *windows.ACCESS_ALLOWED_ACE
			if err := windows.GetAce(acl, i, &ace); err != nil {
				t.Fatal(err)
			}
			if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags&windows.INHERITED_ACE == 0 {
				t.Fatal("new file has an explicit or unexpected ACE")
			}
			sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
			if sid == user.User.Sid.String() && ace.Mask == 0x1f01ff {
				owner = true
			}
			if sid == "S-1-1-0" && ace.Mask&windows.FILE_GENERIC_READ == windows.FILE_GENERIC_READ {
				world = true
			}
		}
		if !owner || !world {
			t.Fatalf("directory grants not inherited: owner=%t world=%t", owner, world)
		}
	}
}

func TestWorkbenchWindowsShortNameWriteRefused(t *testing.T) {
	w, work, _ := testWorkspace(t)
	w.id = newID()
	writeFile(t, filepath.Join(work, ".git", "config"), "metadata")
	alias := shortName(t, filepath.Join(work, ".git"))
	if alias == "" {
		testenv.SkipIfRefused(t, "the temporary volume makes no 8.3 short names", fs.ErrPermission)
	}
	for _, tool := range []string{workbenchWriteFile, workbenchEditFile} {
		args := map[string]any{"path": alias + "/config"}
		if tool == workbenchWriteFile {
			args["content"] = "bad"
		} else {
			args["old"], args["new"] = "metadata", "bad"
		}
		r := call(t, w, tool, args)
		if !r.IsError || !strings.Contains(r.Content, wbReserved) {
			t.Fatalf("alias %s: %+v", alias, r)
		}
	}
	if data, err := os.ReadFile(filepath.Join(work, ".git", "config")); err != nil || string(data) != "metadata" {
		t.Fatal("short-name write changed .git")
	}
}
