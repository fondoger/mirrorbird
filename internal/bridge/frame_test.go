package bridge

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

type chunkReader struct {
	data []byte
	step int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.step, len(r.data))
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

func TestReadFramesFragmentationAndFinalFrame(t *testing.T) {
	want := []string{` {"method":"event","params":{"text":"汉字𠮷"}} `, `{"id":"a","result":null}`, `{"method":"last"}`}
	input := "\r\n \t\n" + want[0] + "\r\n" + want[1] + "\n" + want[2]
	for _, step := range []int{1, 2, 3, 7, 32768} {
		t.Run(strings.Repeat("x", min(step, 10)), func(t *testing.T) {
			var got []string
			err := ReadFrames(&chunkReader{data: []byte(input), step: step}, 0, func(frame []byte) error {
				got = append(got, string(frame))
				return nil
			})
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("frames = %q, error = %v; want %q", got, err, want)
			}
		})
	}
}

func TestReadFramesLimit(t *testing.T) {
	frame := `{"text":"` + strings.Repeat("x", 100000) + `"}`
	for _, ending := range []string{"", "\n", "\r\n"} {
		for _, limit := range []int{len(frame) - 1, len(frame), len(frame) + 1} {
			called := 0
			err := ReadFrames(strings.NewReader(frame+ending), limit, func(got []byte) error {
				called++
				if string(got) != frame {
					t.Fatal("frame bytes changed")
				}
				return nil
			})
			if limit < len(frame) {
				if !errors.Is(err, ErrFrameTooLarge) || called != 0 {
					t.Fatalf("undersized limit: called=%d err=%v", called, err)
				}
			} else if err != nil || called != 1 {
				t.Fatalf("exact/larger limit: called=%d err=%v", called, err)
			}
		}
	}
}

func TestReadFramesCRLFAcrossBufferBoundary(t *testing.T) {
	frame := `"` + strings.Repeat("x", (32<<10)-3) + `"`
	if err := ReadFrames(strings.NewReader(frame+"\r\n{}\n"), len(frame), func([]byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestReadFramesOwnsDeliveredBuffers(t *testing.T) {
	var frames [][]byte
	if err := ReadFrames(strings.NewReader("{\"a\":1}\n{\"b\":2}\n"), 0, func(frame []byte) error {
		frames = append(frames, frame)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 || string(frames[0]) != `{"a":1}` || string(frames[1]) != `{"b":2}` {
		t.Fatalf("delivered buffers were reused: %q", frames)
	}
}

func TestReadFramesInvalidAndConsumerError(t *testing.T) {
	for _, input := range [][]byte{
		[]byte(`{"private":"secret_value"`),
		[]byte("{} {}\n"),
		{'"', 0xff, '"', '\n'},
	} {
		err := ReadFrames(bytes.NewReader(input), 0, func([]byte) error {
			t.Fatal("consumer called with invalid frame")
			return nil
		})
		if !errors.Is(err, ErrInvalidFrame) || strings.Contains(err.Error(), "secret_value") {
			t.Fatalf("invalid frame error = %v", err)
		}
	}
	stop := errors.New("consumer stopped")
	count := 0
	err := ReadFrames(strings.NewReader("{}\n{}\n"), 0, func([]byte) error {
		count++
		return stop
	})
	if !errors.Is(err, stop) || count != 1 {
		t.Fatalf("consumer error: count=%d err=%v", count, err)
	}
	if err := ReadFrames(strings.NewReader(""), 0, func([]byte) error { return nil }); err != nil {
		t.Fatalf("empty EOF: %v", err)
	}
}

type brokenReader struct{ err error }

func (r brokenReader) Read([]byte) (int, error) { return 0, r.err }

func TestReadFramesReaderError(t *testing.T) {
	want := errors.New("read failed")
	if err := ReadFrames(brokenReader{want}, 0, func([]byte) error { return nil }); !errors.Is(err, want) {
		t.Fatalf("reader error = %v", err)
	}
}

type dataAndErrorReader struct {
	data []byte
	err  error
}

func (r *dataAndErrorReader) Read(p []byte) (int, error) {
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, r.err
}

func TestReadFramesProcessesBytesReturnedWithReaderError(t *testing.T) {
	want := errors.New("connection ended")
	reader := &dataAndErrorReader{data: []byte("{}\n{\"final\":true}"), err: want}
	var frames []string
	err := ReadFrames(reader, 0, func(frame []byte) error {
		frames = append(frames, string(frame))
		return nil
	})
	if !errors.Is(err, want) || !reflect.DeepEqual(frames, []string{`{}`, `{"final":true}`}) {
		t.Fatalf("reader bytes/error = %q, %v", frames, err)
	}
}
