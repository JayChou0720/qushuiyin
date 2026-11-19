package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (s *Mock) relayDownload(w http.ResponseWriter, r *http.Request, user, bid, oid string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		fail(w, 405, "BAD_REQUEST")
		return
	}
	started := time.Now()
	var key, mime, hash, platform, adapter string
	var size int64
	err := s.Sessions.DB.QueryRowContext(r.Context(), `SELECT f.storage_key,f.mime_type,f.sha256,f.size_bytes,p.platform,p.adapter_version FROM media_object o JOIN media_file f ON f.id=o.file_id JOIN save_batch_item i ON i.media_object_id=o.id JOIN save_batch b ON b.id=i.batch_id JOIN parse_result p ON p.id=b.result_id AND p.user_id=b.user_id WHERE o.id=? AND o.user_id=? AND i.user_id=? AND b.user_id=? AND b.id=? AND o.status='ready' AND f.status='ready' AND i.prepare_status='READY' AND b.cancel_requested=FALSE AND o.expires_at>UTC_TIMESTAMP(3) AND f.expires_at>UTC_TIMESTAMP(3) AND b.expires_at>UTC_TIMESTAMP(3)`, oid, user, user, user, bid).Scan(&key, &mime, &hash, &size, &platform, &adapter)
	if err != nil {
		fail(w, 404, "RESOURCE_NOT_FOUND")
		return
	}
	file, release, err := s.Relay.Acquire(key)
	if err != nil {
		fail(w, 410, "MEDIA_SOURCE_EXPIRED")
		return
	}
	defer release()
	info, err := file.Stat()
	if err != nil || info.Size() != size {
		fail(w, 410, "MEDIA_SOURCE_EXPIRED")
		return
	}
	releaseConnection, allowed := s.acquireMedia(user, r.URL.Query().Get("i"))
	if !allowed {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	defer releaseConnection()
	counted := &countedMediaResponse{ResponseWriter: w}
	reserve, valid := mediaRangeSize(mediaRangeHeader(r), size)
	if !valid {
		rejectMediaRange(w, size)
		return
	}
	if r.Method == http.MethodGet {
		if r.Header.Get("If-Range") != "" {
			reserve = size
		}
		epoch, _ := strconv.ParseInt(r.URL.Query().Get("e"), 10, 64)
		settle, e := s.reservePreviewBytes(r.Context(), user, "download:"+r.URL.Query().Get("i"), time.Unix(epoch, 0), reserve, size*s.settings().Int("ticket_byte_budget_multiplier"))
		if e != nil {
			rejectMediaBudget(w, e)
			return
		}
		defer func() {
			settle(counted.sent)
			s.reportTransfer(observation{User: user, Trace: bid, Platform: platform, Adapter: adapter, Stage: "download", Mode: "relay", Started: started}, counted)
		}()
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.settings().Seconds("client_download_timeout_seconds")))
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("ETag", "\""+hash+"\"")
	http.ServeContent(counted, r, "media", time.Time{}, file)
}

func mediaRangeHeader(r *http.Request) string {
	return strings.Join(r.Header.Values("Range"), ",")
}

func rejectMediaRange(w http.ResponseWriter, size int64) {
	w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(size, 10))
	fail(w, http.StatusRequestedRangeNotSatisfiable, "BAD_REQUEST")
}

func mediaRangeSize(header string, size int64) (int64, bool) {
	if header == "" {
		return size, true
	}
	if !strings.HasPrefix(header, "bytes=") || strings.Contains(header, ",") {
		return 0, false
	}
	parts := strings.SplitN(strings.TrimPrefix(header, "bytes="), "-", 2)
	if len(parts) != 2 {
		return 0, false
	}
	if parts[0] == "" {
		suffix, e := strconv.ParseInt(parts[1], 10, 64)
		if e != nil || suffix <= 0 {
			return 0, false
		}
		return min(suffix, size), true
	}
	start, e := strconv.ParseInt(parts[0], 10, 64)
	if e != nil || start < 0 || start >= size {
		return 0, false
	}
	end := size - 1
	if parts[1] != "" {
		end, e = strconv.ParseInt(parts[1], 10, 64)
		if e != nil || end < start {
			return 0, false
		}
		end = min(end, size-1)
	}
	return end - start + 1, true
}
