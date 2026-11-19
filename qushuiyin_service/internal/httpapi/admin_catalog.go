package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"qushuiyin/service/internal/auth"
	"qushuiyin/service/internal/parse"
)

func adminReason(raw string) (string, bool) {
	reason := strings.TrimSpace(raw)
	return reason, reason != "" && utf8.RuneCountInString(reason) <= 500
}

func auditChange(ctx context.Context, tx *sql.Tx, actor, action, kind, target, reason, request string, before, after any) error {
	was, err := json.Marshal(before)
	if err != nil {
		return err
	}
	became, err := json.Marshal(after)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO admin_audit(id,operator_id,action,target_type,target_id,before_redacted,after_redacted,reason,request_id) VALUES (?,?,?,?,?,?,?,?,?)",
		auth.ID(), actor, action, kind, target, string(was), string(became), reason, request)
	return err
}

type rowScanner interface{ Scan(...any) error }

type platformCapabilities struct {
	VideoEnabled   bool              `json:"videoEnabled"`
	ImageEnabled   bool              `json:"imageEnabled"`
	CoverEnabled   bool              `json:"coverEnabled"`
	TextEnabled    bool              `json:"textEnabled"`
	DeliveryPolicy map[string]string `json:"deliveryPolicy"`
}

type managedPlatform struct {
	Platform             string               `json:"platform"`
	Status               string               `json:"status"`
	Capabilities         platformCapabilities `json:"capabilities"`
	MaintenanceMessage   string               `json:"maintenanceMessage"`
	AdapterName          string               `json:"adapterName"`
	AdapterVersion       string               `json:"adapterVersion"`
	ShareDomains         []string             `json:"shareDomains"`
	CredentialConfigured bool                 `json:"credentialConfigured"`
	LastVerifiedAt       *time.Time           `json:"lastVerifiedAt"`
	Version              int64                `json:"version"`
	Circuit              string               `json:"circuit"`
}

func (p managedPlatform) policy() parse.Capability {
	return parse.Capability{Status: p.Status, Video: p.Capabilities.VideoEnabled, Image: p.Capabilities.ImageEnabled, Cover: p.Capabilities.CoverEnabled, Text: p.Capabilities.TextEnabled}
}

const platformSelect = "SELECT platform,status,capabilities,message,adapter_name,adapter_version,share_domains,(credential_ref IS NOT NULL AND credential_ref<>''),last_verified_at,version FROM platform_config"

func readManagedPlatform(row rowScanner) (managedPlatform, error) {
	var p managedPlatform
	var capabilities, domains []byte
	err := row.Scan(&p.Platform, &p.Status, &capabilities, &p.MaintenanceMessage, &p.AdapterName, &p.AdapterVersion, &domains, &p.CredentialConfigured, &p.LastVerifiedAt, &p.Version)
	if err == nil {
		err = json.Unmarshal(capabilities, &p.Capabilities)
	}
	if err == nil {
		err = json.Unmarshal(domains, &p.ShareDomains)
	}
	return p, err
}

func loadManagedPlatforms(ctx context.Context, db *sql.DB) ([]managedPlatform, error) {
	rows, err := db.QueryContext(ctx, platformSelect+" ORDER BY platform")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list := []managedPlatform{}
	for rows.Next() {
		p, err := readManagedPlatform(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, p)
	}
	return list, rows.Err()
}

func (s *Mock) adminPlatforms(w http.ResponseWriter, r *http.Request, _ auth.Admin) {
	list, err := loadManagedPlatforms(r.Context(), s.Admin.DB)
	if err != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	for i := range list {
		list[i].Circuit = s.Adapter.CircuitState(list[i].Platform)
	}
	ok(w, 200, dto{"items": list})
}

func (s *Mock) adminPlatformUpdate(w http.ResponseWriter, r *http.Request, a auth.Admin) {
	var req struct {
		ExpectedVersion            int64
		Reason                     string
		Status, MaintenanceMessage *string
		Capabilities               *platformCapabilities
		ShareDomains               *[]string
		VerifiedAt                 *time.Time
	}
	if !s.read(w, r, &req) {
		return
	}
	reason, valid := adminReason(req.Reason)
	if !valid || req.ExpectedVersion < 1 || req.Status == nil && req.MaintenanceMessage == nil && req.Capabilities == nil && req.VerifiedAt == nil && req.ShareDomains == nil ||
		(req.Status != nil && !oneOf(*req.Status, "disabled", "maintenance", "enabled")) ||
		(req.MaintenanceMessage != nil && utf8.RuneCountInString(*req.MaintenanceMessage) > 200) ||
		(req.VerifiedAt != nil && (req.VerifiedAt.Year() < 1000 || req.VerifiedAt.After(time.Now()))) {
		fail(w, 400, "BAD_REQUEST")
		return
	}
	if a.Role == "operator" && (req.Status != nil || req.Capabilities != nil || req.VerifiedAt != nil || req.ShareDomains != nil) {
		fail(w, 403, "ACCESS_DENIED")
		return
	}
	if req.Capabilities != nil {
		if len(req.Capabilities.DeliveryPolicy) != 3 {
			fail(w, 400, "BAD_REQUEST")
			return
		}
		for _, kind := range []string{"video", "image", "cover"} {
			if !oneOf(req.Capabilities.DeliveryPolicy[kind], "direct_preferred", "relay_only", "copy_only") {
				fail(w, 400, "BAD_REQUEST")
				return
			}
		}
	}
	platform := r.PathValue("platform")
	if req.ShareDomains != nil {
		domains, valid := validateShareDomains(platform, *req.ShareDomains)
		if !valid {
			fail(w, 400, "BAD_REQUEST")
			return
		}
		req.ShareDomains = &domains
	}
	ctx := r.Context()
	tx, err := s.Admin.DB.BeginTx(ctx, nil)
	if err != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	defer tx.Rollback()
	before, err := readManagedPlatform(tx.QueryRowContext(ctx, platformSelect+" WHERE platform=? FOR UPDATE", platform))
	if err != nil {
		adminFeedbackReadError(w, err)
		return
	}
	if before.Version != req.ExpectedVersion {
		fail(w, 409, "VERSION_CONFLICT")
		return
	}
	after := before
	if req.Status != nil {
		after.Status = *req.Status
	}
	if req.MaintenanceMessage != nil {
		after.MaintenanceMessage = strings.TrimSpace(*req.MaintenanceMessage)
	}
	if req.Capabilities != nil {
		after.Capabilities = *req.Capabilities
	}
	if req.ShareDomains != nil {
		after.ShareDomains = *req.ShareDomains
	}
	if after.Platform == "bilibili" && after.Capabilities.ImageEnabled {
		fail(w, 400, "BAD_REQUEST")
		return
	}
	if req.VerifiedAt != nil {
		at := req.VerifiedAt.UTC().Truncate(time.Millisecond)
		after.LastVerifiedAt = &at
	}
	after.Version++
	caps, _ := json.Marshal(after.Capabilities)
	domains, _ := json.Marshal(after.ShareDomains)
	_, err = tx.ExecContext(ctx, "UPDATE platform_config SET status=?,capabilities=?,share_domains=?,message=?,last_verified_at=?,version=? WHERE platform=?",
		after.Status, string(caps), string(domains), after.MaintenanceMessage, after.LastVerifiedAt, after.Version, after.Platform)
	if err == nil {
		err = auditChange(ctx, tx, a.ID, "platform_update", "platform", after.Platform, reason, requestID(w),
			dto{"status": before.Status, "capabilities": before.Capabilities, "shareDomains": before.ShareDomains, "maintenanceMessage": before.MaintenanceMessage, "lastVerifiedAt": before.LastVerifiedAt, "version": before.Version},
			dto{"status": after.Status, "capabilities": after.Capabilities, "shareDomains": after.ShareDomains, "maintenanceMessage": after.MaintenanceMessage, "lastVerifiedAt": after.LastVerifiedAt, "version": after.Version})
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	after.Circuit = s.Adapter.CircuitState(after.Platform)
	ok(w, 200, after)
}

const domainSelect = "SELECT d.id,d.platform,d.host,d.status,d.first_seen_at,d.last_seen_at,d.created_at,d.seen_count,d.direct_issued_total,d.direct_fallback_total,d.registered_at,d.version FROM download_domain d WHERE 1=1"

func readAdminDomain(row rowScanner) (dto, error) {
	var id, platform, host, status string
	var first, last, created time.Time
	var observed, issued, fallback, version int64
	var registered *time.Time
	err := row.Scan(&id, &platform, &host, &status, &first, &last, &created, &observed, &issued, &fallback, &registered, &version)
	return dto{"id": id, "platform": platform, "host": host, "status": status, "firstSeenAt": first, "lastSeenAt": last, "createdAt": created, "seenCount": observed,
		"directIssuedTotal": issued, "directFallbackTotal": fallback, "registeredAt": registered, "version": version}, err
}

func (s *Mock) adminDomains(w http.ResponseWriter, r *http.Request, _ auth.Admin) {
	page, valid := s.readAdminPage(w, r)
	if !valid {
		return
	}
	platform, status := r.URL.Query().Get("platform"), r.URL.Query().Get("status")
	host := strings.TrimSpace(r.URL.Query().Get("host"))
	if (platform != "" && !oneOf(platform, "douyin", "kuaishou", "xiaohongshu", "bilibili", "mock")) ||
		(status != "" && !oneOf(status, "observed", "registered", "review", "ignored")) || len(host) > 253 || strings.ContainsAny(host, "/%_:@?#") {
		fail(w, 400, "BAD_REQUEST")
		return
	}
	query := domainSelect
	var args []any
	if platform != "" {
		query += " AND d.platform=?"
		args = append(args, platform)
	}
	if status != "" {
		query += " AND d.status=?"
		args = append(args, status)
	}
	if host != "" {
		query += " AND d.host LIKE ?"
		args = append(args, host+"%")
	}
	query, args = page.query(query, "d", args)
	rows, err := s.Admin.DB.QueryContext(r.Context(), query, args...)
	if err != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	defer rows.Close()
	items := []dto{}
	for rows.Next() {
		item, err := readAdminDomain(rows)
		if err != nil {
			fail(w, 503, "SERVICE_BUSY")
			return
		}
		items = append(items, item)
	}
	if rows.Err() != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	rows.Close()
	totals := []dto{}
	summary, err := s.Admin.DB.QueryContext(r.Context(), `SELECT platform,COUNT(*),SUM(status='registered'),SUM(seen_count),SUM(IF(status='registered',0,seen_count)) FROM download_domain GROUP BY platform ORDER BY platform`)
	if err != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	defer summary.Close()
	for summary.Next() {
		var p string
		var total, registered, seen, unregistered int64
		if summary.Scan(&p, &total, &registered, &seen, &unregistered) != nil {
			fail(w, 503, "SERVICE_BUSY")
			return
		}
		totals = append(totals, dto{"platform": p, "totalHosts": total, "registeredHosts": registered, "seenCount": seen, "unregisteredSeenCount": unregistered})
	}
	if summary.Err() != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	out := page.response(items)
	out["platforms"] = totals
	ok(w, 200, out)
}

func (s *Mock) adminDomainUpdate(w http.ResponseWriter, r *http.Request, a auth.Admin) {
	var req struct {
		ExpectedVersion int64
		Status, Reason  string
	}
	if !s.read(w, r, &req) {
		return
	}
	reason, valid := adminReason(req.Reason)
	if !valid || !opaqueID(r.PathValue("id")) || req.ExpectedVersion < 1 || !oneOf(req.Status, "observed", "registered", "review", "ignored") {
		fail(w, 400, "BAD_REQUEST")
		return
	}
	ctx := r.Context()
	tx, err := s.Admin.DB.BeginTx(ctx, nil)
	if err != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	defer tx.Rollback()
	id := r.PathValue("id")
	before, err := readAdminDomain(tx.QueryRowContext(ctx, domainSelect+" AND d.id=? FOR UPDATE", id))
	if err != nil {
		adminFeedbackReadError(w, err)
		return
	}
	if before["version"] != req.ExpectedVersion {
		fail(w, 409, "VERSION_CONFLICT")
		return
	}
	if before["status"] == req.Status {
		ok(w, 200, before)
		return
	}
	_, err = tx.ExecContext(ctx, `UPDATE download_domain SET status=?,registered_at=IF(?='registered',UTC_TIMESTAMP(3),registered_at),operator_id=?,version=version+1 WHERE id=?`,
		req.Status, req.Status, a.ID, id)
	if err == nil {
		err = auditChange(ctx, tx, a.ID, "domain_update", "download_domain", id, reason, requestID(w),
			dto{"host": before["host"], "status": before["status"], "version": before["version"]}, dto{"host": before["host"], "status": req.Status, "version": req.ExpectedVersion + 1})
	}
	var after dto
	if err == nil {
		after, err = readAdminDomain(tx.QueryRowContext(ctx, domainSelect+" AND d.id=?", id))
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	ok(w, 200, after)
}

func validateShareDomains(platform string, values []string) ([]string, bool) {
	if platform == "" || len(values) == 0 {
		return nil, false
	}
	list := make([]string, 0, len(values))
	for _, value := range values {
		host := strings.ToLower(strings.TrimSpace(value))
		if len(host) == 0 || len(host) > 253 ||
			parse.Platform(&url.URL{Scheme: "https", Host: host}) != platform || slices.Contains(list, host) {
			return nil, false
		}
		for _, label := range strings.Split(host, ".") {
			if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
				return nil, false
			}
			for _, c := range label {
				if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
					return nil, false
				}
			}
		}
		list = append(list, host)
	}
	slices.Sort(list)
	return list, true
}

func (p managedPlatform) allowsSource(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	return slices.ContainsFunc(p.ShareDomains, func(domain string) bool {
		return host == domain || strings.HasSuffix(host, "."+domain)
	})
}

func (s *Mock) sourcePolicy(ctx context.Context, u *url.URL) (parse.Capability, error) {
	platform := parse.Platform(u)
	if !s.ManagedConfig {
		return s.Adapter.Policy[platform], nil
	}
	p, err := s.activePlatform(ctx, platform)
	if errors.Is(err, sql.ErrNoRows) {
		return parse.Capability{}, nil
	}
	if err != nil {
		return parse.Capability{}, parse.Busy
	}
	if !p.allowsSource(u) {
		return parse.Capability{}, parse.Forbidden
	}
	return p.policy(), nil
}

func (s *Mock) activePlatform(ctx context.Context, platform string) (managedPlatform, error) {
	return readManagedPlatform(s.Sessions.DB.QueryRowContext(ctx, platformSelect+" WHERE platform=?", platform))
}

func (s *Mock) platformCacheKey(ctx context.Context, platform, source, key string) (string, error) {
	if !s.ManagedConfig {
		return key, nil
	}
	p, err := s.activePlatform(ctx, platform)
	if err != nil {
		return "", parse.Code("SERVICE_BUSY")
	}
	if p.Status == "maintenance" {
		return "", parse.Maintenance
	}
	if p.Status != "enabled" {
		return "", parse.Unsupported
	}
	u, err := url.Parse(source)
	if err != nil || !p.allowsSource(u) {
		return "", parse.Forbidden
	}
	return fmt.Sprintf("%s:%d:%s", platform, p.Version, key), nil
}

func (s *Mock) managedPolicy(ctx context.Context, platform string) (parse.Capability, error) {
	if !s.ManagedConfig {
		return s.Adapter.Policy[platform], nil
	}
	p, err := s.activePlatform(ctx, platform)
	if errors.Is(err, sql.ErrNoRows) {
		return parse.Capability{}, nil
	}
	return p.policy(), err
}
