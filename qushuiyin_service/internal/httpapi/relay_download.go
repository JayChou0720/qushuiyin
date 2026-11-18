package httpapi

import (
	"net/http"
	"strings"
	"time"
)

func (s *Mock) relayDownload(w http.ResponseWriter, r *http.Request, user, bid, oid string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		fail(w, 405, "BAD_REQUEST")
		return
	}
	ranges := r.Header.Values("Range")
	if len(ranges) > 1 || len(ranges) == 1 && strings.Contains(ranges[0], ",") {
		fail(w, 416, "BAD_REQUEST")
		return
	}
	var key, mime, hash string
	var size int64
	err := s.Sessions.DB.QueryRowContext(r.Context(), `SELECT f.storage_key,f.mime_type,f.sha256,f.size_bytes FROM media_object o JOIN media_file f ON f.id=o.file_id JOIN save_batch_item i ON i.media_object_id=o.id JOIN save_batch b ON b.id=i.batch_id WHERE o.id=? AND o.user_id=? AND i.user_id=? AND b.user_id=? AND b.id=? AND o.status='ready' AND f.status='ready' AND i.prepare_status='READY' AND b.cancel_requested=FALSE AND o.expires_at>UTC_TIMESTAMP(3) AND f.expires_at>UTC_TIMESTAMP(3) AND b.expires_at>UTC_TIMESTAMP(3)`, oid, user, user, user, bid).Scan(&key, &mime, &hash, &size)
	if err != nil {
		fail(w, 404, "RESOURCE_NOT_FOUND")
		return
	}
	file, err := s.Relay.Open(key)
	if err != nil {
		fail(w, 410, "MEDIA_SOURCE_EXPIRED")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() != size {
		fail(w, 410, "MEDIA_SOURCE_EXPIRED")
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("ETag", "\""+hash+"\"")
	http.ServeContent(w, r, "media", time.Time{}, file)
}
