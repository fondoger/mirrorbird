package tunnel

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The CLI's -j output is NOT the service SDK's Tunnel/TunnelPort JSON. These
// narrow wrappers and field names were checked against the installed CLI's
// Microsoft.DevTunnels.Cli.JsonOutput metadata (and its camel-case serializer).
// Live CLI output additionally confirms accessControl is an ARRAY, not the
// SDK's {entries:[]} object. Once ports exist, show omits tunnel-level header
// options; port show supplies their effective values. portUri appears in the
// tunnel's port summary only after hosting, not in port show.
// Service ACL/ID semantics are documented in Microsoft's maintained contracts:
// https://github.com/microsoft/dev-tunnels/tree/main/ts/src/contracts
// https://learn.microsoft.com/azure/developer/dev-tunnels/security
// A changed/unknown schema is an error; never guess at an authentication result.
type cliMessages struct {
	Warning string `json:"warning,omitempty"`
	Error   string `json:"error,omitempty"`
}

type cliAccount struct {
	cliMessages
	Status     string `json:"status"`
	Provider   string `json:"provider,omitempty"`
	Username   string `json:"username,omitempty"`
	TenantID   string `json:"tenantId,omitempty"`
	ObjectID   string `json:"objectId,omitempty"`
	ClientID   string `json:"clientId,omitempty"`
	ResourceID string `json:"resourceId,omitempty"`
}

type cliTunnel struct {
	TunnelID              string          `json:"tunnelId"`
	Name                  string          `json:"name,omitempty"`
	Domain                string          `json:"domain,omitempty"`
	Description           string          `json:"description,omitempty"`
	Labels                []string        `json:"labels,omitempty"`
	HostConnections       *int            `json:"hostConnections,omitempty"`
	ClientConnections     *int            `json:"clientConnections,omitempty"`
	PortCount             *int            `json:"portCount,omitempty"`
	Ports                 []cliPort       `json:"ports,omitempty"`
	AccessControl         json.RawMessage `json:"accessControl,omitempty"`
	HostRequestHeader     *string         `json:"hostRequestHeader,omitempty"`
	OriginRequestHeader   *string         `json:"originRequestHeader,omitempty"`
	RequestTimeoutSeconds *int            `json:"requestTimeoutSeconds,omitempty"`
	TunnelExpiration      string          `json:"tunnelExpiration,omitempty"`
	CurrentUploadRate     string          `json:"currentUploadRate,omitempty"`
	CurrentDownloadRate   string          `json:"currentDownloadRate,omitempty"`
	UploadTotal           string          `json:"uploadTotal,omitempty"`
	DownloadTotal         string          `json:"downloadTotal,omitempty"`
	AccessToken           string          `json:"accessToken,omitempty"`
}

type cliPort struct {
	TunnelID                string          `json:"tunnelId,omitempty"`
	PortNumber              int             `json:"portNumber"`
	Protocol                string          `json:"protocol"`
	PortURI                 string          `json:"portUri,omitempty"`
	Status                  string          `json:"status,omitempty"`
	HeaderOptionDescription string          `json:"headerOptionDescription,omitempty"`
	Description             string          `json:"description,omitempty"`
	Labels                  []string        `json:"labels,omitempty"`
	AccessControl           json.RawMessage `json:"accessControl,omitempty"`
	HostRequestHeader       string          `json:"hostRequestHeader,omitempty"`
	OriginRequestHeader     string          `json:"originRequestHeader,omitempty"`
	RequestTimeoutSeconds   *int            `json:"requestTimeoutSeconds,omitempty"`
	ClientConnections       *int            `json:"clientConnections,omitempty"`
}

func parseAccount(output []byte) (Account, error) {
	var account cliAccount
	if strictJSON(output, &account) != nil || account.Error != "" {
		return Account{}, ErrUnverified
	}
	switch account.Status {
	case "Logged in":
		if (account.Provider != "microsoft" && account.Provider != "github") || account.ClientID != "" || account.ResourceID != "" {
			return Account{}, ErrUnverified
		}
		// Deliberately omit the CLI's username/display strings. They need not be
		// shown by the controller, and could contain sensitive account details.
		return Account{LoggedIn: true}, nil
	case "Not logged in", "Logged out", "Login token expired":
		return Account{}, nil
	default:
		return Account{}, ErrUnverified
	}
}

func parseTunnel(output []byte) (cliTunnel, error) {
	var response struct {
		cliMessages
		Tunnel  *cliTunnel  `json:"tunnel"`
		Tunnels []cliTunnel `json:"tunnels,omitempty"`
		Status  string      `json:"status,omitempty"`
	}
	if strictJSON(output, &response) != nil || response.Error != "" || response.Tunnel == nil || response.Tunnels != nil || response.Status != "" {
		return cliTunnel{}, ErrUnverified
	}
	return *response.Tunnel, nil
}

func parsePort(output []byte) (cliPort, error) {
	var response struct {
		cliMessages
		Port  *cliPort  `json:"port"`
		Ports []cliPort `json:"ports,omitempty"`
	}
	if strictJSON(output, &response) != nil || response.Error != "" || response.Port == nil || response.Ports != nil {
		return cliPort{}, ErrUnverified
	}
	return *response.Port, nil
}

func parsePorts(output []byte) ([]cliPort, error) {
	var response struct {
		cliMessages
		Ports []cliPort `json:"ports"`
		Port  *cliPort  `json:"port,omitempty"`
	}
	if strictJSON(output, &response) != nil || response.Error != "" || response.Ports == nil || response.Port != nil {
		return nil, ErrUnverified
	}
	return response.Ports, nil
}

func verifyDeletion(output []byte, expected string) error {
	var response struct {
		cliMessages
		DeletedTunnel string `json:"deletedTunnel"`
	}
	if strictJSON(output, &response) != nil || response.Error != "" || response.DeletedTunnel != expected {
		return ErrCleanup
	}
	return nil
}

func verifyAccessList(output []byte) error {
	var response struct {
		cliMessages
		Entries []json.RawMessage `json:"accessControlEntries"`
	}
	if strictJSON(output, &response) != nil || response.Error != "" || response.Entries == nil || len(response.Entries) != 0 {
		return ErrUnverified
	}
	return nil
}

// Default owner-only access is represented by no additional entries. Refuse ALL
// custom ACEs, including deny rules or an apparent single-user grant: names can
// be resolved for display by access list, and are not reliable identity proofs.
// A missing embedded ACL (as in port summaries) is not evidence on its own;
// verify always also requires the explicit empty access-list result at BOTH
// tunnel and port scope. An explicitly supplied null/object is not a known ACL.
func defaultACL(acl json.RawMessage) bool {
	if len(acl) == 0 {
		return true
	}
	var entries []json.RawMessage
	return json.Unmarshal(acl, &entries) == nil && entries != nil && len(entries) == 0
}

func validateTunnel(t cliTunnel, expected string, port int, ready bool) error {
	id, err := parseID(t.TunnelID, true)
	if err != nil || !matchesID(expected, t.TunnelID) || !defaultACL(t.AccessControl) || t.AccessToken != "" || (t.Domain != "" && t.Domain != "devtunnels.ms") {
		return ErrUnverified
	}
	// CLI show deliberately omits tunnel header/timeout fields once ports
	// exist. Absence here is not proof of safe defaults: verify still requires
	// port show to explicitly confirm BOTH effective headers and timeout=0.
	// Supplied conflicting values remain a failure even if the port overrides.
	if (t.HostRequestHeader != nil && *t.HostRequestHeader != "unchanged") || (t.OriginRequestHeader != nil && *t.OriginRequestHeader != "unchanged") || (t.RequestTimeoutSeconds != nil && *t.RequestTimeoutSeconds != 0) {
		return ErrUnverified
	}
	if port == 0 { // Creation output has no ports and must confirm our options.
		if t.HostRequestHeader == nil || t.OriginRequestHeader == nil || t.RequestTimeoutSeconds == nil {
			return ErrUnverified
		}
		if len(t.Ports) != 0 || (t.PortCount != nil && *t.PortCount != 0) || (t.HostConnections != nil && *t.HostConnections != 0) {
			return ErrUnverified
		}
		return nil
	}
	// Hosting an existing resource can replace endpoints; do not touch one with
	// an existing host. After our readiness signal a count of zero is permitted
	// because connection counters can lag, but multiple hosts are never allowed.
	if t.HostConnections == nil || *t.HostConnections < 0 || *t.HostConnections > 1 || (!ready && *t.HostConnections != 0) {
		return ErrUnverified
	}
	if len(t.Ports) != 1 || (t.PortCount != nil && *t.PortCount != 1) || validatePort(t.Ports[0], t.TunnelID, port, false) != nil {
		return ErrUnverified
	}
	if t.Ports[0].PortURI != "" {
		if _, err := forwardingOrigin(t.Ports[0].PortURI, id, port); err != nil {
			return err
		}
	} else if ready {
		return ErrUnverified
	}
	return nil
}

func validatePort(p cliPort, expected string, port int, details bool) error {
	if p.PortNumber != port || p.Protocol != "http" || !defaultACL(p.AccessControl) {
		return ErrUnverified
	}
	if (p.TunnelID != "" || details) && p.TunnelID != expected {
		return ErrUnverified
	}
	if details && (p.HostRequestHeader != "unchanged" || p.OriginRequestHeader != "unchanged" || p.RequestTimeoutSeconds == nil || *p.RequestTimeoutSeconds != 0) {
		return ErrUnverified
	}
	// Summary/list records omit these fields; when present they must not
	// contradict the separately verified effective options from port show.
	if (p.HostRequestHeader != "" && p.HostRequestHeader != "unchanged") || (p.OriginRequestHeader != "" && p.OriginRequestHeader != "unchanged") || (p.RequestTimeoutSeconds != nil && *p.RequestTimeoutSeconds != 0) {
		return ErrUnverified
	}
	if p.PortURI != "" {
		id, err := parseID(expected, true)
		if err != nil {
			return ErrUnverified
		}
		if _, err := forwardingOrigin(p.PortURI, id, port); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) verify(ctx context.Context, executable, id string, port int, ready bool) (State, error) {
	state, err := m.inspectResource(ctx, executable, id, port, ready)
	if err != nil {
		return State{}, err
	}
	if err := m.verifyDetails(ctx, executable, state.TunnelID, port); err != nil {
		return State{}, err
	}
	return state, nil
}

func (m *Manager) inspectResource(ctx context.Context, executable, id string, port int, ready bool) (State, error) {
	output, err := m.run(ctx, executable, "inspect tunnel", m.commandTimeout, "show", id, "-j")
	if err != nil {
		return State{}, err
	}
	t, err := parseTunnel(output)
	if err != nil || validateTunnel(t, id, port, ready) != nil {
		return State{}, ErrUnverified
	}
	id = t.TunnelID // Every subsequent command uses the exact cluster-qualified ID.
	state := State{TunnelID: id, Port: port}
	if t.Ports[0].PortURI != "" {
		identity, _ := parseID(id, true)
		state.URL, err = forwardingOrigin(t.Ports[0].PortURI, identity, port)
		if err != nil {
			return State{}, err
		}
	}
	return state, nil
}

func (m *Manager) verifyDetails(ctx context.Context, executable, id string, port int) error {
	checks := []struct {
		action   string
		args     []string
		validate func([]byte) error
	}{
		{"inspect tunnel access", []string{"access", "list", id, "-j"}, verifyAccessList},
		{"list ports", []string{"port", "list", id, "-j"}, func(output []byte) error {
			ports, err := parsePorts(output)
			if err != nil || len(ports) != 1 || validatePort(ports[0], id, port, false) != nil {
				return ErrUnverified
			}
			return nil
		}},
		{"inspect port", []string{"port", "show", id, "-p", strconv.Itoa(port), "-j"}, func(output []byte) error {
			p, err := parsePort(output)
			if err != nil || validatePort(p, id, port, true) != nil {
				return ErrUnverified
			}
			return nil
		}},
		{"inspect port access", []string{"access", "list", id, "-p", strconv.Itoa(port), "-j"}, verifyAccessList},
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, len(checks))
	for _, check := range checks {
		go func() {
			output, err := m.run(ctx, executable, check.action, m.commandTimeout, check.args...)
			if err == nil {
				err = check.validate(output)
			}
			results <- err
		}()
	}
	var failure error
	for range checks {
		if err := <-results; err != nil && failure == nil {
			failure = err
			cancel()
		}
	}
	return failure
}

type tunnelID struct {
	name    string
	cluster string
}

var (
	idPattern      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,47}[a-z0-9]$`)
	clusterPattern = regexp.MustCompile(`^(?:[a-z]{3,4}[0-9]{1,3}|asse|aue|brs|euw|use)$`)
)

func parseID(value string, qualified bool) (tunnelID, error) {
	name, cluster, hasCluster := strings.Cut(value, ".")
	if !idPattern.MatchString(name) || (hasCluster && !clusterPattern.MatchString(cluster)) || (qualified && !hasCluster) {
		return tunnelID{}, ErrUnverified
	}
	return tunnelID{name: name, cluster: cluster}, nil
}

func matchesID(expected, actual string) bool {
	want, err := parseID(expected, false)
	if err != nil {
		return false
	}
	got, err := parseID(actual, true)
	return err == nil && want.name == got.name && (want.cluster == "" || want.cluster == got.cluster)
}

func provesCreation(t cliTunnel, expected, label string) bool {
	if label == "" || !matchesID(expected, t.TunnelID) {
		return false
	}
	havePrivate, haveProof := false, false
	for _, candidate := range t.Labels {
		havePrivate = havePrivate || candidate == privateLabel
		haveProof = haveProof || candidate == label
	}
	return havePrivate && haveProof
}

func forwardingOrigin(raw string, id tunnelID, port int) (string, error) {
	if len(raw) > 256 || raw != strings.TrimSpace(raw) {
		return "", ErrUnverified
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") {
		return "", ErrUnverified
	}
	// A friendly tunnel ID can have a different service-generated relay alias.
	// This checks only canonical HTTPS syntax, cluster and port, NOT resource
	// ownership. serve must compare stdout with authenticated post-ready show
	// for the exact tunnel ID before publishing Running or admitting requests.
	if !clusterPattern.MatchString(id.cluster) || port < 1 || port > 65535 {
		return "", ErrUnverified
	}
	label, ok := strings.CutSuffix(u.Host, "."+id.cluster+".devtunnels.ms")
	alias, portMatches := strings.CutSuffix(label, "-"+strconv.Itoa(port))
	if !ok || !portMatches || !idPattern.MatchString(alias) {
		return "", ErrUnverified
	}
	origin := "https://" + u.Host
	// Reject explicit :443, inspection suffixes, paths, escaping, empty query
	// or fragment markers, and all noncanonical alternatives as well.
	if raw != origin && raw != origin+"/" {
		return "", ErrUnverified
	}
	return origin, nil
}

// encoding/json normally accepts duplicate keys, case-folded duplicates, and
// trailing values. Eliminate those ambiguities before strict schema decoding.
// Bound recursion independently of the CLI's output byte limit.
func strictJSON(output []byte, value any) error {
	if len(output) == 0 || len(output) > maxOutput || !utf8.Valid(output) {
		return ErrUnverified
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.UseNumber()
	if walkJSON(decoder, 0) != nil {
		return ErrUnverified
	}
	if _, err := decoder.Token(); err != io.EOF {
		return ErrUnverified
	}
	decoder = json.NewDecoder(bytes.NewReader(output))
	decoder.DisallowUnknownFields()
	if decoder.Decode(value) != nil {
		return ErrUnverified
	}
	return nil
}

func walkJSON(decoder *json.Decoder, depth int) error {
	if depth > 32 {
		return ErrUnverified
	}
	token, err := decoder.Token()
	if err != nil {
		return ErrUnverified
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		keys := make(map[string]bool)
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return ErrUnverified
			}
			key, ok := token.(string)
			key = strings.ToLower(key)
			if !ok || keys[key] {
				return ErrUnverified
			}
			keys[key] = true
			if walkJSON(decoder, depth+1) != nil {
				return ErrUnverified
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return ErrUnverified
		}
	case '[':
		for decoder.More() {
			if walkJSON(decoder, depth+1) != nil {
				return ErrUnverified
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return ErrUnverified
		}
	default:
		return ErrUnverified
	}
	return nil
}
