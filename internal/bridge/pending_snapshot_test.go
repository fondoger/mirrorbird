package bridge

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestPendingSnapshotWatermarkOwnsItsRequestSet(t *testing.T) {
	m, _, down := readyMux(t)
	events, stop := m.Subscribe(32)
	defer stop()
	mustServer(t, m, `{"id":9007199254740993,"method":"item/commandExecution/requestApproval","params":{"threadId":"a"}}`)
	nextFrame(t, down)
	mustServer(t, m, `{"id":"9007199254740993","method":"item/fileChange/requestApproval","params":{"threadId":"b"}}`)
	nextFrame(t, down)
	frames, boundary := m.PendingRequestsWithSequence()
	if boundary != 3 || len(frames) != 2 || string(frameID(t, frames[0])) != `9007199254740993` || string(frameID(t, frames[1])) != `"9007199254740993"` {
		t.Fatalf("bad pending boundary %d %q", boundary, frames)
	}
	frames[0][0] = 'X'
	owned, sameBoundary := m.PendingRequestsWithSequence()
	if sameBoundary != boundary || owned[0][0] != '{' {
		t.Fatal("pending snapshot aliases Mux state")
	}
	mustServer(t, m, `{"method":"serverRequest/resolved","params":{"requestId":9007199254740993}}`)
	nextFrame(t, down)
	remaining, nextBoundary := m.PendingRequestsWithSequence()
	if nextBoundary != 4 || len(remaining) != 1 || string(frameID(t, remaining[0])) != `"9007199254740993"` {
		t.Fatal("resolved numeric request not removed atomically")
	}
	for expected := uint64(2); expected <= nextBoundary; expected++ {
		if event := <-events; event.Sequence != expected {
			t.Fatal("pending fence disagrees with event sequence")
		}
	}
}

func TestPendingSnapshotIncludesDesktopResolutionWithoutChangingArbitration(t *testing.T) {
	m, up, down := readyMux(t)
	mustServer(t, m, `{"id":42,"method":"item/commandExecution/requestApproval","params":{"threadId":"a"}}`)
	nextFrame(t, down)
	before, boundary := m.PendingRequestsWithSequence()
	if len(before) != 1 {
		t.Fatal(before)
	}
	mustDesktop(t, m, `{"id":42,"result":{"decision":"decline"}}`)
	nextFrame(t, up)
	after, afterBoundary := m.PendingRequestsWithSequence()
	// Desktop writes are not server events. The helper must capture the actual
	// unresolved set under the lock without inventing a different event sequence.
	if len(after) != 0 || afterBoundary != boundary {
		t.Fatal(afterBoundary, after)
	}
	if err := m.Respond(json.RawMessage(`42`), map[string]string{"decision": "accept"}); err != ErrNoPendingRequest {
		t.Fatal(err)
	}
}

func TestPublicRequestIDKeyKeepsExactNumericEquality(t *testing.T) {
	for _, pair := range [][2]string{{`1`, `1.0`}, {`1e0`, `1`}, {`9007199254740993`, `9007199254740993.0`}, {`"escaped id"`, `"escaped id"`}} {
		left, err := RequestIDKey(json.RawMessage(pair[0]))
		if err != nil {
			t.Fatal(err)
		}
		right, err := RequestIDKey(json.RawMessage(pair[1]))
		if err != nil || left != right {
			t.Fatal(pair, left, right, err)
		}
	}
	seen := map[string]bool{}
	for _, id := range []string{`9007199254740992`, `9007199254740993`, `"9007199254740993"`} {
		key, err := RequestIDKey(json.RawMessage(id))
		if err != nil || seen[key] {
			t.Fatal(id, key, err)
		}
		seen[key] = true
	}
	for _, id := range []string{`null`, `true`, `1 2`, `{}`, `[]`} {
		if _, err := RequestIDKey(json.RawMessage(id)); err == nil {
			t.Fatal(fmt.Sprintf("accepted %s", id))
		}
	}
}
