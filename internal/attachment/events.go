package attachment

import (
	"encoding/json"

	"mirrorbird/internal/bridge"
)

// Responses advance the observer's ordering fence but are not state deltas.
// Their bodies belong to the RPC caller, not the bounded live-event queue.
// An empty object keeps the same fence semantics for existing clients.
func compactResponseEvent(event *bridge.Event) error {
	var header struct {
		Method json.RawMessage `json:"method"`
	}
	if err := json.Unmarshal(event.Message, &header); err != nil {
		return err
	}
	if len(header.Method) == 0 {
		event.Message = json.RawMessage(`{}`)
	}
	return nil
}
