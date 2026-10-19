// Package diagnostics records bounded local lifecycle events. Callers supply
// fixed event names and non-secret facts, never RPC bodies, CLI output, URLs,
// account names, credentials, message text, or arbitrary error strings.
package diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const maxBytes int64 = 1 << 20

type Field struct {
	name  string
	value any
}

func String(name, value string) Field    { return Field{name, value} }
func Int(name string, value int64) Field { return Field{name, value} }
func Bool(name string, value bool) Field { return Field{name, value} }

// ErrorFields deliberately never calls err.Error(), which may contain a
// device code, token, protocol payload, or a private command/path.
func ErrorFields(err error) []Field {
	if err == nil {
		return []Field{String("result", "ok")}
	}
	kind := "other_error"
	switch {
	case errors.Is(err, context.Canceled):
		kind = "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		kind = "timeout"
	case errors.Is(err, io.EOF):
		kind = "eof"
	case errors.Is(err, io.ErrUnexpectedEOF):
		kind = "unexpected_eof"
	case errors.Is(err, net.ErrClosed):
		kind = "connection_closed"
	case errors.Is(err, os.ErrPermission):
		kind = "permission_denied"
	}
	fields := []Field{String("result", kind)}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		fields = append(fields, Int("exit_code", int64(exit.ExitCode())))
	}
	var number syscall.Errno
	if errors.As(err, &number) {
		fields = append(fields, Int("os_error", int64(number)))
	}
	return fields
}

type entry struct {
	at     time.Time
	event  string
	fields []Field
}

type recorder struct {
	path    string
	file    *os.File
	size    int64
	limit   int64
	queue   chan entry
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
	mu      sync.RWMutex
	closed  bool
	dropped atomic.Int64
	problem atomic.Pointer[string]
}

var active atomic.Pointer[recorder]
var startupProblem atomic.Pointer[string]

// Start belongs to the primary application's lifetime, not a UI window.
// The queue is nonblocking, including while a caller holds a controller lock.
func Start(home string) (func(), error) {
	r, err := open(home, maxBytes)
	if err != nil {
		message := "无法创建本地诊断日志，请检查设置目录权限和磁盘空间。"
		startupProblem.Store(&message)
		return func() {}, err
	}
	if !active.CompareAndSwap(nil, r) {
		r.close()
		return func() {}, errors.New("diagnostics already started")
	}
	startupProblem.Store(nil)
	return func() {
		if active.CompareAndSwap(r, nil) {
			r.close()
		}
	}, nil
}

func Path() string {
	if r := active.Load(); r != nil {
		return r.path
	}
	return ""
}

func Problem() string {
	if r := active.Load(); r != nil {
		if problem := r.problem.Load(); problem != nil {
			return *problem
		}
	}
	if problem := startupProblem.Load(); problem != nil {
		return *problem
	}
	return ""
}

func Event(name string, fields ...Field) {
	if r := active.Load(); r != nil {
		r.record(name, fields...)
	}
}

func open(home string, limit int64) (*recorder, error) {
	dir := filepath.Join(home, "logs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "mirrorbird.log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		if err == nil {
			err = errors.New("diagnostic log is not a regular file")
		}
		return nil, err
	}
	r := &recorder{
		path: path, file: file, size: info.Size(), limit: limit,
		queue: make(chan entry, 256), stop: make(chan struct{}), done: make(chan struct{}),
	}
	go r.run()
	return r, nil
}

func (r *recorder) record(name string, fields ...Field) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed {
		return
	}
	event := entry{time.Now(), name, append([]Field(nil), fields...)}
	select {
	case r.queue <- event:
	default:
		r.dropped.Add(1)
	}
}

func (r *recorder) close() {
	r.once.Do(func() {
		r.mu.Lock()
		r.closed = true
		close(r.stop)
		r.mu.Unlock()
		<-r.done
	})
}

func (r *recorder) run() {
	defer close(r.done)
	defer func() {
		if r.file != nil {
			if err := r.file.Close(); err != nil {
				r.failed(err)
			}
		}
	}()
	for {
		select {
		case item := <-r.queue:
			r.write(item)
		case <-r.stop:
			for {
				select {
				case item := <-r.queue:
					r.write(item)
				default:
					r.writeDropped()
					return
				}
			}
		}
		r.writeDropped()
	}
}

func (r *recorder) writeDropped() {
	if count := r.dropped.Swap(0); count != 0 {
		r.write(entry{time.Now(), "diagnostics.queue_overflow", []Field{Int("dropped", count)}})
	}
}

func (r *recorder) write(item entry) {
	if r.problem.Load() != nil {
		return
	}
	data := map[string]any{"time": item.at.Format(time.RFC3339Nano), "event": item.event}
	for _, field := range item.fields {
		data[field.name] = field.value
	}
	line, err := json.Marshal(data)
	if err != nil {
		r.failed(err)
		return
	}
	line = append(line, '\n')
	if int64(len(line)) > r.limit {
		r.failed(errors.New("diagnostic entry exceeds file limit"))
		return
	}
	if r.size+int64(len(line)) > r.limit {
		if err := r.rotate(); err != nil {
			r.failed(err)
			return
		}
	}
	n, err := r.file.Write(line)
	r.size += int64(n)
	if err != nil {
		r.failed(err)
	}
}

func (r *recorder) rotate() error {
	if err := r.file.Close(); err != nil {
		return err
	}
	r.file = nil
	backup := r.path + ".1"
	if err := os.Remove(backup); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(r.path, backup); err != nil {
		return err
	}
	file, err := os.OpenFile(r.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	r.file, r.size = file, 0
	return nil
}

func (r *recorder) failed(err error) {
	message := "诊断日志写入失败，请检查磁盘空间、目录权限或日志文件占用。"
	var number syscall.Errno
	if errors.As(err, &number) {
		message += " 系统错误码：" + strconv.FormatUint(uint64(number), 10)
	}
	r.problem.Store(&message)
}
