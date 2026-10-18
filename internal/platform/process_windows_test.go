//go:build windows

package platform

import (
	"os/exec"
	"reflect"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestHideWindowPreservesAttributes(t *testing.T) {
	original := &syscall.SysProcAttr{
		CreationFlags:              windows.CREATE_NEW_PROCESS_GROUP,
		Token:                      syscall.Token(123),
		ProcessAttributes:          &syscall.SecurityAttributes{InheritHandle: 1},
		ThreadAttributes:           &syscall.SecurityAttributes{InheritHandle: 1},
		NoInheritHandles:           true,
		AdditionalInheritedHandles: []syscall.Handle{456},
		ParentProcess:              syscall.Handle(789),
	}
	saved := *original
	cmd := &exec.Cmd{SysProcAttr: original}
	HideWindow(cmd)
	if cmd.SysProcAttr == original {
		t.Fatal("HideWindow mutated shared process attributes")
	}
	if !reflect.DeepEqual(*original, saved) {
		t.Fatal("HideWindow changed the caller's shared attributes")
	}
	expected := saved
	expected.HideWindow = true
	expected.CreationFlags |= windows.CREATE_NO_WINDOW
	if !reflect.DeepEqual(*cmd.SysProcAttr, expected) {
		t.Fatal("HideWindow failed to preserve other process attributes")
	}
	HideWindow(cmd)
	if !reflect.DeepEqual(*cmd.SysProcAttr, expected) {
		t.Fatal("HideWindow is not idempotent")
	}
}

func TestHideWindowInitializesAttributes(t *testing.T) {
	cmd := &exec.Cmd{}
	HideWindow(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow || cmd.SysProcAttr.CreationFlags != windows.CREATE_NO_WINDOW {
		t.Fatal("HideWindow did not set helper window flags")
	}
}

func TestUnassignedJobLifetimeFlags(t *testing.T) {
	// NEVER call ContainCurrentProcess in this test runner. Create an EMPTY
	// job and query it; closing this handle cannot kill the runner or any child.
	job, err := newKillOnCloseJob()
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(job)
	var limits windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if err := windows.QueryInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits)), nil); err != nil {
		t.Fatal(err)
	}
	if limits.BasicLimitInformation.LimitFlags != windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE {
		t.Fatalf("job has wrong lifetime or breakaway flags: %#x", limits.BasicLimitInformation.LimitFlags)
	}
	// This API is not wrapped by our pinned x/sys version.
	getHandleInformation := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetHandleInformation")
	var handleFlags uint32
	result, _, callErr := getHandleInformation.Call(uintptr(job), uintptr(unsafe.Pointer(&handleFlags)))
	if result == 0 {
		t.Fatal(callErr)
	}
	if handleFlags&windows.HANDLE_FLAG_INHERIT != 0 {
		t.Fatal("job handle is inheritable")
	}
}
