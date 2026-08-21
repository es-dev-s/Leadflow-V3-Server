package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

type contextKey string

const authUserKey contextKey = "authUser"

type AuthUser struct {
	ID              string  `json:"id"`
	Email           string  `json:"email"`
	Name            string  `json:"name"`
	Role            string  `json:"role"`
	TeamID          *string `json:"teamId,omitempty"`
	AnalystTeamName *string `json:"analystTeamName,omitempty"`
	SessionID       string  `json:"-"`
}

type Claims struct {
	Email string `json:"email"`
	Name  string `json:"name"`
	Role  string `json:"role"`
	jwt.RegisteredClaims
}

type TokenService struct {
	secret []byte
	ttl    time.Duration
}

func NewTokenService(secret string, ttl time.Duration) *TokenService {
	return &TokenService{secret: []byte(secret), ttl: ttl}
}

func (t *TokenService) Issue(user AuthUser) (token, jti string, expires time.Time, err error) {
	expires = time.Now().UTC().Add(t.ttl)
	jti = uuid.NewString()
	claims := Claims{
		Email: user.Email,
		Name:  user.Name,
		Role:  user.Role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   user.ID,
			ID:        jti,
			ExpiresAt: jwt.NewNumericDate(expires),
			IssuedAt:  jwt.NewNumericDate(time.Now().UTC()),
			Issuer:    "leadflow-backend",
		},
	}
	parsed := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	token, err = parsed.SignedString(t.secret)
	return token, jti, expires, err
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
	if claims.Subject == "" || claims.Email == "" || claims.Role == "" {
		return nil, errors.New("incomplete token claims")
	}
	return &AuthUser{
		ID:        claims.Subject,
		Email:     claims.Email,
		Name:      claims.Name,
		Role:      claims.Role,
		SessionID: strings.TrimSpace(claims.ID),
	}, nil
}

func hashPassword(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func checkPassword(stored, password string) (ok bool, upgradedHash string) {
	stored = strings.TrimSpace(stored)
	if stored == "" {
		return false, ""
	}
	if strings.HasPrefix(stored, "$2a$") || strings.HasPrefix(stored, "$2b$") || strings.HasPrefix(stored, "$2y$") {
		if err := bcrypt.CompareHashAndPassword([]byte(stored), []byte(password)); err != nil {
			return false, ""
		}
		return true, ""
	}
	// Legacy plaintext demo hashes — accept once, then upgrade to bcrypt.
	if stored == password {
		hashed, err := hashPassword(password)
		if err != nil {
			return true, ""
		}
		return true, hashed
	}
	return false, ""
}

func withUser(ctx context.Context, user *AuthUser) context.Context {
	return context.WithValue(ctx, authUserKey, user)
}

func userFromContext(ctx context.Context) (*AuthUser, bool) {
	user, ok := ctx.Value(authUserKey).(*AuthUser)
	return user, ok && user != nil
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	parts := strings.SplitN(h, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}
