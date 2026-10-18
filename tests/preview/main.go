// MirrorBird preview serves only synthetic data on loopback. It cannot discover
// an attachment, start Codex, authenticate a tunnel, or publish a remote service.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"

	"mirrorbird/internal/server"
	"mirrorbird/internal/session"
	"mirrorbird/web"
)

func main() {
	port := flag.Int("port", 43124, "loopback-only preview port")
	flag.Parse()
	if *port < 1024 || *port > 65535 {
		fmt.Fprintln(os.Stderr, "preview port must be 1024–65535")
		os.Exit(1)
	}
	backend := newPreviewBackend()
	api := server.New(backend, web.Assets())
	api.SetSharing(true)
	defer api.SetSharing(false)
	listener, err := net.Listen("tcp4", server.PortAddress(*port))
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot bind local preview:", err)
		os.Exit(1)
	}
	httpServer := &http.Server{Handler: previewHandler(api, backend, *port), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 16 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		api.SetSharing(false)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()
	fmt.Printf("MirrorBird synthetic preview only — no Desktop or tunnel connection.\nOpen http://localhost:%d/ to connect directly to the demo workspace.\n", *port)
	if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, "preview server stopped:", err)
		os.Exit(1)
	}
}

func previewHandler(api *server.Server, backend *previewBackend, port int) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /__preview/stop", func(w http.ResponseWriter, r *http.Request) {
		api.SetSharing(false)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /__preview/reset", func(w http.ResponseWriter, r *http.Request) {
		api.SetSharing(false)
		backend.reset()
		api.SetSharing(true)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /__preview/resync", func(w http.ResponseWriter, r *http.Request) {
		if _, err := backend.Read(r.Context(), "demo-thread", session.ReadOptions{}); err != nil {
			http.Error(w, "preview is disconnected; reset first", http.StatusServiceUnavailable)
			return
		}
		backend.mu.Lock()
		value := backend.snapshots["demo-thread"]
		value.ResyncRequired = true
		value.ResyncReason = "演示：事件积压，请重新同步当前会话"
		backend.snapshots["demo-thread"] = value
		backend.publishLocked("demo-thread", false)
		backend.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /__preview/unknown", func(w http.ResponseWriter, r *http.Request) {
		backend.mu.Lock()
		backend.unknownNext = true
		backend.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /__preview/disconnect", func(w http.ResponseWriter, r *http.Request) {
		backend.mu.Lock()
		backend.connected = false
		backend.broadcastLocked(session.Change{Disconnected: true})
		backend.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.Handle("/", api.Handler())
	return localPreview(mux, port)
}

func localPreview(next http.Handler, port int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		host, hostPort, err := net.SplitHostPort(r.Host)
		if err != nil || hostPort != strconv.Itoa(port) || (host != "localhost" && host != "127.0.0.1") {
			http.Error(w, "this synthetic preview is local-only", http.StatusForbidden)
			return
		}
		if len(r.Header.Values("Sec-Fetch-Site")) > 1 {
			http.Error(w, "cross-origin preview request denied", http.StatusForbidden)
			return
		}
		switch r.Header.Get("Sec-Fetch-Site") {
		case "", "same-origin", "none":
		default:
			http.Error(w, "cross-origin preview request denied", http.StatusForbidden)
			return
		}
		if origins := r.Header.Values("Origin"); len(origins) > 0 {
			origin, err := url.Parse(origins[0])
			if len(origins) != 1 || err != nil || origin.Scheme != "http" || origin.Host != r.Host ||
				origin.User != nil || origin.Opaque != "" || origin.Path != "" || origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" {
				http.Error(w, "cross-origin preview request denied", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

type previewBackend struct {
	mu             sync.Mutex
	connected      bool
	epoch          uint64
	sequence       uint64
	nextTurn       uint64
	nextGeneration uint64
	unknownNext    bool
	catalogue      []session.Summary
	snapshots      map[string]session.Snapshot
	generations    map[string]uint64
	listeners      map[chan session.Change]struct{}
}

func newPreviewBackend() *previewBackend {
	b := &previewBackend{listeners: map[chan session.Change]struct{}{}}
	b.reset()
	return b
}

func (b *previewBackend) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.connected = true
	b.unknownNext = false
	b.epoch++
	b.snapshots = map[string]session.Snapshot{}
	b.generations = map[string]uint64{}
	b.catalogue = []session.Summary{
		{ID: "demo-thread", Name: "演示会话：修复移动端代码预览", Status: "idle", CWD: `C:\repo\demo`, UpdatedAt: 1792281600},
		{ID: "approval-thread", Name: "演示会话：等待工具审批", Status: "active", CWD: `C:\repo\demo`, UpdatedAt: 1792281599},
	}
	// More than 128 ordinary histories, from several synthetic projects, plus a
	// separate archive. Only metadata exists until a particular thread is read.
	projects := []string{"alpha", "beta", "gamma"}
	for i := 1; i <= 178; i++ {
		project := projects[(i-1)%len(projects)]
		b.catalogue = append(b.catalogue, session.Summary{
			ID: fmt.Sprintf("history-%03d", i), Name: fmt.Sprintf("演示历史 %03d · 项目 %s", i, project),
			Status: "idle", CWD: `C:\repo\` + project, UpdatedAt: 1792281500 - int64(i),
		})
	}
	for i := 1; i <= 48; i++ {
		b.catalogue = append(b.catalogue, session.Summary{
			ID: fmt.Sprintf("archive-%03d", i), Name: fmt.Sprintf("已归档演示 %03d · 只读历史", i),
			Status: "idle", Archived: true, CWD: `C:\repo\archive`, UpdatedAt: 1792181500 - int64(i),
		})
	}
	b.publishCatalogueLocked()
}

func flagPointer(value bool) *bool { return &value }

func syntheticSnapshot(summary session.Summary) session.Snapshot {
	value := session.Snapshot{
		Thread: session.Thread{
			ID: summary.ID, Name: summary.Name, CWD: summary.CWD,
			CanAcceptDirectInput: flagPointer(!summary.Archived), Turns: []session.Turn{},
		},
		Pending: []session.PendingRequest{}, ReadOnly: summary.Archived,
	}
	value.Thread.Status.Type = summary.Status
	switch summary.ID {
	case "demo-thread":
		value.Thread.Turns = []session.Turn{{ID: "demo-turn-1", Status: "completed", Items: []session.Item{
			{ID: "demo-user-1", Type: "userMessage", Content: json.RawMessage(`[{"type":"text","text":"检查手机端的代码预览，长代码不应该把页面撑宽。"}]`)},
			{ID: "demo-agent-1", Type: "agentMessage", Text: "已找到问题：代码容器没有设置最小宽度，长路径会撑开整个页面。\n\n我会让代码区域在内部滚动，保留会话正文的阅读宽度。这里是本地演示数据，没有连接真实 Desktop。"},
			{ID: "demo-command-1", Type: "commandExecution", Command: "go test ./internal/server", Output: "ok  mirrorbird/internal/server  0.018s", Status: "completed"},
		}}}
	case "approval-thread":
		value.Thread.Turns = []session.Turn{{ID: "approval-turn", Status: "inProgress", Items: []session.Item{
			{ID: "approval-user", Type: "userMessage", Content: json.RawMessage(`[{"type":"text","text":"运行测试，确认修改通过。"}]`)},
			{ID: "approval-agent", Type: "agentMessage", Text: "准备运行本地测试。请确认下面的命令；这只是审批流程演示，不会实际执行。"},
		}}}
		value.ActiveTurnID = "approval-turn"
		value.Pending = []session.PendingRequest{{ID: json.RawMessage(`9007199254740993`), Method: "item/commandExecution/requestApproval", Params: json.RawMessage(`{"threadId":"approval-thread","turnId":"approval-turn","itemId":"approval-command","command":"go test ./...","cwd":"C:\\repo\\demo","reason":"演示工具审批，不会运行真实命令"}`)}}
	default:
		value.Thread.Turns = []session.Turn{{ID: summary.ID + "-turn", Status: "completed", Items: []session.Item{
			{ID: summary.ID + "-answer", Type: "agentMessage", Text: "这是按需加载的合成历史：" + summary.Name + "。\n\n目录支持搜索、翻页与归档筛选。此预览不会读取本地会话或执行真实模型请求。"},
		}}}
	}
	return value
}

func (b *previewBackend) Connected() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.connected }

func (b *previewBackend) ProjectionGeneration(id string) uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.connected {
		return b.generations[id]
	}
	return 0
}

type previewCursor struct {
	Offset   int    `json:"offset"`
	Search   string `json:"search"`
	Archived bool   `json:"archived"`
}

func (b *previewBackend) ListThreads(ctx context.Context, options session.ListOptions) (session.ThreadPage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return session.ThreadPage{}, err
	}
	if !b.connected {
		return session.ThreadPage{}, session.ErrNoAttachment
	}
	if options.Limit == 0 {
		options.Limit = 50
	}
	if options.Limit < 1 || options.Limit > 100 || len(options.Search) > 512 || len(options.Cursor) > 4096 {
		return session.ThreadPage{}, session.ErrInvalidListOptions
	}
	search := strings.ToLower(strings.TrimSpace(options.Search))
	cursor := previewCursor{Search: search, Archived: options.Archived}
	if options.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(options.Cursor)
		if err != nil || json.Unmarshal(raw, &cursor) != nil || cursor.Offset < 0 || cursor.Search != search || cursor.Archived != options.Archived {
			return session.ThreadPage{}, session.ErrInvalidListOptions
		}
	}
	matches := []session.Summary{}
	for _, item := range b.catalogue {
		if item.Archived == options.Archived && (search == "" || strings.Contains(strings.ToLower(item.Name+" "+item.CWD+" "+item.ID), search)) {
			matches = append(matches, item)
		}
	}
	if cursor.Offset > len(matches) {
		return session.ThreadPage{}, session.ErrInvalidListOptions
	}
	end := min(cursor.Offset+options.Limit, len(matches))
	page := session.ThreadPage{Threads: matches[cursor.Offset:end]}
	if end < len(matches) {
		cursor.Offset = end
		raw, _ := json.Marshal(cursor)
		page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return page, nil
}

func (b *previewBackend) Read(ctx context.Context, id string, options session.ReadOptions) (session.Snapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return session.Snapshot{}, err
	}
	if !b.connected {
		return session.Snapshot{}, session.ErrNoAttachment
	}
	index := b.summaryIndexLocked(id)
	if index < 0 || b.catalogue[index].Archived != options.Archived {
		return session.Snapshot{}, session.ErrInvalidID
	}
	value, exists := b.snapshots[id]
	if !exists {
		value = syntheticSnapshot(b.catalogue[index])
	}
	if !exists || options.Resync {
		b.nextGeneration++
		b.generations[id] = b.nextGeneration
		value.ResyncRequired, value.ResyncReason = false, ""
		b.snapshots[id] = value
		b.publishLocked(id, false)
		value = b.snapshots[id]
	}
	data, _ := json.Marshal(value)
	var copy session.Snapshot
	_ = json.Unmarshal(data, &copy)
	return copy, nil
}

func (b *previewBackend) commandSnapshotLocked(ctx context.Context, id string) (session.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return session.Snapshot{}, err
	}
	if !b.connected {
		return session.Snapshot{}, session.ErrNoAttachment
	}
	value, exists := b.snapshots[id]
	if !exists || value.ReadOnly || value.Thread.CanAcceptDirectInput == nil || !*value.Thread.CanAcceptDirectInput {
		return session.Snapshot{}, session.ErrStateChanged
	}
	if value.ResyncRequired {
		return session.Snapshot{}, session.ErrResyncRequired
	}
	return value, nil
}

func (b *previewBackend) Send(ctx context.Context, id, text, mode, expected string) (json.RawMessage, error) {
	b.mu.Lock()
	value, err := b.commandSnapshotLocked(ctx, id)
	if err != nil {
		b.mu.Unlock()
		return nil, err
	}
	if mode == "steer" {
		if expected == "" || value.ActiveTurnID != expected {
			b.mu.Unlock()
			return nil, session.ErrStateChanged
		}
		last := len(value.Thread.Turns) - 1
		content, _ := json.Marshal([]map[string]string{{"type": "text", "text": text}})
		value.Thread.Turns[last].Items = append(value.Thread.Turns[last].Items, session.Item{ID: "steer-" + strconv.FormatUint(b.sequence+1, 10), Type: "userMessage", Content: content})
		b.snapshots[id] = value
		b.publishLocked(id, false)
		unknown := b.unknownNext
		b.unknownNext = false
		b.mu.Unlock()
		if unknown {
			return nil, session.ErrOutcomeUnknown
		}
		return json.RawMessage(`{}`), nil
	}
	if mode != "start" || expected != "" || value.ActiveTurnID != "" {
		b.mu.Unlock()
		return nil, session.ErrStateChanged
	}
	b.nextTurn++
	turnID := "preview-turn-" + strconv.FormatUint(b.nextTurn, 10)
	content, _ := json.Marshal([]map[string]string{{"type": "text", "text": text}})
	value.Thread.Turns = append(value.Thread.Turns, session.Turn{ID: turnID, Status: "inProgress", Items: []session.Item{
		{ID: turnID + "-user", Type: "userMessage", Content: content},
		{ID: turnID + "-agent", Type: "agentMessage", Text: ""},
	}})
	value.ActiveTurnID = turnID
	value.Thread.Status.Type = "active"
	b.snapshots[id] = value
	b.publishLocked(id, false)
	b.updateSummaryLocked(id)
	epoch := b.epoch
	unknown := b.unknownNext
	b.unknownNext = false
	b.mu.Unlock()
	go b.streamReply(id, turnID, epoch)
	if unknown {
		return nil, session.ErrOutcomeUnknown
	}
	return json.RawMessage(`{}`), nil
}

func (b *previewBackend) streamReply(id, turnID string, epoch uint64) {
	chunks := []string{"已收到手机端的输入。", "\n\n这段回复由本地假后端逐步生成，", "用于验证实时更新、", "运行中追加和中断。", "\n\n真实使用时，", "MirrorBird 会将输入交给", "桌面当前正在运行的同一个会话，", "不会另开一个 agent。"}
	for _, chunk := range chunks {
		time.Sleep(700 * time.Millisecond)
		b.mu.Lock()
		value, exists := b.snapshots[id]
		if !exists || epoch != b.epoch || value.ActiveTurnID != turnID || !b.connected {
			b.mu.Unlock()
			return
		}
		for i := range value.Thread.Turns {
			if value.Thread.Turns[i].ID == turnID {
				for j := range value.Thread.Turns[i].Items {
					if value.Thread.Turns[i].Items[j].ID == turnID+"-agent" {
						value.Thread.Turns[i].Items[j].Text += chunk
					}
				}
			}
		}
		b.snapshots[id] = value
		b.publishLocked(id, true)
		b.mu.Unlock()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	value := b.snapshots[id]
	if b.epoch != epoch || value.ActiveTurnID != turnID || !b.connected {
		return
	}
	value.ActiveTurnID = ""
	value.Thread.Status.Type = "idle"
	value.Thread.Turns[len(value.Thread.Turns)-1].Status = "completed"
	b.snapshots[id] = value
	b.publishLocked(id, false)
	b.updateSummaryLocked(id)
}

func (b *previewBackend) Interrupt(ctx context.Context, id, turnID string) (json.RawMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	value, err := b.commandSnapshotLocked(ctx, id)
	if err != nil {
		return nil, err
	}
	if value.ActiveTurnID != turnID || turnID == "" {
		return nil, session.ErrStateChanged
	}
	value.ActiveTurnID = ""
	value.Thread.Status.Type = "idle"
	value.Thread.Turns[len(value.Thread.Turns)-1].Status = "interrupted"
	value.Pending = nil
	b.snapshots[id] = value
	b.publishLocked(id, false)
	b.updateSummaryLocked(id)
	return json.RawMessage(`{}`), nil
}

func (b *previewBackend) Respond(ctx context.Context, id string, requestID json.RawMessage, decision string) (json.RawMessage, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	value, err := b.commandSnapshotLocked(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, pending := range value.Pending {
		if string(pending.ID) == string(requestID) {
			value.Pending = []session.PendingRequest{}
			value.ActiveTurnID = ""
			value.Thread.Status.Type = "idle"
			last := len(value.Thread.Turns) - 1
			value.Thread.Turns[last].Status = "completed"
			text := "已拒绝这次工具操作。真实工具不会执行。"
			if decision == "accept" {
				text = "已确认这次演示审批。没有执行真实命令。"
			}
			value.Thread.Turns[last].Items = append(value.Thread.Turns[last].Items, session.Item{ID: "approval-done", Type: "agentMessage", Text: text})
			b.snapshots[id] = value
			b.publishLocked(id, false)
			b.updateSummaryLocked(id)
			return json.RawMessage(`{}`), nil
		}
	}
	return nil, session.ErrStateChanged
}

func (b *previewBackend) Subscribe() (<-chan session.Change, func()) {
	ch := make(chan session.Change, 32)
	b.mu.Lock()
	b.listeners[ch] = struct{}{}
	b.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if _, ok := b.listeners[ch]; ok {
				delete(b.listeners, ch)
				close(ch)
			}
		})
	}
}

func (b *previewBackend) publishLocked(id string, contentOnly bool) {
	b.sequence++
	value := b.snapshots[id]
	value.Revision = b.sequence
	b.snapshots[id] = value
	b.broadcastLocked(session.Change{ThreadID: id, Revision: b.sequence, ContentOnly: contentOnly})
}

func (b *previewBackend) publishCatalogueLocked() {
	b.sequence++
	b.broadcastLocked(session.Change{CatalogueChanged: true})
}

func (b *previewBackend) summaryIndexLocked(id string) int {
	for i := range b.catalogue {
		if b.catalogue[i].ID == id {
			return i
		}
	}
	return -1
}

func (b *previewBackend) updateSummaryLocked(id string) {
	if i := b.summaryIndexLocked(id); i >= 0 {
		b.catalogue[i].Status = b.snapshots[id].Thread.Status.Type
		b.catalogue[i].UpdatedAt = time.Now().Unix()
		b.publishCatalogueLocked()
	}
}

func (b *previewBackend) broadcastLocked(change session.Change) {
	for ch := range b.listeners {
		select {
		case ch <- change:
		default:
			delete(b.listeners, ch)
			close(ch)
		}
	}
}
