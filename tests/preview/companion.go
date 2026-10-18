package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"mirrorbird/internal/session"
)

func (b *previewBackend) Options(ctx context.Context, kind, id, cursor string) (session.ComposerOptions, error) {
	if err := ctx.Err(); err != nil {
		return session.ComposerOptions{}, err
	}
	out := session.ComposerOptions{}
	switch kind {
	case "projects":
		if err := json.Unmarshal([]byte(`[{"id":"demo-project","name":"mirrorbird","roots":[{"path":"C:\\repo\\demo"}]},{"id":"other-project","name":"workspace","roots":[{"path":"C:\\repo\\workspace"}]}]`), &out.Projects); err != nil {
			return out, err
		}
	case "models":
		out.Models = []session.Model{
			{ID: "demo-model", Model: "demo-model", Name: "演示模型", Default: true, DefaultEffort: "medium", Efforts: []session.ReasoningOption{{Effort: "low", Description: "快速响应"}, {Effort: "medium", Description: "平衡思考"}, {Effort: "high", Description: "深入思考"}}},
			{ID: "demo-fast", Model: "demo-fast", Name: "演示快速模型", DefaultEffort: "low", Efforts: []session.ReasoningOption{{Effort: "low", Description: "快速响应"}}},
		}
	case "skills":
		out.Skills = []session.Skill{{ID: "demo-review", Name: "code-review", Description: "演示 Skill，不运行真实工具"}, {ID: "demo-design", Name: "frontend-design", Description: "演示页面设计"}}
	default:
		return out, session.ErrInvalidInput
	}
	return out, nil
}

func (b *previewBackend) History(ctx context.Context, id, cursor string, version uint64) (session.HistoryPage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	value := b.snapshots[id]
	end, err := strconv.Atoi(cursor)
	if err != nil || end < 0 || end > len(value.Thread.Turns) || value.HistoryVersion != version {
		return session.HistoryPage{}, session.ErrStateChanged
	}
	start := max(0, end-10)
	page := session.HistoryPage{Turns: value.Thread.Turns[start:end], HistoryVersion: version}
	if start > 0 {
		page.NextCursor = strconv.Itoa(start)
	}
	return page, ctx.Err()
}

func (b *previewBackend) CreateThread(ctx context.Context, projectID string, input session.MessageInput) (json.RawMessage, error) {
	if projectID != "demo-project" && projectID != "other-project" {
		return nil, session.ErrInvalidInput
	}
	if strings.TrimSpace(input.Text) == "" {
		return nil, session.ErrInvalidInput
	}
	b.mu.Lock()
	b.nextTurn++
	id := fmt.Sprintf("created-%d", b.nextTurn)
	title := []rune(strings.Split(input.Text, "\n")[0])
	if len(title) > 40 {
		title = title[:40]
	}
	summary := session.Summary{ID: id, Name: string(title), CWD: `C:\repo\demo`, ProjectID: projectID, UpdatedAt: time.Now().Unix(), Status: "idle"}
	b.catalogue = append([]session.Summary{summary}, b.catalogue...)
	value := syntheticSnapshot(summary)
	value.Thread.Turns = []session.Turn{}
	value.Thread.Model, value.Thread.Effort = input.Model, input.Effort
	b.nextGeneration++
	b.generations[id] = b.nextGeneration
	value.HistoryVersion = b.nextGeneration
	b.snapshots[id] = value
	b.publishCatalogueLocked()
	b.mu.Unlock()
	if _, err := b.Enqueue(ctx, id, id+"-initial", input); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]string{"threadId": id})
}

func (b *previewBackend) Enqueue(ctx context.Context, id, messageID string, input session.MessageInput) (json.RawMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	value, err := b.commandSnapshotLocked(ctx, id)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(input.Text) == "" {
		return nil, session.ErrInvalidInput
	}
	value.Queue = append(value.Queue, session.QueuedMessage{ID: messageID, MessageInput: input, Status: "queued", CreatedAt: time.Now().Unix()})
	b.snapshots[id] = value
	b.publishLocked(id, false)
	return json.RawMessage(`{}`), nil
}

func (b *previewBackend) pumpQueueLocked(id string) {
	value := b.snapshots[id]
	if value.ActiveTurnID != "" || value.ResyncRequired || len(value.Pending) > 0 || len(value.Queue) == 0 || value.Queue[0].Status != "queued" {
		return
	}
	message := value.Queue[0]
	value.Queue[0].Status = "sending"
	value.Thread.Model, value.Thread.Effort = message.Model, message.Effort
	b.snapshots[id] = value
	go func() {
		_, err := b.Send(context.Background(), id, message.Text, "start", "")
		b.mu.Lock()
		defer b.mu.Unlock()
		current := b.snapshots[id]
		if len(current.Queue) == 0 || current.Queue[0].ID != message.ID {
			return
		}
		if err != nil {
			current.Queue[0].Status, current.Queue[0].Error = "failed", "演示操作结果不确定，请核对后再重试。"
		} else {
			current.Queue = current.Queue[1:]
		}
		b.snapshots[id] = current
		b.publishLocked(id, false)
	}()
}

func (b *previewBackend) Act(ctx context.Context, id string, action session.ThreadAction) (json.RawMessage, error) {
	b.mu.Lock()
	value, err := b.commandSnapshotLocked(ctx, id)
	if err != nil {
		b.mu.Unlock()
		return nil, err
	}
	switch action.Kind {
	case "rename":
		value.Thread.Name = action.Name
		index := b.summaryIndexLocked(id)
		b.catalogue[index].Name = action.Name
		b.publishCatalogueLocked()
	case "settings":
		value.Thread.Model, value.Thread.Effort = action.Model, action.Effort
	case "compact":
		if value.ActiveTurnID != "" {
			b.mu.Unlock()
			return nil, session.ErrStateChanged
		}
		if len(value.Thread.Turns) > 0 {
			last := &value.Thread.Turns[len(value.Thread.Turns)-1]
			last.Items = append(last.Items, session.Item{ID: fmt.Sprint("compact-", b.sequence), Type: "contextCompaction"})
		}
	case "edit":
		if value.ActiveTurnID != "" || len(value.Queue) > 0 || value.Thread.Turns[len(value.Thread.Turns)-1].ID != action.TurnID {
			b.mu.Unlock()
			return nil, session.ErrStateChanged
		}
		value.Thread.Turns = value.Thread.Turns[:len(value.Thread.Turns)-1]
		value.HistoryVersion++
		b.snapshots[id] = value
		b.mu.Unlock()
		return b.Send(ctx, id, action.Text, "start", "")
	case "fork":
		index := -1
		for i := range value.Thread.Turns {
			if value.Thread.Turns[i].ID == action.TurnID {
				index = i
				break
			}
		}
		if index < 0 || value.Thread.Turns[index].Status == "inProgress" {
			b.mu.Unlock()
			return nil, session.ErrStateChanged
		}
		b.nextTurn++
		newID := fmt.Sprintf("fork-%d", b.nextTurn)
		encoded, _ := json.Marshal(value)
		var fork session.Snapshot
		if err := json.Unmarshal(encoded, &fork); err != nil {
			b.mu.Unlock()
			return nil, err
		}
		fork.Thread.ID, fork.Thread.Name = newID, value.Thread.Name+" · 分支"
		fork.Thread.Turns = fork.Thread.Turns[:index+1]
		fork.Queue = nil
		fork.Pending = nil
		fork.ActiveTurnID, fork.Thread.Status.Type = "", "idle"
		b.nextGeneration++
		b.generations[newID] = b.nextGeneration
		fork.HistoryVersion = b.nextGeneration
		b.snapshots[newID] = fork
		b.catalogue = append([]session.Summary{{ID: newID, Name: fork.Thread.Name, CWD: fork.Thread.CWD, Status: "idle", UpdatedAt: time.Now().Unix()}}, b.catalogue...)
		b.publishCatalogueLocked()
		b.mu.Unlock()
		return json.Marshal(map[string]string{"threadId": newID})
	case "queue-delete", "queue-guide", "queue-retry":
		index := -1
		for i := range value.Queue {
			if value.Queue[i].ID == action.QueueID {
				index = i
				break
			}
		}
		if index < 0 || value.Queue[index].Status == "sending" {
			b.mu.Unlock()
			return nil, session.ErrStateChanged
		}
		if action.Kind == "queue-delete" {
			value.Queue = append(value.Queue[:index], value.Queue[index+1:]...)
		} else if action.Kind == "queue-retry" {
			value.Queue[index].Status = "queued"
		} else {
			if index != 0 {
				b.mu.Unlock()
				return nil, session.ErrStateChanged
			}
			message := value.Queue[0]
			value.Queue[0].Status = "sending"
			b.snapshots[id] = value
			b.mu.Unlock()
			if value.ActiveTurnID != "" {
				if _, err := b.Interrupt(ctx, id, value.ActiveTurnID); err != nil {
					return nil, err
				}
			}
			if _, err := b.Send(ctx, id, message.Text, "start", ""); err != nil {
				return nil, err
			}
			b.mu.Lock()
			value = b.snapshots[id]
			value.Queue = value.Queue[1:]
		}
	default:
		b.mu.Unlock()
		return nil, session.ErrInvalidInput
	}
	b.snapshots[id] = value
	b.publishLocked(id, false)
	b.mu.Unlock()
	return json.RawMessage(`{}`), nil
}
