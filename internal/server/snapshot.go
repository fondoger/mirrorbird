package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"

	"mirrorbird/internal/session"
)

type approvalParams struct {
	ThreadID  string          `json:"threadId,omitempty"`
	TurnID    string          `json:"turnId,omitempty"`
	ItemID    string          `json:"itemId,omitempty"`
	Command   string          `json:"command,omitempty"`
	CWD       string          `json:"cwd,omitempty"`
	Reason    string          `json:"reason,omitempty"`
	GrantRoot string          `json:"grantRoot,omitempty"`
	Changes   json.RawMessage `json:"changes,omitempty"`
}

type browserApproval struct {
	ID          string         `json:"id"`
	Method      string         `json:"method"`
	Params      approvalParams `json:"params"`
	DesktopOnly bool           `json:"desktopOnly,omitempty"`
}

type snapshotResponse struct {
	session.Snapshot
	Pending []browserApproval `json:"pending"`
}

func browserSnapshot(value session.Snapshot) snapshotResponse {
	pending := make([]browserApproval, 0, len(value.Pending))
	for _, request := range value.Pending {
		if !validApprovalID(request.ID) {
			continue
		}
		var params approvalParams
		// Forward only the small, documented display surface; never arbitrary
		// tool arguments, auth data, or the raw server request envelope.
		_ = json.Unmarshal(request.Params, &params)
		desktopOnly := request.DesktopOnly
		if request.Method == "item/fileChange/requestApproval" {
			// File approval params identify the item, while its full patch lives
			// in the transcript. Never offer a blind approval without that patch.
			params.Changes = nil
			if params.ThreadID == value.Thread.ID {
				for _, turn := range value.Thread.Turns {
					if turn.ID != params.TurnID {
						continue
					}
					for _, item := range turn.Items {
						if item.ID == params.ItemID && item.Type == "fileChange" {
							params.Changes = item.Changes
						}
					}
				}
			}
			var changes []json.RawMessage
			if json.Unmarshal(params.Changes, &changes) != nil || len(changes) == 0 {
				desktopOnly = true
			}
		}
		pending = append(pending, browserApproval{
			ID: base64.RawURLEncoding.EncodeToString(request.ID), Method: request.Method, Params: params, DesktopOnly: desktopOnly,
		})
	}
	value.Pending = nil
	return snapshotResponse{Snapshot: value, Pending: pending}
}

func validApprovalID(raw json.RawMessage) bool {
	if len(raw) == 0 || len(raw) > 2048 {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return false
	}
	switch value.(type) {
	case string, json.Number:
	default:
		return false
	}
	return decoder.Decode(&value) == io.EOF
}

func decodeApprovalID(handle string) (json.RawMessage, error) {
	if len(handle) > 2731 {
		return nil, errors.New("invalid approval handle")
	}
	raw, err := base64.RawURLEncoding.DecodeString(handle)
	if err != nil || !validApprovalID(raw) {
		return nil, errors.New("invalid approval handle")
	}
	return json.RawMessage(raw), nil
}
