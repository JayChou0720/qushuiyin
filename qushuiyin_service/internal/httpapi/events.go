package httpapi

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"time"

	"github.com/go-sql-driver/mysql"
	"qushuiyin/service/internal/auth"
)

var identifier = regexp.MustCompile(`^[A-Za-z0-9_\-]{1,64}$`)
var versionTag = regexp.MustCompile(`^[A-Za-z0-9_.+\-]{1,32}$`)

func opaqueID(x string) bool { return identifier.MatchString(x) }

type clientEvent struct {
	EventID, Name, Stage, Outcome, ClientVersion, OSFamily                  string
	FlowID, ClientActionID, JobID, BatchID, ItemID, DeliveryMode, ErrorCode *string
	DurationMS, Bytes                                                       *uint64
	ClientTime                                                              time.Time
}

func oneOf(value string, allowed ...string) bool {
	for _, x := range allowed {
		if value == x {
			return true
		}
	}
	return false
}

func (s *Mock) validateEvent(raw json.RawMessage, now time.Time) (clientEvent, string) {
	var e clientEvent
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&e) != nil || !opaqueID(e.EventID) {
		return e, "BAD_REQUEST"
	}
	for _, value := range []*string{e.FlowID, e.ClientActionID, e.JobID, e.BatchID, e.ItemID} {
		if value != nil && !opaqueID(*value) {
			return e, "BAD_REQUEST"
		}
	}
	if !oneOf(e.Name, "result_viewed", "preview_started", "preview_finished", "save_action_started", "save_action_finished", "client_download_finished", "album_save_finished") {
		return e, "BAD_REQUEST"
	}
	if !oneOf(e.Stage, "result", "preview", "save", "download", "album") || !oneOf(e.Outcome, "started", "succeeded", "failed", "cancelled", "partial", "unknown") || !versionTag.MatchString(e.ClientVersion) || !oneOf(e.OSFamily, "ios", "android", "windows", "macos", "devtools", "unknown") {
		return e, "BAD_REQUEST"
	}
	if e.DeliveryMode != nil && !oneOf(*e.DeliveryMode, "direct", "relay") {
		return e, "BAD_REQUEST"
	}
	if e.ErrorCode != nil {
		code := *e.ErrorCode
		if messages[code] == "" && messageFor(code) == messages["INTERNAL_ERROR"] && !oneOf(code, "CLIPBOARD_EMPTY", "CLIPBOARD_DENIED", "PRIVACY_DECLINED", "ALBUM_PERMISSION_DENIED", "SYSTEM_PERMISSION_DENIED", "CLIENT_NETWORK_ERROR", "CLIENT_DOWNLOAD_FAILED", "DIRECT_DOWNLOAD_REJECTED", "DIRECT_CONTENT_INVALID", "CLIENT_STORAGE_LOW", "ALBUM_SAVE_FAILED", "USER_CANCELLED", "SAVE_OUTCOME_UNKNOWN") {
			return e, "BAD_REQUEST"
		}
	}
	window := time.Duration(s.settings().Int("client_event_retention_hours")) * time.Hour
	if e.ClientTime.Before(now.Add(-window)) || e.ClientTime.After(now.Add(window)) {
		return e, "BAD_REQUEST"
	}
	return e, ""
}

func (s *Mock) eventOwner(e clientEvent, user string, now time.Time) bool {
	if s.Persistent {
		return s.persistentEventOwner(e, user, now)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var jobResult string
	if e.JobID != nil {
		j := s.jobs[*e.JobID]
		if j == nil || j.User != user || !now.Before(j.Expires) || (e.FlowID != nil && *e.FlowID != j.FlowID) {
			return false
		}
		jobResult = j.ResultID
	}
	if e.BatchID == nil {
		return e.ItemID == nil
	}
	b := s.batches[*e.BatchID]
	if b == nil || b.User != user || !now.Before(b.ExpiresAt) || (jobResult != "" && jobResult != b.ResultID) || (e.FlowID != nil && *e.FlowID != b.FlowID) || (e.ClientActionID != nil && *e.ClientActionID != b.ClientActionID) {
		return false
	}
	return e.ItemID == nil || b.findItem(*e.ItemID) != nil
}

func (s *Mock) ingestEvents(w http.ResponseWriter, r *http.Request, c credential) {
	var request struct{ Events []json.RawMessage }
	if !s.read(w, r, &request) {
		return
	}
	if len(request.Events) == 0 || int64(len(request.Events)) > s.settings().Int("event_batch_max") {
		fail(w, 400, "BAD_REQUEST")
		return
	}
	ctx := r.Context()
	tx, err := s.Sessions.DB.BeginTx(ctx, nil)
	if err != nil {
		privateWriteError(w, err)
		return
	}
	defer tx.Rollback()
	if err = s.lockUser(ctx, tx, c); err != nil {
		privateWriteError(w, err)
		return
	}
	accepted := []string{}
	duplicates := []string{}
	rejected := []dto{}
	now := time.Now().UTC().Truncate(time.Millisecond)
	for _, raw := range request.Events {
		e, code := s.validateEvent(raw, now)
		if code != "" {
			var id any
			if opaqueID(e.EventID) {
				id = e.EventID
			}
			rejected = append(rejected, dto{"eventId": id, "code": code})
			continue
		}
		// Scope deduplication so a collision cannot reveal another user's event.
		digest := auth.Digest(s.Sessions.Key, "event:"+c.User+":"+e.EventID)
		var existing int
		err = tx.QueryRowContext(ctx, "SELECT 1 FROM event_ingest WHERE event_id=?", digest).Scan(&existing)
		if err == nil {
			duplicates = append(duplicates, e.EventID)
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			privateWriteError(w, err)
			return
		}
		if !s.eventOwner(e, c.User, now) {
			rejected = append(rejected, dto{"eventId": e.EventID, "code": "RESOURCE_NOT_FOUND"})
			continue
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO event_ingest(id,event_id,user_id,name,flow_id,client_action_id,job_id,batch_id,item_id,delivery_mode,stage,outcome,error_code,duration_ms,bytes,client_time,received_at,client_version,os_family) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", auth.ID(), digest, c.User, e.Name, e.FlowID, e.ClientActionID, e.JobID, e.BatchID, e.ItemID, e.DeliveryMode, e.Stage, e.Outcome, e.ErrorCode, e.DurationMS, e.Bytes, e.ClientTime.UTC(), now, e.ClientVersion, e.OSFamily)
		if err != nil {
			var mysqlErr *mysql.MySQLError
			if errors.As(err, &mysqlErr) && mysqlErr.Number == 1062 {
				duplicates = append(duplicates, e.EventID)
				continue
			}
			privateWriteError(w, err)
			return
		}
		accepted = append(accepted, e.EventID)
	}
	if err = tx.Commit(); err != nil {
		privateWriteError(w, err)
		return
	}
	ok(w, 200, dto{"acceptedEventIds": accepted, "duplicateEventIds": duplicates, "rejected": rejected})
}
