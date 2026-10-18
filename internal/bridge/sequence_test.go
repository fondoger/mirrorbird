package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

type sequenceResult struct {
	result   json.RawMessage
	sequence uint64
	err      error
}

func TestCallWithSequenceReturnsResponseBoundaryNotLatestStatus(t *testing.T) {
	m, up, down := readyMux(t)
	events, stop := m.Subscribe(64)
	defer stop()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	completed := make(chan sequenceResult, 1)
	go func() {
		result, sequence, err := m.CallWithSequence(ctx, "thread/resume", map[string]string{"threadId": "thread-a"})
		completed <- sequenceResult{result, sequence, err}
	}()
	id := frameID(t, nextFrame(t, up))
	mustServer(t, m, `{"method":"item/delta","params":{"delta":"before"}}`)
	nextFrame(t, down)
	mustServer(t, m, fmt.Sprintf(`{"id":%s,"result":{"thread":{"id":"thread-a","text":"before"}}}`, id))
	// Delay consuming the Call result while unrelated/newer events advance the
	// global Status sequence. Its response boundary must stay exactly three.
	for i := range 20 {
		mustServer(t, m, fmt.Sprintf(`{"method":"item/delta","params":{"delta":%d}}`, i))
		nextFrame(t, down)
	}
	out := <-completed
	if out.err != nil || out.sequence != 3 || string(out.result) != `{"thread":{"id":"thread-a","text":"before"}}` {
		t.Fatalf("response boundary = %+v", out)
	}
	if m.Status().Sequence != 23 {
		t.Fatal("test did not advance global event sequence")
	}
	if event := <-events; event.Sequence != 2 {
		t.Fatal("missing event preceding snapshot response")
	}
	if event := <-events; event.Sequence != out.sequence || string(frameID(t, event.Message)) != string(id) {
		t.Fatal("Call sequence differs from published response Event sequence")
	}
	for i := range 20 {
		if event := <-events; event.Sequence != out.sequence+uint64(i)+1 {
			t.Fatal("post-snapshot event ordering incorrect")
		}
	}
}

func TestCallWithSequenceRPCErrorKeepsSequence(t *testing.T) {
	m, up, _ := readyMux(t)
	completed := make(chan sequenceResult, 1)
	go func() {
		result, sequence, err := m.CallWithSequence(context.Background(), "failing/method", nil)
		completed <- sequenceResult{result, sequence, err}
	}()
	id := frameID(t, nextFrame(t, up))
	mustServer(t, m, fmt.Sprintf(`{"id":%s,"error":{"code":-1,"message":"rejected"}}`, id))
	out := <-completed
	var rpcErr *RPCError
	if out.sequence != 2 || !errors.As(out.err, &rpcErr) || rpcErr.Code != -1 || out.result != nil {
		t.Fatalf("error response sequence/result = %+v", out)
	}
}

func TestCallWithSequenceCancellationAndCloseReturnZero(t *testing.T) {
	for _, closeMux := range []bool{false, true} {
		m, up, _ := readyMux(t)
		ctx, cancel := context.WithCancel(context.Background())
		completed := make(chan sequenceResult, 1)
		go func() {
			result, sequence, err := m.CallWithSequence(ctx, "waiting/method", nil)
			completed <- sequenceResult{result, sequence, err}
		}()
		nextFrame(t, up)
		want := context.Canceled
		if closeMux {
			m.Close(nil)
			want = ErrClosed
		} else {
			cancel()
		}
		out := <-completed
		cancel()
		if !errors.Is(out.err, want) || out.sequence != 0 || out.result != nil {
			t.Fatalf("local termination incorrectly has server sequence: %+v", out)
		}
	}
}

func TestCallWithSequenceRetainsOutOfOrderMapping(t *testing.T) {
	m, up, _ := readyMux(t)
	first := make(chan sequenceResult, 1)
	second := make(chan sequenceResult, 1)
	start := func(method string, out chan<- sequenceResult) {
		go func() {
			result, sequence, err := m.CallWithSequence(context.Background(), method, nil)
			out <- sequenceResult{result, sequence, err}
		}()
	}
	start("first", first)
	firstID := frameID(t, nextFrame(t, up))
	start("second", second)
	secondID := frameID(t, nextFrame(t, up))
	mustServer(t, m, fmt.Sprintf(`{"id":%s,"result":2}`, secondID))
	mustServer(t, m, fmt.Sprintf(`{"id":%s,"result":1}`, firstID))
	one, two := <-first, <-second
	if one.sequence != 3 || two.sequence != 2 || string(one.result) != "1" || string(two.result) != "2" || one.err != nil || two.err != nil {
		t.Fatalf("out-of-order response boundaries: %+v %+v", one, two)
	}
}
