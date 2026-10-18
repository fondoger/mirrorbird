package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testID = "mirrorbird-test.usw2"
const testPort = 43127
const testRelayAlias = "jqfmk0vr"
const testOrigin = "https://" + testRelayAlias + "-43127.usw2.devtunnels.ms"

type fakeCall struct {
	start bool
	args  []string
}

type fakeRunner struct {
	mu          sync.Mutex
	calls       []fakeCall
	resolveErr  error
	onRun       func(context.Context, []string) ([]byte, error)
	onStart     func(context.Context, io.Writer, []string) (hostProcess, error)
	id          string
	labels      []string
	created     bool
	portCreated bool
	ready       bool
	process     *fakeProcess
	changed     func(string, any) any
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{id: testID, portCreated: true}
}

func (r *fakeRunner) Resolve(string) (string, error) {
	return "C:/trusted/devtunnel.exe", r.resolveErr
}

func (r *fakeRunner) Run(ctx context.Context, _ string, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, fakeCall{args: append([]string(nil), args...)})
	r.mu.Unlock()
	if r.onRun != nil {
		return r.onRun(ctx, args)
	}
	return r.normalRun(ctx, args)
}

func (r *fakeRunner) normalRun(ctx context.Context, args []string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var response any
	key := strings.Join(args[:min(2, len(args))], " ")
	switch {
	case key == "user show":
		response = map[string]any{"status": "Logged in", "provider": "microsoft", "username": "not displayed"}
	case key == "user logout":
		return nil, nil
	case args[0] == "create":
		r.id = args[1] + ".usw2"
		r.created = true
		r.portCreated = false
		r.labels = nil
		for i, arg := range args {
			if arg == "--labels" {
				if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") || strings.ContainsAny(args[i+1], " \t\r\n") || (i+2 < len(args) && !strings.HasPrefix(args[i+2], "-")) {
					return nil, errors.New("each label requires a separate --labels switch")
				}
				r.labels = append(r.labels, args[i+1])
			}
		}
		response = map[string]any{"tunnel": r.tunnel()}
	case args[0] == "show":
		response = map[string]any{"tunnel": r.tunnel()}
	case key == "port create":
		r.portCreated = true
		response = map[string]any{"port": r.port()}
	case key == "port show":
		response = map[string]any{"port": r.port()}
	case key == "port list":
		response = map[string]any{"ports": []any{map[string]any{"portNumber": testPort, "protocol": "http", "clientConnections": 0}}}
	case key == "access list":
		response = map[string]any{"accessControlEntries": []any{}}
	case args[0] == "delete":
		response = map[string]any{"deletedTunnel": args[1]}
	default:
		return nil, errors.New("unexpected fake command")
	}
	if r.changed != nil {
		response = r.changed(key, response)
	}
	return json.Marshal(response)
}

// tunnel and port require r.mu. These fixtures match live CLI output: ACLs are
// arrays, tunnel header fields disappear once a port exists, and a forwarding
// URI is absent until hosting. Port show always supplies the effective options.
func (r *fakeRunner) tunnel() map[string]any {
	t := map[string]any{
		"tunnelId": r.id, "description": "MirrorBird private companion",
		"labels": r.labels, "accessControl": []any{},
		"hostConnections": 0, "clientConnections": 0,
	}
	if r.portCreated {
		port := map[string]any{"portNumber": testPort, "protocol": "http", "headerOptionDescription": "Host:unchanged Origin:unchanged"}
		if r.ready {
			id, _ := parseID(r.id, true)
			port["portUri"] = "https://" + testRelayAlias + "-43127." + id.cluster + ".devtunnels.ms/"
		}
		t["ports"] = []any{port}
	} else {
		t["hostRequestHeader"] = "unchanged"
		t["originRequestHeader"] = "unchanged"
		t["requestTimeoutSeconds"] = 0
	}
	if r.ready {
		t["hostConnections"] = 1
	}
	return t
}

func (r *fakeRunner) port() map[string]any {
	return map[string]any{
		"tunnelId": r.id, "portNumber": testPort, "protocol": "http",
		"accessControl":     []any{},
		"hostRequestHeader": "unchanged", "originRequestHeader": "unchanged", "requestTimeoutSeconds": 0,
	}
}

func (r *fakeRunner) Start(ctx context.Context, _ string, output io.Writer, args ...string) (hostProcess, error) {
	r.mu.Lock()
	r.calls = append(r.calls, fakeCall{start: true, args: append([]string(nil), args...)})
	r.mu.Unlock()
	if r.onStart != nil {
		return r.onStart(ctx, output, args)
	}
	if len(args) >= 2 && args[0] == "user" && args[1] == "login" {
		p := &fakeProcess{ctx: ctx, exit: make(chan struct{})}
		r.mu.Lock()
		r.process = p
		r.mu.Unlock()
		_, _ = io.WriteString(output, testLoginText)
		close(p.exit)
		return p, nil
	}
	return r.normalStart(ctx, output, args, true)
}

func (r *fakeRunner) normalStart(ctx context.Context, output io.Writer, args []string, ready bool) (hostProcess, error) {
	for _, arg := range args {
		if arg == "-p" || arg == "--protocol" {
			return nil, errors.New("batch update of existing tunnel ports is not supported")
		}
	}
	p := &fakeProcess{ctx: ctx, exit: make(chan struct{}), exitErr: errors.New("secret host error output must not escape")}
	r.mu.Lock()
	r.process = p
	r.ready = ready
	id, _ := parseID(r.id, true)
	r.mu.Unlock()
	if ready {
		_, _ = fmt.Fprintf(output, "Connection to host tunnel relay restored.\r\nHosting port: %d\r\nConnect via browser: https://%s-%d.%s.devtunnels.ms\r\nInspect network activity: https://%s-%d-inspect.%s.devtunnels.ms\r\n\r\nReady to accept connections for tunnel: %s.%s\r\n", testPort, testRelayAlias, testPort, id.cluster, testRelayAlias, testPort, id.cluster, id.name, id.cluster)
	}
	return p, nil
}

func (r *fakeRunner) snapshot() []fakeCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]fakeCall(nil), r.calls...)
}

func (r *fakeRunner) count(command string) int {
	n := 0
	for _, call := range r.snapshot() {
		if call.args[0] == command {
			n++
		}
	}
	return n
}

type fakeProcess struct {
	ctx     context.Context
	exit    chan struct{}
	exitErr error
	waits   atomic.Int32
}

func (p *fakeProcess) Wait() error {
	p.waits.Add(1)
	select {
	case <-p.ctx.Done():
		return p.ctx.Err()
	case <-p.exit:
		return p.exitErr
	}
}

func testManager(t *testing.T, r *fakeRunner) *Manager {
	t.Helper()
	m := New("")
	m.runner = r
	m.startupTimeout = time.Second
	m.commandTimeout = 500 * time.Millisecond
	m.readinessTimeout = 200 * time.Millisecond
	m.cleanupTimeout = 500 * time.Millisecond
	m.loginTimeout = time.Second
	m.loginChallengeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { _ = m.Stop() })
	return m
}

func waitClosed(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("lifecycle did not complete")
	}
}

func TestExistingStartVerifiedAndStopRetainsID(t *testing.T) {
	r := newFakeRunner()
	m := testManager(t, r)
	waitClosed(t, m.Done())
	ctx, cancel := context.WithCancel(context.Background())
	state, err := m.Start(ctx, Options{TunnelID: testID, Port: testPort})
	if err != nil {
		t.Fatal(err)
	}
	if want := (State{Running: true, TunnelID: testID, URL: testOrigin, Port: testPort}); state != want || m.State() != want {
		t.Fatalf("state = %+v, manager = %+v", state, m.State())
	}
	done := m.Done()
	cancel() // Startup-only cancellation must NOT kill a ready host.
	select {
	case <-r.process.ctx.Done():
		t.Fatal("caller startup context leaked into host lifetime")
	case <-done:
		t.Fatal("host exited when startup context canceled")
	default:
	}
	calls := r.snapshot()
	var hostIndex = -1
	for i, call := range calls {
		if call.start {
			hostIndex = i
			want := []string{"host", testID, "--host-header", "unchanged", "--origin-header", "unchanged"}
			if !reflect.DeepEqual(call.args, want) {
				t.Fatalf("unsafe host args: %v", call.args)
			}
		}
	}
	if hostIndex != 6 || len(calls) != 12 {
		t.Fatalf("must verify account plus five checks before host and five after: %+v", calls)
	}
	if err := m.Stop(); err != nil {
		t.Fatal(err)
	}
	waitClosed(t, done)
	if got := m.State(); got.Running || got.URL != "" || got.TunnelID != testID || got.Port != testPort {
		t.Fatalf("stopped state = %+v", got)
	}
	if r.process.waits.Load() != 1 || r.count("delete") != 0 || r.count("create") != 0 {
		t.Fatalf("waits=%d, calls=%+v", r.process.waits.Load(), r.snapshot())
	}
}

func TestCreateUsesPrivateDedicatedResourceAndRetainsIt(t *testing.T) {
	r := newFakeRunner()
	m := testManager(t, r)
	state, err := m.Start(context.Background(), Options{Port: testPort})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(state.TunnelID, "mirrorbird-") || state.TunnelID == testID {
		t.Fatalf("not a dedicated tunnel: %+v", state)
	}
	var create, portCreate []string
	for _, call := range r.snapshot() {
		if call.args[0] == "create" {
			create = call.args
		}
		if len(call.args) > 1 && call.args[0] == "port" && call.args[1] == "create" {
			portCreate = call.args
		}
		for _, arg := range call.args {
			for _, unsafe := range []string{"--allow-anonymous", "--repo", "--access-token", "--sp-secret", "--verbose", "--use-device-code-auth"} {
				if arg == unsafe {
					t.Fatalf("unsafe flag %q", arg)
				}
			}
		}
	}
	if len(create) == 0 || !strings.Contains(strings.Join(create, " "), "--labels mirrorbird-private --labels mb-owner-") || !strings.Contains(strings.Join(create, " "), "--request-timeout 0 -j") {
		t.Fatalf("creation flags = %v", create)
	}
	wantPort := []string{"port", "create", state.TunnelID, "-p", "43127", "--protocol", "http", "--host-header", "unchanged", "--origin-header", "unchanged", "--request-timeout", "0", "-j"}
	if !reflect.DeepEqual(portCreate, wantPort) {
		t.Fatalf("port flags = %v", portCreate)
	}
	_ = m.Stop()
	if r.count("delete") != 0 || m.State().TunnelID != state.TunnelID || r.process.waits.Load() != 1 {
		t.Fatal("Stop deleted or forgot a reusable private tunnel")
	}
}

func TestStartupValidationFailuresDoNotHostOrChangeExisting(t *testing.T) {
	cases := []struct {
		name   string
		change func(string, any) any
	}{
		{"wrong resource", func(key string, value any) any {
			if strings.HasPrefix(key, "show ") {
				value.(map[string]any)["tunnel"].(map[string]any)["tunnelId"] = "other-resource.usw2"
			}
			return value
		}},
		{"tunnel custom ACL", func(key string, value any) any {
			if strings.HasPrefix(key, "show ") {
				value.(map[string]any)["tunnel"].(map[string]any)["accessControl"] = []any{map[string]any{"type": "Anonymous", "scopes": []string{"connect"}}}
			}
			return value
		}},
		{"port custom ACL", func(key string, value any) any {
			if key == "port show" {
				value.(map[string]any)["port"].(map[string]any)["accessControl"] = []any{map[string]any{"type": "Repositories"}}
			}
			return value
		}},
		{"extra port", func(key string, value any) any {
			if key == "port list" {
				v := value.(map[string]any)
				v["ports"] = append(v["ports"].([]any), map[string]any{"portNumber": 22, "protocol": "ssh"})
			}
			return value
		}},
		{"rewritten host", func(key string, value any) any {
			if key == "port show" {
				value.(map[string]any)["port"].(map[string]any)["hostRequestHeader"] = "localhost"
			}
			return value
		}},
		{"rewritten origin", func(key string, value any) any {
			if key == "port show" {
				value.(map[string]any)["port"].(map[string]any)["originRequestHeader"] = "http://localhost"
			}
			return value
		}},
		{"wrong timeout", func(key string, value any) any {
			if key == "port show" {
				value.(map[string]any)["port"].(map[string]any)["requestTimeoutSeconds"] = 100
			}
			return value
		}},
		{"existing host", func(key string, value any) any {
			if strings.HasPrefix(key, "show ") {
				value.(map[string]any)["tunnel"].(map[string]any)["hostConnections"] = 1
			}
			return value
		}},
		{"unknown field", func(key string, value any) any {
			if strings.HasPrefix(key, "show ") {
				value.(map[string]any)["tunnel"].(map[string]any)["newAccessModel"] = true
			}
			return value
		}},
		{"access list missing", func(key string, value any) any {
			if key == "access list" {
				return map[string]any{}
			}
			return value
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newFakeRunner()
			r.changed = tc.change
			m := testManager(t, r)
			if state, err := m.Start(context.Background(), Options{TunnelID: testID, Port: testPort}); !errors.Is(err, ErrUnverified) || state.Running {
				t.Fatalf("state=%+v err=%v", state, err)
			}
			if r.count("host") != 0 || r.count("delete") != 0 || r.count("create") != 0 {
				t.Fatalf("modified unsafe existing resource: %+v", r.snapshot())
			}
			waitClosed(t, m.Done())
		})
	}
}

func TestReadyOriginMustEqualAuthenticatedResourceReadback(t *testing.T) {
	for _, advertised := range []string{
		"https://another-service-alias-43127.usw2.devtunnels.ms",
		"https://mirrorbird-test-43127.usw2.devtunnels.ms", // friendly ID is not authoritative either
		"https://jqfmk0vr-43128.usw2.devtunnels.ms",
		"https://jqfmk0vr-43127.asse.devtunnels.ms",
		"https://jqfmk0vr-43127-inspect.usw2.devtunnels.ms",
	} {
		t.Run(advertised, func(t *testing.T) {
			r := newFakeRunner()
			r.onStart = func(ctx context.Context, output io.Writer, args []string) (hostProcess, error) {
				process, err := r.normalStart(ctx, io.Discard, args, true)
				_, _ = fmt.Fprintf(output, "Hosting port: 43127\nConnect via browser: %s\n\nReady to accept connections for tunnel: %s\n", advertised, testID)
				return process, err
			}
			m := testManager(t, r)
			state, err := m.Start(context.Background(), Options{TunnelID: testID, Port: testPort})
			if !errors.Is(err, ErrUnverified) || state.Running || m.State().Running || r.process.waits.Load() != 1 || r.count("delete") != 0 {
				t.Fatalf("unbound origin accepted: state=%+v err=%v", state, err)
			}
		})
	}
}

func TestPostReadyResourceCannotChangeIdentityWithMatchingAlias(t *testing.T) {
	r := newFakeRunner()
	r.changed = func(key string, response any) any {
		if r.ready && strings.HasPrefix(key, "show ") {
			response.(map[string]any)["tunnel"].(map[string]any)["tunnelId"] = "other-resource.usw2"
		}
		return response
	}
	m := testManager(t, r)
	state, err := m.Start(context.Background(), Options{TunnelID: testID, Port: testPort})
	if !errors.Is(err, ErrUnverified) || state.Running || m.State().Running || r.process.waits.Load() != 1 {
		t.Fatalf("wrong resource with matching alias accepted: %+v %v", state, err)
	}
}

func TestFailureAfterReadyRevokesAndRollsBackOnlyCreated(t *testing.T) {
	for _, create := range []bool{false, true} {
		t.Run(fmt.Sprint(create), func(t *testing.T) {
			r := newFakeRunner()
			r.changed = func(key string, value any) any {
				if key == "access list" && r.ready {
					return map[string]any{"accessControlEntries": []any{map[string]any{"type": "Anonymous"}}}
				}
				return value
			}
			m := testManager(t, r)
			id := testID
			if create {
				id = ""
			}
			state, err := m.Start(context.Background(), Options{TunnelID: id, Port: testPort})
			if !errors.Is(err, ErrUnverified) || state.Running || m.State().Running {
				t.Fatalf("state=%+v err=%v", state, err)
			}
			wantDeletes := 0
			if create {
				wantDeletes = 1
			}
			if r.count("delete") != wantDeletes || r.process.waits.Load() != 1 {
				t.Fatalf("deletes=%d waits=%d", r.count("delete"), r.process.waits.Load())
			}
			_ = m.Stop()
			if r.count("delete") != wantDeletes || r.process.waits.Load() != 1 {
				t.Fatal("cleanup repeated")
			}
		})
	}
}

func TestUnreadyHostIsCanceledAndReaped(t *testing.T) {
	r := newFakeRunner()
	r.onStart = func(ctx context.Context, output io.Writer, args []string) (hostProcess, error) {
		// Reporting a port/URL without the final readiness statement is NOT ready.
		_, _ = io.WriteString(output, "Hosting port: 43127\nConnect via browser: "+testOrigin+"/\n")
		return r.normalStart(ctx, output, args, false)
	}
	m := testManager(t, r)
	m.readinessTimeout = 15 * time.Millisecond
	_, err := m.Start(context.Background(), Options{TunnelID: testID, Port: testPort})
	if !errors.Is(err, ErrNotReady) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v", err)
	}
	if r.process.waits.Load() != 1 || r.process.ctx.Err() == nil || r.count("delete") != 0 {
		t.Fatal("unready host was not safely reaped")
	}
}

func TestConcurrentStartStopDuringStartup(t *testing.T) {
	r := newFakeRunner()
	entered, release := make(chan struct{}), make(chan struct{})
	r.onRun = func(ctx context.Context, args []string) ([]byte, error) {
		if args[0] == "show" {
			close(entered)
			<-ctx.Done()
			<-release
			return nil, ctx.Err()
		}
		return r.normalRun(ctx, args)
	}
	m := testManager(t, r)
	result := make(chan error, 1)
	go func() {
		_, err := m.Start(context.Background(), Options{TunnelID: testID, Port: testPort})
		result <- err
	}()
	waitClosed(t, entered)
	done := m.Done()
	if _, err := m.Start(context.Background(), Options{TunnelID: testID, Port: testPort}); !errors.Is(err, ErrBusy) {
		t.Fatalf("second start=%v", err)
	}
	const n = 16
	var stopped sync.WaitGroup
	stopped.Add(n)
	for range n {
		go func() { defer stopped.Done(); _ = m.Stop() }()
	}
	close(release)
	stopped.Wait()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("startup error=%v", err)
	}
	waitClosed(t, done)
	if r.count("host") != 0 || r.count("delete") != 0 {
		t.Fatal("canceled startup hosted or deleted existing tunnel")
	}
}

func TestConcurrentStopReapsExactlyOnce(t *testing.T) {
	r := newFakeRunner()
	m := testManager(t, r)
	if _, err := m.Start(context.Background(), Options{TunnelID: testID, Port: testPort}); err != nil {
		t.Fatal(err)
	}
	done := m.Done()
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() { defer wg.Done(); _ = m.Stop() }()
	}
	wg.Wait()
	waitClosed(t, done)
	if r.process.waits.Load() != 1 || r.count("delete") != 0 || m.State().Running {
		t.Fatal("concurrent Stop violated lifecycle ownership")
	}
}

func TestUnexpectedExitClosesDoneAndRevokesState(t *testing.T) {
	r := newFakeRunner()
	m := testManager(t, r)
	if _, err := m.Start(context.Background(), Options{TunnelID: testID, Port: testPort}); err != nil {
		t.Fatal(err)
	}
	done := m.Done()
	close(r.process.exit)
	waitClosed(t, done)
	if s := m.State(); s.Running || s.URL != "" || s.TunnelID != testID {
		t.Fatalf("unsafe exited state=%+v", s)
	}
	if r.process.waits.Load() != 1 || r.count("delete") != 0 {
		t.Fatal("exit double-waited or deleted reusable resource")
	}
}

func TestCanceledCreateCleanupUsesFreshContextAndProof(t *testing.T) {
	r := newFakeRunner()
	portEntered := make(chan struct{})
	r.onRun = func(ctx context.Context, args []string) ([]byte, error) {
		if args[0] == "port" && args[1] == "create" {
			close(portEntered)
			<-ctx.Done()
			return nil, errors.New("token: do not print")
		}
		return r.normalRun(ctx, args)
	}
	m := testManager(t, r)
	result := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _, err := m.Start(ctx, Options{Port: testPort}); result <- err }()
	waitClosed(t, portEntered)
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "token") {
		t.Fatalf("error=%v", err)
	}
	if r.count("delete") != 1 || r.count("host") != 0 {
		t.Fatalf("cleanup calls=%+v", r.snapshot())
	}
}

func TestFailedCreateNeverDeletesUnrelatedResource(t *testing.T) {
	for _, mismatch := range []string{"id", "label"} {
		t.Run(mismatch, func(t *testing.T) {
			r := newFakeRunner()
			r.onRun = func(ctx context.Context, args []string) ([]byte, error) {
				if args[0] == "create" {
					_, _ = r.normalRun(ctx, args)
					return nil, errors.New("sensitive CLI failure")
				}
				return r.normalRun(ctx, args)
			}
			r.changed = func(key string, value any) any {
				if strings.HasPrefix(key, "show ") {
					t := value.(map[string]any)["tunnel"].(map[string]any)
					if mismatch == "id" {
						t["tunnelId"] = testID
					} else {
						t["labels"] = []any{privateLabel}
					}
				}
				return value
			}
			m := testManager(t, r)
			_, err := m.Start(context.Background(), Options{Port: testPort})
			if !errors.Is(err, ErrCommand) || errors.Is(err, ErrCleanup) || strings.Contains(err.Error(), "sensitive") {
				t.Fatalf("error=%v", err)
			}
			if r.count("delete") != 0 || r.count("host") != 0 {
				t.Fatal("deleted an unproven resource")
			}
		})
	}
}

func TestCleanupNeverDeletesMismatchedIDOrMissingProof(t *testing.T) {
	for _, mismatch := range []string{"id", "label"} {
		t.Run(mismatch, func(t *testing.T) {
			r := newFakeRunner()
			r.onStart = func(context.Context, io.Writer, []string) (hostProcess, error) {
				return nil, errors.New("sensitive host startup failure")
			}
			r.changed = func(key string, value any) any {
				// At cleanup, Start has already attempted host (normal verify
				// completed); only then corrupt the ownership evidence.
				attemptedHost := false
				for _, call := range r.calls {
					attemptedHost = attemptedHost || call.start
				}
				if attemptedHost && strings.HasPrefix(key, "show ") {
					resource := value.(map[string]any)["tunnel"].(map[string]any)
					if mismatch == "id" {
						resource["tunnelId"] = testID
					} else {
						resource["labels"] = []any{privateLabel}
					}
				}
				return value
			}
			m := testManager(t, r)
			_, err := m.Start(context.Background(), Options{Port: testPort})
			if !errors.Is(err, ErrCommand) || !errors.Is(err, ErrCleanup) || strings.Contains(err.Error(), "sensitive") || r.count("delete") != 0 {
				t.Fatalf("err=%v calls=%+v", err, r.snapshot())
			}
		})
	}
}

func TestCancellationWhileWaitingForHost(t *testing.T) {
	r := newFakeRunner()
	hostStarted := make(chan struct{})
	r.onStart = func(ctx context.Context, output io.Writer, args []string) (hostProcess, error) {
		p, err := r.normalStart(ctx, output, args, false)
		close(hostStarted)
		return p, err
	}
	m := testManager(t, r)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := m.Start(ctx, Options{TunnelID: testID, Port: testPort}); result <- err }()
	waitClosed(t, hostStarted)
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	if r.process.ctx.Err() == nil || r.process.waits.Load() != 1 || r.count("delete") != 0 {
		t.Fatal("startup cancellation did not reap host safely")
	}
}

func TestImmediateHostExitCannotPublishRunningState(t *testing.T) {
	r := newFakeRunner()
	r.onStart = func(ctx context.Context, output io.Writer, args []string) (hostProcess, error) {
		p, err := r.normalStart(ctx, output, args, false)
		close(r.process.exit)
		return p, err
	}
	m := testManager(t, r)
	_, err := m.Start(context.Background(), Options{TunnelID: testID, Port: testPort})
	if !errors.Is(err, ErrHostExited) || m.State().Running || r.process.waits.Load() != 1 {
		t.Fatalf("err=%v state=%+v waits=%d", err, m.State(), r.process.waits.Load())
	}
}

func TestAccountOutputAndCommandFailuresAreRedacted(t *testing.T) {
	for _, reply := range []string{"command", "json", "limit"} {
		t.Run(reply, func(t *testing.T) {
			r := newFakeRunner()
			r.onRun = func(context.Context, []string) ([]byte, error) {
				switch reply {
				case "command":
					return []byte("secret CLI token"), errors.New("secret command failure")
				case "json":
					return []byte(`{"status":"Logged in","provider":"microsoft","error":"secret"}`), nil
				default:
					return []byte(strings.Repeat("secret", maxOutput/6+1)), nil
				}
			}
			m := testManager(t, r)
			account, err := m.Account(context.Background())
			if err == nil || account.LoggedIn || strings.Contains(err.Error(), "secret") {
				t.Fatalf("account=%+v err=%v", account, err)
			}
		})
	}
}

func TestStartAfterStopUsesFreshDoneAndReverifies(t *testing.T) {
	r := newFakeRunner()
	m := testManager(t, r)
	if _, err := m.Start(context.Background(), Options{TunnelID: testID, Port: testPort}); err != nil {
		t.Fatal(err)
	}
	oldDone, oldProcess := m.Done(), r.process
	_ = m.Stop()
	r.mu.Lock()
	r.ready = false
	r.mu.Unlock()
	if _, err := m.Start(context.Background(), Options{TunnelID: testID, Port: testPort}); err != nil {
		t.Fatal(err)
	}
	if m.Done() == oldDone || r.count("host") != 2 || oldProcess.waits.Load() != 1 || r.count("delete") != 0 {
		t.Fatal("second lifecycle reused channels/process or skipped verification")
	}
	waitClosed(t, oldDone)
}

func TestPortScopedAccessListCannotHideBehindTunnelDefault(t *testing.T) {
	r := newFakeRunner()
	r.onRun = func(ctx context.Context, args []string) ([]byte, error) {
		if len(args) > 4 && args[0] == "access" && args[1] == "list" && args[3] == "-p" {
			return []byte(`{"accessControlEntries":[{"type":"Anonymous","isInherited":false,"scopes":["connect"]}]}`), nil
		}
		return r.normalRun(ctx, args)
	}
	m := testManager(t, r)
	_, err := m.Start(context.Background(), Options{TunnelID: testID, Port: testPort})
	if !errors.Is(err, ErrUnverified) || r.count("host") != 0 || r.count("delete") != 0 {
		t.Fatalf("err=%v calls=%+v", err, r.snapshot())
	}
}

func TestExtraPortAfterReadinessFailsClosed(t *testing.T) {
	r := newFakeRunner()
	r.changed = func(key string, value any) any {
		if key == "port list" && r.ready {
			v := value.(map[string]any)
			v["ports"] = append(v["ports"].([]any), map[string]any{"portNumber": 22, "protocol": "ssh"})
		}
		return value
	}
	m := testManager(t, r)
	_, err := m.Start(context.Background(), Options{TunnelID: testID, Port: testPort})
	if !errors.Is(err, ErrUnverified) || m.State().Running || r.process.waits.Load() != 1 || r.count("delete") != 0 {
		t.Fatalf("err=%v state=%+v", err, m.State())
	}
}

func TestCleanupFailureIsBoundedRedactedAndNotRetried(t *testing.T) {
	r := newFakeRunner()
	r.onStart = func(context.Context, io.Writer, []string) (hostProcess, error) {
		return nil, errors.New("sensitive start output")
	}
	r.onRun = func(ctx context.Context, args []string) ([]byte, error) {
		if args[0] == "delete" {
			<-ctx.Done()
			return nil, errors.New("secret cleanup output")
		}
		return r.normalRun(ctx, args)
	}
	m := testManager(t, r)
	m.cleanupTimeout = 10 * time.Millisecond
	_, err := m.Start(context.Background(), Options{Port: testPort})
	if !errors.Is(err, ErrCleanup) || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("error=%v", err)
	}
	_ = m.Stop()
	if r.count("delete") != 1 {
		t.Fatal("cleanup retried destructive operation")
	}
}

func TestAccountMutationBlocksConcurrentStart(t *testing.T) {
	r := newFakeRunner()
	entered := make(chan struct{})
	r.onStart = func(ctx context.Context, output io.Writer, args []string) (hostProcess, error) {
		p, err := r.normalStart(ctx, output, args, false)
		close(entered)
		return p, err
	}
	m := testManager(t, r)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- m.Login(ctx) }()
	waitClosed(t, entered)
	if _, err := m.Start(context.Background(), Options{Port: testPort}); !errors.Is(err, ErrBusy) {
		t.Fatalf("start during login=%v", err)
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("login=%v", err)
	}
}

func TestVerificationCommandFailureStopsStartup(t *testing.T) {
	for _, failAt := range []int{2, 3, 4, 5, 6, 8, 9, 10, 11, 12} {
		t.Run(fmt.Sprint(failAt), func(t *testing.T) {
			r := newFakeRunner()
			r.onRun = func(ctx context.Context, args []string) ([]byte, error) {
				if len(r.snapshot()) == failAt {
					return []byte("secret raw output"), errors.New("secret failure")
				}
				return r.normalRun(ctx, args)
			}
			m := testManager(t, r)
			_, err := m.Start(context.Background(), Options{TunnelID: testID, Port: testPort})
			if !errors.Is(err, ErrCommand) || strings.Contains(err.Error(), "secret") || m.State().Running || r.count("delete") != 0 {
				t.Fatalf("err=%v calls=%+v", err, r.snapshot())
			}
			if r.process != nil && r.process.waits.Load() != 1 {
				t.Fatal("verification failure did not reap host")
			}
		})
	}
}

func TestCanceledCreateWithoutConfirmedOutputCanOnlyCleanOwnMarker(t *testing.T) {
	r := newFakeRunner()
	created := make(chan struct{})
	r.onRun = func(ctx context.Context, args []string) ([]byte, error) {
		if args[0] == "create" {
			_, _ = r.normalRun(ctx, args) // server created; reply was lost
			close(created)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return r.normalRun(ctx, args)
	}
	m := testManager(t, r)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := m.Start(ctx, Options{Port: testPort}); result <- err }()
	waitClosed(t, created)
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) || errors.Is(err, ErrCleanup) {
		t.Fatalf("error=%v", err)
	}
	if r.count("delete") != 1 || r.count("host") != 0 {
		t.Fatalf("unconfirmed creation cleanup=%+v", r.snapshot())
	}
}

func TestPreCanceledStartDoesNoWork(t *testing.T) {
	r := newFakeRunner()
	m := testManager(t, r)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Start(ctx, Options{Port: testPort}); !errors.Is(err, context.Canceled) || len(r.snapshot()) != 0 {
		t.Fatalf("err=%v calls=%+v", err, r.snapshot())
	}
}

func TestHostStartFailureRollsBackCreationOnly(t *testing.T) {
	r := newFakeRunner()
	r.onStart = func(context.Context, io.Writer, []string) (hostProcess, error) {
		return nil, errors.New("secret startup error")
	}
	m := testManager(t, r)
	_, err := m.Start(context.Background(), Options{Port: testPort})
	if !errors.Is(err, ErrCommand) || strings.Contains(err.Error(), "secret") || r.count("delete") != 1 {
		t.Fatalf("error=%v calls=%+v", err, r.snapshot())
	}
}

func TestAccountLoginLogoutAndBusyGuards(t *testing.T) {
	r := newFakeRunner()
	m := testManager(t, r)
	account, err := m.Account(context.Background())
	if err != nil || !account.LoggedIn || account.Name != "" {
		t.Fatalf("account=%+v err=%v", account, err)
	}
	if err := m.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.Logout(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, call := range r.snapshot() {
		if call.args[0] == "user" && call.args[1] == "login" {
			want := []string{"user", "login", "--use-device-code-auth", "--entra"}
			if !call.start || !reflect.DeepEqual(call.args, want) {
				t.Fatalf("login must stream device auth: %+v", call)
			}
		}
	}
	if _, err := m.Start(context.Background(), Options{TunnelID: testID, Port: testPort}); err != nil {
		t.Fatal(err)
	}
	if err := m.Logout(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("logout while hosting=%v", err)
	}
	if err := m.Login(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("login while hosting=%v", err)
	}
}

func TestNotLoggedInCannotCreateOrHost(t *testing.T) {
	r := newFakeRunner()
	r.changed = func(key string, value any) any {
		if key == "user show" {
			return map[string]any{"status": "Not logged in"}
		}
		return value
	}
	m := testManager(t, r)
	_, err := m.Start(context.Background(), Options{Port: testPort})
	if !errors.Is(err, ErrNotLoggedIn) || len(r.snapshot()) != 1 {
		t.Fatalf("err=%v calls=%+v", err, r.snapshot())
	}
}

func TestInvalidOptionsNeverReachCLI(t *testing.T) {
	for _, opts := range []Options{
		{Port: 0}, {Port: -1}, {Port: 65536},
		{TunnelID: "--allow-anonymous", Port: testPort}, {TunnelID: "x y", Port: testPort},
		{TunnelID: "https://somewhere", Port: testPort}, {TunnelID: "name.usw2.extra", Port: testPort},
		{TunnelID: "ab", Port: testPort}, {TunnelID: "name;rm", Port: testPort},
	} {
		r := newFakeRunner()
		m := testManager(t, r)
		if _, err := m.Start(context.Background(), opts); !errors.Is(err, ErrOptions) || len(r.snapshot()) != 0 {
			t.Fatalf("opts=%+v err=%v calls=%+v", opts, err, r.snapshot())
		}
	}
}

func TestResolveErrorIsRedacted(t *testing.T) {
	r := newFakeRunner()
	r.resolveErr = errors.New("private path /token")
	m := testManager(t, r)
	if _, err := m.Account(context.Background()); !errors.Is(err, ErrExecutable) || strings.Contains(err.Error(), "/token") {
		t.Fatalf("account error=%v", err)
	}
	if _, err := m.Start(context.Background(), Options{Port: testPort}); !errors.Is(err, ErrExecutable) || len(r.snapshot()) != 0 {
		t.Fatalf("start error=%v", err)
	}
}

func TestLoginStreamsBeforeExitAndKeepsHostContextIndependent(t *testing.T) {
	r := newFakeRunner()
	var process *fakeProcess
	r.onStart = func(ctx context.Context, output io.Writer, args []string) (hostProcess, error) {
		process = &fakeProcess{ctx: ctx, exit: make(chan struct{})}
		_, _ = io.WriteString(output, "secret diagnostic, never publish\n")
		for i := range len(testLoginText) {
			_, _ = io.WriteString(output, testLoginText[i:i+1])
		}
		_, _ = io.WriteString(output, testLoginText+strings.ReplaceAll(testLoginText, testLoginCode, "OTHER1234"))
		return process, nil
	}
	m := testManager(t, r)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	prompted := make(chan LoginChallenge, 2)
	result := make(chan error, 1)
	var notifications atomic.Int32
	go func() {
		result <- m.LoginWithProgress(ctx, func(challenge LoginChallenge) {
			notifications.Add(1)
			if !m.mu.TryLock() {
				t.Error("callback ran under Manager mutex")
			} else {
				m.mu.Unlock()
				if m.State() != (State{}) {
					t.Error("login modified host state")
				}
			}
			prompted <- challenge
		})
	}()
	select {
	case challenge := <-prompted:
		if challenge != (LoginChallenge{URL: testLoginURL, Code: testLoginCode}) {
			t.Fatal("callback did not receive the expected typed challenge")
		}
	case <-time.After(time.Second):
		t.Fatal("challenge was withheld until process exit")
	}
	if _, err := m.Account(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("account during login=%v", err)
	}
	if err := m.Login(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("second login=%v", err)
	}
	if err := m.Logout(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("logout during login=%v", err)
	}
	if _, err := m.Start(context.Background(), Options{Port: testPort}); !errors.Is(err, ErrBusy) {
		t.Fatalf("host during login=%v", err)
	}
	if calls := r.snapshot(); len(calls) != 1 || !calls[0].start || !reflect.DeepEqual(calls[0].args, []string{"user", "login", "--use-device-code-auth", "--entra"}) {
		t.Fatalf("login must stream only the Entra device-code command: %+v", calls)
	}
	select {
	case <-result:
		t.Fatal("login completed before CLI exit")
	default:
	}
	close(process.exit)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if process.waits.Load() != 1 || notifications.Load() != 1 || process.ctx.Err() == nil {
		t.Fatal("login did not reap once, cancel its context, and notify once")
	}
	if calls := r.snapshot(); len(calls) != 2 || calls[1].start || !reflect.DeepEqual(calls[1].args, []string{"user", "show", "-j"}) {
		t.Fatal("successful login was not independently confirmed")
	}

	r.onStart = nil
	if _, err := m.Start(context.Background(), Options{TunnelID: testID, Port: testPort}); err != nil {
		t.Fatal(err)
	}
	cancel() // Login cancellation must never become the new host's lifetime.
	if r.process.ctx.Err() != nil || !m.State().Running {
		t.Fatal("login context leaked into host lifetime")
	}
	if err := m.LoginWithProgress(context.Background(), func(LoginChallenge) { t.Error("prompt while hosting") }); !errors.Is(err, ErrBusy) {
		t.Fatalf("login while hosting=%v", err)
	}
}

func TestLoginNoChallengeDeadlineReapsWithoutRawLeaks(t *testing.T) {
	for name, text := range map[string]string{
		"silent":        "",
		"diagnostics":   "secret token output\n",
		"malformed":     strings.ReplaceAll(testLoginText, testLoginCode, "EXAM-1234"),
		"non Microsoft": strings.ReplaceAll(testLoginText, testLoginURL, "https://evil.example/device?secret"),
		"GitHub":        "Browse to https://github.com/login/device and enter code " + testLoginCode + "\n",
		"partial":       "To sign in, use a web browser to open the page ",
		"line limit":    strings.Repeat("x", maxLoginLine+1),
		"total limit":   strings.Repeat("trace\n", maxOutput/6+1),
	} {
		t.Run(name, func(t *testing.T) {
			r := newFakeRunner()
			var process *fakeProcess
			r.onStart = func(ctx context.Context, output io.Writer, _ []string) (hostProcess, error) {
				process = &fakeProcess{ctx: ctx, exit: make(chan struct{})}
				_, _ = io.WriteString(output, text)
				return process, nil
			}
			m := testManager(t, r)
			m.loginChallengeTimeout = 10 * time.Millisecond
			err := m.LoginWithProgress(context.Background(), func(LoginChallenge) { t.Error("unsafe challenge escaped") })
			if !errors.Is(err, ErrLoginNoChallenge) || errors.Is(err, ErrLoginTimeout) {
				t.Fatalf("error=%v", err)
			}
			assertLoginErrorRedacted(t, err)
			if process.waits.Load() != 1 || process.ctx.Err() == nil || len(r.snapshot()) != 1 {
				t.Fatal("no-challenge failure did not cancel/reap only the login child")
			}
			if _, err := m.Account(context.Background()); err != nil {
				t.Fatalf("credentialsBusy was not released: %v", err)
			}
		})
	}
}

func TestLoginTotalAndCallerDeadlinesAfterChallenge(t *testing.T) {
	for _, callerDeadline := range []bool{false, true} {
		t.Run(fmt.Sprint(callerDeadline), func(t *testing.T) {
			r := newFakeRunner()
			var process *fakeProcess
			r.onStart = func(ctx context.Context, output io.Writer, _ []string) (hostProcess, error) {
				process = &fakeProcess{ctx: ctx, exit: make(chan struct{})}
				_, _ = io.WriteString(output, testLoginText)
				return process, nil
			}
			m := testManager(t, r)
			m.loginChallengeTimeout = 5 * time.Millisecond
			ctx := context.Background()
			if callerDeadline {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 30*time.Millisecond)
				defer cancel()
			} else {
				m.loginTimeout = 30 * time.Millisecond
			}
			count := 0
			err := m.LoginWithProgress(ctx, func(LoginChallenge) { count++ })
			if !errors.Is(err, ErrLoginTimeout) || !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrLoginNoChallenge) || count != 1 {
				t.Fatalf("timeout=%v notifications=%d", err, count)
			}
			assertLoginErrorRedacted(t, err)
			if process.waits.Load() != 1 || process.ctx.Err() == nil || len(r.snapshot()) != 1 {
				t.Fatal("timeout did not reap once or incorrectly confirmed the account")
			}
		})
	}
}

type loginWaitProcess struct {
	wait  func() error
	waits atomic.Int32
}

func (p *loginWaitProcess) Wait() error {
	p.waits.Add(1)
	return p.wait()
}

func TestLoginCancellationKeepsCredentialsBusyUntilReaped(t *testing.T) {
	for _, showPrompt := range []bool{false, true} {
		t.Run(fmt.Sprint(showPrompt), func(t *testing.T) {
			r := newFakeRunner()
			started, canceled, reaped := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var release sync.Once
			defer release.Do(func() { close(reaped) })
			process := &loginWaitProcess{}
			r.onStart = func(ctx context.Context, output io.Writer, _ []string) (hostProcess, error) {
				process.wait = func() error {
					<-ctx.Done()
					close(canceled)
					<-reaped
					return errors.New("secret CLI failure " + testLoginCode)
				}
				if showPrompt {
					_, _ = io.WriteString(output, testLoginText)
				}
				close(started)
				return process, nil
			}
			m := testManager(t, r)
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			prompted := make(chan struct{}, 1)
			result := make(chan error, 1)
			go func() { result <- m.LoginWithProgress(ctx, func(LoginChallenge) { prompted <- struct{}{} }) }()
			waitClosed(t, started)
			if showPrompt {
				select {
				case <-prompted:
				case <-time.After(time.Second):
					t.Fatal("missing prompt")
				}
			}
			cancel(errors.New("secret caller cause " + testLoginCode))
			waitClosed(t, canceled)
			select {
			case <-result:
				t.Fatal("login returned before Wait reaped the child")
			default:
			}
			if _, err := m.Account(context.Background()); !errors.Is(err, ErrBusy) {
				t.Fatal("credentials guard released before reap")
			}
			release.Do(func() { close(reaped) })
			err := <-result
			if !errors.Is(err, context.Canceled) || process.waits.Load() != 1 || len(r.snapshot()) != 1 {
				t.Fatalf("cancellation result=%v waits=%d", err, process.waits.Load())
			}
			assertLoginErrorRedacted(t, err)
			if _, err := m.Account(context.Background()); err != nil {
				t.Fatalf("credentials guard not released after reap: %v", err)
			}
		})
	}
}

func TestLoginFailuresAndCachedSuccessRequireAccountConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name string
		want error
	}{
		{"resolve", ErrExecutable}, {"start", ErrCommand}, {"exit", ErrCommand},
		{"account command", ErrCommand}, {"account JSON", ErrUnverified},
		{"not logged in", ErrNotLoggedIn}, {"cached success", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newFakeRunner()
			var process *fakeProcess
			if tc.name == "resolve" {
				r.resolveErr = errors.New("secret executable path " + testLoginCode)
			}
			r.onStart = func(ctx context.Context, output io.Writer, _ []string) (hostProcess, error) {
				_, _ = io.WriteString(output, "secret raw output "+testLoginCode+"\n")
				if tc.name == "start" {
					return nil, errors.New("secret start error " + testLoginCode)
				}
				process = &fakeProcess{ctx: ctx, exit: make(chan struct{})}
				if tc.name == "exit" {
					process.exitErr = errors.New("secret exit error " + testLoginCode)
				}
				close(process.exit)
				return process, nil
			}
			r.onRun = func(ctx context.Context, args []string) ([]byte, error) {
				switch tc.name {
				case "account command":
					return []byte("secret output " + testLoginCode), errors.New("secret account error")
				case "account JSON":
					return []byte(`{"status":"Logged in","provider":"microsoft","token":"secret"}`), nil
				case "not logged in":
					return []byte(`{"status":"Not logged in"}`), nil
				default:
					return r.normalRun(ctx, args)
				}
			}
			m := testManager(t, r)
			err := m.LoginWithProgress(context.Background(), func(LoginChallenge) { t.Error("raw output became a challenge") })
			if !errors.Is(err, tc.want) {
				t.Fatalf("error=%v want=%v", err, tc.want)
			}
			assertLoginErrorRedacted(t, err)
			if process != nil && process.waits.Load() != 1 {
				t.Fatal("login child not reaped once")
			}
			wantCalls := 2
			if tc.name == "resolve" {
				wantCalls = 0
			} else if tc.name == "start" || tc.name == "exit" {
				wantCalls = 1
			}
			if len(r.snapshot()) != wantCalls {
				t.Fatal("account was checked before success or not checked after success")
			}
		})
	}
}

func TestPreCanceledLoginDoesNoWork(t *testing.T) {
	r := newFakeRunner()
	m := testManager(t, r)
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errors.New("secret caller cause " + testLoginCode))
	err := m.LoginWithProgress(ctx, func(LoginChallenge) { t.Error("callback after pre-cancellation") })
	if !errors.Is(err, context.Canceled) || len(r.snapshot()) != 0 {
		t.Fatalf("error=%v calls=%+v", err, r.snapshot())
	}
	assertLoginErrorRedacted(t, err)
}

func assertLoginErrorRedacted(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	for _, secret := range []string{testLoginCode, "secret", "evil.example", "https://"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatal("login error exposed raw output or credentials")
		}
	}
}
