// Package tunnel runs the locally installed Microsoft Dev Tunnels CLI. It never
// accepts credentials, enables anonymous access, or intentionally forwards more
// than one port. Only the service's default owner-only ACL is supported: custom
// entries, even an apparent Users/owner grant, are conservatively rejected. A
// missing or unfamiliar CLI response is never assumed to mean private access.
package tunnel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"sync"
	"time"
)

var (
	ErrExecutable       = errors.New("a trusted, locally installed devtunnel executable is required")
	ErrOptions          = errors.New("a valid tunnel ID and local HTTP port are required")
	ErrBusy             = errors.New("another Dev Tunnels operation is active")
	ErrCommand          = errors.New("Dev Tunnels command failed")
	ErrNotLoggedIn      = errors.New("sign in to Dev Tunnels with your Microsoft account first")
	ErrUnverified       = errors.New("could not verify an owner-only, single-port Dev Tunnel; check its configuration locally")
	ErrNotReady         = errors.New("Dev Tunnel did not report verified readiness before the startup deadline")
	ErrHostExited       = errors.New("Dev Tunnel host exited")
	ErrCleanup          = errors.New("could not verify or remove the newly created tunnel; check your tunnels locally")
	ErrLoginNoChallenge = errors.New("Dev Tunnels did not provide a supported Microsoft sign-in prompt; check your connection and try again")
	ErrLoginTimeout     = errors.New("Dev Tunnels sign-in timed out; try signing in again")
)

// Options contains no credentials or CLI switches. TunnelID may be empty to
// create a dedicated persistent private tunnel, or an exact ID previously saved
// from State. IDs returned by this package include their service cluster.
type Options struct {
	TunnelID string
	Port     int
}

type Account struct {
	LoggedIn bool
	Name     string
}

// LoginChallenge is a short-lived Microsoft device sign-in prompt. URL is an
// allowlisted HTTPS verification page, without query parameters or credentials.
// Code is bounded ASCII alphanumeric text. Do not persist or log either field;
// callers must clear the prompt when LoginWithProgress returns or is canceled.
type LoginChallenge struct {
	URL  string
	Code string
}

// State contains only verified, non-secret data. URL is an HTTPS origin without
// a trailing slash; it is empty whenever Running is false. A stopped successful
// tunnel retains its ID and port for reuse, but Start must verify it again.
type State struct {
	Running  bool
	TunnelID string
	URL      string
	Port     int
}

// Manager owns at most one startup/host lifecycle. Callers must not copy it.
// Construct it with New. No subprocess is started by New.
type Manager struct {
	mu                    sync.Mutex
	executable            string
	runner                commandRunner
	active                *lifecycle
	state                 State
	credentialsBusy       bool
	startupTimeout        time.Duration
	commandTimeout        time.Duration
	readinessTimeout      time.Duration
	cleanupTimeout        time.Duration
	loginTimeout          time.Duration
	loginChallengeTimeout time.Duration
}

type lifecycle struct {
	cancel        context.CancelFunc
	cancelStartup context.CancelFunc
	hostContext   context.Context
	done          chan struct{}
	result        chan startResult
	stopping      bool // protected by Manager.mu
}

type startResult struct {
	state State
	err   error
}

var idleDone = func() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}()

func New(executable string) *Manager {
	return &Manager{
		executable: executable, runner: execRunner{},
		startupTimeout: 90 * time.Second, commandTimeout: 15 * time.Second,
		readinessTimeout: 25 * time.Second, cleanupTimeout: 15 * time.Second,
		loginTimeout: 5 * time.Minute, loginChallengeTimeout: 30 * time.Second,
	}
}

// State returns a copy, never an account token or raw CLI output.
func (m *Manager) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// Done returns the current lifecycle's completion channel, or a closed channel
// when idle. Capture it immediately after Start, then recheck State().Running:
// the host might have exited between those calls. Stop and unexpected host exit
// both close it, after the sole Wait has reaped the process.
func (m *Manager) Done() <-chan struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil {
		return idleDone
	}
	return m.active.done
}

// Account asks only the CLI's supported user-show command. This package never
// opens the CLI's token cache. Unknown output is an error, not a successful login.
func (m *Manager) Account(ctx context.Context) (Account, error) {
	if !m.beginCredentials(false) {
		return Account{}, ErrBusy
	}
	defer m.endCredentials()
	executable, err := m.resolve()
	if err != nil {
		return Account{}, err
	}
	return m.account(ctx, executable)
}

// Login uses the same bounded Microsoft device-code flow as LoginWithProgress,
// without displaying its prompt. Interactive callers should supply a callback.
func (m *Manager) Login(ctx context.Context) error {
	return m.LoginWithProgress(ctx, nil)
}

// LoginWithProgress streams at most one validated Microsoft sign-in challenge.
// notify runs synchronously outside the Manager mutex and must return promptly.
// Raw CLI output never escapes. Cancellation stops and reaps only this login's
// child; account changes are refused while hosting/starting. A successful CLI
// exit (including an already-cached login without a prompt) is confirmed again
// with user show before returning success.
func (m *Manager) LoginWithProgress(ctx context.Context, notify func(LoginChallenge)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !m.beginCredentials(true) {
		return ErrBusy
	}
	defer m.endCredentials()
	executable, err := m.resolve()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, m.loginTimeout)
	defer cancel()
	if err := m.login(ctx, executable, notify); err != nil {
		return loginError(ctx, err)
	}
	account, err := m.account(ctx, executable)
	if err != nil {
		return loginError(ctx, err)
	}
	if !account.LoggedIn {
		return ErrNotLoggedIn
	}
	return nil
}

func loginError(ctx context.Context, err error) error {
	if ctx.Err() == context.DeadlineExceeded {
		return errors.Join(ErrLoginTimeout, context.DeadlineExceeded)
	}
	if ctx.Err() != nil {
		return ctx.Err() // Never expose an arbitrary context cancellation cause.
	}
	return err
}

func (m *Manager) login(ctx context.Context, executable string, notify func(LoginChallenge)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	output := newLoginOutput()
	deadline := time.NewTimer(m.loginChallengeTimeout)
	defer deadline.Stop()
	process, err := m.runner.Start(ctx, executable, output,
		"user", "login", "--use-device-code-auth", "--entra")
	if err != nil {
		return commandError("login", ctx, err)
	}
	exited := make(chan struct{})
	var waitErr error // Published by closing exited, after stdout has drained.
	go func() {
		waitErr = process.Wait() // Sole Wait owner, also on cancellation/timeouts.
		output.close()
		close(exited)
	}()
	defer func() {
		cancel()
		<-exited // Do not release credentialsBusy until the child is reaped.
	}()
	prompts, noChallenge := output.ready, deadline.C
	for {
		var prompt loginPrompt
		select {
		case prompt = <-prompts:
		case <-exited:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if waitErr != nil {
				return commandError("login", ctx, waitErr)
			}
			// A fast successful exit must not hide a prompt already received.
			select {
			case prompt = <-prompts:
			default:
				return nil
			}
		case <-noChallenge:
			// Prefer a prompt already buffered at the deadline boundary.
			select {
			case prompt = <-prompts:
			default:
				return errors.Join(ErrLoginNoChallenge, context.DeadlineExceeded)
			}
		case <-ctx.Done():
			return ctx.Err()
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if prompt.err != nil {
			return prompt.err
		}
		prompts, noChallenge = nil, nil
		deadline.Stop()
		if notify != nil {
			notify(prompt.challenge)
		}
	}
}

// Logout only clears the CLI login via its logout command. Stop first; logging
// out must not leave a running host with a previously issued host token.
func (m *Manager) Logout(ctx context.Context) error {
	if !m.beginCredentials(true) {
		return ErrBusy
	}
	defer m.endCredentials()
	executable, err := m.resolve()
	if err != nil {
		return err
	}
	_, err = m.run(ctx, executable, "logout", m.commandTimeout, "user", "logout")
	return err
}

func (m *Manager) beginCredentials(mutating bool) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.credentialsBusy || (mutating && m.active != nil) {
		return false
	}
	m.credentialsBusy = true
	return true
}

func (m *Manager) endCredentials() {
	m.mu.Lock()
	m.credentialsBusy = false
	m.mu.Unlock()
}

func (m *Manager) resolve() (string, error) {
	if err := validateExecutable(m.executable); err != nil {
		return "", err
	}
	executable, err := m.runner.Resolve(m.executable)
	if err != nil {
		return "", ErrExecutable
	}
	return executable, nil
}

func (m *Manager) account(ctx context.Context, executable string) (Account, error) {
	output, err := m.run(ctx, executable, "account status", m.commandTimeout, "user", "show", "-j")
	if err != nil {
		return Account{}, err
	}
	return parseAccount(output)
}

// Start's context bounds startup ONLY. Once Start succeeds, a caller's deferred
// cancel does not kill the host; Stop owns its lifetime. Start is bounded even
// with context.Background(). Concurrent Start calls fail rather than queue.
// Stop can cancel an in-progress Start and waits for its bounded cleanup.
func (m *Manager) Start(ctx context.Context, options Options) (State, error) {
	if options.Port < 1 || options.Port > 65535 {
		return State{}, ErrOptions
	}
	if options.TunnelID != "" {
		if _, err := parseID(options.TunnelID, false); err != nil {
			return State{}, ErrOptions
		}
	}
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	m.mu.Lock()
	if m.active != nil || m.credentialsBusy {
		m.mu.Unlock()
		return State{}, ErrBusy
	}
	startup, cancelStartup := context.WithTimeout(ctx, m.startupTimeout)
	hostContext, cancelHost := context.WithCancel(context.Background())
	life := &lifecycle{
		cancel: cancelHost, cancelStartup: cancelStartup, hostContext: hostContext,
		done: make(chan struct{}), result: make(chan startResult, 1),
	}
	m.active = life
	m.mu.Unlock()
	go m.serve(startup, life, options)
	result := <-life.result
	return result.state, result.err
}

// Stop revokes Running immediately, cancels startup and hosting, and waits until
// the process is reaped. Concurrent calls share that wait. It never deletes a
// successfully started tunnel, nor any resource supplied by the caller.
func (m *Manager) Stop() error {
	m.mu.Lock()
	life := m.active
	if life == nil {
		m.mu.Unlock()
		return nil
	}
	life.stopping = true
	m.state.Running = false
	m.state.URL = ""
	m.mu.Unlock()
	life.cancelStartup()
	life.cancel()
	<-life.done
	return nil
}

func (m *Manager) serve(ctx context.Context, life *lifecycle, options Options) {
	var startupErr error
	var hostExited chan struct{}
	var executable, createdID, ownershipLabel string
	started := false
	defer func() {
		life.cancelStartup()
		life.cancel()
		if hostExited != nil {
			<-hostExited
		}
		if !started && createdID != "" {
			if err := m.rollback(executable, createdID, ownershipLabel); err != nil {
				startupErr = errors.Join(startupErr, err)
			}
		}
		m.mu.Lock()
		m.state.Running = false
		m.state.URL = ""
		m.active = nil
		close(life.done)
		m.mu.Unlock()
		if !started {
			life.result <- startResult{err: startupErr}
		}
	}()

	executable, startupErr = m.resolve()
	if startupErr != nil {
		return
	}
	account, err := m.account(ctx, executable)
	if err != nil {
		startupErr = err
		return
	}
	if !account.LoggedIn {
		startupErr = ErrNotLoggedIn
		return
	}

	id := options.TunnelID
	if id == "" {
		id, ownershipLabel, startupErr = newResourceName()
		if startupErr != nil {
			return
		}
		// Remember only our randomly chosen name and proof label. On failure,
		// rollback must read back and prove BOTH before it can delete anything.
		createdID = id
		output, err := m.run(ctx, executable, "create", m.commandTimeout,
			"create", id, "--labels", privateLabel, "--labels", ownershipLabel,
			"--description", "MirrorBird private companion",
			"--host-header", "unchanged", "--origin-header", "unchanged", "--request-timeout", "0", "-j")
		if err != nil {
			// A definite command failure is not proof of creation (for example,
			// an ID conflict). Do not delete anything. Cancellation may have
			// raced a successful create; marker-verified rollback handles that.
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				createdID = ""
			}
			startupErr = err
			return
		}
		created, err := parseTunnel(output)
		if err != nil || !provesCreation(created, id, ownershipLabel) {
			startupErr = ErrUnverified
			return
		}
		id = created.TunnelID
		createdID = id
		if err := validateTunnel(created, id, 0, false); err != nil {
			startupErr = err
			return
		}
		output, err = m.run(ctx, executable, "create port", m.commandTimeout,
			"port", "create", id, "-p", strconv.Itoa(options.Port), "--protocol", "http",
			"--host-header", "unchanged", "--origin-header", "unchanged", "--request-timeout", "0", "-j")
		if err != nil {
			startupErr = err
			return
		}
		port, err := parsePort(output)
		if err != nil || validatePort(port, id, options.Port, true) != nil {
			startupErr = ErrUnverified
			return
		}
	}

	resource, err := m.verify(ctx, executable, id, options.Port, false)
	if err != nil {
		startupErr = err
		return
	}
	id = resource.TunnelID
	if err := ctx.Err(); err != nil {
		startupErr = err
		return
	}
	identity, _ := parseID(id, true) // verify has already checked it.
	output := newHostOutput(identity, options.Port)
	// Existing tunnels must be hosted without -p/--protocol: those switches
	// trigger an unsupported batch port update in the CLI. Host forwards the
	// configured ports, so the complete exact-one-port checks remain mandatory.
	process, err := m.runner.Start(life.hostContext, executable, output,
		"host", id, "--host-header", "unchanged", "--origin-header", "unchanged")
	if err != nil {
		startupErr = commandError("host", ctx, err)
		return
	}
	hostExited = make(chan struct{})
	go func() {
		_ = process.Wait() // One owner; neither Stop nor another goroutine calls Wait.
		close(hostExited)
		life.cancelStartup() // Interrupt verification if the host exits before ready.
	}()

	readyContext, cancelReady := context.WithTimeout(ctx, m.readinessTimeout)
	defer cancelReady()
	var origin string
	select {
	case ready := <-output.ready:
		if ready.err != nil {
			startupErr = ready.err
			return
		}
		origin = ready.origin
	case <-hostExited:
		startupErr = ErrHostExited
		return
	case <-readyContext.Done():
		select {
		case <-hostExited:
			startupErr = ErrHostExited
		default:
			startupErr = errors.Join(ErrNotReady, readyContext.Err())
		}
		return
	}

	// host can update port/tunnel options. Re-read both ACL levels and the whole
	// port set after readiness, before returning an origin to the HTTP admission
	// gate. A stale saved URL or success from cmd.Start alone is never sufficient.
	resource, err = m.verify(ctx, executable, id, options.Port, true)
	if err != nil {
		select {
		case <-hostExited:
			startupErr = ErrHostExited
		default:
			startupErr = err
		}
		return
	}
	// The relay hostname can use a service-generated alias, not the friendly
	// tunnel ID. Only this authenticated readback for the exact resource can
	// bind the stdout origin to it; a syntactically safe hostname is not enough.
	if resource.URL != origin {
		startupErr = ErrUnverified
		return
	}
	m.mu.Lock()
	if life.stopping {
		m.mu.Unlock()
		startupErr = context.Canceled
		return
	}
	if err := ctx.Err(); err != nil {
		m.mu.Unlock()
		startupErr = err
		return
	}
	select {
	case <-hostExited:
		m.mu.Unlock()
		startupErr = ErrHostExited
		return
	default:
	}
	resource.Running = true
	m.state = resource
	started = true
	m.mu.Unlock()
	life.cancelStartup() // Deliberately NOT the host context.
	life.result <- startResult{state: resource}
	<-hostExited
}

func (m *Manager) run(ctx context.Context, executable, action string, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	output, err := m.runner.Run(ctx, executable, args...)
	if err != nil {
		return nil, commandError(action, ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(output) > maxOutput {
		return nil, commandError(action, ctx, errOutputLimit)
	}
	return output, nil
}

const privateLabel = "mirrorbird-private"

func newResourceName() (string, string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", "", ErrCommand
	}
	suffix := hex.EncodeToString(random[:])
	return "mirrorbird-" + suffix, "mb-owner-" + suffix, nil
}

// rollback is exclusively for a name randomly generated by this startup. A
// failed create response may be ambiguous, so re-read its exact ID and marker
// instead of trusting output from a failed command or deleting by a short prefix.
func (m *Manager) rollback(executable, id, ownershipLabel string) error {
	ctx, cancel := context.WithTimeout(context.Background(), m.cleanupTimeout)
	defer cancel()
	output, err := m.run(ctx, executable, "verify cleanup", m.cleanupTimeout, "show", id, "-j")
	if err != nil {
		return ErrCleanup
	}
	resource, err := parseTunnel(output)
	if err != nil || !provesCreation(resource, id, ownershipLabel) {
		return ErrCleanup
	}
	output, err = m.run(ctx, executable, "cleanup", m.cleanupTimeout, "delete", resource.TunnelID, "-f", "-j")
	if err != nil || verifyDeletion(output, resource.TunnelID) != nil {
		return ErrCleanup
	}
	return nil
}
