package httpapi

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"qushuiyin/service/internal/auth"
)

const adminCookie = "qsy_admin"

func (s *Mock) adminGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Host, s.AdminHost) {
			fail(w, 404, "RESOURCE_NOT_FOUND")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			origins := r.Header.Values("Origin")
			if len(origins) != 1 || !s.AdminOrigins[origins[0]] {
				fail(w, 403, "ACCESS_DENIED")
				return
			}
		}
		next(w, r)
	}
}
func setAdminCookie(w http.ResponseWriter, token string, expires time.Time, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: adminCookie, Value: token, Path: "/admin", Expires: expires, MaxAge: maxAge, Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}
func (s *Mock) adminLogin(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		fail(w, 403, "ACCESS_DENIED")
		return
	}
	s.mu.Lock()
	allowed := s.minute("admin-login:"+host, s.Settings.Int("ip_parse_requests_per_minute"))
	s.mu.Unlock()
	if !allowed {
		fail(w, 429, "RATE_LIMITED")
		return
	}
	var req struct{ Username, Password string }
	if !s.read(w, r, &req) {
		return
	}
	password := []byte(req.Password)
	req.Password = ""
	defer clear(password)
	if !auth.ValidAdminUsername(req.Username) || len(password) > 256 {
		fail(w, 401, "ADMIN_LOGIN_FAILED")
		return
	}
	select {
	case s.adminHashSlots <- struct{}{}:
		defer func() { <-s.adminHashSlots }()
	default:
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	a, token, expires, err := s.Admin.Login(r.Context(), req.Username, password, host)
	if errors.Is(err, auth.ErrAdminLogin) {
		fail(w, 401, "ADMIN_LOGIN_FAILED")
		return
	}
	if err != nil {
		fail(w, 503, "SERVICE_BUSY")
		return
	}
	setAdminCookie(w, token, expires, int(s.Admin.TTL/time.Second))
	ok(w, 200, a)
}
func (s *Mock) withAdmin(next func(http.ResponseWriter, *http.Request, auth.Admin, string)) http.HandlerFunc {
	return s.adminGuard(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(adminCookie)
		if err != nil || cookie.Value == "" || len(cookie.Value) > 128 {
			fail(w, 401, "AUTH_REQUIRED")
			return
		}
		a, err := s.Admin.Current(r.Context(), cookie.Value)
		if errors.Is(err, auth.ErrSession) {
			fail(w, 401, "SESSION_EXPIRED")
			return
		}
		if err != nil {
			fail(w, 503, "SERVICE_BUSY")
			return
		}
		next(w, r, a, cookie.Value)
	})
}
func (s *Mock) adminRoutes(m *http.ServeMux) {
	s.adminHashSlots = make(chan struct{}, 4)
	m.HandleFunc("POST /admin/v1/sessions", s.adminGuard(s.adminLogin))
	m.HandleFunc("GET /admin/v1/me", s.withAdmin(func(w http.ResponseWriter, _ *http.Request, a auth.Admin, _ string) { ok(w, 200, a) }))
	m.HandleFunc("DELETE /admin/v1/sessions/current", s.withAdmin(func(w http.ResponseWriter, r *http.Request, _ auth.Admin, token string) {
		if err := s.Admin.Revoke(r.Context(), token); err != nil {
			fail(w, 503, "SERVICE_BUSY")
			return
		}
		setAdminCookie(w, "", time.Unix(0, 0).UTC(), -1)
		ok(w, 200, dto{})
	}))
}
