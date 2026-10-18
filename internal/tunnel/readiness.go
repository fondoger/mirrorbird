package tunnel

import (
	"bytes"
	"strconv"
	"strings"
)

type hostReady struct {
	origin string
	err    error
}

// hostOutput is a bounded stdout sink, not a log. It only retains one partial
// line and a syntactically checked origin, never exposes raw output, and keeps
// draining after readiness/failure. The manager still must bind this origin to
// authenticated resource readback. exec.Cmd owns its single writer; tests use
// it the same way. No goroutine reads stdout after another goroutine calls Wait.
type hostOutput struct {
	identity tunnelID
	port     int
	ready    chan hostReady
	line     []byte
	total    int
	portSeen bool
	origin   string
	finished bool
}

func newHostOutput(id tunnelID, port int) *hostOutput {
	return &hostOutput{identity: id, port: port, ready: make(chan hostReady, 1)}
}

func (o *hostOutput) Write(p []byte) (int, error) {
	n := len(p)
	if o.finished {
		return n, nil
	}
	if n > maxOutput-o.total {
		o.finish(hostReady{err: ErrNotReady})
		return n, nil
	}
	o.total += n
	for len(p) != 0 && !o.finished {
		end := bytes.IndexByte(p, '\n')
		fragment := p
		if end >= 0 {
			fragment = p[:end]
		}
		if len(o.line)+len(fragment) > 4096 {
			o.finish(hostReady{err: ErrNotReady})
			break
		}
		o.line = append(o.line, fragment...)
		if end < 0 {
			break
		}
		o.consumeLine(strings.TrimSuffix(string(o.line), "\r"))
		o.line = o.line[:0]
		p = p[end+1:]
	}
	return n, nil
}

func (o *hostOutput) consumeLine(line string) {
	switch {
	case strings.HasPrefix(line, "Hosting port: "):
		if line != "Hosting port: "+strconv.Itoa(o.port) || o.portSeen {
			o.finish(hostReady{err: ErrUnverified})
			return
		}
		o.portSeen = true
	case strings.HasPrefix(line, "Connect via browser: "):
		if !o.portSeen || o.origin != "" {
			o.finish(hostReady{err: ErrUnverified})
			return
		}
		// The CLI joins a port's web forwarding URI list with ", ". Require
		// one canonical HTTPS origin in the expected cluster for this port.
		// Its service alias need not equal the friendly ID, but serve will
		// require equality with the exact resource's authenticated portUri.
		for _, candidate := range strings.Split(strings.TrimPrefix(line, "Connect via browser: "), ", ") {
			origin, err := forwardingOrigin(candidate, o.identity, o.port)
			if err != nil || (o.origin != "" && o.origin != origin) {
				o.finish(hostReady{err: ErrUnverified})
				return
			}
			o.origin = origin
		}
	case strings.HasPrefix(line, "Ready to accept connections for tunnel: "):
		id := strings.TrimPrefix(line, "Ready to accept connections for tunnel: ")
		qualified := o.identity.name + "." + o.identity.cluster
		if (id != qualified && id != o.identity.name) || !o.portSeen || o.origin == "" {
			o.finish(hostReady{err: ErrUnverified})
			return
		}
		o.finish(hostReady{origin: o.origin})
	}
}

func (o *hostOutput) finish(result hostReady) {
	o.finished = true
	o.line = nil
	o.ready <- result
}

const maxLoginLine = 4096

type loginPrompt struct {
	challenge LoginChallenge
	err       error
}

// loginOutput is a single-writer stdout sink, never a log. It buffers only one
// bounded partial line, publishes the first valid challenge, then discards all
// later output. Wait owns close, after the runner has finished draining stdout.
// The callback runs in LoginWithProgress, not in this writer or under a mutex.
type loginOutput struct {
	ready    chan loginPrompt
	line     []byte
	total    int
	finished bool
}

func newLoginOutput() *loginOutput {
	return &loginOutput{ready: make(chan loginPrompt, 1)}
}

func (o *loginOutput) Write(p []byte) (int, error) {
	n := len(p)
	if o.finished {
		return n, nil
	}
	if n > maxOutput-o.total {
		o.finish(loginPrompt{err: ErrLoginNoChallenge})
		return n, nil
	}
	o.total += n
	for len(p) != 0 && !o.finished {
		end := bytes.IndexByte(p, '\n')
		fragment := p
		if end >= 0 {
			fragment = p[:end]
		}
		if len(o.line)+len(fragment) > maxLoginLine {
			o.finish(loginPrompt{err: ErrLoginNoChallenge})
			break
		}
		o.line = append(o.line, fragment...)
		if end < 0 {
			break
		}
		o.consumeLine()
		p = p[end+1:]
	}
	return n, nil // Drain even malformed/excess output without retaining it.
}

func (o *loginOutput) consumeLine() {
	challenge, ok := parseLoginChallenge(strings.TrimSuffix(string(o.line), "\r"))
	clear(o.line)
	o.line = o.line[:0]
	if ok {
		o.finish(loginPrompt{challenge: challenge})
	}
}

func (o *loginOutput) close() {
	if !o.finished {
		o.consumeLine() // A final, complete sentence need not end with a newline.
		o.finished = true
		o.line = nil
	}
}

func (o *loginOutput) finish(prompt loginPrompt) {
	o.finished = true
	clear(o.line)
	o.line = nil
	o.ready <- prompt
}

// Match only the verified English Entra device-code sentence, not arbitrary
// links, GitHub prompts, JSON, or diagnostics. Literal URLs make userinfo,
// alternate ports, encoded paths, suffix hosts, queries and fragments impossible.
func parseLoginChallenge(line string) (LoginChallenge, bool) {
	const prefix = "To sign in, use a web browser to open the page "
	const suffix = " to authenticate."
	body, ok := strings.CutPrefix(line, prefix)
	if !ok {
		return LoginChallenge{}, false
	}
	body, ok = strings.CutSuffix(body, suffix)
	if !ok {
		return LoginChallenge{}, false
	}
	verificationURL, code, ok := strings.Cut(body, " and enter the code ")
	if !ok || len(code) < 6 || len(code) > 16 {
		return LoginChallenge{}, false
	}
	switch verificationURL {
	case "https://login.microsoft.com/device", "https://microsoft.com/devicelogin", "https://www.microsoft.com/devicelogin":
	default:
		return LoginChallenge{}, false
	}
	for i := range len(code) {
		c := code[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return LoginChallenge{}, false
		}
	}
	return LoginChallenge{URL: strings.Clone(verificationURL), Code: strings.Clone(code)}, true
}
