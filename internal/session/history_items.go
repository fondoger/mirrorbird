package session

import (
	"context"
	"encoding/json"
	"unicode/utf8"
)

// Summarize legacy full-history replies without dropping access to their items.
// New shims request upstream summaries and do not transfer this history at all.
func summarizeLargeHistory(snapshot *Snapshot) error {
	data, err := json.Marshal(snapshot.Thread)
	if err != nil {
		return ErrResyncRequired
	}
	large := len(data) > maxProjectionBytes
	for index := range snapshot.Thread.Turns {
		turn := &snapshot.Thread.Turns[index]
		if turn.Status == "inProgress" || (!large && len(turn.Items) <= maxItemsPerTurn) {
			continue
		}
		seen := make(map[string]bool)
		lastAgent := -1
		for i, item := range turn.Items {
			if seen[item.ID] || validateItem(item) != nil {
				return ErrResyncRequired
			}
			seen[item.ID] = true
			if item.Type == "agentMessage" {
				lastAgent = i
			}
		}
		items := make([]Item, 0, 2)
		for i, item := range turn.Items {
			if item.Type == "userMessage" || item.Type == "agentMessage" && (item.Phase == "final_answer" || i == lastAgent && item.Phase == "") {
				items = append(items, item)
			}
		}
		turn.Items, turn.ItemsView = items, "summary"
	}
	return nil
}

type ItemsPage struct {
	TurnID         string `json:"turnId"`
	Items          []Item `json:"items"`
	NextCursor     string `json:"nextCursor,omitempty"`
	HistoryVersion uint64 `json:"historyVersion"`
}

// Items reads persisted display details, never replacing the live projection or
// its approval/action authority. Paging is tied to the same history generation.
func (s *Service) Items(ctx context.Context, id, turnID, cursor string, version uint64) (ItemsPage, error) {
	if !validID(turnID) || len(cursor) > 3000 || !utf8.ValidString(cursor) {
		return ItemsPage{}, ErrInvalidInput
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
		return ItemsPage{}, err
	}
	defer s.unpin(p)

	// Start with a small item page, then shrink at the same cursor if its wire
	// envelope or typed display state exceeds the corresponding byte budget.
	for limit := 16; ; limit /= 2 {
		params := map[string]any{"threadId": id, "turnId": turnID, "limit": limit, "sortDirection": "asc"}
		if cursor != "" {
			params["cursor"] = cursor
		}
		data, err := p.conn.client.Call(ctx, "thread/items/list", params)
		if err != nil {
			return ItemsPage{}, err
		}
		var page struct {
			Data []struct {
				TurnID string `json:"turnId"`
				Item   Item   `json:"item"`
			} `json:"data"`
			NextCursor string `json:"nextCursor"`
		}
		if len(data) > maxRPCBytes {
			if limit > 1 {
				continue
			}
			return ItemsPage{}, ErrResyncRequired
		}
		if !utf8.Valid(data) || json.Unmarshal(data, &page) != nil || page.Data == nil || len(page.Data) > limit ||
			len(page.NextCursor) > 3000 || page.NextCursor != "" && (page.NextCursor == cursor || len(page.Data) == 0) {
			return ItemsPage{}, ErrResyncRequired
		}
		out := ItemsPage{TurnID: turnID, Items: make([]Item, 0, len(page.Data)), NextCursor: page.NextCursor, HistoryVersion: version}
		seen := make(map[string]bool)
		for _, entry := range page.Data {
			if entry.TurnID != turnID || seen[entry.Item.ID] || validateItem(entry.Item) != nil {
				return ItemsPage{}, ErrResyncRequired
			}
			seen[entry.Item.ID] = true
			out.Items = append(out.Items, entry.Item)
		}
		encoded, err := json.Marshal(out)
		if err != nil {
			return ItemsPage{}, ErrResyncRequired
		}
		if len(encoded) > maxProjectionBytes {
			if limit > 1 {
				continue
			}
			return ItemsPage{}, ErrResyncRequired
		}
		s.mu.Lock()
		err = s.sameProjectionLocked(ctx, p)
		if err == nil && (p.snapshot.HistoryVersion != version || p.snapshot.ResyncRequired) {
			err = ErrStateChanged
		}
		s.mu.Unlock()
		if err != nil {
			return ItemsPage{}, err
		}
		return out, nil
	}
}
