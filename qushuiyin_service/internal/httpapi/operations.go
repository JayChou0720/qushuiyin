package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"
)

func (s *Mock) reportOperationAlerts(ctx context.Context, now time.Time) {
	if s.Sessions.DB == nil {
		return
	}
	settings := s.settings()
	ctx, cancel := context.WithTimeout(ctx, settings.Seconds("database_operation_timeout_seconds"))
	defer cancel()
	db := s.Sessions.DB
	var used int64
	err := db.QueryRowContext(ctx, "SELECT value FROM usage_counter WHERE subject_key='global' AND stat_date=? AND kind='media_egress'", now.In(shanghaiDay).Format("2006-01-02")).Scan(&used)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		slog.Warn("egress check unavailable", "event", "operations_check_failed")
	}
	cap := settings.Int("global_media_egress_per_day_bytes")
	if used > 0 && exceedsPercent(used, cap, settings.Int("media_egress_alert_percent")) {
		slog.Warn("daily media egress nearing cap", "event", "media_egress_high", "bytes", used, "budget", cap)
	}

	cutoff := now.Add(-settings.Seconds("direct_alert_window_seconds")).Truncate(time.Minute)
	rows, err := db.QueryContext(ctx, `SELECT platform,
		SUM(IF(kind IN ('registered','unregistered'),count,0)),
		SUM(IF(kind='unregistered',count,0)),
		SUM(IF(kind='direct_issued',count,0)),
		SUM(IF(kind='direct_fallback',count,0))
		FROM ops_minute_metric WHERE bucket_at>=? GROUP BY platform`, cutoff.UTC())
	if err != nil {
		slog.Warn("download domain check unavailable", "event", "operations_check_failed")
		return
	}
	defer rows.Close()
	minSamples := settings.Int("direct_alert_min_samples")
	for rows.Next() {
		var platform string
		var seen, unregistered, issued, fallback int64
		if rows.Scan(&platform, &seen, &unregistered, &issued, &fallback) != nil {
			slog.Warn("download domain count unavailable", "event", "operations_check_failed")
			return
		}
		if seen >= minSamples && exceedsPercent(unregistered, seen, settings.Int("direct_unregistered_alert_percent")) {
			slog.Warn("unregistered download domain ratio high", "event", "download_domains_unregistered", "platform", platform, "count", unregistered, "total", seen)
		}
		if issued >= minSamples && exceedsPercent(fallback, issued, settings.Int("direct_fallback_alert_percent")) {
			slog.Warn("direct download fallback ratio high", "event", "direct_fallback_high", "platform", platform, "count", fallback, "total", issued)
		}
	}
	if rows.Err() != nil {
		slog.Warn("download domain check interrupted", "event", "operations_check_failed")
	}
}

func exceedsPercent(n, total, threshold int64) bool {
	return total > 0 && float64(n)*100 > float64(total)*float64(threshold)
}
