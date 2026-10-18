package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type captureWriter struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	frames chan []byte
	hook   func()
}

func newCapture() *captureWriter { return &captureWriter{frames: make(chan []byte, 8192)} }

func (w *captureWriter) Write(p []byte) (int, error) {
	if w.hook != nil {
		w.hook()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buffer.Write(p)
	w.frames <- clone(p)
	return len(p), nil
}

func (w *captureWriter) text() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer.String()
}

func nextFrame(t *testing.T, w *captureWriter) []byte {
	t.Helper()
	select {
	case frame := <-w.frames:
		if len(frame) == 0 || frame[len(frame)-1] != '\n' {
			t.Fatalf("write missing newline: %q", frame)
		}
		return frame[:len(frame)-1]
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for protocol write")
		return nil
	}
}

func frameID(t *testing.T, frame []byte) json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(frame, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields["id"]) == 0 {
		t.Fatal("missing request ID")
	}
	return fields["id"]
}

func readyMux(t *testing.T) (*Mux, *captureWriter, *captureWriter) {
	t.Helper()
	up, down := newCapture(), newCapture()
	m := New(up, down)
	t.Cleanup(func() { m.Close(nil) })
	if err := m.FromDesktop([]byte(`{"id":0,"method":"initialize","params":{"clientInfo":{"name":"desktop"}},"trace":"keep"}`)); err != nil {
		t.Fatal(err)
	}
	id := frameID(t, nextFrame(t, up))
	if err := m.FromServer([]byte(fmt.Sprintf(`{"id":%s,"result":{"capabilities":{}},"trace":"init"}`, id))); err != nil {
		t.Fatal(err)
	}
	nextFrame(t, down)
	if !m.Ready() {
		t.Fatal("successful initialize did not make bridge ready")
	}
	return m, up, down
}

func mustDesktop(t *testing.T, m *Mux, frame string) {
	t.Helper()
	if err := m.FromDesktop([]byte(frame)); err != nil {
		t.Fatal(err)
	}
}

func mustServer(t *testing.T, m *Mux, frame string) {
	t.Helper()
	if err := m.FromServer([]byte(frame)); err != nil {
		t.Fatal(err)
	}
}

func TestDesktopRoundTripAllIDTypesOutOfOrderAndExtras(t *testing.T) {
	up, down := newCapture(), newCapture()
	m := New(up, down)
	defer m.Close(nil)
	ids := []string{`0`, `-42`, `9007199254740993`, `1.25`, `1e+30`, `"0"`, `""`, `"汉字a"`, `"mirrorbird:1"`, `-0`, `1e999999999999999999999`}
	mapped := make([]json.RawMessage, len(ids))
	seen := make(map[string]bool)
	for i, id := range ids {
		frame := ` {"trace":{"id":"nested","escaped":"a"}, "id" : ` + id + `, "method":"unknown/request", "params":{"foo":1}, "extension": [true, null] } `
		mustDesktop(t, m, frame)
		wire := nextFrame(t, up)
		mapped[i] = frameID(t, wire)
		if seen[string(mapped[i])] {
			t.Fatal("generated IDs collided")
		}
		seen[string(mapped[i])] = true
		want := strings.Replace(frame, `"id" : `+id+`,`, `"id" : `+string(mapped[i])+`,`, 1)
		if string(wire) != want {
			t.Fatalf("request fields/bytes changed:\n got %s\nwant %s", wire, want)
		}
	}
	for i := len(ids) - 1; i >= 0; i-- {
		frame := ` { "trace": {"raw":"a"}, "id" : ` + string(mapped[i]) + ` , "result" : {"number":9007199254740993}, "future":true } `
		mustServer(t, m, frame)
		want := strings.Replace(frame, string(mapped[i]), ids[i], 1)
		if got := string(nextFrame(t, down)); got != want {
			t.Fatalf("response fields/bytes changed:\n got %s\nwant %s", got, want)
		}
	}
	if status := m.Status(); status.PendingDesktop != 0 {
		t.Fatalf("leaked requests: %+v", status)
	}
}

func TestLocalAndDesktopNamespacesAndLocalErrors(t *testing.T) {
	m, up, down := readyMux(t)
	mustDesktop(t, m, `{"id":"mirrorbird:3","method":"desktop/call"}`)
	desktopID := frameID(t, nextFrame(t, up))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	type outcome struct {
		data json.RawMessage
		err  error
	}
	completed := make(chan outcome, 1)
	go func() {
		data, err := m.Call(ctx, "thread/read", map[string]any{"threadId": "t"})
		completed <- outcome{data, err}
	}()
	localID := frameID(t, nextFrame(t, up))
	if bytes.Equal(localID, desktopID) {
		t.Fatal("local and Desktop IDs collided")
	}
	before := down.text()
	mustServer(t, m, fmt.Sprintf(`{"id":%s,"error":{"code":-32042,"message":"private upstream error","data":{"retry":false},"extension":7},"trace":1}`, localID))
	result := <-completed
	var rpcErr *RPCError
	if !errors.As(result.err, &rpcErr) || rpcErr.Code != -32042 || rpcErr.Message != "private upstream error" || string(rpcErr.Data) != `{"retry":false}` || !strings.Contains(string(rpcErr.Raw), `"extension":7`) {
		t.Fatalf("lost RPC error details: %#v", result.err)
	}
	if strings.Contains(result.err.Error(), "private") {
		t.Fatal("RPC error text leaked private content into diagnostics")
	}
	if down.text() != before {
		t.Fatal("local response reached Desktop")
	}
	mustServer(t, m, fmt.Sprintf(`{"id":%s,"result":true}`, desktopID))
	if got := string(nextFrame(t, down)); got != `{"id":"mirrorbird:3","result":true}` {
		t.Fatalf("Desktop response = %s", got)
	}
}

func TestInitializeOnlyOnceAndOnlyAfterSuccess(t *testing.T) {
	up, down := newCapture(), newCapture()
	m := New(up, down)
	defer m.Close(nil)
	if _, err := m.Call(context.Background(), "thread/read", nil); !errors.Is(err, ErrNotReady) {
		t.Fatalf("uninitialized call error = %v", err)
	}
	for _, method := range []string{"initialize", "initialized"} {
		if _, err := m.Call(context.Background(), method, nil); !errors.Is(err, ErrInitializationReserved) {
			t.Fatalf("companion %s error = %v", method, err)
		}
	}
	mustDesktop(t, m, `{"method":"initialized","extension":"optional"}`)
	if got := string(nextFrame(t, up)); got != `{"method":"initialized","extension":"optional"}` || m.Ready() {
		t.Fatal("initialized notification was changed or unlocked calls")
	}
	mustDesktop(t, m, `{"id":"init","method":"initialize","params":{"capabilities":{"experimentalApi":true}}}`)
	id := frameID(t, nextFrame(t, up))
	if m.Ready() {
		t.Fatal("ready before initialization response")
	}
	if err := m.FromDesktop([]byte(`{"id":2,"method":"initialize"}`)); !errors.Is(err, ErrAlreadyInitialized) {
		t.Fatalf("duplicate initialize error = %v", err)
	}
	mustServer(t, m, fmt.Sprintf(`{"id":%s,"error":{"code":-1,"message":"failed"}}`, id))
	nextFrame(t, down)
	if m.Ready() {
		t.Fatal("failed initialization made bridge ready")
	}
	mustServer(t, m, fmt.Sprintf(`{"id":%s,"result":{}}`, id))
	if m.Ready() {
		t.Fatal("duplicate result after failed initialization unlocked calls")
	}
}

func TestNotificationsAndServerEventsPreserveBytes(t *testing.T) {
	m, up, down := readyMux(t)
	events, cancel := m.Subscribe(8)
	defer cancel()
	frame := ` { "method" : "future/event", "params" : { "id" : 1, "text":"a" }, "trace" : {"value":9007199254740993} } `
	mustServer(t, m, frame)
	if got := string(nextFrame(t, down)); got != frame {
		t.Fatalf("unknown notification changed: %s", got)
	}
	event := <-events
	if event.Sequence != 2 || string(event.Message) != frame {
		t.Fatalf("unexpected event: %+v", event)
	}
	mustDesktop(t, m, frame)
	if got := string(nextFrame(t, up)); got != frame {
		t.Fatal("Desktop notification changed")
	}
	before := down.text()
	mustServer(t, m, `{"id":"unmapped","result":null,"trace":"late"}`)
	late := <-events
	if late.Sequence != 3 || !bytes.Contains(late.Message, []byte(`"trace":"late"`)) {
		t.Fatal("unmapped server response was not published")
	}
	if down.text() != before {
		t.Fatal("unmapped response reached Desktop")
	}
}

func TestOppositeDirectionSameIDAndDuplicateApprovals(t *testing.T) {
	m, up, down := readyMux(t)
	mustDesktop(t, m, `{"id":7,"method":"desktop/request"}`)
	id := frameID(t, nextFrame(t, up))
	serverFrame := fmt.Sprintf(`{"id":%s,"method":"item/commandExecution/requestApproval","params":{"threadId":"t"},"extra":"untouched"}`, id)
	mustServer(t, m, serverFrame)
	if got := string(nextFrame(t, down)); got != serverFrame {
		t.Fatal("server request ID or extras changed")
	}
	pending := m.PendingRequests()
	if len(pending) != 1 || string(pending[0]) != serverFrame {
		t.Fatal("server request not pending")
	}
	pending[0][0] = '!'
	if string(m.PendingRequests()[0]) != serverFrame {
		t.Fatal("PendingRequests exposed internal storage")
	}
	mustServer(t, m, fmt.Sprintf(`{"id":%s,"result":"desktop result"}`, id))
	if got := string(nextFrame(t, down)); got != `{"id":7,"result":"desktop result"}` {
		t.Fatal("server response confused opposite ID direction")
	}
	if err := m.Respond(id, map[string]string{"decision": "decline"}); err != nil {
		t.Fatal(err)
	}
	answer := nextFrame(t, up)
	if !bytes.Equal(frameID(t, answer), id) {
		t.Fatal("approval ID changed")
	}
	before := up.text()
	mustDesktop(t, m, fmt.Sprintf(`{"id":%s,"result":{"decision":"accept"}}`, id))
	if err := m.Respond(id, true); !errors.Is(err, ErrNoPendingRequest) {
		t.Fatalf("duplicate approval error = %v", err)
	}
	if up.text() != before || len(m.PendingRequests()) != 0 {
		t.Fatal("approval delivered twice")
	}
}

func TestDesktopApprovalWinsAndResolvedNotificationRetires(t *testing.T) {
	m, up, down := readyMux(t)
	mustServer(t, m, `{"id":12,"method":"approval","params":{}}`)
	nextFrame(t, down)
	answer := ` {"id":12,"error":{"code":-1,"message":"declined"},"extra":[1]} `
	mustDesktop(t, m, answer)
	if got := string(nextFrame(t, up)); got != answer {
		t.Fatal("Desktop server response fields changed")
	}
	if err := m.Respond(json.RawMessage(`12`), true); !errors.Is(err, ErrNoPendingRequest) {
		t.Fatalf("second responder error = %v", err)
	}
	mustServer(t, m, `{"id":"12","method":"approval"}`)
	nextFrame(t, down)
	resolved := `{"method":"serverRequest/resolved","params":{"threadId":"t","requestId":"12"},"trace":9}`
	mustServer(t, m, resolved)
	if got := string(nextFrame(t, down)); got != resolved {
		t.Fatal("resolved notification not forwarded unchanged")
	}
	if err := m.Respond(json.RawMessage(`"12"`), true); !errors.Is(err, ErrNoPendingRequest) {
		t.Fatalf("resolved request answer error = %v", err)
	}
}

func TestCanceledCallConsumesLateResponse(t *testing.T) {
	m, up, down := readyMux(t)
	ctx, cancel := context.WithCancel(context.Background())
	completed := make(chan error, 1)
	go func() { _, err := m.Call(ctx, "thread/read", nil); completed <- err }()
	id := frameID(t, nextFrame(t, up))
	cancel()
	if err := <-completed; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled call error = %v", err)
	}
	if status := m.Status(); status.PendingLocal != 0 {
		t.Fatalf("cancellation leaked request: %+v", status)
	}
	before := down.text()
	for range 2 {
		mustServer(t, m, fmt.Sprintf(`{"id":%s,"result":{"secret":"local only"}}`, id))
	}
	if down.text() != before {
		t.Fatal("late/duplicate local response leaked to Desktop")
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	before = up.text()
	if _, err := m.Call(ctx2, "thread/read", nil); !errors.Is(err, context.Canceled) || up.text() != before {
		t.Fatal("already canceled call wrote to upstream")
	}
}

func TestSlowSubscriberClosesWithoutBlockingDesktop(t *testing.T) {
	m, _, down := readyMux(t)
	slow, cancelSlow := m.Subscribe(1)
	defer cancelSlow()
	fast, cancelFast := m.Subscribe(8)
	defer cancelFast()
	for i := range 3 {
		mustServer(t, m, fmt.Sprintf(`{"method":"event","params":{"i":%d}}`, i))
		nextFrame(t, down)
	}
	if event, ok := <-slow; !ok || event.Sequence != 2 {
		t.Fatal("slow subscriber lost its buffered event")
	}
	if _, ok := <-slow; ok {
		t.Fatal("slow subscriber was not disconnected")
	}
	for i := range 3 {
		if event := <-fast; event.Sequence != uint64(i+2) {
			t.Fatalf("event order = %d", event.Sequence)
		}
	}
	if m.Status().Subscribers != 1 {
		t.Fatal("slow subscriber retained")
	}
}

func TestSubscriberCopiesAndCancellation(t *testing.T) {
	m, _, down := readyMux(t)
	a, cancelA := m.Subscribe(2)
	b, cancelB := m.Subscribe(2)
	frame := []byte(`{"id":5,"method":"approval"}`)
	if err := m.FromServer(frame); err != nil {
		t.Fatal(err)
	}
	nextFrame(t, down)
	frame[0] = '!'
	aEvent, bEvent := <-a, <-b
	aEvent.Message[0] = '?'
	if bEvent.Message[0] != '{' || m.PendingRequests()[0][0] != '{' {
		t.Fatal("event slices alias another subscriber or pending request")
	}
	cancelA()
	cancelA()
	cancelB()
	if _, ok := <-a; ok {
		t.Fatal("cancel did not close subscription")
	}
	if m.Status().Subscribers != 0 {
		t.Fatal("cancel leaked subscription")
	}
}

func TestCloseWakesPendingAndSubscribers(t *testing.T) {
	m, up, _ := readyMux(t)
	events, cancel := m.Subscribe(1)
	defer cancel()
	completed := make(chan error, 1)
	go func() { _, err := m.Call(context.Background(), "wait", nil); completed <- err }()
	nextFrame(t, up)
	want := errors.New("upstream EOF")
	m.Close(want)
	m.Close(errors.New("second close"))
	select {
	case err := <-completed:
		if !errors.Is(err, want) {
			t.Fatalf("close error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not wake local caller")
	}
	if _, ok := <-events; ok {
		t.Fatal("Close did not close subscriber")
	}
	if status := m.Status(); !status.Closed || status.Ready || status.PendingLocal != 0 || status.Subscribers != 0 {
		t.Fatalf("closed status = %+v", status)
	}
	if err := m.FromDesktop([]byte(`{"method":"notification"}`)); !errors.Is(err, want) {
		t.Fatalf("closed Desktop error = %v", err)
	}
	if _, err := m.Call(context.Background(), "method", nil); !errors.Is(err, want) {
		t.Fatalf("closed Call error = %v", err)
	}
}

func TestWritersRunOutsideStateLock(t *testing.T) {
	m, up, down := readyMux(t)
	up.hook = func() { _ = m.Status(); _ = m.PendingRequests() }
	down.hook = func() { _ = m.Ready(); _ = m.Status() }
	completed := make(chan struct{})
	go func() {
		_ = m.FromDesktop([]byte(`{"method":"notify"}`))
		_ = m.FromServer([]byte(`{"method":"notify"}`))
		close(completed)
	}()
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("writer callback deadlocked on state mutex")
	}
}

type blockingWriter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return len(p), nil
}

func TestCallCancellationAndCloseWithBlockedWrite(t *testing.T) {
	m, _, down := readyMux(t)
	blocked := &blockingWriter{started: make(chan struct{}), release: make(chan struct{})}
	m.upstream = blocked
	ctx, cancel := context.WithCancel(context.Background())
	completed := make(chan error, 1)
	go func() { _, err := m.Call(ctx, "wait", nil); completed <- err }()
	select {
	case <-blocked.started:
	case <-time.After(time.Second):
		t.Fatal("local write never started")
	}
	cancel()
	select {
	case err := <-completed:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked writer prevented context cancellation")
	}
	// The independently locked server -> Desktop direction still works.
	mustServer(t, m, `{"method":"still/live"}`)
	nextFrame(t, down)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel2()
	if _, err := m.Call(ctx2, "queued", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued caller did not honor context: %v", err)
	}
	m.Close(nil)
	close(blocked.release)
}

type shortWriter struct{ bytes.Buffer }

func (w *shortWriter) Write(p []byte) (int, error) { return w.Buffer.Write(p[:min(2, len(p))]) }

type errorWriter struct{ err error }

func (w errorWriter) Write([]byte) (int, error) { return 0, w.err }

func TestShortWritesAndWriteFailure(t *testing.T) {
	w := &shortWriter{}
	m := New(w, io.Discard)
	mustDesktop(t, m, `{"method":"event"}`)
	if w.String() != "{\"method\":\"event\"}\n" {
		t.Fatalf("short writer lost bytes: %q", w.String())
	}
	m.Close(nil)
	want := errors.New("pipe broken")
	bad := New(errorWriter{want}, io.Discard)
	if err := bad.FromDesktop([]byte(`{"id":1,"method":"read"}`)); !errors.Is(err, want) || !bad.Status().Closed {
		t.Fatalf("write failure = %v, status = %+v", err, bad.Status())
	}
}

func TestConcurrentApprovalsDeliverOnlyOnce(t *testing.T) {
	m, up, down := readyMux(t)
	mustServer(t, m, `{"id":"approval","method":"approval"}`)
	nextFrame(t, down)
	before := up.text()
	var wg sync.WaitGroup
	var successes atomic.Int32
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.Respond(json.RawMessage(`"approval"`), true); err == nil {
				successes.Add(1)
			} else if !errors.Is(err, ErrNoPendingRequest) {
				t.Errorf("unexpected response error: %v", err)
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 || strings.Count(strings.TrimPrefix(up.text(), before), "\n") != 1 {
		t.Fatal("concurrent approval delivered more than once")
	}
}

func TestNumericIDsMatchWithoutPrecisionLoss(t *testing.T) {
	pairs := [][2]string{
		{`0`, `-0.0e100`}, {`1`, `1.000`}, {`1e+5`, `100000`},
		{`9007199254740993`, `9007199254740993.0`},
		{`1e99999999999999999999`, `10e99999999999999999998`},
		{`"a"`, `"a"`},
	}
	for _, pair := range pairs {
		a, errA := requestIDKey(json.RawMessage(pair[0]))
		b, errB := requestIDKey(json.RawMessage(pair[1]))
		if errA != nil || errB != nil || a != b {
			t.Fatalf("equivalent IDs %v: %q %q, errors %v %v", pair, a, b, errA, errB)
		}
	}
	a, _ := requestIDKey(json.RawMessage(`9007199254740993`))
	b, _ := requestIDKey(json.RawMessage(`9007199254740992`))
	c, _ := requestIDKey(json.RawMessage(`"9007199254740993"`))
	if a == b || a == c || b == c {
		t.Fatal("distinct string/numeric IDs conflated")
	}
}

func TestMalformedMessagesAreRejectedWithoutPrivateText(t *testing.T) {
	m := New(io.Discard, io.Discard)
	defer m.Close(nil)
	for _, frame := range []string{
		`{"private":"secret_value"`, `[]`, `null`, `{"id":null,"result":true}`,
		`{"id":{},"result":true}`, `{"id":true,"result":true}`,
		`{"id":1,"id":2,"method":"request"}`, `{"id":1,"id":2,"method":"request"}`,
		`{"method":null}`, `{"id":1}`, `{"id":1,"result":true,"error":{}}`,
		`{"id":1,"method":"request","result":true}`, "{\n\"method\":\"event\"}",
	} {
		for _, handle := range []func([]byte) error{m.FromDesktop, m.FromServer} {
			if err := handle([]byte(frame)); !errors.Is(err, ErrInvalidMessage) || strings.Contains(err.Error(), "secret_value") {
				t.Fatalf("malformed message error = %v for %s", err, frame)
			}
		}
	}
}

func TestPendingOrderAndDuplicateServerRequest(t *testing.T) {
	m, _, down := readyMux(t)
	frames := []string{`{"id":"b","method":"approval"}`, `{"id":3,"method":"approval"}`, `{"id":"a","method":"approval"}`}
	for _, frame := range frames {
		mustServer(t, m, frame)
		nextFrame(t, down)
	}
	before := down.text()
	mustServer(t, m, frames[0])
	if down.text() != before {
		t.Fatal("duplicate pending request was re-delivered")
	}
	var got []string
	for _, frame := range m.PendingRequests() {
		got = append(got, string(frame))
	}
	if !reflect.DeepEqual(got, frames) {
		t.Fatalf("pending order = %v", got)
	}
}

func TestPendingAndSubscriberCountBounds(t *testing.T) {
	m := New(io.Discard, io.Discard)
	defer m.Close(nil)
	for range maxClientPending {
		mustDesktop(t, m, `{"id":1,"method":"wait"}`)
	}
	if err := m.FromDesktop([]byte(`{"id":2,"method":"wait"}`)); !errors.Is(err, ErrCapacity) {
		t.Fatalf("client pending limit = %v", err)
	}
	for i := range maxServerPending {
		mustServer(t, m, fmt.Sprintf(`{"id":%d,"method":"approval"}`, i))
	}
	if err := m.FromServer([]byte(`{"id":"overflow","method":"approval"}`)); !errors.Is(err, ErrCapacity) {
		t.Fatalf("server pending limit = %v", err)
	}
	for range maxSubscribers {
		_, cancel := m.Subscribe(1)
		defer cancel()
	}
	extra, cancel := m.Subscribe(1)
	defer cancel()
	if _, ok := <-extra; ok {
		t.Fatal("subscriber capacity did not close extra channel")
	}
	clamped, cancelClamp := New(io.Discard, io.Discard).Subscribe(maxSubscriberBuffer + 999)
	defer cancelClamp()
	if cap(clamped) != maxSubscriberBuffer {
		t.Fatal("subscriber buffer was not bounded")
	}
}
