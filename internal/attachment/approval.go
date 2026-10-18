package attachment

import (
	"bytes"
	"encoding/json"
)

// PhoneApprovalSupported limits the phone to concrete one-shot command and file
// approvals. Rich scopes the phone cannot explain accurately stay in Desktop.
// It is shared by projection/display and the final local-IPC enforcement point.
func PhoneApprovalSupported(method string, raw json.RawMessage) bool {
	var p map[string]json.RawMessage
	if json.Unmarshal(raw, &p) != nil || len(p) == 0 {
		return false
	}
	text := func(key string) string {
		var value string
		_ = json.Unmarshal(p[key], &value)
		return value
	}
	if text("threadId") == "" || text("turnId") == "" || text("itemId") == "" {
		return false
	}
	switch method {
	case "item/commandExecution/requestApproval":
		if text("command") == "" || text("cwd") == "" {
			return false
		}
		for _, key := range []string{"additionalPermissions", "networkApprovalContext", "environmentId"} {
			if presentJSON(p[key]) {
				return false
			}
		}
		if kind := text("kind"); kind != "" && kind != "command" {
			return false
		}
		if presentJSON(p["availableDecisions"]) {
			var decisions []json.RawMessage
			if json.Unmarshal(p["availableDecisions"], &decisions) != nil {
				return false
			}
			accept, decline := false, false
			for _, raw := range decisions {
				var decision string
				_ = json.Unmarshal(raw, &decision)
				accept = accept || decision == "accept"
				decline = decline || decision == "decline"
			}
			if !accept || !decline {
				return false
			}
		}
		return true
	case "item/fileChange/requestApproval":
		return !presentJSON(p["grantRoot"])
	default:
		return false
	}
}

func presentJSON(raw json.RawMessage) bool {
	return len(raw) != 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
