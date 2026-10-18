package attachment

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Version 2 adds the Project/model/Skills and thread-management operations.
const ProtocolVersion = 2

type wireError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *wireError) Error() string { return fmt.Sprintf("%s (code %d)", e.Message, e.Code) }

func (e *wireError) Is(target error) bool {
	return (target == ErrRequestResolved && e.Code == 410) ||
		(target == ErrOutdatedShim && e.Code == 502 && e.Message == "unsupported attachment operation") ||
		(target == ErrUnsupportedOperation && (e.Code == 501 || (e.Code == 502 && e.Message == "unsupported attachment operation")))
}

type packet struct {
	ID     string          `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *wireError      `json:"error,omitempty"`
}

func encode(value any) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}

var ErrDisconnected = errors.New("desktop attachment disconnected")
var ErrRequestResolved = errors.New("request is no longer pending")
var ErrUnsupportedOperation = errors.New("unsupported app-server operation")
var ErrOutdatedShim = errors.New("ChatGPT Desktop is running an outdated MirrorBird shim")
