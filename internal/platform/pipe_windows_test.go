//go:build windows

package platform

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func uniquePipeName(t *testing.T) string {
	t.Helper()
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		t.Fatal(err)
	}
	return "mirrorbird-test-" + hex.EncodeToString(entropy[:])
}

func TestPipeRoundTripExclusiveAndPrivate(t *testing.T) {
	name := uniquePipeName(t)
	listener, err := ListenPipe(name)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	// Named pipes are case-insensitive: verify duplicate and case-alias names
	// cannot create a competing listener while the first handle is retained.
	for _, duplicate := range []string{name, appPipePrefix + strings.ToUpper(strings.TrimPrefix(name, appPipePrefix))} {
		second, err := ListenPipe(duplicate)
		if err == nil {
			second.Close()
			t.Fatal("a second listener joined an existing pipe")
		}
	}

	type accepted struct {
		conn net.Conn
		err  error
	}
	acceptCh := make(chan accepted, 1)
	go func() {
		conn, err := listener.Accept()
		acceptCh <- accepted{conn, err}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := DialPipe(ctx, localPipePrefix+name)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var server net.Conn
	select {
	case result := <-acceptCh:
		if result.err != nil {
			t.Fatal(result.err)
		}
		server = result.conn
	case <-ctx.Done():
		t.Fatal("listener did not accept a current-user client")
	}
	defer server.Close()
	if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := server.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	fd, ok := server.(interface{ Fd() uintptr })
	if !ok {
		t.Fatal("winio connection does not expose its pipe handle")
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(fd.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	assertOnlyCurrentUserAccess(t, sd, "", true)

	payload := []byte("mirrorbird local pipe \x00 雀\n")
	if _, err := client.Write(payload); err != nil {
		t.Fatal(err)
	}
	received := make([]byte, len(payload))
	if _, err := io.ReadFull(server, received); err != nil {
		t.Fatal(err)
	}
	if string(received) != string(payload) {
		t.Fatal("pipe changed byte-stream payload")
	}
	if _, err := server.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(client, received[:2]); err != nil || string(received[:2]) != "ok" {
		t.Fatalf("response = %q, %v", received[:2], err)
	}
}

func TestPipeDialDeadlineWhileListenerNotAccepting(t *testing.T) {
	name := uniquePipeName(t)
	listener, err := ListenPipe(name)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	// Winio retains a disconnected first instance until Accept starts, so a
	// valid but busy local pipe exercises DialPipeContext's cancellation path.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	conn, err := DialPipe(ctx, name)
	if conn != nil {
		conn.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("busy pipe dial returned %v", err)
	}
}

func TestPipeCloseReleasesNameAndAccept(t *testing.T) {
	name := uniquePipeName(t)
	listener, err := ListenPipe(name)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	acceptDone := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if conn != nil {
			conn.Close()
		}
		acceptDone <- err
	}()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-acceptDone:
		if err == nil {
			t.Fatal("Accept succeeded after Close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not unblock Accept")
	}
	reopened, err := ListenPipe(name)
	if err != nil {
		t.Fatalf("closed listener did not release its name: %v", err)
	}
	reopened.Close()
}
