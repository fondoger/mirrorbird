package diagnostics

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

type privateError struct{}

func (privateError) Error() string { panic("arbitrary error text must not be read") }

func TestLogErrorFieldsNeverSerializePrivateErrorText(t *testing.T) {
	r, err := open(t.TempDir(), 2048)
	if err != nil {
		t.Fatal(err)
	}
	r.record("attachment.closed", ErrorFields(privateError{})...)
	r.record("tunnel.command", ErrorFields(&os.PathError{Op: "open", Path: "secret-token-path", Err: syscall.Errno(5)})...)
	r.close()
	data, err := os.ReadFile(r.path)
	if err != nil || strings.Contains(string(data), "secret") || !strings.Contains(string(data), `"os_error":5`) {
		t.Fatal("unsafe or missing diagnostic error classification", err, string(data))
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil || row["time"] == nil || row["event"] == nil {
			t.Fatal("invalid log entry", line, err)
		}
	}
}

func TestLogsRotateAndKeepOnlyTwoBoundedFiles(t *testing.T) {
	r, err := open(t.TempDir(), 512)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 100 {
		r.record("tunnel.command", Int("sequence", int64(i)))
	}
	r.close()
	files, err := os.ReadDir(filepath.Dir(r.path))
	if err != nil || len(files) != 2 || r.problem.Load() != nil {
		t.Fatal(files, err, r.problem.Load())
	}
	for _, file := range files {
		info, err := file.Info()
		if err != nil || info.Size() > 512 {
			t.Fatal("log retention limit exceeded", info, err)
		}
	}
	data, err := os.ReadFile(r.path)
	if err != nil || !strings.Contains(string(data), `"sequence":99`) {
		t.Fatal("latest event was not flushed", string(data), err)
	}
}

func TestLoggingQueueNeverBlocksLifecycleCallers(t *testing.T) {
	r := &recorder{queue: make(chan entry, 1)}
	for range 1000 {
		r.record("operation.started")
	}
	if len(r.queue) != 1 || r.dropped.Load() != 999 {
		t.Fatal("unbounded or blocking queue")
	}
}

func TestLoggingFailureIsVisibleAndNotRetriedUnboundedly(t *testing.T) {
	r, err := open(t.TempDir(), 2048)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.file.Close(); err != nil {
		t.Fatal(err)
	}
	r.record("test.write_failure")
	r.close()
	if r.problem.Load() == nil {
		t.Fatal("log write failure was silent")
	}
}

func TestConcurrentLoggingAndShutdown(t *testing.T) {
	for _, duringWrite := range []bool{false, true} {
		r, err := open(t.TempDir(), maxBytes)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				for range 20 {
					r.record("test.concurrent")
				}
			})
		}
		if duringWrite {
			r.close()
		}
		wg.Wait()
		r.close()
		if len(r.queue) != 0 {
			t.Fatal("an event was accepted after the writer stopped")
		}
		if !duringWrite {
			data, err := os.ReadFile(r.path)
			if err != nil || strings.Count(string(data), "test.concurrent") != 160 {
				t.Fatal("shutdown did not flush all accepted events", err)
			}
		}
	}
}

func TestDroppedEventsAreReported(t *testing.T) {
	r, err := open(t.TempDir(), 2048)
	if err != nil {
		t.Fatal(err)
	}
	r.dropped.Store(99)
	r.close()
	data, err := os.ReadFile(r.path)
	if err != nil || !strings.Contains(string(data), `"event":"diagnostics.queue_overflow"`) ||
		!strings.Contains(string(data), `"dropped":99`) {
		t.Fatal("dropped events were not reported", string(data), err)
	}
}

func TestRotationFailureIsVisible(t *testing.T) {
	r, err := open(t.TempDir(), 128)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(r.path+".1", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.path+".1", "occupied"), []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		r.record("test.rotation_failure")
	}
	r.close()
	if r.problem.Load() == nil {
		t.Fatal("rotation failure was silent")
	}
}

func TestLoggerInitializationFailureIsVisibleAndRecoverable(t *testing.T) {
	home := filepath.Join(t.TempDir(), "occupied")
	if err := os.WriteFile(home, []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	closeLog, err := Start(home)
	closeLog()
	if err == nil || Problem() == "" || Path() != "" {
		t.Fatal("initialization failure was not surfaced")
	}
	closeLog, err = Start(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer closeLog()
	if Problem() != "" {
		t.Fatal("successful initialization did not clear the warning")
	}
}

func TestApplicationLoggerPathAndShutdown(t *testing.T) {
	home := t.TempDir()
	closeLog, err := Start(home)
	if err != nil {
		t.Fatal(err)
	}
	path := Path()
	Event("app.started", String("version", "dev"), Bool("sharing", false))
	closeLog()
	closeLog()
	if path != filepath.Join(home, "logs", "mirrorbird.log") || Path() != "" {
		t.Fatal(path, Path())
	}
	if data, err := os.ReadFile(path); err != nil || !strings.Contains(string(data), "app.started") {
		t.Fatal(string(data), err)
	}
	Event("after_close")
	_, err = os.Stat(filepath.Join(home, "logs", "mirrorbird.log.1"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unexpected rotation", err)
	}
}
