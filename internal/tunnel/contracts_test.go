package tunnel

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestOwnerOnlyAccessListRequiresKnownExplicitEmptyArray(t *testing.T) {
	for _, input := range []string{
		`{"accessControlEntries":[]}`,
		`{"accessControlEntries":[],"warning":"CLI update available"}`,
	} {
		if err := verifyAccessList([]byte(input)); err != nil {
			t.Fatalf("valid default ACL: %v", err)
		}
	}
	for _, input := range []string{
		`{}`, `null`, `[]`, `{"entries":[]}`, `{"accessControlEntries":null}`,
		`{"accessControlEntries":{}}`, `{"accessControlEntries":[]}{"accessControlEntries":[]}`,
		`{"accessControlEntries":[],"error":"private output"}`,
		`{"accessControlEntries":[],"isPublic":true}`,
		`{"accessControlEntries":[],"AccessControlEntries":[{"type":"Anonymous"}]}`,
		`{"accessControlEntries":[{"type":"Anonymous","scopes":["connect"]}]}`,
		`{"accessControlEntries":[{"type":"Users","provider":"microsoft","subjects":["owner"],"scopes":["manage"]}]}`,
		`{"accessControlEntries":[{"type":"Repositories","subjects":["org/repo"]}]}`,
		`{"accessControlEntries":[{"type":"Organizations","isInverse":true}]}`,
		`{"accessControlEntries":[{"type":"Groups","isDeny":true}]}`,
		`{"accessControlEntries":[{}]}`, `{"accessControlEntries":[null]}`,
	} {
		if err := verifyAccessList([]byte(input)); !errors.Is(err, ErrUnverified) {
			t.Errorf("accepted unverified ACL %s", input)
		}
	}
}

func TestDeletionResponseMustConfirmExactResource(t *testing.T) {
	if err := verifyDeletion([]byte(`{"deletedTunnel":"mirrorbird-test.usw2"}`), testID); err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{
		`{}`, `null`, `{"deletedTunnel":"mirrorbird-test"}`,
		`{"deletedTunnel":"other-resource.usw2"}`, `{"deletedTunnel":"mirrorbird-test.usw2","error":"secret"}`,
		`{"deletedTunnels":["mirrorbird-test.usw2"]}`,
	} {
		if err := verifyDeletion([]byte(input), testID); !errors.Is(err, ErrCleanup) {
			t.Errorf("unconfirmed cleanup accepted: %s", input)
		}
	}
}

func TestEmbeddedACLIsOnlySupplementalEvidence(t *testing.T) {
	if !defaultACL(nil) || !defaultACL(json.RawMessage(`[]`)) || !defaultACL(json.RawMessage(` [ ] `)) {
		t.Fatal("default embedded CLI ACL rejected")
	}
	for _, input := range []string{`null`, `{}`, `{"entries":[]}`, `[{}]`, `[null]`, `[{"type":"Users"}]`, `[{"type":"Anonymous"}]`, `true`, `"Owner only"`} {
		if defaultACL(json.RawMessage(input)) {
			t.Errorf("unverified embedded ACL accepted: %s", input)
		}
	}
}

// These literal fixtures follow the parent session's observed installed CLI
// responses, with non-secret synthetic IDs. They are deliberately not SDK JSON.
const observedCreateJSON = `{
  "tunnel": {
    "tunnelId": "mirrorbird-test.usw2",
    "description": "MirrorBird private companion",
    "labels": ["mirrorbird-private", "mb-owner-test"],
    "accessControl": [],
    "hostConnections": 0,
    "hostRequestHeader": "unchanged",
    "originRequestHeader": "unchanged",
    "requestTimeoutSeconds": 0
  }
}`

const observedPortJSON = `{
  "port": {
    "tunnelId": "mirrorbird-test.usw2",
    "portNumber": 43127,
    "protocol": "http",
    "accessControl": [],
    "hostRequestHeader": "unchanged",
    "originRequestHeader": "unchanged",
    "requestTimeoutSeconds": 0
  }
}`

const observedPreHostJSON = `{
  "tunnel": {
    "tunnelId": "mirrorbird-test.usw2",
    "accessControl": [],
    "hostConnections": 0,
    "ports": [{"portNumber": 43127, "protocol": "http", "headerOptionDescription": "Host:unchanged Origin:unchanged"}]
  }
}`

func TestObservedCLIResourceShapes(t *testing.T) {
	created, err := parseTunnel([]byte(observedCreateJSON))
	if err != nil || validateTunnel(created, testID, 0, false) != nil || !provesCreation(created, testID, "mb-owner-test") {
		t.Fatalf("real create shape rejected: %+v %v", created, err)
	}
	port, err := parsePort([]byte(observedPortJSON))
	if err != nil || validatePort(port, testID, testPort, true) != nil {
		t.Fatalf("real port shape rejected: %+v %v", port, err)
	}
	resource, err := parseTunnel([]byte(observedPreHostJSON))
	if err != nil || validateTunnel(resource, testID, testPort, false) != nil {
		t.Fatalf("pre-host show rejected omitted tunnel fields: %+v %v", resource, err)
	}
	if validateTunnel(resource, testID, testPort, true) == nil {
		t.Fatal("pre-host resource without a verified URL was accepted as ready")
	}
}

func TestObservedHostedResourceBindsGeneratedRelayAlias(t *testing.T) {
	const hosted = `{
	  "tunnel": {
	    "tunnelId": "mirrorbird-e2e-20261018.asse",
	    "hostConnections": 1,
	    "accessControl": [],
	    "ports": [{"portNumber": 43123, "protocol": "http", "portUri": "https://jqfmk0vr-43123.asse.devtunnels.ms/", "headerOptionDescription": "Host:unchanged Origin:unchanged"}]
	  }
	}`
	resource, err := parseTunnel([]byte(hosted))
	if err != nil || validateTunnel(resource, "mirrorbird-e2e-20261018.asse", 43123, true) != nil {
		t.Fatalf("observed service-generated alias rejected: %+v %v", resource, err)
	}
	if validateTunnel(resource, "other-resource.asse", 43123, true) == nil {
		t.Fatal("resource ID mismatch accepted because relay URI was well formed")
	}
	for _, wrong := range []string{
		"https://jqfmk0vr-43124.asse.devtunnels.ms/",
		"https://jqfmk0vr-43123.usw2.devtunnels.ms/",
		"https://jqfmk0vr-43123-inspect.asse.devtunnels.ms/",
		"https://jqfmk0vr-43123.asse.devtunnels.ms.evil.example/",
	} {
		bad := strings.Replace(hosted, "https://jqfmk0vr-43123.asse.devtunnels.ms/", wrong, 1)
		resource, err := parseTunnel([]byte(bad))
		if err == nil && validateTunnel(resource, "mirrorbird-e2e-20261018.asse", 43123, true) == nil {
			t.Fatalf("unsafe authoritative URI accepted: %s", wrong)
		}
	}
}

func TestMissingTunnelOptionsStillRequiresExplicitEffectivePortOptions(t *testing.T) {
	for _, field := range []string{"hostRequestHeader", "originRequestHeader", "requestTimeoutSeconds"} {
		for _, ready := range []bool{false, true} {
			t.Run(field+map[bool]string{false: "/before-host", true: "/after-host"}[ready], func(t *testing.T) {
				r := newFakeRunner()
				r.ready = ready
				r.changed = func(key string, response any) any {
					if key == "port show" {
						delete(response.(map[string]any)["port"].(map[string]any), field)
					}
					return response
				}
				m := testManager(t, r)
				if _, err := m.verify(context.Background(), "fake-devtunnel", testID, testPort, ready); !errors.Is(err, ErrUnverified) {
					t.Fatalf("missing effective %s accepted: %v", field, err)
				}
			})
		}
	}
}

func TestObservedShowWithOmittedOptionsRequiresBothAccessLists(t *testing.T) {
	for _, ready := range []bool{false, true} {
		r := newFakeRunner()
		r.ready = ready
		m := testManager(t, r)
		state, err := m.verify(context.Background(), "fake-devtunnel", testID, testPort, ready)
		if err != nil || state.TunnelID != testID || state.Port != testPort || (ready && state.URL != testOrigin) || (!ready && state.URL != "") {
			t.Fatalf("ready=%v state=%+v err=%v", ready, state, err)
		}
		calls := r.snapshot()
		seen := make(map[string]bool)
		for _, call := range calls {
			seen[strings.Join(call.args, " ")] = true
		}
		if len(calls) != 5 || !seen["access list "+testID+" -j"] || !seen["access list "+testID+" -p 43127 -j"] {
			t.Fatalf("omitted tunnel options skipped required verification: %+v", calls)
		}
	}
}

func TestObservedArrayACLDoesNotPermitSDKObjectsOrCustomEntries(t *testing.T) {
	for _, acl := range []string{`null`, `{}`, `{"entries":[]}`, `[{"type":"Users","subjects":["owner"]}]`, `[{"type":"Anonymous"}]`, `[null]`} {
		t.Run(acl, func(t *testing.T) {
			resource, err := parseTunnel([]byte(strings.Replace(observedCreateJSON, `"accessControl": []`, `"accessControl": `+acl, 1)))
			if err == nil && validateTunnel(resource, testID, 0, false) == nil {
				t.Fatalf("unverified create ACL accepted: %s", acl)
			}
			port, err := parsePort([]byte(strings.Replace(observedPortJSON, `"accessControl": []`, `"accessControl": `+acl, 1)))
			if err == nil && validatePort(port, testID, testPort, true) == nil {
				t.Fatalf("unverified port ACL accepted: %s", acl)
			}
		})
	}
}

func TestPresentTunnelOptionsMustNotContradictEffectivePort(t *testing.T) {
	for _, conflicting := range []string{`"hostRequestHeader":"localhost",`, `"hostRequestHeader":"",`, `"originRequestHeader":"http://localhost",`, `"requestTimeoutSeconds":100,`} {
		input := strings.Replace(observedPreHostJSON, `"accessControl": [],`, `"accessControl": [],`+conflicting, 1)
		resource, err := parseTunnel([]byte(input))
		if err == nil && validateTunnel(resource, testID, testPort, false) == nil {
			t.Fatalf("conflicting tunnel options accepted: %s", conflicting)
		}
	}
}

func TestCreationStillRequiresExplicitTunnelOptions(t *testing.T) {
	for _, field := range []string{"hostRequestHeader", "originRequestHeader", "requestTimeoutSeconds"} {
		var input map[string]any
		if err := json.Unmarshal([]byte(observedCreateJSON), &input); err != nil {
			t.Fatal(err)
		}
		delete(input["tunnel"].(map[string]any), field)
		data, _ := json.Marshal(input)
		resource, err := parseTunnel(data)
		if err == nil && validateTunnel(resource, testID, 0, false) == nil {
			t.Fatalf("creation accepted missing option %s", field)
		}
	}
}

func TestAccountOutputStatusIsExplicitAndRedacted(t *testing.T) {
	cases := []struct {
		input    string
		loggedIn bool
		valid    bool
	}{
		{`{"status":"Logged in","provider":"microsoft","username":"secret value","tenantId":"tenant","objectId":"id"}`, true, true},
		{`{"status":"Logged in","provider":"github","username":"owner"}`, true, true},
		{`{"status":"Not logged in"}`, false, true},
		{`{"status":"Logged out","username":"private"}`, false, true},
		{`{"status":"Login token expired"}`, false, true},
		{`{"status":"Logged in","provider":"microsoft","clientId":"service-principal"}`, false, false},
		{`{"status":"Logged in","provider":"microsoft","resourceId":"managed-identity"}`, false, false},
		{`{"status":"Logged in","provider":"future-provider"}`, false, false},
		{`{"status":"Logged in"}`, false, false},
		{`{"loggedIn":true}`, false, false},
		{`{"status":"Logged in","provider":"microsoft","error":"secret"}`, false, false},
		{`{"status":"Logging in","provider":"microsoft"}`, false, false},
		{`{}`, false, false},
		{`null`, false, false},
		{`"Logged in"`, false, false},
		{`Logged in as owner using Microsoft.`, false, false},
	}
	for _, tc := range cases {
		account, err := parseAccount([]byte(tc.input))
		if (err == nil) != tc.valid || account.LoggedIn != tc.loggedIn || account.Name != "" {
			t.Errorf("input=%s account=%+v err=%v", tc.input, account, err)
		}
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Fatal("raw account output leaked")
		}
	}
}

func TestExactResourceIDs(t *testing.T) {
	for _, value := range []string{"mirrorbird-test.usw2", "l3rs99qw.usw2", "abc.use", "owner-123.euw", "abc.east1"} {
		if _, err := parseID(value, true); err != nil {
			t.Errorf("ID %q rejected", value)
		}
	}
	for _, value := range []string{"ab.usw2", "abc", "ABC.usw2", "abc.USW2", "-abc.usw2", "abc-.usw2", "abc--flag.usw2\n", "abc.usw2.evil", "abc..usw2", "abc.usw2/", "abc.usw2:443", "abc.other", "abc.us-w2", strings.Repeat("a", 50) + ".usw2"} {
		if _, err := parseID(value, true); err == nil {
			t.Errorf("unsafe ID %q accepted", value)
		}
	}
	if !matchesID("mirrorbird-test", testID) || !matchesID(testID, testID) || matchesID("mirrorbird", testID) || matchesID("mirrorbird-test.use", testID) {
		t.Fatal("ID matching permitted prefixes/cross-cluster resolution")
	}
}

func TestForwardingOriginsRequireServiceClusterAndPort(t *testing.T) {
	id, _ := parseID(testID, true)
	// The hostname is NOT an identity proof. These can only become Running
	// when stdout equals authenticated post-ready readback for the exact ID.
	for _, input := range []string{testOrigin, testOrigin + "/", "https://mirrorbird-test-43127.usw2.devtunnels.ms/", "https://different-service-alias-43127.usw2.devtunnels.ms/"} {
		if actual, err := forwardingOrigin(input, id, testPort); err != nil || actual != strings.TrimSuffix(input, "/") {
			t.Fatalf("valid URL %q => %q %v", input, actual, err)
		}
	}
	for _, input := range []string{
		"http://mirrorbird-test-43127.usw2.devtunnels.ms/",
		"https://mirrorbird-test-43128.usw2.devtunnels.ms/",
		"https://invalid.alias-43127.usw2.devtunnels.ms/",
		"https://-invalid-43127.usw2.devtunnels.ms/",
		"https://jqfmk0vr-043127.usw2.devtunnels.ms/",
		"https://jqfmk0vr-43127.asse.devtunnels.ms/",
		"https://jqfmk0vr-43127.example.com/",
		"https://jqfmk0vr-43127-inspect.usw2.devtunnels.ms/",
		"https://" + strings.Repeat("a", 50) + "-43127.usw2.devtunnels.ms/",
		"https://mirrorbird-test-43127.euw.devtunnels.ms/",
		"https://mirrorbird-test-43127.usw2.devtunnels.ms.evil.example/",
		"https://mirrorbird-test-43127.usw2.devtunnels.ms:443/",
		"https://mirrorbird-test-43127.usw2.devtunnels.ms:8080/",
		"https://mirrorbird-test-43127.usw2.devtunnels.ms./",
		"https://mirrorbird-test-43127-inspect.usw2.devtunnels.ms/",
		"https://mirrorbird-test-43127.devtunnels.ms/",
		"https://user:secret@mirrorbird-test-43127.usw2.devtunnels.ms/",
		"https://MIRRORBIRD-test-43127.usw2.devtunnels.ms/",
		testOrigin + "/?access_token=secret", testOrigin + "/?", testOrigin + "/#secret", testOrigin + "/#",
		testOrigin + "/path", testOrigin + "/%2f", testOrigin + "/../", testOrigin + "\\@evil.example",
		" " + testOrigin, testOrigin + "\n", "//mirrorbird-test-43127.usw2.devtunnels.ms/",
		"https:mirrorbird-test-43127.usw2.devtunnels.ms", "javascript:alert(1)",
	} {
		if actual, err := forwardingOrigin(input, id, testPort); err == nil || actual != "" {
			t.Errorf("unsafe URL %q => %q %v", input, actual, err)
		}
	}
}

func TestStrictJSONRejectsAmbiguousOrUnboundedData(t *testing.T) {
	for _, input := range []string{
		`{"status":"Logged in","status":"Logged out"}`,
		`{"status":"Logged in","STATUS":"Logged out"}`,
		`{"status":"Logged in","status":"Logged out"}`,
		`{"status":"Logged in"} null`, `{"status":true}`, `{"unknown":true}`,
		strings.Repeat("[", 40) + "0" + strings.Repeat("]", 40),
		strings.Repeat(" ", maxOutput+1), "\xff", "", "  ",
	} {
		var account cliAccount
		if err := strictJSON([]byte(input), &account); err == nil {
			t.Errorf("accepted ambiguous/unknown JSON (length %d)", len(input))
		}
	}
}

func TestCLIWrappersNotGuessedRawSDKContracts(t *testing.T) {
	if _, err := parseTunnel([]byte(`{"tunnelId":"mirrorbird-test","clusterId":"usw2","accessControl":{"entries":[]}}`)); err == nil {
		t.Fatal("accepted SDK object instead of CLI wrapper")
	}
	for _, input := range []string{`{}`, `{"tunnel":null}`, `{"tunnel":{}}`, `{"tunnels":[]}`, `{"tunnel":{},"status":"created"}`} {
		tunnel, err := parseTunnel([]byte(input))
		if err == nil && validateTunnel(tunnel, testID, testPort, false) == nil {
			t.Errorf("accepted missing resource fields %s", input)
		}
	}
	for _, input := range []string{`{}`, `{"port":null}`, `{"ports":[]}`, `{"port":{"portNumber":43127,"protocol":"http"},"error":"bad"}`} {
		if _, err := parsePort([]byte(input)); err == nil {
			t.Errorf("accepted malformed port %s", input)
		}
	}
	for _, input := range []string{`{}`, `{"ports":null}`, `{"ports":[],"port":{}}`, `{"ports":[],"futureACL":[]}`} {
		if _, err := parsePorts([]byte(input)); err == nil {
			t.Errorf("accepted malformed port list %s", input)
		}
	}
}
