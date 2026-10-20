package attachment

import (
	"context"
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"

	"mirrorbird/internal/bridge"
)

// composerDefaults reads the effective app-server configuration without giving
// the caller access to configuration layers, paths, credentials or other fields.
// No caller-supplied config/read parameters may reach the backend.
func (h *Host) composerDefaults(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	var input map[string]json.RawMessage
	if json.Unmarshal(params, &input) != nil || input == nil || len(input) != 0 {
		return nil, errors.New("composer/defaults requires an empty object")
	}
	callCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	data, err := h.Mux.Call(callCtx, "config/read", map[string]bool{"includeLayers": false})
	if err != nil {
		var rpc *bridge.RPCError
		if errors.As(err, &rpc) {
			// Retain only the code so unsupported RPCs still get their stable
			// classification. Config errors must not carry upstream text/data.
			return nil, &bridge.RPCError{Code: rpc.Code, Message: "could not read composer defaults"}
		}
		if callCtx.Err() != nil {
			return nil, callCtx.Err()
		}
		return nil, errors.New("could not read composer defaults")
	}
	var response struct {
		Config *struct {
			Model  string `json:"model"`
			Effort string `json:"model_reasoning_effort"`
		} `json:"config"`
	}
	if !utf8.Valid(data) || json.Unmarshal(data, &response) != nil || response.Config == nil ||
		len(response.Config.Model) > 512 || len(response.Config.Effort) > 32 {
		return nil, errors.New("invalid composer defaults response")
	}
	return encode(struct {
		Model  string `json:"model"`
		Effort string `json:"effort"`
	}{Model: response.Config.Model, Effort: response.Config.Effort}), nil
}
