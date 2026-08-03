package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type contextKey string

const authUserKey contextKey = "authUser"

type AuthUser struct {
	ID    string
	Email string
	Name  string
	Role  string
}

type Claims struct {
	Email string `json:"email"`
	Name  string `json:"name"`
	Role  string `json:"role"`
	jwt.RegisteredClaims
}

type TokenService struct {
	secret []byte
}

func NewTokenService(secret string) *TokenService {
	return &TokenService{secret: []byte(secret)}
}

func (t *TokenService) Parse(tokenStr string) (*AuthUser, error) {
	parsed, err := jwt.ParseWithClaims(tokenStr, &Claims{}, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodHS256 {
			return nil, errors.New("unexpected signing method")
		}
		return t.secret, nil
	})
	if err != nil {
		return nil, err
	}
	claims, ok := parsed.Claims.(*Claims)
	if !ok || !parsed.Valid {
		return nil, errors.New("invalid token")
	}
	if claims.Subject == "" || claims.Role == "" {
		return nil, errors.New("incomplete token claims")
	}
	return &AuthUser{
		ID:    claims.Subject,
		Email: claims.Email,
		Name:  claims.Name,
		Role:  claims.Role,
	}, nil
}

func withUser(ctx context.Context, u *AuthUser) context.Context {
	return context.WithValue(ctx, authUserKey, u)
}

func userFromContext(ctx context.Context) (*AuthUser, bool) {
	u, ok := ctx.Value(authUserKey).(*AuthUser)
	return u, ok && u != nil
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func authCookieName() string {
	if v := strings.TrimSpace(envOr("AUTH_COOKIE_NAME", "")); v != "" {
		return v
	}
	return "leadflow_access"
}

// accessToken prefers Authorization Bearer, then the shared HttpOnly cookie
// used by the CRM UI (same site via Next proxy).
func accessToken(r *http.Request) string {
	if tok := bearerToken(r); tok != "" {
		return tok
	}
	c, err := r.Cookie(authCookieName())
	if err != nil || c == nil {
		return ""
	}
	return strings.TrimSpace(c.Value)
}

func canReadSupport(role string) bool {
	return role == "SUPPORT" || role == "SUPERADMIN"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func nowUTC() time.Time { return time.Now().UTC() }
