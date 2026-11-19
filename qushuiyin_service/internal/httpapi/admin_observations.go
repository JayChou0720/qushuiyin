package httpapi

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"qushuiyin/service/internal/auth"
)

type observation struct {
	User, Trace, Platform, Adapter     string
	Stage, Outcome, Source, Mode, Code string
	Started                            time.Time
	Bytes                              int64
}

func observe(ctx context.Context, tx *sql.Tx, o observation) error {
	now := time.Now().UTC()
	ms := max(0, now.Sub(o.Started).Milliseconds())
	if o.Source == "" {
		o.Source = "none"
	}
	if o.Mode == "" {
		o.Mode = "none"
	}
	if o.Platform == "" {
		o.Platform = "unknown"
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO operation_sample(id,created_at,user_id,trace_id,platform,adapter_version,stage,outcome,source_kind,delivery_mode,error_code,duration_ms,bytes)
		VALUES (?,?,?,?,?,?,?,?,?,?,NULLIF(?,''),?,?)`, auth.ID(), now, o.User, o.Trace, o.Platform, o.Adapter, o.Stage, o.Outcome, o.Source, o.Mode, o.Code, ms, o.Bytes)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO daily_metric(id,stat_date,platform,stage,outcome,source_kind,delivery_mode,count,bytes,duration_sum_ms)
		VALUES (?,?,?,?,?,?,?,1,?,?) ON DUPLICATE KEY UPDATE count=count+1,bytes=bytes+VALUES(bytes),duration_sum_ms=duration_sum_ms+VALUES(duration_sum_ms)`,
		auth.ID(), now.In(shanghaiDay).Format("2006-01-02"), o.Platform, o.Stage, o.Outcome, o.Source, o.Mode, o.Bytes, ms)
	return err
}

func (s *Mock) reportTransfer(o observation, w *countedMediaResponse) {
	if w.status == 304 || w.status == 412 {
		return
	}
	o.Bytes = w.sent
	want, err := strconv.ParseInt(w.Header().Get("Content-Length"), 10, 64)
	if (w.status != 200 && w.status != 206) || err != nil || want != w.sent || w.writeFailed {
		o.Outcome, o.Code = "failed", "MEDIA_FETCH_FAILED"
	} else {
		o.Outcome = "succeeded"
	}
	if w.status == 206 && o.Stage == "download" {
		o.Stage = "range_download"
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.settings().Seconds("database_operation_timeout_seconds"))
	defer cancel()
	tx, err := s.Sessions.DB.BeginTx(ctx, nil)
	if err == nil {
		defer tx.Rollback()
		var state string
		err = tx.QueryRowContext(ctx, "SELECT status FROM app_user WHERE id=? FOR UPDATE", o.User).Scan(&state)
		if err == nil && state != "active" {
			return
		}
		if err == nil {
			err = observe(ctx, tx, o)
		}
		if err == nil {
			err = tx.Commit()
		}
	}
	if err != nil {
		slog.Warn("media transfer observation unavailable")
	}
}

func adminPlatformFilter(value string) bool {
	return value == "" || oneOf(value, "douyin", "xiaohongshu", "kuaishou", "bilibili", "mock", "unknown")
}

// Date filters represent complete Shanghai calendar days, as used by daily_metric.
func adminDate(value string) (time.Time, error) {
	return time.ParseInLocation("2006-01-02", value, shanghaiDay)
}

func (s *Mock) adminFailures(w http.ResponseWriter, r *http.Request, _ auth.Admin) {
	page, valid := s.readAdminPage(w, r)
	if !valid {
		return
	}
	platform, stage, trace := r.URL.Query().Get("platform"), r.URL.Query().Get("stage"), r.URL.Query().Get("traceId")
	if !adminPlatformFilter(platform) || (trace != "" && !opaqueID(trace)) || (stage != "" && !oneOf(stage, "parse", "media_prepare", "media_retry", "download", "range_download", "client_download", "preview", "thumbnail", "image", "album", "save")) {
		fail(w, 400, "BAD_REQUEST")
		return
	}
	retention := time.Now().UTC().AddDate(0, 0, -int(s.settings().Int("diagnostic_retention_days")))
	query := `SELECT s.id,s.trace_id,s.platform,s.adapter_version,s.stage,s.error_code,s.duration_ms,s.created_at
	FROM (
		SELECT id,trace_id,platform,adapter_version,stage,error_code,duration_ms,created_at FROM operation_sample
		WHERE error_code IS NOT NULL AND created_at>=?
		UNION ALL
		SELECT j.id,j.id,COALESCE(j.platform,'unknown'),COALESCE(j.adapter_version,''),'parse',j.error_code,
			GREATEST(0,TIMESTAMPDIFF(MICROSECOND,COALESCE(j.started_at,j.created_at),j.finished_at) DIV 1000),j.finished_at
		FROM parse_job j JOIN app_user u ON u.id=j.user_id AND u.status='active'
		WHERE j.status='FAILED' AND j.error_code IS NOT NULL AND j.finished_at>=?
			AND j.metrics_recorded=FALSE
		UNION ALL
		SELECT e.id,COALESCE(e.job_id,e.batch_id,e.id),COALESCE(p.platform,j.platform,'unknown'),COALESCE(p.adapter_version,j.adapter_version,''),IF(e.stage='download','client_download',e.stage),e.error_code,
			e.duration_ms,e.received_at
		FROM event_ingest e JOIN app_user u ON u.id=e.user_id AND u.status='active'
		LEFT JOIN parse_job j ON j.id=e.job_id AND j.user_id=e.user_id
		LEFT JOIN save_batch b ON b.id=e.batch_id AND b.user_id=e.user_id
		LEFT JOIN parse_result p ON p.id=b.result_id AND p.user_id=e.user_id
		WHERE e.name IN ('client_download_finished','album_save_finished','save_action_finished') AND e.error_code IS NOT NULL
			AND e.outcome='failed' AND e.received_at>=?
	) s WHERE 1=1`
	args := []any{retention, retention, retention}
	for _, filter := range []struct{ value, column string }{{platform, "s.platform"}, {stage, "s.stage"}, {trace, "s.trace_id"}} {
		if filter.value != "" {
			query += " AND " + filter.column + "=?"
			args = append(args, filter.value)
		}
	}
	for _, bound := range []struct{ param, condition string }{{"from", " AND s.created_at>=?"}, {"to", " AND s.created_at<?"}} {
		if val := r.URL.Query().Get(bound.param); val != "" {
			date, err := adminDate(val)
			if err != nil {
				fail(w, 400, "BAD_REQUEST")
				return
			}
			if bound.param == "to" {
				date = date.AddDate(0, 0, 1)
			}
			query += bound.condition
			args = append(args, date.UTC())
		}
	}
	query, args = page.query(query, "s", args)
	rows, err := s.Admin.DB.QueryContext(r.Context(), query, args...)
	if err != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	defer rows.Close()
	items := []dto{}
	for rows.Next() {
		var id, trace, platform, version, stage, code string
		var duration *int64
		var at time.Time
		if rows.Scan(&id, &trace, &platform, &version, &stage, &code, &duration, &at) != nil {
			fail(w, 503, "SERVICE_BUSY")
			return
		}
		items = append(items, dto{"id": id, "traceId": trace, "platform": platform, "adapterVersion": version, "stage": stage, "errorCode": code, "durationMs": duration, "createdAt": at})
	}
	if rows.Err() != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	ok(w, 200, page.response(items))
}

func (s *Mock) adminOverview(w http.ResponseWriter, r *http.Request, _ auth.Admin) {
	today := time.Now().In(shanghaiDay).Format("2006-01-02")
	from, to := today, today
	if val := r.URL.Query().Get("from"); val != "" {
		from = val
	}
	if val := r.URL.Query().Get("to"); val != "" {
		to = val
	}
	start, e1 := adminDate(from)
	end, e2 := adminDate(to)
	platform := r.URL.Query().Get("platform")
	if e1 != nil || e2 != nil || end.Before(start) || end.After(dayStart(time.Now()).In(shanghaiDay)) ||
		start.Before(end.AddDate(0, 0, 1-int(s.settings().Int("aggregate_metric_retention_days")))) || !adminPlatformFilter(platform) {
		fail(w, 400, "BAD_REQUEST")
		return
	}
	ctx := r.Context()
	var queuedParse, queuedMedia, storage int64
	err := s.Admin.DB.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM parse_job WHERE status='QUEUED' AND deadline_at>UTC_TIMESTAMP(3)),
		(SELECT COUNT(*) FROM save_batch_item i JOIN save_batch b ON b.id=i.batch_id WHERE i.prepare_status IN ('PENDING','PREPARING') AND i.lease_owner IS NULL AND b.cancel_requested=FALSE AND b.expires_at>UTC_TIMESTAMP(3)),
		(SELECT COALESCE(SUM(size_bytes),0) FROM media_file)`).Scan(&queuedParse, &queuedMedia, &storage)
	if err != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	query := `WITH observed AS (
		SELECT DATE(CONVERT_TZ(s.created_at,'+00:00','+08:00')) day_key, s.platform,s.stage,s.duration_ms
		FROM operation_sample s WHERE s.created_at>=? AND s.created_at<?
	), ranked AS (
		SELECT day_key,platform,stage,duration_ms,ROW_NUMBER() OVER(PARTITION BY day_key,platform,stage ORDER BY duration_ms) pos,
		       COUNT(*) OVER(PARTITION BY day_key,platform,stage) sample_count FROM observed
	), percentiles AS (
		SELECT day_key,platform,stage,MAX(IF(pos=CEIL(sample_count*0.50),duration_ms,NULL)) p50,
			MAX(IF(pos=CEIL(sample_count*0.95),duration_ms,NULL)) p95 FROM ranked GROUP BY day_key,platform,stage
	), stages AS (
		SELECT stat_date day_key,platform,stage,outcome,source_kind,delivery_mode,count,bytes FROM daily_metric WHERE stat_date>=? AND stat_date<=?
		UNION ALL
		SELECT DATE(CONVERT_TZ(j.finished_at,'+00:00','+08:00')),COALESCE(j.platform,'unknown'),'parse',LOWER(j.status),'legacy','none',COUNT(*),0
		FROM parse_job j JOIN app_user u ON u.id=j.user_id AND u.status='active'
		WHERE j.status IN ('SUCCEEDED','FAILED','CANCELLED') AND j.finished_at>=? AND j.finished_at<?
		AND j.metrics_recorded=FALSE
		GROUP BY 1,2,4
		UNION ALL
		SELECT DATE(CONVERT_TZ(e.received_at,'+00:00','+08:00')),COALESCE(p.platform,j.platform,'unknown'),IF(e.stage='download','client_download',e.stage),e.outcome,'client',COALESCE(e.delivery_mode,'none'),COUNT(*),0
		FROM event_ingest e JOIN app_user u ON u.id=e.user_id AND u.status='active'
		LEFT JOIN parse_job j ON j.id=e.job_id AND j.user_id=e.user_id
		LEFT JOIN save_batch b ON b.id=e.batch_id AND b.user_id=e.user_id
		LEFT JOIN parse_result p ON p.id=b.result_id AND p.user_id=e.user_id
		WHERE e.received_at>=? AND e.received_at<? AND e.name IN ('client_download_finished','album_save_finished','save_action_finished')
		GROUP BY 1,2,3,4,6
	) SELECT DATE_FORMAT(m.day_key,'%Y-%m-%d'),m.platform,m.stage,m.outcome,m.source_kind,m.delivery_mode,
		SUM(m.count),SUM(m.bytes),p.p50,p.p95
	FROM stages m LEFT JOIN percentiles p ON p.day_key=m.day_key AND p.platform=m.platform AND p.stage=m.stage
	WHERE (?='' OR m.platform=?) GROUP BY m.day_key,m.platform,m.stage,m.outcome,m.source_kind,m.delivery_mode,p.p50,p.p95
	ORDER BY m.day_key DESC,m.platform,m.stage,m.outcome,m.delivery_mode`
	endUTC := end.AddDate(0, 0, 1).UTC()
	rows, err := s.Admin.DB.QueryContext(ctx, query, start.UTC(), endUTC, from, to, start.UTC(), endUTC, start.UTC(), endUTC, platform, platform)
	if err != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	defer rows.Close()
	items := []dto{}
	for rows.Next() {
		var date, platform, stage, outcome, source, mode string
		var count, bytes int64
		var p50, p95 *int64
		if rows.Scan(&date, &platform, &stage, &outcome, &source, &mode, &count, &bytes, &p50, &p95) != nil {
			fail(w, 503, "SERVICE_BUSY")
			return
		}
		items = append(items, dto{"date": date, "platform": platform, "stage": stage, "outcome": outcome, "sourceKind": source, "deliveryMode": mode, "count": count, "bytes": bytes, "p50Ms": p50, "p95Ms": p95})
	}
	if rows.Err() != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	ok(w, 200, dto{"items": items, "queuedParses": queuedParse, "queuedMedia": queuedMedia, "storageBytes": storage, "storageBudgetBytes": s.settings().Int("media_cache_budget_bytes"), "from": from, "to": to})
}
