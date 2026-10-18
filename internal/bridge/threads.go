package bridge

import (
	"encoding/json"
	"errors"
	"sync"
	"unicode/utf8"
)

// MaxWatchedThreads bounds thread IDs with positive local watch counts. It does
// not count upstream threads that Desktop may keep loaded after local release.
const MaxWatchedThreads = 32

const maxThreadIDBytes = 1024

var ErrUnsubscribeReserved = errors.New("bridge: local callers must release thread watches, not unsubscribe")

type threadWatch struct {
	watchers          int
	pending           int // lifecycle requests that captured this exact record
	revision          uint64
	desktopOrder      uint64
	desktopSubscribed bool
}

// WatchThread registers local interest without issuing any RPC. Call it BEFORE
// thread/resume, and check for a nil return: a closed Mux, invalid ID, exhausted
// capacity, or an in-flight Desktop unsubscribe denies the watch. A denied watch
// must not be followed by a local resume. The returned release is idempotent and
// never writes to either connection.
//
// While at least one watch exists, a Desktop thread/unsubscribe succeeds locally
// without detaching the shared app-server subscription. Releasing the last watch
// deliberately does NOT send an automatic unsubscribe: a concurrent Desktop
// resume must never be detached by companion cleanup. Consequently a formerly
// watched thread may remain loaded upstream until a later explicit Desktop
// unsubscribe or connection exit. Zero-watcher records survive only while a
// relevant lifecycle request is pending; the request limits bound that retention.
func (m *Mux) WatchThread(threadID string) (release func()) {
	if !validThreadID(threadID) {
		return nil
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	// FromDesktop reserves its pending entry before writing, under the same
	// lock. Whichever operation wins either protects the unsubscribe or denies
	// this watch until its outcome is known. Never race a detach already sent.
	for _, pending := range m.clients {
		if pending.threadMethod == "thread/unsubscribe" && pending.threadID == threadID {
			m.mu.Unlock()
			return nil
		}
	}
	watch := m.threads[threadID]
	if watch == nil || watch.watchers == 0 {
		active := 0
		for _, entry := range m.threads {
			if entry.watchers > 0 {
				active++
			}
		}
		if active >= MaxWatchedThreads {
			m.mu.Unlock()
			return nil
		}
		if watch == nil {
			watch = &threadWatch{}
			m.threads[threadID] = watch
		}
	}
	if watch.watchers >= maxClientPending {
		m.mu.Unlock()
		return nil
	}
	watch.watchers++
	m.threadOrder++
	watch.revision = m.threadOrder
	m.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			if m.threads[threadID] == watch {
				watch.watchers--
				m.reclaimThreadLocked(threadID, watch)
			}
			m.mu.Unlock()
		})
	}
}

func validThreadID(id string) bool {
	return id != "" && len(id) <= maxThreadIDBytes && utf8.ValidString(id)
}

// Keep metadata only for lifecycle operations, with bounded opaque thread IDs.
// Invalid/unrecognized requests still go to the server for its normal error.
func threadMetadata(method string, params json.RawMessage) (string, string) {
	switch method {
	case "thread/start":
		return "thread/start", ""
	case "thread/resume", "thread/unsubscribe":
		var fields struct {
			ThreadID string `json:"threadId"`
		}
		if json.Unmarshal(params, &fields) != nil || !validThreadID(fields.ThreadID) {
			return "", ""
		}
		if method == "thread/resume" {
			return "thread/resume", fields.ThreadID
		}
		return "thread/unsubscribe", fields.ThreadID
	default:
		return "", ""
	}
}

func (m *Mux) beginThreadOperationLocked(pending *clientPending) {
	if pending.threadMethod == "" {
		return
	}
	m.threadOrder++
	pending.threadOrder = m.threadOrder
	if watch := m.threads[pending.threadID]; watch != nil {
		watch.revision = pending.threadOrder
		watch.pending++
		pending.threadWatch = watch
	}
}

func (m *Mux) reclaimThreadLocked(threadID string, watch *threadWatch) {
	if m.threads[threadID] == watch && watch.watchers == 0 && watch.pending == 0 {
		delete(m.threads, threadID)
	}
}

// Called exactly once when a mapped request settles, including errors and local
// cancellation. Late responses have no mapping and cannot touch a replacement.
func (m *Mux) settleThreadOperationLocked(pending *clientPending) {
	if watch := pending.threadWatch; watch != nil {
		watch.pending--
		m.reclaimThreadLocked(pending.threadID, watch)
		pending.threadWatch = nil
	}
}

func (m *Mux) suppressUnsubscribeLocked(threadID string) bool {
	watch := m.threads[threadID]
	if watch == nil || watch.watchers == 0 {
		return false
	}
	m.threadOrder++
	watch.revision, watch.desktopOrder = m.threadOrder, m.threadOrder
	watch.desktopSubscribed = false
	return true
}

func (m *Mux) completeThreadOperationLocked(pending *clientPending, msg message) {
	if pending.threadMethod == "" || msg.result == nil {
		return
	}
	if pending.threadMethod == "thread/unsubscribe" {
		var result struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(msg.result, &result) != nil || result.Status != "unsubscribed" {
			return
		}
		watch := m.threads[pending.threadID]
		if watch != nil && watch == pending.threadWatch && pending.threadOrder >= watch.desktopOrder {
			watch.desktopSubscribed = false
			watch.desktopOrder = pending.threadOrder
		}
		return
	}
	var result struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if json.Unmarshal(msg.result, &result) != nil {
		return
	}
	threadID := result.Thread.ID
	if threadID == "" && pending.threadMethod == "thread/resume" {
		threadID = pending.threadID
	}
	if watch := m.threads[threadID]; watch != nil && pending.done == nil && pending.threadOrder >= watch.desktopOrder {
		if pending.threadMethod == "thread/resume" && watch != pending.threadWatch {
			return
		}
		if pending.threadMethod == "thread/start" && pending.threadOrder < watch.revision {
			return
		}
		watch.desktopSubscribed = true
		watch.desktopOrder = pending.threadOrder
	}
}

// This is a synthetic local acknowledgement, not an upstream server event.
// It uses the same Desktop write mutex but is never published to subscribers.
func (m *Mux) acknowledgeUnsubscribe(id json.RawMessage) error {
	frame := make([]byte, 0, len(id)+48)
	frame = append(frame, `{"id":`...)
	frame = append(frame, id...)
	frame = append(frame, `,"result":{"status":"unsubscribed"}}`...)
	m.downMu.Lock()
	defer m.downMu.Unlock()
	m.mu.Lock()
	if m.closed {
		err := m.closeErr
		m.mu.Unlock()
		return err
	}
	m.mu.Unlock()
	if err := writeFrame(m.desktop, frame); err != nil {
		m.Close(err)
		return err
	}
	return nil
}
