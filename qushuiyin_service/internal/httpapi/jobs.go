package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"qushuiyin/service/internal/auth"
	"qushuiyin/service/internal/parse"
)

type jobRequest struct {
	ShareText    string
	SelectedURL  *string
	Source       string
	ForceRefresh bool
	FlowID       string
}
type job struct {
	ID, User, Platform, ResultID, Failure, Status string
	FlowID                                        string
	Created, Deadline, Expires                    time.Time
	cancel                                        context.CancelFunc
	History                                       *historySource
}
type historySource struct {
	URL string `json:"url"`
	Key string `json:"key"`
}
type asset struct {
	ID, Kind, Role string
	Order          int
	Version        int64
}
type result struct {
	ID, User             string
	Value                *parse.Result
	Assets               []asset
	Cover                *asset
	CreatedAt, ExpiresAt time.Time
}

var urlPattern = regexp.MustCompile("https?://[^\\s<>\"'，。！？；、）》】]+")

func (s *Mock) createJob(w http.ResponseWriter, r *http.Request, c credential) {
	var req jobRequest
	if !s.read(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.ShareText) == "" {
		fail(w, 400, "INPUT_EMPTY")
		return
	}
	if int64(utf8.RuneCountInString(req.ShareText)) > s.Settings.Int("max_share_text_chars") {
		fail(w, 400, "INPUT_TOO_LONG")
		return
	}
	if (req.Source != "paste" && req.Source != "manual" && req.Source != "history") || req.FlowID == "" || len(req.FlowID) > 64 {
		fail(w, 400, "BAD_REQUEST")
		return
	}
	seen := map[string]bool{}
	var candidates []dto
	selected := ""
	for _, raw := range urlPattern.FindAllString(req.ShareText, -1) {
		raw = strings.TrimRight(raw, ".,;!)］」』")
		if seen[raw] {
			continue
		}
		seen[raw] = true
		u, e := url.Parse(raw)
		if e != nil {
			continue
		}
		candidates = append(candidates, dto{"url": raw, "platform": parse.Platform(u), "displayHost": u.Hostname()})
		if req.SelectedURL != nil && *req.SelectedURL == raw {
			selected = raw
		}
	}
	if len(candidates) == 0 {
		fail(w, 400, "NO_LINK_FOUND")
		return
	}
	if int64(len(candidates)) > s.Settings.Int("max_candidate_urls") {
		fail(w, 400, "TOO_MANY_LINKS")
		return
	}
	if req.SelectedURL == nil {
		if len(candidates) > 1 {
			send(w, 422, "MULTIPLE_LINKS", nil, dto{"candidates": candidates})
			return
		}
		selected = candidates[0]["url"].(string)
	}
	if selected == "" {
		fail(w, 400, "LINK_INVALID")
		return
	}
	u, _ := url.Parse(selected)
	p := parse.Platform(u)
	if p == "" {
		fail(w, 400, "LINK_NOT_ALLOWED")
		return
	}
	switch s.Adapter.Policy[p].Status {
	case "enabled":
	case "maintenance":
		fail(w, 503, "PLATFORM_MAINTENANCE")
		return
	default:
		fail(w, 422, "PLATFORM_UNSUPPORTED")
		return
	}
	now := time.Now().UTC()
	if s.Persistent {
		s.createPersistentParse(w, r, c, req, selected, p)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(now)
	key, record, proceed := s.replay(w, r, c, "parse", req, now)
	if !proceed {
		return
	}
	if record.ID != "" {
		j := s.jobs[record.ID]
		if j == nil {
			fail(w, 410, "JOB_EXPIRED")
			return
		}
		s.jobAccepted(w, j)
		return
	}
	active := 0
	for _, v := range s.jobs {
		if v.User == c.User && v.active() && now.Before(v.Deadline) {
			active++
		}
	}
	if int64(active) >= s.Settings.Int("user_active_parse_jobs") || s.parsePool == nil || int64(len(s.jobs)) >= s.Settings.Int("memory_mock_max_entries") {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	if !s.minute("parse:"+c.User, s.Settings.Int("user_parse_per_minute")) {
		fail(w, 429, "RATE_LIMITED")
		return
	}
	j := &job{ID: auth.ID(), User: c.User, Platform: p, FlowID: req.FlowID, Status: "QUEUED", Created: now, Deadline: now.Add(s.Settings.Seconds("parse_job_timeout_seconds")), Expires: now.Add(s.Settings.Seconds("result_ttl_seconds"))}
	// Preserve the selected URL verbatim for replay. Normalize only the history key.
	canonical := *u
	canonical.Host = strings.ToLower(canonical.Host)
	canonical.Fragment, canonical.RawFragment = "", ""
	j.History = &historySource{selected, auth.Digest(s.Sessions.Key, "history:"+c.User+":"+canonical.String())}
	jobCtx, cancel := context.WithDeadline(s.parseContext, j.Deadline)
	j.cancel = cancel
	s.jobs[j.ID] = j
	record.ID = j.ID
	s.keys[key] = record
	s.jobAccepted(w, j)
	source := parse.MockSourceKey(p, u, selected)
	cacheKey := auth.Digest(s.Sessions.Key, "parse-cache:"+p+":"+parse.MockVersion+":anonymous:"+source)
	go s.runJob(jobCtx, j, selected, cacheKey, req.ForceRefresh)
}

func (j *job) active() bool { return j.Status == "QUEUED" || j.Status == "RUNNING" }

func (s *Mock) runJob(ctx context.Context, j *job, selected, cacheKey string, forceRefresh bool) {
	defer j.cancel()
	select {
	case s.parsePool <- struct{}{}:
		defer func() { <-s.parsePool }()
	case <-ctx.Done():
	}
	s.mu.Lock()
	if s.jobs[j.ID] != j || !j.active() {
		s.mu.Unlock()
		return
	}
	if ctx.Err() != nil {
		j.Status, j.Failure = "FAILED", "PARSE_TIMEOUT"
		s.mu.Unlock()
		return
	}
	j.Status = "RUNNING"
	s.mu.Unlock()

	parsed, err := s.parseCache.resolve(ctx, cacheKey, forceRefresh, func() (*parse.Result, error) {
		return s.Adapter.Parse(ctx, selected)
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.jobs[j.ID] != j || !j.active() {
		return
	}
	if ctx.Err() != nil || !time.Now().Before(j.Deadline) {
		j.Status, j.Failure = "FAILED", "PARSE_TIMEOUT"
		return
	}
	if err != nil {
		j.Status, j.Failure = "FAILED", parseError(err)
		return
	}
	res := &result{ID: auth.ID(), User: j.User, Value: parsed, CreatedAt: time.Now().UTC(), ExpiresAt: j.Expires}
	for _, x := range parsed.Assets {
		res.Assets = append(res.Assets, asset{ID: auth.ID(), Kind: x.Kind, Role: x.Role, Order: x.Order})
	}
	if x := parsed.Cover; x != nil {
		res.Cover = &asset{ID: auth.ID(), Kind: x.Kind, Role: x.Role, Order: x.Order}
	}
	s.results[res.ID] = res
	j.Status, j.ResultID = "SUCCEEDED", res.ID
}
func (s *Mock) jobAccepted(w http.ResponseWriter, j *job) {
	ok(w, 202, dto{"jobId": j.ID, "status": j.Status, "pollAfterMs": s.Settings.Int("poll_interval_ms"), "deadlineAt": j.Deadline})
}
func (s *Mock) jobStatus(w http.ResponseWriter, r *http.Request, c credential) {
	if s.Persistent {
		s.persistentJobStatus(w, r, c)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobs[r.PathValue("jobId")]
	if j == nil || j.User != c.User {
		fail(w, 404, "RESOURCE_NOT_FOUND")
		return
	}
	if !time.Now().Before(j.Expires) {
		fail(w, 410, "JOB_EXPIRED")
		return
	}
	if j.active() {
		switch {
		case r.Method == "DELETE":
			j.Status = "CANCELLED"
			j.cancel()
		case !time.Now().Before(j.Deadline):
			j.Status, j.Failure = "FAILED", "PARSE_TIMEOUT"
			j.cancel()
		}
	}
	var e, id any
	var history *historySource
	if j.Failure != "" {
		failure := issue(j.Failure)
		failure["traceId"] = j.ID
		e = failure
	}
	if j.ResultID != "" {
		id = j.ResultID
		history = j.History
	}
	w.Header().Set("Cache-Control", "no-store")
	ok(w, 200, dto{"jobId": j.ID, "status": j.Status, "platform": j.Platform, "resultId": id, "createdAt": j.Created, "deadlineAt": j.Deadline, "error": e, "historySource": history})
}
func (a asset) public() dto {
	return dto{"assetId": a.ID, "type": a.Kind, "role": a.Role, "order": a.Order, "mimeType": nil, "width": nil, "height": nil, "sizeBytes": nil, "converted": false, "previewUrl": nil, "previewExpiresAt": nil, "availability": "candidate", "reasonCode": nil}
}
func (s *Mock) resultData(w http.ResponseWriter, r *http.Request, c credential) {
	if s.Persistent {
		res, err := s.loadPersistentResult(r.Context(), c.User, r.PathValue("resultId"))
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				fail(w, 404, "RESOURCE_NOT_FOUND")
			} else {
				fail(w, 503, "SERVICE_BUSY")
			}
			return
		}
		view := Mock{Relay: s.Relay, results: map[string]*result{res.ID: res.result}}
		view.resultData(w, r, c)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	x := s.results[r.PathValue("resultId")]
	if x == nil || x.User != c.User {
		fail(w, 404, "RESOURCE_NOT_FOUND")
		return
	}
	if !time.Now().Before(x.ExpiresAt) {
		fail(w, 410, "RESULT_EXPIRED")
		return
	}
	assets := make([]dto, 0, len(x.Assets))
	for _, a := range x.Assets {
		assets = append(assets, a.public())
	}
	var cover any
	if x.Cover != nil {
		cover = x.Cover.public()
	}
	v := x.Value
	ok(w, 200, dto{"resultId": x.ID, "platform": v.Platform, "contentId": v.ContentID, "mediaType": v.MediaType, "title": v.Title, "description": v.Description, "authorName": v.AuthorName, "durationMs": nil, "assetCount": len(assets), "assets": assets, "cover": cover, "warnings": v.Warnings, "createdAt": x.CreatedAt, "expiresAt": x.ExpiresAt, "capabilities": dto{"canPreview": true, "canSave": true, "canSaveCover": x.Cover != nil, "canCopyText": v.Description != nil}})
}
