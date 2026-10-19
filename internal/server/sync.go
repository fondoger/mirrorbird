package server

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"

	"mirrorbird/internal/session"
)

const (
	maxSyncBases   = 32
	maxSyncBytes   = 16 << 20
	maxSyncPatches = 2048
	syncBaseTTL    = 10 * time.Minute
)

// Baselines contain only the browser projection. They are immutable, bounded,
// tab-specific and discarded with the sharing lifetime; no per-token journal.
type syncBase struct {
	document snapshotResponse
	bytes    int64
	used     time.Time
}

type snapshotPatch struct {
	Op    string          `json:"op"`
	Path  []any           `json:"path"`
	Value json.RawMessage `json:"value,omitempty"`
}

type syncResponse struct {
	Mode        string            `json:"mode"`
	Cursor      string            `json:"cursor"`
	BaseCursor  string            `json:"baseCursor,omitempty"`
	Snapshot    *snapshotResponse `json:"snapshot,omitempty"`
	Patch       *[]snapshotPatch  `json:"patch,omitempty"`
	ResetReason string            `json:"resetReason,omitempty"`
}

func (s *Server) syncThread(w http.ResponseWriter, r *http.Request, a admission) {
	query, err := boundedQuery(r, "cursor", "resync")
	if err != nil || len(query.Get("cursor")) > 128 {
		failure(w, http.StatusBadRequest, "invalid sync query", "invalid_input")
		return
	}
	resync, err := queryBool(query, "resync")
	if err != nil {
		failure(w, http.StatusBadRequest, "invalid resync value", "invalid_input")
		return
	}
	cursor := query.Get("cursor")
	s.mu.Lock()
	base := s.syncBases[cursor]
	if base != nil && (base.document.Thread.ID != a.threadID || time.Since(base.used) > syncBaseTTL) {
		base = nil
	}
	s.mu.Unlock()

	ctx, cancel := requestContext(r.Context(), a.lifetime, 45*time.Second)
	defer cancel()
	value, err := s.Backend.Read(ctx, a.threadID, session.ReadOptions{Resync: resync})
	if !s.valid(a) {
		accessChanged(w)
		return
	}
	if err != nil {
		backendFailure(w, err, false)
		return
	}
	document := browserSnapshot(value)
	response := syncResponse{Mode: "snapshot", Snapshot: &document, ResetReason: "initial"}
	switch {
	case resync:
		response.ResetReason = "resync"
	case cursor == "":
	case base == nil:
		response.ResetReason = "baseline_missing"
	case base.document.ProjectionGeneration != document.ProjectionGeneration:
		response.ResetReason = "projection_changed"
	default:
		patch := make([]snapshotPatch, 0)
		err = diffSnapshot(reflect.ValueOf(base.document), reflect.ValueOf(document), nil, &patch)
		if errors.Is(err, errPatchLimit) {
			response.ResetReason = "change_limit"
		} else if err != nil {
			failure(w, http.StatusInternalServerError, "cannot encode sync state", "invalid_response")
			return
		} else {
			response = syncResponse{Mode: "delta", BaseCursor: cursor, Patch: &patch}
		}
	}

	weight := syncWeight(reflect.ValueOf(document))
	s.mu.Lock()
	if !s.sharing || s.generation != a.generation || ctx.Err() != nil {
		s.mu.Unlock()
		accessChanged(w)
		return
	}
	if weight > maxSyncBytes {
		response = syncResponse{Mode: "snapshot", Snapshot: &document, ResetReason: "capacity"}
	} else {
		// A request advances its own baseline, never another tab's. Concurrent
		// readers may retain the old immutable value while it is evicted.
		if base != nil {
			s.removeSyncBaseLocked(cursor)
		}
		now := time.Now()
		for key, entry := range s.syncBases {
			if now.Sub(entry.used) > syncBaseTTL {
				s.removeSyncBaseLocked(key)
			}
		}
		for len(s.syncBases) >= maxSyncBases || s.syncBytes+weight > maxSyncBytes {
			var oldest string
			for key, entry := range s.syncBases {
				if oldest == "" || entry.used.Before(s.syncBases[oldest].used) {
					oldest = key
				}
			}
			s.removeSyncBaseLocked(oldest)
		}
		if s.syncBases == nil {
			s.syncBases = make(map[string]*syncBase)
		}
		response.Cursor = rand.Text()
		s.syncBases[response.Cursor] = &syncBase{document: document, bytes: weight, used: now}
		s.syncBytes += weight
	}
	s.mu.Unlock()
	if !s.valid(a) {
		accessChanged(w)
		return
	}
	reply(w, http.StatusOK, response)
}

func (s *Server) removeSyncBaseLocked(cursor string) {
	if base := s.syncBases[cursor]; base != nil {
		s.syncBytes -= base.bytes
		delete(s.syncBases, cursor)
	}
}

var errPatchLimit = errors.New("snapshot change limit reached")

func addSnapshotPatch(out *[]snapshotPatch, op string, path []any, value any) error {
	if len(*out) >= maxSyncPatches {
		return errPatchLimit
	}
	var raw json.RawMessage
	var err error
	if op != "remove" {
		raw, err = json.Marshal(value)
		if err != nil {
			return err
		}
	}
	*out = append(*out, snapshotPatch{Op: op, Path: slices.Clone(path), Value: raw})
	return nil
}

func patchPath(path []any, key any) []any {
	return append(slices.Clone(path), key)
}

// Compare typed immutable snapshots, not re-encoded multi-megabyte JSON trees.
// Opaque JSON fields stay atomic; ordinary text/output uses suffix appends.
func diffSnapshot(before, after reflect.Value, path []any, out *[]snapshotPatch) error {
	if reflect.DeepEqual(before.Interface(), after.Interface()) {
		return nil
	}
	if after.Type() == reflect.TypeFor[json.RawMessage]() {
		return addSnapshotPatch(out, "set", path, after.Interface())
	}
	switch after.Kind() {
	case reflect.Struct:
		left, right := snapshotFields(before), snapshotFields(after)
		keys := make([]string, 0, len(left)+len(right))
		for key := range left {
			keys = append(keys, key)
		}
		for key := range right {
			if _, ok := left[key]; !ok {
				keys = append(keys, key)
			}
		}
		slices.Sort(keys)
		for _, key := range keys {
			old, existed := left[key]
			next, exists := right[key]
			child := patchPath(path, key)
			var err error
			switch {
			case !exists:
				err = addSnapshotPatch(out, "remove", child, nil)
			case !existed:
				err = addSnapshotPatch(out, "set", child, next.Interface())
			default:
				err = diffSnapshot(old, next, child, out)
			}
			if err != nil {
				return err
			}
		}
		return nil
	case reflect.String:
		old, next := before.String(), after.String()
		if old != "" && strings.HasPrefix(next, old) {
			return addSnapshotPatch(out, "append", path, next[len(old):])
		}
	case reflect.Slice:
		if before.IsNil() || after.IsNil() {
			break
		}
		left := make([]reflect.Value, before.Len())
		for i := range left {
			left[i] = before.Index(i)
		}
		keyed := after.Type().Elem().Kind() == reflect.Struct
		for i := 0; i < after.Len(); i++ {
			next := after.Index(i)
			if keyed && (i >= len(left) || left[i].FieldByName("ID").String() != next.FieldByName("ID").String()) {
				found := -1
				for j := i; j < len(left); j++ {
					if left[j].FieldByName("ID").String() == next.FieldByName("ID").String() {
						found = j
						break
					}
				}
				if found >= 0 {
					if err := spliceSnapshot(out, path, i, found-i, []any{}); err != nil {
						return err
					}
					left = slices.Delete(left, i, found)
				} else {
					if err := spliceSnapshot(out, path, i, 0, []any{next.Interface()}); err != nil {
						return err
					}
					left = slices.Insert(left, i, next)
				}
			}
			if i >= len(left) {
				values := make([]any, after.Len()-i)
				for j := range values {
					values[j] = after.Index(i + j).Interface()
				}
				return spliceSnapshot(out, path, i, 0, values)
			}
			if err := diffSnapshot(left[i], next, patchPath(path, i), out); err != nil {
				return err
			}
		}
		if len(left) > after.Len() {
			return spliceSnapshot(out, path, after.Len(), len(left)-after.Len(), []any{})
		}
		return nil
	}
	return addSnapshotPatch(out, "set", path, after.Interface())
}

func spliceSnapshot(out *[]snapshotPatch, path []any, index, count int, values []any) error {
	return addSnapshotPatch(out, "splice", path, struct {
		Index       int   `json:"index"`
		DeleteCount int   `json:"deleteCount"`
		Items       []any `json:"items"`
	}{index, count, values})
}

// The fixed browser schema uses explicit JSON names, one embedded Snapshot and
// omitempty. Explicit fields override the embedded pending-request collection.
func snapshotFields(value reflect.Value) map[string]reflect.Value {
	fields := make(map[string]reflect.Value)
	for i := 0; i < value.NumField(); i++ {
		field, member := value.Type().Field(i), value.Field(i)
		if field.Anonymous {
			for key, child := range snapshotFields(member) {
				fields[key] = child
			}
			continue
		}
		name, options, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		empty := member.IsZero()
		if member.Kind() == reflect.Slice || member.Kind() == reflect.String {
			empty = member.Len() == 0
		}
		if options == "omitempty" && empty {
			continue
		}
		fields[name] = member
	}
	return fields
}

// Conservative retained-memory charge, including structs, backing arrays and
// pointed-to data. Shared strings/patches are deliberately counted each time.
func syncWeight(value reflect.Value) int64 {
	size := int64(value.Type().Size())
	switch value.Kind() {
	case reflect.String:
		size += int64(value.Len())
	case reflect.Pointer:
		if !value.IsNil() {
			size += syncWeight(value.Elem())
		}
	case reflect.Slice:
		if value.Type().Elem().Kind() == reflect.Uint8 {
			size += int64(value.Cap())
		} else {
			size += int64(value.Cap()-value.Len()) * int64(value.Type().Elem().Size())
			for i := 0; i < value.Len(); i++ {
				size += syncWeight(value.Index(i))
			}
		}
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			member := value.Field(i)
			size += syncWeight(member) - int64(member.Type().Size())
		}
	}
	return size
}
