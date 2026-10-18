// Package bridge multiplexes one desktop JSONL connection and local callers onto
// a single app-server connection. It never starts a process or logs protocol data.
package bridge

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"
)

// MaxFrameBytes bounds a JSON message, excluding its CRLF or LF terminator.
const MaxFrameBytes = 16 << 20

var (
	ErrFrameTooLarge = errors.New("bridge: JSON frame exceeds size limit")
	ErrInvalidFrame  = errors.New("bridge: invalid JSON frame")
)

// ReadFrames consumes nonblank JSON lines, without their line terminators. Each
// delivered slice is owned by consume and is not reused. It accepts fragmented
// UTF-8, CRLF, and a final unterminated JSON value. EOF is a normal completion;
// other reader errors and consume errors are returned unchanged. Error messages
// never include any part of a malformed frame. A nonpositive maxBytes selects
// MaxFrameBytes.
func ReadFrames(r io.Reader, maxBytes int, consume func([]byte) error) error {
	if r == nil || consume == nil {
		return errors.New("bridge: reader and consumer are required")
	}
	if maxBytes <= 0 {
		maxBytes = MaxFrameBytes
	}

	reader := bufio.NewReaderSize(r, 32<<10)
	var frame []byte
	for {
		part, err := reader.ReadSlice('\n')
		terminated := len(part) != 0 && part[len(part)-1] == '\n'
		if terminated {
			part = part[:len(part)-1]
		}

		// Permit one extra byte only when it is a possible CRLF terminator.
		// Subtract first so a caller-supplied MaxInt limit cannot overflow.
		remaining := maxBytes - len(frame)
		if len(part) > remaining {
			lastIsCR := len(part) > 0 && part[len(part)-1] == '\r'
			if len(part) == 0 && len(frame) > 0 {
				lastIsCR = frame[len(frame)-1] == '\r'
			}
			if len(part)-remaining != 1 || !lastIsCR {
				return ErrFrameTooLarge
			}
		}
		frame = append(frame, part...)
		if err == bufio.ErrBufferFull {
			continue
		}

		if len(frame) > 0 && frame[len(frame)-1] == '\r' {
			frame = frame[:len(frame)-1]
		}
		if !blankJSONLine(frame) {
			if !utf8.Valid(frame) || !json.Valid(frame) {
				return ErrInvalidFrame
			}
			// Do not leave a stripped terminator or spare capacity writable by
			// consumers that retain this frame.
			if err := consume(frame[:len(frame):len(frame)]); err != nil {
				return err
			}
		}
		frame = nil
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func blankJSONLine(frame []byte) bool {
	return len(bytes.Trim(frame, " \t\r")) == 0
}
