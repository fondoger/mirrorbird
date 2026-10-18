package session

import "encoding/json"

type Item struct {
	ID             string          `json:"id"`
	Type           string          `json:"type"`
	Text           string          `json:"text,omitempty"`
	Content        json.RawMessage `json:"content,omitempty"`
	Status         string          `json:"status,omitempty"`
	Command        string          `json:"command,omitempty"`
	CommandActions json.RawMessage `json:"commandActions,omitempty"`
	Output         string          `json:"aggregatedOutput,omitempty"`
	Changes        json.RawMessage `json:"changes,omitempty"`
	Name           string          `json:"name,omitempty"`
	Summary        []string        `json:"summary,omitempty"`
	Phase          string          `json:"phase,omitempty"`
	Tool           string          `json:"tool,omitempty"`
	Arguments      json.RawMessage `json:"arguments,omitempty"`
	Result         json.RawMessage `json:"result,omitempty"`
	Error          json.RawMessage `json:"error,omitempty"`
	Progress       string          `json:"progress,omitempty"`
	ExitCode       *int            `json:"exitCode,omitempty"`
}

type Turn struct {
	ID          string          `json:"id"`
	Status      string          `json:"status"`
	Items       []Item          `json:"items"`
	ItemsView   string          `json:"itemsView,omitempty"`
	Error       json.RawMessage `json:"error,omitempty"`
	Plan        json.RawMessage `json:"plan,omitempty"`
	Explanation string          `json:"explanation,omitempty"`
	Diff        string          `json:"diff,omitempty"`
	StartedAt   *int64          `json:"startedAt,omitempty"`
	CompletedAt *int64          `json:"completedAt,omitempty"`
	DurationMS  *int64          `json:"durationMs,omitempty"`
}

type Thread struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	CWD    string `json:"cwd"`
	Status struct {
		Type        string   `json:"type"`
		ActiveFlags []string `json:"activeFlags,omitempty"`
	} `json:"status"`
	Turns                []Turn          `json:"turns"`
	Model                string          `json:"model,omitempty"`
	Effort               string          `json:"reasoningEffort,omitempty"`
	ProjectID            string          `json:"projectId,omitempty"`
	Source               json.RawMessage `json:"source,omitempty"`
	HistoryMode          string          `json:"historyMode,omitempty"`
	CanAcceptDirectInput *bool           `json:"canAcceptDirectInput"`
	UpdatedAt            int64           `json:"updatedAt,omitempty"`
}

type Summary struct {
	ID                   string          `json:"id"`
	Name                 string          `json:"name"`
	Status               string          `json:"status"`
	Archived             bool            `json:"archived"`
	CWD                  string          `json:"cwd,omitempty"`
	UpdatedAt            int64           `json:"updatedAt,omitempty"`
	CanAcceptDirectInput *bool           `json:"canAcceptDirectInput"`
	ProjectID            string          `json:"projectId,omitempty"`
	Source               json.RawMessage `json:"source,omitempty"`
}

type ListOptions struct {
	Cursor   string
	Search   string
	Archived bool
	Limit    int
}

type ThreadPage struct {
	Threads    []Summary `json:"threads"`
	NextCursor string    `json:"nextCursor,omitempty"`
}

type ReadOptions struct {
	Archived bool
	Resync   bool
}

type PendingRequest struct {
	ID          json.RawMessage `json:"id"`
	Method      string          `json:"method"`
	Params      json.RawMessage `json:"params"`
	DesktopOnly bool            `json:"desktopOnly,omitempty"`
}

type Snapshot struct {
	Thread           Thread           `json:"thread"`
	ActiveTurnID     string           `json:"activeTurnId,omitempty"`
	Revision         uint64           `json:"revision"`
	Pending          []PendingRequest `json:"pending"`
	HistoryTruncated bool             `json:"historyTruncated,omitempty"`
	ResyncRequired   bool             `json:"resyncRequired,omitempty"`
	ResyncReason     string           `json:"resyncReason,omitempty"`
	ReadOnly         bool             `json:"readOnly,omitempty"`
	ReadOnlyReason   string           `json:"readOnlyReason,omitempty"`
	HistoryCursor    string           `json:"historyCursor,omitempty"`
	HistoryVersion   uint64           `json:"historyVersion"`
	Queue            []QueuedMessage  `json:"queue"`
}

func activeTurn(thread Thread) string {
	for i := len(thread.Turns) - 1; i >= 0; i-- {
		if thread.Turns[i].Status == "inProgress" {
			return thread.Turns[i].ID
		}
	}
	return ""
}

func copySnapshot(in Snapshot) Snapshot {
	out := in
	out.Thread.CanAcceptDirectInput = copyBool(in.Thread.CanAcceptDirectInput)
	out.Thread.Source = cloneRaw(in.Thread.Source)
	out.Queue = append([]QueuedMessage(nil), in.Queue...)
	out.Thread.Status.ActiveFlags = append([]string(nil), in.Thread.Status.ActiveFlags...)
	out.Thread.Turns = make([]Turn, len(in.Thread.Turns))
	for i, turn := range in.Thread.Turns {
		out.Thread.Turns[i] = copyTurn(turn)
	}
	out.Pending = make([]PendingRequest, len(in.Pending))
	for i, request := range in.Pending {
		out.Pending[i] = copyPending(request)
	}
	return out
}

func copyTurn(in Turn) Turn {
	out := in
	out.StartedAt = copyInt64(in.StartedAt)
	out.CompletedAt = copyInt64(in.CompletedAt)
	out.DurationMS = copyInt64(in.DurationMS)
	out.Error = cloneRaw(in.Error)
	out.Plan = cloneRaw(in.Plan)
	out.Items = make([]Item, len(in.Items))
	for i, item := range in.Items {
		out.Items[i] = copyItem(item)
	}
	return out
}

func copyItem(in Item) Item {
	out := in
	out.Content = cloneRaw(in.Content)
	out.CommandActions = cloneRaw(in.CommandActions)
	out.Changes = cloneRaw(in.Changes)
	out.Arguments = cloneRaw(in.Arguments)
	out.Result = cloneRaw(in.Result)
	out.Error = cloneRaw(in.Error)
	out.Summary = append([]string(nil), in.Summary...)
	if in.ExitCode != nil {
		value := *in.ExitCode
		out.ExitCode = &value
	}
	return out
}

func copyPending(in PendingRequest) PendingRequest {
	out := in
	out.ID = cloneRaw(in.ID)
	out.Params = cloneRaw(in.Params)
	return out
}

func copyBool(in *bool) *bool {
	if in == nil {
		return nil
	}

	value := *in
	return &value
}

func copyInt64(in *int64) *int64 {
	if in == nil {
		return nil
	}
	value := *in
	return &value
}

func acceptsInput(in *bool) bool { return in != nil && *in }

func cloneRaw(in json.RawMessage) json.RawMessage { return append(json.RawMessage(nil), in...) }
