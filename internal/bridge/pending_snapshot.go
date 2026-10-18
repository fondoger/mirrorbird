package bridge

import (
	"encoding/json"
	"sort"
)

// PendingRequestsWithSequence takes the unresolved request set and the server
// event watermark under the same lock. Adapters must subscribe first, then apply
// only request/resolved events after this watermark to this set. This is not a
// thread-history snapshot and does not change reply arbitration.
func (m *Mux) PendingRequestsWithSequence() ([]json.RawMessage, uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pending := make([]*serverPending, 0, len(m.servers))
	for _, request := range m.servers {
		pending = append(pending, request)
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].sequence < pending[j].sequence })
	frames := make([]json.RawMessage, 0, len(pending))
	for _, request := range pending {
		frames = append(frames, clone(request.frame))
	}
	return frames, m.sequence
}

// RequestIDKey lets protocol adapters use exactly the Mux's lossless ID equality
// without converting JSON numbers to float64 or conflating strings and numbers.
// Keep the original RawMessage, not this internal key, when answering a request.
func RequestIDKey(id json.RawMessage) (string, error) {
	return requestIDKey(id)
}
