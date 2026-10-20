package attachment

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"mirrorbird/internal/bridge"
)

func composerReply(t *testing.T, responseField string, response json.RawMessage) (json.RawMessage, error) {
	t.Helper()
	h, upstream := readyHost(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	type outcome struct {
		data json.RawMessage
		err  error
	}
	done := make(chan outcome, 1)
	go func() {
		data, err := h.dispatch(ctx, "composer/defaults", json.RawMessage(`{}`))
		done <- outcome{data, err}
	}()
	var call struct {
		ID     json.RawMessage            `json:"id"`
		Method string                     `json:"method"`
		Params map[string]json.RawMessage `json:"params"`
	}
	select {
	case frame := <-upstream:
		if err := json.Unmarshal(frame, &call); err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("composer defaults did not call the backend")
	}
	if call.Method != "config/read" || len(call.Params) != 1 || string(call.Params["includeLayers"]) != "false" {
		t.Fatalf("config/read parameters must be fixed: %+v", call)
	}
	if err := h.Mux.FromServer(encode(map[string]any{"id": call.ID, responseField: response})); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		return result.data, result.err
	case <-ctx.Done():
		t.Fatal("composer defaults did not return")
		return nil, ctx.Err()
	}
}

func TestComposerDefaultsOnlyExposesEffectiveModelAndEffort(t *testing.T) {
	data, err := composerReply(t, "result", json.RawMessage(`{
		"config":{"model":"gpt-6-astra","model_reasoning_effort":"ultra","api_key":"private-config-value"},
		"layers":[{"path":"private-config-path"}],"origins":{"private":"value"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]string
	if json.Unmarshal(data, &result) != nil || len(result) != 2 || result["model"] != "gpt-6-astra" || result["effort"] != "ultra" {
		t.Fatalf("unexpected projected defaults: %s", data)
	}
	if strings.Contains(string(data), "private") {
		t.Fatal("private configuration escaped projection")
	}
}

func TestComposerDefaultsAllowsUnsetConfigValues(t *testing.T) {
	for _, response := range []string{`{"config":{}}`, `{"config":{"model":null,"model_reasoning_effort":null}}`} {
		data, err := composerReply(t, "result", json.RawMessage(response))
		if err != nil || string(data) != `{"model":"","effort":""}` {
			t.Fatalf("unset defaults: %s, %v", data, err)
		}
	}
}

func TestComposerDefaultsRejectsCallerConfigParameters(t *testing.T) {
	for _, input := range []string{"", "null", "[]", "true", `{"includeLayers":true}`, `{"cwd":"C:/other"}`, `{"config":{}}`, `{`} {
		t.Run(input, func(t *testing.T) {
			h, upstream := readyHost(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := h.dispatch(ctx, "composer/defaults", json.RawMessage(input)); err == nil {
				t.Fatal("invalid parameters were accepted")
			}
			select {
			case <-upstream:
				t.Fatal("caller configuration parameters reached the backend")
			default:
			}
		})
	}
}

func TestComposerDefaultsRejectsMalformedResponse(t *testing.T) {
	for _, response := range []string{
		`null`, `[]`, `{}`, `{"config":null}`, `{"config":[]}`,
		`{"config":{"model":12}}`, `{"config":{"model_reasoning_effort":["ultra"]}}`,
		`{"config":{"model":"` + strings.Repeat("m", 513) + `"}}`,
		`{"config":{"model_reasoning_effort":"` + strings.Repeat("e", 33) + `"}}`,
	} {
		data, err := composerReply(t, "result", json.RawMessage(response))
		if err == nil || len(data) != 0 || err.Error() != "invalid composer defaults response" {
			t.Fatalf("invalid config must return only a fixed error: %s, %v", data, err)
		}
	}
}

func TestComposerDefaultsRedactsUpstreamErrors(t *testing.T) {
	for _, code := range []int{-32601, -32603} {
		data, err := composerReply(t, "error", encode(map[string]any{
			"code": code, "message": "private-config-value", "data": map[string]string{"path": "private-config-path"},
		}))
		var rpc *bridge.RPCError
		if len(data) != 0 || !errors.As(err, &rpc) || rpc.Code != code || len(rpc.Data) != 0 || len(rpc.Raw) != 0 || strings.Contains(rpc.Message, "private") {
			t.Fatalf("config error retained private data: %#v", err)
		}
	}
}
