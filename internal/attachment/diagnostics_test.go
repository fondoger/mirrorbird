package attachment

import (
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"mirrorbird/internal/diagnostics"
)

func TestReadFailureDiagnosticsExcludePacketContents(t *testing.T) {
	for _, tc := range []struct {
		name, input, reason string
	}{
		{"frame", "{\"PRIVATE_TOKEN\"\n", "invalid_frame"},
		{"event", "{\"method\":\"event\",\"params\":[\"PRIVATE_TOKEN\"]}\n", "invalid_event"},
		{"eof", "", "peer_closed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stopLog, err := diagnostics.Start(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer stopLog()
			path := diagnostics.Path()
			left, right := net.Pipe()
			c := testClient(left)
			defer c.Close()
			defer right.Close()
			go c.read()
			if tc.input != "" {
				if err := right.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
					t.Fatal(err)
				}
				if _, err := right.Write([]byte(tc.input)); err != nil {
					t.Fatal(err)
				}
			}
			_ = right.Close()
			select {
			case <-c.Done():
			case <-time.After(time.Second):
				t.Fatal("attachment did not close")
			}
			stopLog()
			data, err := os.ReadFile(path)
			if err != nil || !strings.Contains(string(data), `"reason":"`+tc.reason+`"`) ||
				strings.Contains(string(data), "PRIVATE_TOKEN") {
				t.Fatal("missing or unsafe disconnect diagnostic", string(data), err)
			}
		})
	}
}
