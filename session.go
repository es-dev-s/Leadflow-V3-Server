package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strings"
)

const (
	sessionHeaderName    = "X-LeadFlow-Session"
	sessionQueryName     = "sid"
	errSignedInElsewhere = "signed in elsewhere"
)

func sessionHash(jti string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(jti)))
	return hex.EncodeToString(sum[:])
}

// logoutAction decides what a logout request may touch.
// presented is the tab sid; jwtSessionID is the cookie/Bearer jti.
//
// A same-browser stale tab (old sid + newer cookie) must not clear anything.
// A replaced device (stale JWT) must not Set-Cookie-clear either — that
// response would wipe a newer login in the same cookie jar if logout raced.
func logoutAction(presented, jwtSessionID string, jwtIsLive bool) (clearLive, clearCookie bool) {
	presented = strings.TrimSpace(presented)
	jwtSessionID = strings.TrimSpace(jwtSessionID)
	if presented != "" && jwtSessionID != "" && presented != jwtSessionID {
		return false, false
	}
	if !jwtIsLive {
		return false, false
	}
	return true, true
}

func sessionMatchesHash(stored *string, jti string) bool {
	jti = strings.TrimSpace(jti)
	if stored == nil || strings.TrimSpace(*stored) == "" || jti == "" {
		return false
	}
	want := sessionHash(jti)
	got := strings.TrimSpace(*stored)
	if len(got) != len(want) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
