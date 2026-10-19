package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"mirrorbird/internal/session"
)

// Synthetic large transcript for browser identity and transfer-size checks.
func (b *previewBackend) incrementalStep(action string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	value, exists := b.snapshots["demo-thread"]
	if !exists {
		return false
	}
	switch action {
	case "prepare":
		value.Thread.Turns = nil
		output := strings.Repeat("previous output ", 12_000)
		for i := 0; i < 6; i++ {
			id := fmt.Sprintf("perf-old-%d", i)
			value.Thread.Turns = append(value.Thread.Turns, session.Turn{ID: id, Status: "completed", Items: []session.Item{
				{ID: id + "-tool", Type: "commandExecution", Command: "old command", Output: output, Status: "completed"},
				{ID: id + "-answer", Type: "agentMessage", Text: "Previous answer."},
			}})
		}
		value.Thread.Turns = append(value.Thread.Turns, session.Turn{ID: "perf-live", Status: "inProgress", Items: []session.Item{
			{ID: "perf-user", Type: "userMessage", Content: json.RawMessage(`[{"type":"text","text":"Keep this selection."}]`)},
			{ID: "perf-finished", Type: "commandExecution", Command: "finished command", Output: output, Status: "completed"},
			{ID: "perf-running", Type: "commandExecution", Command: "stream output", Output: "line\n", Status: "inProgress"},
			{ID: "perf-answer", Type: "agentMessage", Text: "hello"},
		}})
		value.ActiveTurnID, value.Thread.Status.Type = "perf-live", "active"
		value.Pending, value.Queue = nil, nil
		value.HistoryCursor, value.HistoryTruncated = "", false
	case "append":
		turn := &value.Thread.Turns[len(value.Thread.Turns)-1]
		if turn.ID != "perf-live" || turn.Status != "inProgress" {
			return false
		}
		turn.Items[2].Output += "next\n"
		turn.Items[3].Text += " 增量 😀"
	case "finish":
		turn := &value.Thread.Turns[len(value.Thread.Turns)-1]
		if turn.ID != "perf-live" {
			return false
		}
		turn.Status, turn.Items[2].Status, turn.Items[3].Phase = "completed", "completed", "final_answer"
		value.ActiveTurnID, value.Thread.Status.Type = "", "idle"
	case "trim":
		value.Thread.Turns = append(value.Thread.Turns[1:], session.Turn{ID: "perf-next", Status: "completed", Items: []session.Item{
			{ID: "perf-next-answer", Type: "agentMessage", Text: "New turn."},
		}})
	default:
		return false
	}
	b.snapshots["demo-thread"] = value
	b.publishLocked("demo-thread", action == "append")
	return true
}
