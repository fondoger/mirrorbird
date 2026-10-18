package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWatchSuppressesUnsubscribeWithOriginalID(t *testing.T) {
	for _, id := range []string{`9007199254740993`, `1.000`, `-0`, `"desktop"`, `""`} {
		t.Run(id, func(t *testing.T) {
			m, up, down := readyMux(t)
			beforeUp := up.text()
			release := m.WatchThread("thread-a")
			if release == nil {
				t.Fatal("watch denied")
			}
			defer release()
			events, cancel := m.Subscribe(1)
			defer cancel()
			mustDesktop(t, m, fmt.Sprintf(` {"id": %s, "method":"thread/unsubscribe", "params":{"threadId":"thread-a"}, "extra":true} `, id))
			want := `{"id":` + id + `,"result":{"status":"unsubscribed"}}`
			if got := string(nextFrame(t, down)); got != want {
				t.Fatalf("synthetic response = %s; want %s", got, want)
			}
			if up.text() != beforeUp || m.Status().PendingDesktop != 0 {
				t.Fatal("suppressed unsubscribe reached upstream or retained request")
			}
			if m.Status().Sequence != 1 {
				t.Fatal("synthetic response incremented server event sequence")
			}
			select {
			case event := <-events:
				t.Fatalf("synthetic response published as upstream event: %+v", event)
			default:
			}
			if m.Status().WatchedThreads != 1 {
				t.Fatal("watch not counted")
			}
		})
	}
}

func TestWatchReleaseReclaimsBookkeepingWithoutDetaching(t *testing.T) {
	m, up, down := readyMux(t)
	first, second := m.WatchThread("thread-a"), m.WatchThread("thread-a")
	if first == nil || second == nil {
		t.Fatal("multiple watchers denied")
	}
	before := up.text()
	first()
	first()
	if m.Status().WatchedThreads != 1 || up.text() != before {
		t.Fatal("partial release removed another watcher or issued RPC")
	}
	mustDesktop(t, m, `{"id":10,"method":"thread/unsubscribe","params":{"threadId":"thread-a"}}`)
	nextFrame(t, down)
	if up.text() != before {
		t.Fatal("remaining watcher did not protect unsubscribe")
	}
	second()
	if status := m.Status(); status.WatchedThreads != 0 || status.RetainedThreads != 0 || up.text() != before {
		t.Fatalf("last release retained bookkeeping or detached upstream: %+v", status)
	}
	mustDesktop(t, m, `{"id":11,"method":"thread/unsubscribe","params":{"threadId":"thread-a"}}`)
	id := frameID(t, nextFrame(t, up))
	if m.WatchThread("thread-a") != nil {
		t.Fatal("watch accepted while upstream unsubscribe was unresolved")
	}
	mustServer(t, m, fmt.Sprintf(`{"id":%s,"result":{"status":"unsubscribed"}}`, id))
	if got := string(nextFrame(t, down)); got != `{"id":11,"result":{"status":"unsubscribed"}}` {
		t.Fatal("explicit unsubscribe response not routed normally")
	}
	if m.Status().RetainedThreads != 0 {
		t.Fatal("successful explicit unsubscribe did not reclaim slot")
	}
	if release := m.WatchThread("thread-a"); release == nil {
		t.Fatal("watch still denied after completed unsubscribe")
	} else {
		release()
	}
}

func TestWatchOnlySuppressesTargetThread(t *testing.T) {
	m, up, down := readyMux(t)
	release := m.WatchThread("watched")
	defer release()
	mustDesktop(t, m, `{"id":"other","method":"thread/unsubscribe","params":{"threadId":"other"},"trace":"keep"}`)
	frame := nextFrame(t, up)
	id := frameID(t, frame)
	if string(frame) != `{"id":`+string(id)+`,"method":"thread/unsubscribe","params":{"threadId":"other"},"trace":"keep"}` {
		t.Fatal("unwatched unsubscribe bytes changed beyond ID")
	}
	mustServer(t, m, fmt.Sprintf(`{"id":%s,"result":{"status":"unsubscribed"}}`, id))
	nextFrame(t, down)
	before := up.text()
	mustDesktop(t, m, `{"method":"thread/unsubscribe","params":{"threadId":"watched"}}`)
	if up.text() != before || len(down.frames) != 0 {
		t.Fatal("notification bypassed watch or manufactured a response")
	}
	for _, params := range []string{`null`, `{}`, `{"threadId":5}`, `{"threadId":""}`} {
		mustDesktop(t, m, `{"id":10,"method":"thread/unsubscribe","params":`+params+`}`)
		nextFrame(t, up) // Invalid params remain the real server's responsibility.
	}
}

func TestWatchCapacityCountsOnlyPositiveThreadIDs(t *testing.T) {
	m, up, _ := readyMux(t)
	before := up.text()
	for i := range 4 * MaxWatchedThreads {
		release := m.WatchThread(fmt.Sprintf("visited-%d", i))
		if release == nil {
			t.Fatalf("sequential watch %d denied", i)
		}
		release()
	}
	if status := m.Status(); status.RetainedThreads != 0 || status.WatchedThreads != 0 || up.text() != before {
		t.Fatalf("sequential opens retained slots or wrote upstream: %+v", status)
	}
	releases := make([]func(), MaxWatchedThreads)
	for i := range releases {
		releases[i] = m.WatchThread(fmt.Sprintf("active-%d", i))
		if releases[i] == nil {
			t.Fatalf("active watch %d denied", i)
		}
	}
	if release := m.WatchThread("overflow"); release != nil {
		release()
		t.Fatal("active capacity exceeded")
	}
	// Extra consumers of an active ID do not consume another thread slot.
	extra := m.WatchThread("active-0")
	if extra == nil {
		t.Fatal("same active thread denied")
	}
	extra()
	releases[0]()
	if release := m.WatchThread("replacement"); release == nil {
		t.Fatal("zero-watch slot was not reclaimed")
	} else {
		release()
	}
	for _, release := range releases {
		release()
	}
	for _, invalid := range []string{"", strings.Repeat("a", maxThreadIDBytes+1), string([]byte{0xff})} {
		if release := m.WatchThread(invalid); release != nil {
			release()
			t.Fatal("invalid thread ID accepted")
		}
	}
	m.Close(nil)
	if release := m.WatchThread("thread-1"); release != nil {
		t.Fatal("watch accepted after close")
	}
	if status := m.Status(); status.WatchedThreads != 0 || status.RetainedThreads != 0 {
		t.Fatal("close retained thread slots")
	}
}

func TestWatchVersusDesktopUnsubscribeAtomic(t *testing.T) {
	m, up, down := readyMux(t)
	for i := range 100 {
		threadID := fmt.Sprintf("raced-thread-%d", i)
		start := make(chan struct{})
		watch := make(chan func(), 1)
		desktop := make(chan error, 1)
		go func() {
			<-start
			watch <- m.WatchThread(threadID)
		}()
		go func() {
			<-start
			desktop <- m.FromDesktop([]byte(fmt.Sprintf(`{"id":1,"method":"thread/unsubscribe","params":{"threadId":%q}}`, threadID)))
		}()
		close(start)
		release := <-watch
		if err := <-desktop; err != nil {
			t.Fatal(err)
		}
		if release != nil {
			if len(up.frames) != 0 {
				t.Fatal("watch won but unsubscribe was still sent upstream")
			}
			nextFrame(t, down)
			release()
			// Reclaim the intentionally retained slot for the next iteration.
			mustDesktop(t, m, fmt.Sprintf(`{"id":2,"method":"thread/unsubscribe","params":{"threadId":%q}}`, threadID))
		} else if len(down.frames) != 0 {
			t.Fatal("denied watch still synthesized success")
		}
		id := frameID(t, nextFrame(t, up))
		mustServer(t, m, fmt.Sprintf(`{"id":%s,"result":{"status":"unsubscribed"}}`, id))
		nextFrame(t, down)
	}
}

func TestDesktopResumeAndOutOfOrderResponsesCannotLoseWatch(t *testing.T) {
	m, up, down := readyMux(t)
	release := m.WatchThread("thread-a")
	if release == nil {
		t.Fatal("watch denied")
	}
	release()
	mustDesktop(t, m, `{"id":1,"method":"thread/unsubscribe","params":{"threadId":"thread-a"}}`)
	unsubscribeID := frameID(t, nextFrame(t, up))
	mustDesktop(t, m, `{"id":2,"method":"thread/resume","params":{"threadId":"thread-a"}}`)
	resumeID := frameID(t, nextFrame(t, up))
	mustServer(t, m, fmt.Sprintf(`{"id":%s,"result":{"thread":{"id":"thread-a"}}}`, resumeID))
	nextFrame(t, down)
	mustServer(t, m, fmt.Sprintf(`{"id":%s,"result":{"status":"unsubscribed"}}`, unsubscribeID))
	nextFrame(t, down)
	if m.Status().RetainedThreads != 0 {
		t.Fatal("settled Desktop lifecycle retained local bookkeeping")
	}
	release = m.WatchThread("thread-a")
	if release == nil {
		t.Fatal("watch denied after pending unsubscribe completed")
	}
	mustDesktop(t, m, `{"id":3,"method":"thread/resume","params":{"threadId":"thread-a"}}`)
	resumeID = frameID(t, nextFrame(t, up))
	mustDesktop(t, m, `{"id":4,"method":"thread/unsubscribe","params":{"threadId":"thread-a"}}`)
	nextFrame(t, down)
	mustServer(t, m, fmt.Sprintf(`{"id":%s,"result":{"thread":{"id":"thread-a"}}}`, resumeID))
	nextFrame(t, down)
	m.mu.Lock()
	desktopSubscribed := m.threads["thread-a"].desktopSubscribed
	m.mu.Unlock()
	if desktopSubscribed {
		t.Fatal("late resume response overwrote later suppressed unsubscribe")
	}
	before := up.text()
	release()
	if up.text() != before {
		t.Fatal("release detached a raced Desktop subscription")
	}
}

func TestUnsubscribeFailureReleasesPendingGuard(t *testing.T) {
	m, up, down := readyMux(t)
	release := m.WatchThread("thread-a")
	release()
	for _, outcome := range []string{`"error":{"code":-1,"message":"failed"}`, `"result":{"status":"unknownFutureStatus"}`} {
		mustDesktop(t, m, `{"id":1,"method":"thread/unsubscribe","params":{"threadId":"thread-a"}}`)
		id := frameID(t, nextFrame(t, up))
		mustServer(t, m, `{"id":`+string(id)+`,`+outcome+`}`)
		nextFrame(t, down)
		if m.Status().RetainedThreads != 0 {
			t.Fatal("settled unsubscribe retained local bookkeeping")
		}
		if release := m.WatchThread("thread-a"); release == nil {
			t.Fatal("failed unsubscribe did not release pending watch guard")
		} else {
			release()
		}
	}
}

func TestWatchLocalResumeReleaseAndUnsubscribeRestriction(t *testing.T) {
	m, up, down := readyMux(t)
	before := up.text()
	if _, err := m.Call(context.Background(), "thread/unsubscribe", map[string]string{"threadId": "thread-a"}); !errors.Is(err, ErrUnsubscribeReserved) || up.text() != before {
		t.Fatalf("companion unsubscribe was not reserved: %v", err)
	}
	release := m.WatchThread("thread-a")
	if release == nil {
		t.Fatal("watch denied")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	completed := make(chan error, 1)
	go func() {
		_, err := m.Call(ctx, "thread/resume", map[string]string{"threadId": "thread-a"})
		completed <- err
	}()
	id := frameID(t, nextFrame(t, up))
	mustDesktop(t, m, `{"id":1,"method":"thread/unsubscribe","params":{"threadId":"thread-a"}}`)
	nextFrame(t, down)
	before = up.text()
	release()
	mustServer(t, m, fmt.Sprintf(`{"id":%s,"result":{"thread":{"id":"thread-a"}}}`, id))
	if err := <-completed; err != nil {
		t.Fatal(err)
	}
	if up.text() != before || len(down.frames) != 0 || m.Status().RetainedThreads != 0 {
		t.Fatal("local resume response or release affected Desktop transport")
	}
}

func TestThreadStartResultRecordsDesktopSubscription(t *testing.T) {
	m, up, down := readyMux(t)
	release := m.WatchThread("new-thread")
	defer release()
	mustDesktop(t, m, `{"id":1,"method":"thread/start","params":{}}`)
	id := frameID(t, nextFrame(t, up))
	mustServer(t, m, fmt.Sprintf(`{"id":%s,"result":{"thread":{"id":"new-thread"}}}`, id))
	nextFrame(t, down)
	m.mu.Lock()
	subscribed := m.threads["new-thread"].desktopSubscribed
	m.mu.Unlock()
	if !subscribed {
		t.Fatal("thread/start result did not record Desktop interest")
	}
}

func TestReleaseNeverWaitsForProtocolWriter(t *testing.T) {
	m, up, _ := readyMux(t)
	release := m.WatchThread("thread-a")
	if release == nil {
		t.Fatal("watch denied")
	}
	blocked := &blockingWriter{started: make(chan struct{}), release: make(chan struct{})}
	m.desktop = blocked
	var unblock sync.Once
	defer unblock.Do(func() { close(blocked.release) })
	finished := make(chan error, 1)
	go func() {
		finished <- m.FromDesktop([]byte(`{"id":1,"method":"thread/unsubscribe","params":{"threadId":"thread-a"}}`))
	}()
	select {
	case <-blocked.started:
	case <-time.After(time.Second):
		t.Fatal("synthetic acknowledgement never started")
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); release() }()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("release blocked on synthetic acknowledgement")
	}
	// The upstream gate is not held while synthetic Desktop output blocks.
	mustDesktop(t, m, `{"method":"independent/upstream"}`)
	nextFrame(t, up)
	m.Close(nil)
	unblock.Do(func() { close(blocked.release) })
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("synthetic writer did not finish after pipe unblocked")
	}
}

func TestWatchMatchingDecodedThreadID(t *testing.T) {
	m, up, down := readyMux(t)
	release := m.WatchThread("thread-a")
	defer release()
	before := up.text()
	escapedThread := `"thread-` + `\` + `u0061"`
	mustDesktop(t, m, `{"id":"original","method":"thread/unsubscribe","params":{"threadId":`+escapedThread+`}}`)
	response := nextFrame(t, down)
	if up.text() != before || string(frameID(t, response)) != `"original"` {
		t.Fatal("equivalent escaped thread ID bypassed watch")
	}
	var result map[string]json.RawMessage
	if json.Unmarshal(response, &result) != nil || string(result["result"]) != `{"status":"unsubscribed"}` {
		t.Fatal("invalid synthetic result")
	}
}

func TestZeroWatchPendingResumeSettlesOnSuccessFailureAndCancel(t *testing.T) {
	for _, outcome := range []string{"success", "failure", "cancel"} {
		t.Run(outcome, func(t *testing.T) {
			m, up, down := readyMux(t)
			release := m.WatchThread("pending")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := m.Call(ctx, "thread/resume", map[string]string{"threadId": "pending"}); done <- err }()
			id := frameID(t, nextFrame(t, up))
			release()
			if status := m.Status(); status.WatchedThreads != 0 || status.RetainedThreads != 1 || status.PendingLocal != 1 {
				t.Fatal(status)
			}
			active := make([]func(), MaxWatchedThreads)
			for i := range active {
				active[i] = m.WatchThread(fmt.Sprintf("other-%d", i))
				if active[i] == nil {
					t.Fatal("pending zero watcher consumed active capacity", i)
				}
			}
			switch outcome {
			case "success":
				mustServer(t, m, fmt.Sprintf(`{"id":%s,"result":{"thread":{"id":"pending"}}}`, id))
			case "failure":
				mustServer(t, m, fmt.Sprintf(`{"id":%s,"error":{"code":-1,"message":"failed"}}`, id))
			case "cancel":
				cancel()
			}
			err := <-done
			if (outcome == "success") != (err == nil) {
				t.Fatal(outcome, err)
			}
			if status := m.Status(); status.RetainedThreads != 0 || status.PendingLocal != 0 || len(down.frames) != 0 {
				t.Fatal(status)
			}
			for _, stop := range active {
				stop()
			}
			fresh := m.WatchThread("pending")
			if fresh == nil {
				t.Fatal("settled thread cannot be watched")
			}
			m.mu.Lock()
			record := m.threads["pending"]
			m.mu.Unlock()
			// Late or duplicate replies cannot mutate/delete the replacement.
			mustServer(t, m, fmt.Sprintf(`{"id":%s,"result":{"thread":{"id":"pending"}}}`, id))
			m.mu.Lock()
			same := m.threads["pending"] == record && record.watchers == 1 && record.pending == 0
			m.mu.Unlock()
			if !same || len(down.frames) != 0 {
				t.Fatal("old lifecycle response changed replacement")
			}
			fresh()
			if m.Status().RetainedThreads != 0 {
				t.Fatal("settled replacement retained")
			}
		})
	}
}

func TestMultiplePendingLifecycleOperationsRetainExactRecordUntilAllSettle(t *testing.T) {
	m, up, down := readyMux(t)
	release := m.WatchThread("thread-a")
	mustDesktop(t, m, `{"id":1,"method":"thread/resume","params":{"threadId":"thread-a"}}`)
	first := frameID(t, nextFrame(t, up))
	mustDesktop(t, m, `{"id":2,"method":"thread/resume","params":{"threadId":"thread-a"}}`)
	second := frameID(t, nextFrame(t, up))
	m.mu.Lock()
	original := m.threads["thread-a"]
	m.mu.Unlock()
	release()
	mustServer(t, m, fmt.Sprintf(`{"id":%s,"result":{"thread":{"id":"thread-a"}}}`, second))
	nextFrame(t, down)
	if m.Status().RetainedThreads != 1 {
		t.Fatal("one result reclaimed another pending lifecycle")
	}
	fresh := m.WatchThread("thread-a")
	if fresh == nil {
		t.Fatal("reactivating pending record failed")
	}
	mustDesktop(t, m, `{"id":3,"method":"thread/unsubscribe","params":{"threadId":"thread-a"}}`)
	nextFrame(t, down)
	mustServer(t, m, fmt.Sprintf(`{"id":%s,"result":{"thread":{"id":"thread-a"}}}`, first))
	nextFrame(t, down)
	m.mu.Lock()
	valid := m.threads["thread-a"] == original && original.pending == 0 && original.watchers == 1 && !original.desktopSubscribed
	m.mu.Unlock()
	if !valid {
		t.Fatal("old resume response lost new watcher or overwrote later lifecycle")
	}
	before := up.text()
	fresh()
	if m.Status().RetainedThreads != 0 || up.text() != before {
		t.Fatal("release retained settled record or detached upstream")
	}
}
