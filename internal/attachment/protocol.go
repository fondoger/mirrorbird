package attachment

import (
	"encoding/json"
	"errors"
	"fmt"
)

type wireError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *wireError) Error() string { return fmt.Sprintf("%s (code %d)", e.Message, e.Code) }

func (e *wireError) Is(target error) bool {
	return target == ErrRequestResolved && e.Code == 410
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
