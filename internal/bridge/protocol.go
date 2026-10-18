package bridge

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf8"
)

var ErrInvalidMessage = errors.New("bridge: invalid JSON-RPC message")

type message struct {
	frame     []byte
	id        json.RawMessage
	idKey     string
	idStart   int
	idEnd     int
	method    string
	hasMethod bool
	params    json.RawMessage
	result    json.RawMessage
	rpcError  json.RawMessage
}

// Inspect only protocol fields. The original representation (including unknown
// fields, whitespace, escape spellings and number precision) remains untouched.
func parseMessage(frame []byte) (message, error) {
	var msg message
	if len(frame) > MaxFrameBytes {
		return msg, ErrFrameTooLarge
	}
	if !utf8.Valid(frame) || bytes.IndexByte(frame, '\n') >= 0 || !json.Valid(frame) {
		return msg, ErrInvalidMessage
	}
	dec := json.NewDecoder(bytes.NewReader(frame))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return msg, ErrInvalidMessage
	}
	msg.frame = frame
	seen := make(map[string]bool, 5)
	for dec.More() {
		token, err := dec.Token()
		if err != nil {
			return message{}, ErrInvalidMessage
		}
		key, ok := token.(string)
		if !ok {
			return message{}, ErrInvalidMessage
		}
		start := int(dec.InputOffset())
		for start < len(frame) && (frame[start] == ':' || jsonSpace(frame[start])) {
			start++
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return message{}, ErrInvalidMessage
		}
		switch key {
		case "id", "method", "params", "result", "error":
			if seen[key] {
				return message{}, ErrInvalidMessage
			}
			seen[key] = true
		default:
			continue
		}
		switch key {
		case "id":
			msg.id, msg.idStart, msg.idEnd = raw, start, int(dec.InputOffset())
			msg.idKey, err = requestIDKey(raw)
			if err != nil {
				return message{}, err
			}
		case "method":
			if err := json.Unmarshal(raw, &msg.method); err != nil || bytes.Equal(raw, []byte("null")) {
				return message{}, ErrInvalidMessage
			}
			msg.hasMethod = true
		case "params":
			msg.params = raw
		case "result":
			msg.result = raw
		case "error":
			msg.rpcError = raw
		}
	}
	if msg.hasMethod {
		if msg.result != nil || msg.rpcError != nil {
			return message{}, ErrInvalidMessage
		}
	} else if msg.id == nil || (msg.result == nil) == (msg.rpcError == nil) {
		return message{}, ErrInvalidMessage
	}
	return msg, nil
}

func jsonSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

func (msg message) replaceID(id json.RawMessage) []byte {
	out := make([]byte, 0, len(msg.frame)-len(msg.id)+len(id))
	out = append(out, msg.frame[:msg.idStart]...)
	out = append(out, id...)
	out = append(out, msg.frame[msg.idEnd:]...)
	return out
}

// A direction's ID key distinguishes strings from numbers, but regards numeric
// spellings such as 1, 1.0 and 1e0 as the same ID. Normalize the exponent rather
// than expanding it: even an enormous exponent cannot cause enormous allocation.
func requestIDKey(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || !json.Valid(raw) {
		return "", ErrInvalidMessage
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "", ErrInvalidMessage
		}
		return "s:" + s, nil
	}
	if raw[0] != '-' && (raw[0] < '0' || raw[0] > '9') {
		return "", ErrInvalidMessage
	}
	number := string(raw)
	negative := strings.HasPrefix(number, "-")
	number = strings.TrimPrefix(number, "-")
	mantissa, exponent := number, "0"
	if i := strings.IndexAny(number, "eE"); i >= 0 {
		mantissa, exponent = number[:i], number[i+1:]
	}
	fraction := 0
	if i := strings.IndexByte(mantissa, '.'); i >= 0 {
		fraction = len(mantissa) - i - 1
		mantissa = mantissa[:i] + mantissa[i+1:]
	}
	mantissa = strings.TrimLeft(mantissa, "0")
	if mantissa == "" {
		return "n:0", nil
	}
	trimmed := strings.TrimRight(mantissa, "0")
	adjustment := len(mantissa) - len(trimmed) - fraction
	// Common IDs avoid big.Int entirely; the fallback handles arbitrary JSON
	// numeric precision without float64 rounding or exponent expansion.
	var normalized string
	if e, err := strconv.ParseInt(exponent, 10, 64); err == nil && e >= -(1<<62) && e <= 1<<62 {
		normalized = strconv.FormatInt(e+int64(adjustment), 10)
	} else {
		e, ok := new(big.Int).SetString(exponent, 10)
		if !ok {
			return "", ErrInvalidMessage
		}
		e.Add(e, big.NewInt(int64(adjustment)))
		normalized = e.String()
	}
	if negative {
		trimmed = "-" + trimmed
	}
	return "n:" + trimmed + "e" + normalized, nil
}

// RPCError preserves the upstream code, message and optional data. Raw also
// retains any error-object extensions. Error() intentionally omits remote text:
// app-server messages and data can contain private prompts or paths.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
	Raw     json.RawMessage `json:"-"`
}

func (e *RPCError) Error() string {
	return "bridge: upstream RPC error (code " + strconv.Itoa(e.Code) + ")"
}

func decodeRPCError(raw json.RawMessage) error {
	var wire struct {
		Code    *int            `json:"code"`
		Message *string         `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &wire) != nil || wire.Code == nil || wire.Message == nil {
		return ErrInvalidMessage
	}
	return &RPCError{Code: *wire.Code, Message: *wire.Message, Data: wire.Data, Raw: clone(raw)}
}

func clone(b []byte) []byte {
	return append([]byte(nil), b...)
}

func writeFrame(w io.Writer, frame []byte) error {
	if w == nil {
		return errors.New("bridge: missing protocol writer")
	}
	out := make([]byte, len(frame)+1)
	copy(out, frame)
	out[len(frame)] = '\n'
	for len(out) != 0 {
		n, err := w.Write(out)
		if n < 0 || n > len(out) {
			return io.ErrShortWrite
		}
		out = out[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
