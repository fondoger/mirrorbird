//go:build windows

package platform

import (
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var processJob struct {
	once   sync.Once
	handle windows.Handle
	err    error
}

// ContainCurrentProcess puts the CURRENT process in an unnamed Windows Job
// Object with KILL_ON_JOB_CLOSE, before that process starts any children. Call
// it only from the dedicated shim, never from the GUI. Child processes inherit
// job membership, but not the job handle; breakaway is not enabled.
//
// A successful call is irreversible for the life of this process. The returned
// cleanup function deliberately does NOTHING: closing the last job handle while
// this process is still a member would immediately terminate this process too,
// possibly interrupting pending protocol writes or other deferred cleanup. The
// raw, noninheritable handle is retained without a Go finalizer. Windows closes
// it on process exit (including forced termination), killing the backend tree.
// Finish I/O and exit the shim normally; do not wait for cleanup to stop children.
//
// Repeated calls return the same result. Setup or nested-job assignment failures
// are fatal to backend startup: callers must not launch an uncontained child.
func ContainCurrentProcess() (cleanup func(), err error) {
	processJob.once.Do(func() {
		job, err := newKillOnCloseJob()
		if err != nil {
			processJob.err = err
			return
		}
		if err := windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
			// Assignment failed, so this process is not a member of this job
			// and closing its handle is safe. Never retry without containment.
			windows.CloseHandle(job)
			processJob.err = fmt.Errorf("assign shim to job (nested jobs must be supported): %w", err)
			return
		}
		processJob.handle = job
	})
	if processJob.err != nil {
		return nil, processJob.err
	}
	return func() {}, nil
}

// newKillOnCloseJob is separate from assignment so its flags can be tested
// without containing or terminating the test runner.
func newKillOnCloseJob() (windows.Handle, error) {
	// A nil SECURITY_ATTRIBUTES makes the unnamed handle noninheritable.
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, fmt.Errorf("create shim job: %w", err)
	}
	// Explicitly clear inheritance as a defense against future changes to
	// creation attributes. A child retaining a handle would defeat exit cleanup.
	if err := windows.SetHandleInformation(job, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		windows.CloseHandle(job)
		return 0, fmt.Errorf("make shim job noninheritable: %w", err)
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		windows.CloseHandle(job)
		return 0, fmt.Errorf("set shim job lifetime: %w", err)
	}
	return job, nil
}

// HideWindow suppresses a console window for a helper subprocess. It preserves
// the caller's other creation flags, token, handles, and process attributes.
// Do not apply it to a GUI launch unless that behavior is explicitly desired.
func HideWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	} else {
		// SysProcAttr may be shared with another command. Copy it so hiding
		// this helper cannot accidentally change how that command is launched.
		attrs := *cmd.SysProcAttr
		cmd.SysProcAttr = &attrs
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}
