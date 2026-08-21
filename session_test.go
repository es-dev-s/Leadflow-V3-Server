package main

import (
	"testing"
	"time"
)

func TestSessionHashIsStableAndUnique(t *testing.T) {
	a := sessionHash("session-a")
	b := sessionHash("session-b")
	if a == "" || len(a) != 64 {
		t.Fatalf("expected sha256 hex, got %q", a)
	}
	if a == b {
		t.Fatal("different session ids must not hash the same")
	}
	if sessionHash("session-a") != a {
		t.Fatal("hash must be stable")
	}
}

func TestSessionMatchesHash(t *testing.T) {
	jti := "live-session"
	hash := sessionHash(jti)
	if !sessionMatchesHash(&hash, jti) {
		t.Fatal("current session must match")
	}
	if sessionMatchesHash(&hash, "other-session") {
		t.Fatal("replaced session must not match")
	}
	if sessionMatchesHash(nil, jti) {
		t.Fatal("empty stored hash must not match")
	}
	if sessionMatchesHash(&hash, "") {
		t.Fatal("empty jti must not match")
	}
}

func TestTokenJTIRoundTrip(t *testing.T) {
	tokens := NewTokenService("test-secret-for-session", time.Hour)
	user := AuthUser{ID: "id-1", Email: "a@demo.local", Name: "A", Role: RoleSuperadmin}
	token, jti, _, err := tokens.Issue(user)
	if err != nil {
		t.Fatal(err)
	}
	if jti == "" {
		t.Fatal("login must issue a session id")
	}
	parsed, err := tokens.Parse(token)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.SessionID != jti {
		t.Fatalf("jti mismatch: %s vs %s", parsed.SessionID, jti)
	}
	token2, jti2, _, err := tokens.Issue(user)
	if err != nil {
		t.Fatal(err)
	}
	if token == token2 || jti == jti2 {
		t.Fatal("each login must mint a new session")
	}
}
