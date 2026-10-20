package session

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"mirrorbird/internal/attachment"
	"mirrorbird/internal/diagnostics"
)

var ErrInvalidInput = errors.New("invalid companion input")
var ErrUnsupported = errors.New("this Desktop does not support the requested operation")
var ErrAttachmentOutdated = errors.New("restart ChatGPT Desktop to load the updated MirrorBird shim")

type Project struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Roots []struct {
		Path string `json:"path"`
	} `json:"roots"`
}

type ReasoningOption struct {
	Effort      string `json:"reasoningEffort"`
	Description string `json:"description"`
}

type Model struct {
	ID            string            `json:"id"`
	Model         string            `json:"model"`
	Name          string            `json:"displayName"`
	Description   string            `json:"description"`
	Default       bool              `json:"isDefault"`
	DefaultEffort string            `json:"defaultReasoningEffort"`
	Efforts       []ReasoningOption `json:"supportedReasoningEfforts"`
}

type Skill struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Path        string `json:"-"`
}

type ComposerOptions struct {
	Projects   []Project `json:"projects,omitempty"`
	Models     []Model   `json:"models,omitempty"`
	Skills     []Skill   `json:"skills,omitempty"`
	Warnings   []string  `json:"warnings,omitempty"`
	NextCursor string    `json:"nextCursor,omitempty"`
}

type MessageInput struct {
	Text      string `json:"text"`
	Model     string `json:"model,omitempty"`
	Effort    string `json:"effort,omitempty"`
	SkillID   string `json:"skillId,omitempty"`
	SkillName string `json:"skillName,omitempty"`
	SkillPath string `json:"-"`
}

type QueuedMessage struct {
	ID string `json:"id"`
	MessageInput
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	CreatedAt int64  `json:"createdAt"`
}

type ThreadAction struct {
	Kind    string `json:"kind"`
	Name    string `json:"name,omitempty"`
	TurnID  string `json:"turnId,omitempty"`
	QueueID string `json:"queueId,omitempty"`
	MessageInput
}

type HistoryPage struct {
	Turns          []Turn `json:"turns"`
	NextCursor     string `json:"nextCursor,omitempty"`
	HistoryVersion uint64 `json:"historyVersion"`
}

func (s *Service) Options(ctx context.Context, kind, threadID, cursor string) (ComposerOptions, error) {
	if len(cursor) > 4096 || !utf8.ValidString(cursor) {
		return ComposerOptions{}, ErrInvalidInput
	}
	c, err := s.currentConnection()
	if err != nil {
		return ComposerOptions{}, err
	}
	params := map[string]any{"limit": 100}
	if cursor != "" {
		params["cursor"] = cursor
	}
	method := ""
	switch kind {
	case "projects":
		method = "project/list"
	case "models":
		method = "model/list"
	case "skills":
		s.mu.Lock()
		p, e := s.projectionLocked(ctx, threadID)
		if e == nil {
			params = map[string]any{"cwds": []string{p.snapshot.Thread.CWD}}
		}
		s.mu.Unlock()
		if e != nil {
			return ComposerOptions{}, e
		}
		method = "skills/list"
	default:
		return ComposerOptions{}, ErrInvalidInput
	}
	data, err := c.client.Call(ctx, method, params)
	if err != nil {
		if errors.Is(err, attachment.ErrOutdatedShim) {
			return ComposerOptions{}, ErrAttachmentOutdated
		}
		if errors.Is(err, attachment.ErrUnsupportedOperation) {
			return ComposerOptions{}, ErrUnsupported
		}
		return ComposerOptions{}, err
	}
	if len(data) > maxRPCBytes || !utf8.Valid(data) {
		return ComposerOptions{}, ErrResyncRequired
	}
	var out ComposerOptions
	switch kind {
	case "projects":
		var page struct {
			Data []Project `json:"data"`
			Next string    `json:"nextCursor"`
		}
		if json.Unmarshal(data, &page) != nil || page.Data == nil || len(page.Data) > 100 {
			return out, ErrResyncRequired
		}
		out.Projects, out.NextCursor = page.Data, page.Next
	case "models":
		var page struct {
			Data []Model `json:"data"`
			Next string  `json:"nextCursor"`
		}
		if json.Unmarshal(data, &page) != nil || page.Data == nil || len(page.Data) > 100 {
			return out, ErrResyncRequired
		}
		out.Models, out.NextCursor = page.Data, page.Next
	case "skills":
		var result struct {
			Data []struct {
				Skills []struct {
					Name, Description, Path string
					Enabled                 bool
					ShortDescription        string `json:"shortDescription"`
				} `json:"skills"`
				Errors []json.RawMessage `json:"errors"`
			} `json:"data"`
		}
		if json.Unmarshal(data, &result) != nil || result.Data == nil {
			return out, ErrResyncRequired
		}
		out.Skills = []Skill{}
		for _, entry := range result.Data {
			if len(entry.Errors) != 0 {
				out.Warnings = append(out.Warnings, "部分 Skills 加载失败，请在 Desktop 检查。")
			}
			for _, skill := range entry.Skills {
				if !skill.Enabled {
					continue
				}
				description := skill.ShortDescription
				if description == "" {
					description = skill.Description
				}
				out.Skills = append(out.Skills, Skill{
					ID:   base64.RawURLEncoding.EncodeToString([]byte(skill.Path)),
					Name: skill.Name, Description: description, Path: skill.Path,
				})
			}
		}
	}
	if len(out.NextCursor) > 4096 || (cursor != "" && cursor == out.NextCursor) {
		return ComposerOptions{}, ErrResyncRequired
	}
	if current, e := s.currentConnection(); e != nil || current != c {
		return ComposerOptions{}, ErrNoAttachment
	}
	return out, nil
}

func validMessage(input MessageInput) bool {
	return strings.TrimSpace(input.Text) != "" && utf8.ValidString(input.Text) && len(input.Text) <= 32768 &&
		len(input.Model) <= 512 && len(input.Effort) <= 32 && len(input.SkillID) <= 4096
}

func (s *Service) resolveMessage(ctx context.Context, threadID string, input MessageInput) (MessageInput, error) {
	if !validMessage(input) {
		return input, ErrInvalidInput
	}
	// A browser chooses an installed skill handle, never an arbitrary local path.
	input.SkillName, input.SkillPath = "", ""
	if input.SkillID != "" {
		options, err := s.Options(ctx, "skills", threadID, "")
		if err != nil {
			return input, err
		}
		for _, skill := range options.Skills {
			if skill.ID == input.SkillID {
				input.SkillName, input.SkillPath = skill.Name, skill.Path
				return input, nil
			}
		}
		return input, ErrInvalidInput
	}
	return input, nil
}

func turnParams(id string, input MessageInput) map[string]any {
	content := []map[string]any{{"type": "text", "text": input.Text, "text_elements": []any{}}}
	if input.SkillPath != "" {
		content = append(content, map[string]any{"type": "skill", "name": input.SkillName, "path": input.SkillPath})
	}
	params := map[string]any{"threadId": id, "input": content}
	if input.Model != "" {
		params["model"] = input.Model
	}
	if input.Effort != "" {
		params["effort"] = input.Effort
	}
	return params
}

func (s *Service) Enqueue(ctx context.Context, id, messageID string, input MessageInput) (json.RawMessage, error) {
	if !validID(messageID) {
		return nil, ErrInvalidInput
	}
	input, err := s.resolveMessage(ctx, id, input)
	if err != nil {
		return nil, err
	}
	p, release, err := s.acquireCommand(ctx, id)
	if err != nil {
		return nil, err
	}
	defer release()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.sameProjectionLocked(ctx, p); err != nil {
		return nil, err
	}
	for _, message := range p.queue {
		if message.ID == messageID {
			return nil, ErrStateChanged
		}
	}
	if len(p.queue) >= 32 {
		return nil, ErrProjectionCapacity
	}
	p.queue = append(p.queue, QueuedMessage{ID: messageID, MessageInput: input, Status: "queued", CreatedAt: time.Now().Unix()})
	p.pins++ // A confirmed queue keeps its subscription alive without any browser.
	s.changedLocked(id, p)
	return json.RawMessage(`{}`), nil
}

func (s *Service) kickQueueLocked(p *projection) {
	if !p.ready || p.queueRunning || len(p.queue) == 0 || p.queue[0].Status != "queued" ||
		p.stateError() != nil || p.startPending || p.snapshot.ActiveTurnID != "" ||
		p.snapshot.Thread.Status.Type == "active" || len(p.pending) != 0 || p.ctx.Err() != nil {
		return
	}
	p.queueRunning = true
	go s.runQueued(p)
}

func (s *Service) runQueued(original *projection) {
	ctx, cancel := context.WithTimeout(original.ctx, 50*time.Second)
	defer cancel()
	p, release, err := s.acquireCommand(ctx, original.id)
	if err == nil {
		defer release()
		if p != original {
			err = ErrStateChanged
		}
	}
	s.mu.Lock()
	if err != nil || len(original.queue) == 0 || original.queue[0].Status != "queued" ||
		original.snapshot.ActiveTurnID != "" || original.snapshot.Thread.Status.Type == "active" || len(original.pending) != 0 {
		original.queueRunning = false
		if err != nil && s.projections[original.id] == original && original.ctx.Err() == nil && len(original.queue) > 0 && original.queue[0].Status == "queued" {
			original.queue[0].Status = "failed"
			original.queue[0].Error = "无法开始这条消息，请重新同步并核对后重试。"
			s.changedLocked(original.id, original)
		} else if err == nil {
			s.kickQueueLocked(original)
		}
		s.mu.Unlock()
		return
	}
	message := original.queue[0]
	original.queue[0].Status = "sending"
	s.changedLocked(original.id, original)
	s.mu.Unlock()
	_, err = s.callCommand(ctx, original, "turn/start", turnParams(original.id, message.MessageInput), nil)
	s.mu.Lock()
	defer s.mu.Unlock()
	original.queueRunning = false
	if len(original.queue) == 0 || original.queue[0].ID != message.ID {
		return
	}
	if err == nil {
		original.queue = original.queue[1:]
		original.pins--
	} else {
		original.queue[0].Status = "failed"
		original.queue[0].Error = "结果无法确认，可能已开始执行。请先重新同步并核对，再决定是否重试。"
		if errors.Is(err, ErrUnsupported) || errors.Is(err, ErrAttachmentOutdated) {
			original.queue[0].Error = "当前 Desktop 或连接组件不支持发送，请更新并重启 Desktop。"
		}
	}
	if s.projections[original.id] == original {
		s.changedLocked(original.id, original)
	}
}

func (s *Service) History(ctx context.Context, id, cursor string, version uint64) (HistoryPage, error) {
	if cursor == "" || len(cursor) > 4096 || !utf8.ValidString(cursor) {
		return HistoryPage{}, ErrInvalidInput
	}
	s.mu.Lock()
	p, err := s.projectionLocked(ctx, id)
	if err == nil && p.snapshot.HistoryVersion != version {
		err = ErrStateChanged
	}
	if err == nil {
		p.pins++
	}
	s.mu.Unlock()
	if err != nil {
		return HistoryPage{}, err
	}
	defer s.unpin(p)
	var page turnsPage
	anchor, upstream := "", ""
	if strings.HasPrefix(cursor, "before:") {
		anchor = strings.TrimPrefix(cursor, "before:")
	} else if strings.HasPrefix(cursor, "page:") {
		raw, e := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(cursor, "page:"))
		if e != nil {
			return HistoryPage{}, ErrInvalidInput
		}
		upstream = string(raw)
	} else {
		return HistoryPage{}, ErrInvalidInput
	}
	for {
		params := map[string]any{"threadId": id, "limit": maxTurns, "sortDirection": "desc", "itemsView": "summary"}
		if upstream != "" {
			params["cursor"] = upstream
		}
		data, err := p.conn.client.Call(ctx, "thread/turns/list", params)
		if err != nil {
			return HistoryPage{}, err
		}
		page = turnsPage{}
		if len(data) > maxRPCBytes || json.Unmarshal(data, &page) != nil || page.Data == nil ||
			len(page.Data) > maxTurns || len(page.NextCursor) > 3000 || (page.NextCursor != "" && page.NextCursor == upstream) {
			return HistoryPage{}, ErrResyncRequired
		}
		if anchor == "" {
			break
		}
		found := false
		for index, turn := range page.Data {
			if turn.ID == anchor {
				page.Data = page.Data[index+1:]
				found = true
				break
			}
		}
		if found {
			break
		}
		if page.NextCursor == "" {
			return HistoryPage{}, ErrStateChanged
		}
		upstream = page.NextCursor
	}
	reverseTurns(page.Data)
	// Old history is read-only and never becomes authority for live commands.
	history := Snapshot{Thread: Thread{ID: id, Turns: page.Data}, ReadOnly: true}
	if err := normalizeSnapshot(&history); err != nil {
		return HistoryPage{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.sameProjectionLocked(ctx, p); err != nil {
		return HistoryPage{}, err
	}
	if p.snapshot.HistoryVersion != version || p.snapshot.ResyncRequired {
		return HistoryPage{}, ErrStateChanged
	}
	next := ""
	if page.NextCursor != "" {
		next = "page:" + base64.RawURLEncoding.EncodeToString([]byte(page.NextCursor))
	}
	return HistoryPage{Turns: history.Thread.Turns, NextCursor: next, HistoryVersion: version}, nil
}

func (s *Service) CreateThread(ctx context.Context, projectID string, input MessageInput) (result json.RawMessage, err error) {
	started, stage := time.Now(), "validate_input"
	defer func() {
		fields := append(diagnostics.ErrorFields(err), diagnostics.String("stage", stage),
			diagnostics.Int("duration_ms", time.Since(started).Milliseconds()))
		diagnostics.Event("session.create_thread", fields...)
	}()
	if !validID(projectID) || !validMessage(input) || input.SkillID != "" {
		return nil, ErrInvalidInput
	}
	stage = "connection"
	c, err := s.currentConnection()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	stop := context.AfterFunc(c.ctx, cancel)
	defer func() { stop(); cancel() }()
	stage = "project_lookup"
	var project *Project
	cursor := ""
	for {
		page, err := s.Options(ctx, "projects", "", cursor)
		if err != nil {
			return nil, err
		}
		for _, candidate := range page.Projects {
			if candidate.ID == projectID {
				project = &candidate
				break
			}
		}
		if project != nil || page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if project == nil {
		return nil, ErrInvalidInput
	}
	stage = "command_gate"
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.commandGate:
	}
	defer func() { s.commandGate <- struct{}{} }()
	s.mu.Lock()
	valid := s.conn == c && !clientClosed(c.client) && !c.orderFailed && ctx.Err() == nil
	s.mu.Unlock()
	if !valid {
		return nil, ErrStateChanged
	}
	params := map[string]any{"projectId": projectID}
	if len(project.Roots) > 0 {
		params["cwd"] = project.Roots[0].Path
	}
	if input.Model != "" {
		params["model"] = input.Model
	}
	stage = "thread_start"
	data, err := c.client.Call(ctx, "thread/start", params)
	if err != nil {
		if errors.Is(err, attachment.ErrOutdatedShim) {
			return nil, ErrAttachmentOutdated
		}
		if errors.Is(err, attachment.ErrUnsupportedOperation) {
			return nil, ErrUnsupported
		}
		return nil, errors.Join(ErrOutcomeUnknown, err)
	}
	stage = "validate_thread"
	var created struct {
		Thread Thread `json:"thread"`
	}
	if len(data) > maxRPCBytes || !utf8.Valid(data) || json.Unmarshal(data, &created) != nil ||
		!validID(created.Thread.ID) || !acceptsInput(created.Thread.CanAcceptDirectInput) ||
		created.Thread.Status.Type != "idle" || len(created.Thread.Turns) != 0 {
		return nil, ErrOutcomeUnknown
	}
	stage = "watch_thread"
	release, err := s.watchCreatedThread(ctx, c, created.Thread.ID)
	if err != nil {
		return nil, errors.Join(ErrOutcomeUnknown, err)
	}
	defer release()
	s.mu.Lock()
	valid = s.conn == c && !clientClosed(c.client) && !c.orderFailed && ctx.Err() == nil
	s.mu.Unlock()
	if !valid {
		return nil, ErrOutcomeUnknown
	}
	// A fresh thread has no persisted history until its first user message.
	// Resuming it to initialize a projection/queue would fail with "no rollout
	// found". Send the initial turn under the same command gate, then let the
	// browser's normal selection read hydrate it. Neither RPC is retried.
	stage = "turn_start"
	if _, err = c.client.Call(ctx, "turn/start", turnParams(created.Thread.ID, input)); err != nil {
		return nil, errors.Join(ErrOutcomeUnknown, err)
	}
	release()
	stage = "acknowledge"
	s.mu.Lock()
	valid = s.conn == c && !clientClosed(c.client) && !c.orderFailed && ctx.Err() == nil
	s.mu.Unlock()
	if !valid {
		return nil, ErrOutcomeUnknown
	}
	return json.Marshal(map[string]string{"threadId": created.Thread.ID})
}

// Hold a local watch through the first turn so Desktop cannot unsubscribe the
// shared thread while creation is in flight. This never reads persisted history.
func (s *Service) watchCreatedThread(ctx context.Context, c *connection, id string) (func(), error) {
	s.mu.Lock()
	if s.conn != c || clientClosed(c.client) || c.orderFailed || ctx.Err() != nil {
		s.mu.Unlock()
		return nil, ErrStateChanged
	}
	reserved := false
	select {
	case c.watchSlots <- struct{}{}:
		reserved = true
	default:
		if len(s.projections) >= maxProjections {
			var oldest *projection
			for _, candidate := range s.projections {
				if candidate.pins == 0 && candidate.ready && (oldest == nil || candidate.lastUse < oldest.lastUse) {
					oldest = candidate
				}
			}
			if oldest == nil {
				s.mu.Unlock()
				return nil, ErrProjectionCapacity
			}
			s.retireProjectionLocked(oldest)
		}
	}
	s.mu.Unlock()
	if !reserved {
		waitCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		select {
		case c.watchSlots <- struct{}{}:
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, ErrProjectionCapacity
		case <-c.ctx.Done():
			return nil, ErrNoAttachment
		case <-c.client.Done():
			return nil, ErrNoAttachment
		}
	}
	// The watch key includes the thread ID. Creates hold commandGate until this
	// cleanup completes, so it cannot collide with another creation or projection.
	params := map[string]string{"threadId": id, "watchId": "create"}
	attempted, released := false, false
	release := func() {
		if released {
			return
		}
		released = true
		if attempted {
			cleanupCtx, cancel := context.WithTimeout(c.ctx, 2*time.Second)
			_, err := c.client.Call(cleanupCtx, "unwatch", params)
			cancel()
			if err != nil {
				fields := append(diagnostics.ErrorFields(err), diagnostics.String("reason", "watch_release_failed"))
				diagnostics.Event("session.connection_released", fields...)
				c.client.Close()
			}
		}
		<-c.watchSlots
	}
	s.mu.Lock()
	valid := s.conn == c && !clientClosed(c.client) && !c.orderFailed && ctx.Err() == nil
	s.mu.Unlock()
	if !valid {
		release()
		return nil, ErrStateChanged
	}
	attempted = true
	if _, err := c.client.Call(ctx, "watch", params); err != nil {
		// An absent acknowledgement does not prove the shim rejected the watch.
		release()
		return nil, err
	}
	return release, nil
}

func createdThreadID(data json.RawMessage) (string, error) {
	var response struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if len(data) > maxRPCBytes || json.Unmarshal(data, &response) != nil || !validID(response.Thread.ID) {
		return "", ErrOutcomeUnknown
	}
	return response.Thread.ID, nil
}

// Rehydrate in the same pinned lifetime after a deliberate history rewrite.
func (s *Service) rebuild(ctx context.Context, p *projection) error {
	s.mu.Lock()
	if err := s.sameProjectionLocked(ctx, p); err != nil {
		s.mu.Unlock()
		return err
	}
	p.ready = false
	p.initialized = make(chan struct{})
	p.buffer, p.bufferBytes, p.initErr = nil, 0, nil
	p.snapshot.ResyncRequired = false
	p.pending = make(map[string]*pendingEntry)
	s.mu.Unlock()
	err := s.initialize(ctx, p)
	s.mu.Lock()
	if err != nil {
		p.initErr = err
	}
	close(p.initialized)
	s.mu.Unlock()
	return err
}

func (s *Service) Act(ctx context.Context, id string, action ThreadAction) (json.RawMessage, error) {
	if len(action.Name) > 320 || utf8.RuneCountInString(action.Name) > 80 || len(action.TurnID) > 512 || len(action.QueueID) > 512 {
		return nil, ErrInvalidInput
	}
	if action.Kind == "edit" {
		var err error
		action.MessageInput, err = s.resolveMessage(ctx, id, action.MessageInput)
		if err != nil {
			return nil, err
		}
	}
	p, release, err := s.acquireCommand(ctx, id)
	if err != nil {
		return nil, err
	}
	defer release()
	if strings.HasPrefix(action.Kind, "queue-") {
		return s.queueAction(ctx, p, action)
	}
	s.mu.Lock()
	idle := p.snapshot.ActiveTurnID == "" && !p.startPending && p.snapshot.Thread.Status.Type != "active"
	queueEmpty := len(p.queue) == 0
	last := ""
	if n := len(p.snapshot.Thread.Turns); n > 0 {
		last = p.snapshot.Thread.Turns[n-1].ID
	}
	boundary := p.turn(action.TurnID)
	forkable := action.TurnID != "" && (boundary == nil || boundary.Status != "inProgress")
	s.mu.Unlock()
	switch action.Kind {
	case "rename":
		if strings.TrimSpace(action.Name) == "" {
			return nil, ErrInvalidInput
		}
		return s.callCommand(ctx, p, "thread/name/set", map[string]string{"threadId": id, "name": strings.TrimSpace(action.Name)}, nil)
	case "settings":
		if len(action.Model) > 512 || len(action.Effort) > 32 || action.Model == "" {
			return nil, ErrInvalidInput
		}
		params := map[string]any{"threadId": id, "model": action.Model}
		if action.Effort != "" {
			params["effort"] = action.Effort
		}
		return s.callCommand(ctx, p, "thread/settings/update", params, nil)
	case "compact":
		if !idle || !queueEmpty {
			return nil, ErrStateChanged
		}
		return s.callCommand(ctx, p, "thread/compact/start", map[string]string{"threadId": id}, nil)
	case "fork":
		if !forkable {
			return nil, ErrStateChanged
		}
		data, err := s.callCommand(ctx, p, "thread/fork", map[string]any{"threadId": id, "lastTurnId": action.TurnID, "excludeTurns": true, "deferGoalContinuation": true}, nil)
		if err != nil {
			return nil, err
		}
		newID, err := createdThreadID(data)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]string{"threadId": newID})
	case "edit":
		if !idle || !queueEmpty || action.TurnID == "" || action.TurnID != last {
			return nil, ErrStateChanged
		}
		if _, err := s.callCommand(ctx, p, "thread/revert", map[string]string{"threadId": id, "beforeTurnId": action.TurnID}, nil); err != nil {
			return nil, err
		}
		if err := s.rebuild(ctx, p); err != nil {
			return nil, ErrOutcomeUnknown
		}
		return s.callCommand(ctx, p, "turn/start", turnParams(id, action.MessageInput), nil)
	default:
		return nil, ErrInvalidInput
	}
}

func (s *Service) queueAction(ctx context.Context, p *projection, action ThreadAction) (json.RawMessage, error) {
	s.mu.Lock()
	err := s.sameProjectionLocked(ctx, p)
	if err == nil {
		err = p.stateError()
	}
	if err != nil {
		s.mu.Unlock()
		return nil, err
	}
	index := -1
	for i := range p.queue {
		if p.queue[i].ID == action.QueueID {
			index = i
			break
		}
	}
	if index < 0 || p.queue[index].Status == "sending" {
		s.mu.Unlock()
		return nil, ErrStateChanged
	}
	message := p.queue[index]
	active := p.snapshot.ActiveTurnID
	switch action.Kind {
	case "queue-delete":
		p.queue = append(p.queue[:index], p.queue[index+1:]...)
		p.pins--
	case "queue-retry":
		if p.queue[index].Status != "failed" {
			s.mu.Unlock()
			return nil, ErrStateChanged
		}
		p.queue[index].Status, p.queue[index].Error = "queued", ""
	case "queue-guide":
		if index != 0 || message.Status != "queued" {
			s.mu.Unlock()
			return nil, ErrStateChanged
		}
		p.queue[index].Status = "sending"
	default:
		s.mu.Unlock()
		return nil, ErrInvalidInput
	}
	s.changedLocked(p.id, p)
	s.mu.Unlock()
	if action.Kind != "queue-guide" {
		return json.RawMessage(`{}`), nil
	}
	if active != "" {
		_, err := s.callCommand(ctx, p, "turn/interrupt", map[string]string{"threadId": p.id, "turnId": active}, nil)
		if err != nil {
			return nil, s.failQueuedGuide(p, err)
		}
		// Some versions acknowledge interruption before emitting turn/completed.
		for {
			s.mu.Lock()
			done := p.snapshot.ActiveTurnID == ""
			progress := p.conn.progress
			s.mu.Unlock()
			if done {
				break
			}
			select {
			case <-ctx.Done():
				return nil, s.failQueuedGuide(p, ErrOutcomeUnknown)
			case <-p.ctx.Done():
				return nil, s.failQueuedGuide(p, ErrOutcomeUnknown)
			case <-progress:
			}
		}
	}
	if _, err := s.callCommand(ctx, p, "turn/start", turnParams(p.id, message.MessageInput), nil); err != nil {
		return nil, s.failQueuedGuide(p, err)
	}
	s.mu.Lock()
	p.queue = p.queue[1:]
	p.pins--
	s.changedLocked(p.id, p)
	s.mu.Unlock()
	return json.RawMessage(`{}`), nil
}

func (s *Service) failQueuedGuide(p *projection, err error) error {
	s.mu.Lock()
	if len(p.queue) > 0 {
		p.queue[0].Status = "failed"
		p.queue[0].Error = "引导结果无法确认，请重新同步并核对。"
		s.changedLocked(p.id, p)
	}
	s.mu.Unlock()
	return err
}
