package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"qushuiyin/service/internal/auth"
	"qushuiyin/service/internal/media"
	"qushuiyin/service/internal/parse"
)

type sourceSnapshot struct {
	Kind, Role string
	Order      int
	URLs       []string
}

func snapshot(a parse.Asset) sourceSnapshot {
	return sourceSnapshot{a.Kind, a.Role, a.Order, append([]string{a.Source}, a.Alternates...)}
}
func (a sourceSnapshot) asset() parse.Asset {
	v := parse.Asset{Kind: a.Kind, Role: a.Role, Order: a.Order}
	if len(a.URLs) > 0 {
		v.Source = a.URLs[0]
		v.Alternates = a.URLs[1:]
	}
	return v
}
func sourceAsset(res *result, id string) *parse.Asset {
	for i, a := range res.Assets {
		if a.ID == id {
			return &res.Value.Assets[i]
		}
	}
	if res.Cover != nil && res.Cover.ID == id {
		return res.Value.Cover
	}
	return nil
}

func (s *Mock) deliveryPolicy(ctx context.Context, res *result, a asset) (string, error) {
	if s.Relay.Domains == nil {
		return "relay_only", nil
	}
	var state string
	var raw []byte
	if e := s.Sessions.DB.QueryRowContext(ctx, "SELECT status,capabilities FROM platform_config WHERE platform=?", res.Value.Platform).Scan(&state, &raw); e != nil {
		return "", e
	}
	var caps struct {
		Video, Image, Cover                      bool
		Policy                                   map[string]string `json:"deliveryPolicy"`
		VideoEnabled, ImageEnabled, CoverEnabled bool
	}
	if e := json.Unmarshal(raw, &caps); e != nil {
		return "", e
	}
	if state != "enabled" {
		return "", parse.Code("PLATFORM_MAINTENANCE")
	}
	kind := a.Kind
	if a.Role == "cover" {
		kind = "cover"
	}
	if kind == "video" && !caps.VideoEnabled || kind == "image" && !caps.ImageEnabled || kind == "cover" && !caps.CoverEnabled {
		return "", parse.Code("CONTENT_TYPE_UNSUPPORTED")
	}
	p := caps.Policy[kind]
	if p != "direct_preferred" && p != "relay_only" && p != "copy_only" {
		return "", parse.Code("SERVICE_BUSY")
	}
	return p, nil
}

func (s *Mock) callParser(ctx context.Context, source string) (*parse.Result, error) {
	select {
	case s.parsePool <- struct{}{}:
		defer func() { <-s.parsePool }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return s.Adapter.Parse(ctx, source)
}

// Reserve the batch-wide budget before the network call. A crash spends that
// reservation; it never resets the budget or duplicates an unbounded refresh.
func (s *Mock) refreshSources(ctx context.Context, b *batch, res *persistentResult, direct bool) error {
	select {
	case s.refreshSlots <- struct{}{}:
		defer func() { <-s.refreshSlots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	tx, e := s.Sessions.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var state, enc string
	if e = tx.QueryRowContext(ctx, "SELECT status FROM app_user WHERE id=? FOR UPDATE", b.User).Scan(&state); e != nil {
		return e
	}
	if state != "active" {
		return context.Canceled
	}
	if e = tx.QueryRowContext(ctx, "SELECT state_enc FROM save_batch WHERE id=? AND user_id=? FOR UPDATE", b.ID, b.User).Scan(&enc); e != nil {
		return e
	}
	latest := &batch{}
	if e = s.openTask(b.ID, "batch", enc, latest); e != nil {
		return e
	}
	if latest.Cancelled || !time.Now().Before(latest.ExpiresAt) {
		return context.Canceled
	}
	if latest.OutputVersion != b.OutputVersion {
		b.SourceOverrides = latest.SourceOverrides
		b.OutputVersion = latest.OutputVersion
		b.ReparseAttempts = latest.ReparseAttempts
		b.RefreshAttempt = latest.RefreshAttempt
		return nil
	}
	if direct {
		if latest.ReparseAttempts >= s.Settings.Int("direct_reparse_attempts") {
			return sql.ErrNoRows
		}
		latest.ReparseAttempts++
	} else {
		if latest.RefreshAttempt >= s.Settings.Int("resource_refresh_attempts_per_batch") {
			return sql.ErrNoRows
		}
		latest.RefreshAttempt++
	}
	enc, e = s.sealTask(b.ID, "batch", latest)
	if e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, "UPDATE save_batch SET state_enc=?,direct_reparse_attempt=?,refresh_attempt=?,version=version+1 WHERE id=? AND user_id=?", enc, latest.ReparseAttempts, latest.RefreshAttempt, b.ID, b.User)
	if e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	b.ReparseAttempts, b.RefreshAttempt = latest.ReparseAttempts, latest.RefreshAttempt
	if res.history == nil {
		return parse.Code("MEDIA_SOURCE_EXPIRED")
	}
	var hash string
	if e = s.Sessions.DB.QueryRowContext(ctx, "SELECT source_hash FROM parse_result WHERE id=? AND user_id=?", res.ID, b.User).Scan(&hash); e != nil {
		return e
	}
	call, cancel := context.WithTimeout(ctx, s.Settings.Seconds("parser_call_timeout_seconds"))
	defer cancel()
	fresh, e := s.parseCache.resolve(call, hash, true, func() (*parse.Result, error) {
		v, e := s.callParser(call, res.history.URL)
		if e == nil && !sameResources(res.Value, v) {
			return nil, parse.Code("CONTENT_CHANGED")
		}
		return v, e
	})
	if e != nil {
		return e
	}
	overrides := map[string]sourceSnapshot{}
	for i, a := range res.Assets {
		overrides[a.ID] = snapshot(fresh.Assets[i])
	}
	if res.Cover != nil && fresh.Cover != nil {
		overrides[res.Cover.ID] = snapshot(*fresh.Cover)
	}
	tx, e = s.Sessions.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = tx.QueryRowContext(ctx, "SELECT status FROM app_user WHERE id=? FOR UPDATE", b.User).Scan(&state); e != nil {
		return e
	}
	if state != "active" {
		return context.Canceled
	}
	if e = tx.QueryRowContext(ctx, "SELECT state_enc FROM save_batch WHERE id=? AND user_id=? FOR UPDATE", b.ID, b.User).Scan(&enc); e != nil {
		return e
	}
	if e = s.openTask(b.ID, "batch", enc, latest); e != nil {
		return e
	}
	if latest.Cancelled || !time.Now().Before(latest.ExpiresAt) {
		return context.Canceled
	}
	latest.SourceOverrides = overrides
	latest.OutputVersion = auth.ID()
	enc, e = s.sealTask(b.ID, "batch", latest)
	if e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, "UPDATE save_batch SET state_enc=?,version=version+1 WHERE id=? AND user_id=?", enc, b.ID, b.User)
	if e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	b.SourceOverrides, b.OutputVersion = overrides, latest.OutputVersion
	return nil
}

func sameResources(old, fresh *parse.Result) bool {
	if old == nil || fresh == nil || old.Platform != fresh.Platform || old.ContentID == nil || fresh.ContentID == nil || *old.ContentID == "" || *old.ContentID != *fresh.ContentID || old.MediaType != fresh.MediaType || len(old.Assets) != len(fresh.Assets) {
		return false
	}
	for i, a := range old.Assets {
		v := fresh.Assets[i]
		if a.Kind != v.Kind || a.Role != v.Role || a.Order != v.Order {
			return false
		}
		if a.Kind == "image" {
			u, e := url.Parse(a.Source)
			n, e2 := url.Parse(v.Source)
			if e != nil || e2 != nil || u.Path == "" || u.Path != n.Path {
				return false
			}
		}
	}
	return true
}

func (s *Mock) prepareDelivery(ctx context.Context, b *batch, x *item, res *persistentResult) {
	x.Status, x.Mode, x.Failure, x.CopyAvailable = "FAILED", "", "MEDIA_SOURCE_EXPIRED", false
	policy, e := s.deliveryPolicy(ctx, res.result, x.Asset)
	if e != nil {
		x.Failure = parseError(e)
		return
	}
	source := sourceAsset(res.result, x.Asset.ID)
	if source == nil {
		return
	}
	a := *source
	if v, ok := b.SourceOverrides[x.Asset.ID]; ok {
		a = v.asset()
	}
	d := s.Relay.Decide(ctx, res.Value.Platform, policy, a, x.Fallback)
	for policy == "direct_preferred" && !x.Fallback && d.Unregistered && b.ReparseAttempts < s.Settings.Int("direct_reparse_attempts") {
		e = s.refreshSources(ctx, b, res, true)
		if e != nil {
			if e.Error() == "CONTENT_CHANGED" {
				x.Failure = "CONTENT_CHANGED"
				return
			}
			break
		}
		if v, ok := b.SourceOverrides[x.Asset.ID]; ok {
			a = v.asset()
		}
		d = s.Relay.Decide(ctx, res.Value.Platform, policy, a, false)
	}
	x.DirectURL, x.DirectHost, x.MIME = d.URL, d.Host, d.MIME
	x.AvailableUntil = minTime(b.ExpiresAt, d.Expires)
	if d.Size != nil {
		x.Size = *d.Size
	} else {
		x.Size = 0
	}
	if d.Mode == "direct" {
		x.Status, x.Mode, x.Failure = "READY", "direct", ""
		return
	}
	if d.Mode == "relay" {
		content := ""
		if res.Value.ContentID != nil {
			content = *res.Value.ContentID
		}
		file, e := s.Relay.Prepare(ctx, res.Value.Platform, content, s.adapterVersion()+b.OutputVersion, a)
		if errors.Is(e, media.ErrSourceExpired) {
			d.URL = ""
			if r := s.refreshSources(ctx, b, res, false); r == nil {
				if v, ok := b.SourceOverrides[x.Asset.ID]; ok {
					a = v.asset()
				}
				d = s.Relay.Decide(ctx, res.Value.Platform, policy, a, true)
				x.DirectURL, x.DirectHost = d.URL, d.Host
				x.AvailableUntil = minTime(b.ExpiresAt, d.Expires)
				file, e = s.Relay.Prepare(ctx, res.Value.Platform, content, s.adapterVersion()+b.OutputVersion, a)
			} else if r.Error() == "CONTENT_CHANGED" {
				x.Failure = "CONTENT_CHANGED"
				return
			}
		}
		if e == nil {
			x.Status, x.Mode, x.Failure = "READY", "relay", ""
			x.ObjectID = auth.ID()
			x.FileID, x.MIME, x.Hash, x.Size = file.ID, file.MIME, file.Hash, file.Size
			x.Converted = file.Converted
			x.AvailableUntil = minTime(b.ExpiresAt, file.Expires)
			x.DirectURL = ""
			return
		}
		if e.Error() == "FORMAT_UNSUPPORTED" || e.Error() == "SERVICE_BUSY" {
			x.Failure = e.Error()
			x.DirectURL = ""
			return
		}
		if ctx.Err() != nil {
			x.Failure = "PREPARE_TIMEOUT"
			x.DirectURL = ""
			return
		}
		if e.Error() == "FILE_TOO_LARGE" {
			d.Failure = "FILE_TOO_LARGE"
		} else {
			d.Failure = "DIRECT_DOWNLOAD_UNAVAILABLE"
		}
		d.CopyAvailable = d.URL != "" && policy != "relay_only"
	}
	if d.Failure != "" {
		x.Failure = d.Failure
	}
	x.CopyAvailable = d.CopyAvailable
	if !x.CopyAvailable {
		x.DirectURL = ""
	}
}

func (s *Mock) saveDirect(ctx context.Context, tx *sql.Tx, user string, x item) error {
	var enc, host, key any
	var until, mime, size any
	if x.DirectURL != "" && time.Now().Before(x.AvailableUntil) {
		v, e := s.sealTask(x.ID, "direct", x.DirectURL)
		if e != nil {
			return e
		}
		n, _ := strconv.Atoi(strings.TrimPrefix(s.TaskKeyVersion, "v"))
		enc, host, key, until, mime = v, x.DirectHost, n, x.AvailableUntil, x.MIME
		if x.Size > 0 {
			size = x.Size
		}
	}
	_, e := tx.ExecContext(ctx, "UPDATE save_batch_item SET direct_url_enc=?,direct_host=?,direct_expires_at=?,probe_mime_type=?,probe_size_bytes=?,key_version=? WHERE id=? AND user_id=?", enc, host, until, mime, size, key, x.ID, user)
	return e
}

type hostWindow struct {
	Until            time.Time
	Issued, Fallback int64
}

var errCopyQuota = errors.New("copy quota exceeded")

func (s *Mock) commitDeliverySignals(ctx context.Context, tx *sql.Tx, b *batch, x item, windows map[string]hostWindow) error {
	var platform string
	if x.DirectIssuedPending+x.CopyPending+x.RelayIssuedPending > 0 || x.FallbackPending {
		if e := tx.QueryRowContext(ctx, "SELECT platform FROM parse_result WHERE id=? AND user_id=?", b.ResultID, b.User).Scan(&platform); e != nil {
			return e
		}
		for _, m := range []struct {
			outcome, mode string
			n             int64
		}{{"direct", "direct", x.DirectIssuedPending}, {"copy_link", "none", x.CopyPending}, {"relay", "relay", x.RelayIssuedPending}} {
			if m.n > 0 {
				if e := recordMediaMetric(ctx, tx, platform, "download_url_issued", m.outcome, m.mode, m.n); e != nil {
					return e
				}
			}
		}
		if x.FallbackPending {
			if e := recordMediaMetric(ctx, tx, platform, "direct_fallback_requested", "accepted", "direct", 1); e != nil {
				return e
			}
		}
	}
	if x.CopyPending > 0 {
		day := time.Now().In(shanghaiDay).Format("2006-01-02")
		_, e := tx.ExecContext(ctx, "INSERT INTO usage_counter(id,subject_key,stat_date,kind,value) VALUES (?,?,?,'copy_link',0) ON DUPLICATE KEY UPDATE value=value", auth.ID(), b.User, day)
		if e != nil {
			return e
		}
		v, e := tx.ExecContext(ctx, "UPDATE usage_counter SET value=value+? WHERE subject_key=? AND stat_date=? AND kind='copy_link' AND value<=?", x.CopyPending, b.User, day, s.Settings.Int("user_copy_link_per_day")-x.CopyPending)
		if e != nil {
			return e
		}
		n, _ := v.RowsAffected()
		if n != 1 {
			return errCopyQuota
		}
	}
	if x.DirectHost == "" || x.DirectIssuedPending == 0 && !x.FallbackPending {
		return nil
	}
	w := windows[x.DirectHost]
	if !time.Now().Before(w.Until) {
		if int64(len(windows)) >= s.Settings.Int("memory_rate_limit_max_subjects") {
			for h, v := range windows {
				if !time.Now().Before(v.Until) {
					delete(windows, h)
				}
			}
			if int64(len(windows)) >= s.Settings.Int("memory_rate_limit_max_subjects") {
				return errors.New("domain window capacity")
			}
		}
		w = hostWindow{Until: time.Now().Add(s.Settings.Seconds("direct_host_review_window_seconds"))}
	}
	falls := int64(0)
	if x.FallbackPending {
		falls = 1
	}
	w.Issued += x.DirectIssuedPending
	w.Fallback += falls
	windows[x.DirectHost] = w
	if _, e := tx.ExecContext(ctx, "UPDATE download_domain SET direct_issued_total=direct_issued_total+?,direct_fallback_total=direct_fallback_total+?,version=version+1 WHERE host=?", x.DirectIssuedPending, falls, x.DirectHost); e != nil {
		return e
	}
	if w.Fallback >= s.Settings.Int("direct_host_review_min_failures") && w.Issued > 0 && w.Fallback*100 >= w.Issued*s.Settings.Int("direct_host_review_failure_percent") {
		if _, e := tx.ExecContext(ctx, "UPDATE download_domain SET status='review',version=version+1 WHERE host=? AND status='registered'", x.DirectHost); e != nil {
			return e
		}
	}
	return nil
}

func recordMediaMetric(ctx context.Context, tx *sql.Tx, platform, stage, outcome, mode string, n int64) error {
	_, e := tx.ExecContext(ctx, `INSERT INTO daily_metric(id,stat_date,platform,stage,outcome,source_kind,delivery_mode,count,bytes,duration_sum_ms) VALUES (?,?,?,?,?,'none',?,?,0,0) ON DUPLICATE KEY UPDATE count=count+VALUES(count)`, auth.ID(), time.Now().In(shanghaiDay).Format("2006-01-02"), platform, stage, outcome, mode, n)
	return e
}

func deliveryLog(windows, previous map[string]hostWindow, settings interface{ Int(string) int64 }) {
	for host, w := range windows {
		if w.Fallback != previous[host].Fallback && w.Fallback >= settings.Int("direct_host_review_min_failures") && w.Issued > 0 && w.Fallback*100 >= w.Issued*settings.Int("direct_host_review_failure_percent") {
			slog.Warn("download domain needs review", "host", host)
		}
	}
}
