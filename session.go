package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strings"
)

const (
	sessionHeaderName = "X-LeadFlow-Session"
	sessionQueryName  = "sid"
	errSignedInElsewhere = "signed in elsewhere"
)

func sessionHash(jti string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(jti)))
	return hex.EncodeToString(sum[:])
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
