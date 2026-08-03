package main

import (
	"net/http"
	"strings"
	"time"
)

const defaultAuthCookieName = "leadflow_access"

// AuthCookieConfig controls the HttpOnly session cookie used by browsers.
type AuthCookieConfig struct {
	Name     string
	Secure   bool
	SameSite http.SameSite
	Path     string
	TTL      time.Duration
}

func loadAuthCookieConfig(ttl time.Duration) AuthCookieConfig {
	name := envOr("AUTH_COOKIE_NAME", defaultAuthCookieName)
	secure := false
	switch strings.ToLower(envOr("COOKIE_SECURE", "")) {
	case "1", "true", "yes":
		secure = true
	case "0", "false", "no":
		secure = false
	default:
		// Auto: secure cookies in production; allow HTTP for local/LAN.
		secure = strings.EqualFold(envOr("APP_ENV", ""), "production")
	}

	sameSite := http.SameSiteLaxMode
	switch strings.ToLower(envOr("COOKIE_SAMESITE", "Lax")) {
	case "strict":
		sameSite = http.SameSiteStrictMode
	case "none":
		sameSite = http.SameSiteNoneMode
		secure = true // browsers require Secure with SameSite=None
	default:
		sameSite = http.SameSiteLaxMode
	}

	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return AuthCookieConfig{
		Name:     name,
		Secure:   secure,
		SameSite: sameSite,
		Path:     "/",
		TTL:      ttl,
	}
}

func (c AuthCookieConfig) set(w http.ResponseWriter, token string, expires time.Time) {
	maxAge := int(time.Until(expires).Seconds())
	if maxAge < 1 {
		maxAge = int(c.TTL.Seconds())
	}
	http.SetCookie(w, &http.Cookie{
		Name:     c.Name,
		Value:    token,
		Path:     c.Path,
		HttpOnly: true,
		Secure:   c.Secure,
		SameSite: c.SameSite,
		MaxAge:   maxAge,
		Expires:  expires,
	})
}

func (c AuthCookieConfig) clear(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     c.Name,
		Value:    "",
		Path:     c.Path,
		HttpOnly: true,
		Secure:   c.Secure,
		SameSite: c.SameSite,
		MaxAge:   -1,
		Expires:  time.Unix(0, 0).UTC(),
	})
}

func (c AuthCookieConfig) read(r *http.Request) string {
	cookie, err := r.Cookie(c.Name)
	if err != nil || cookie == nil {
		return ""
	}
	return strings.TrimSpace(cookie.Value)
}

// accessToken returns Bearer token if present, otherwise the HttpOnly cookie.
// Query-string tokens are intentionally not accepted.
func (s *Server) accessToken(r *http.Request) string {
	if tok := bearerToken(r); tok != "" {
		return tok
	}
	return s.authCookie.read(r)
}
