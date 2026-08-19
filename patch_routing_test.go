package main

import (
	"encoding/json"
	"testing"
)

func TestQualificationOnlyPatchRequiresSingleField(t *testing.T) {
	only := map[string]json.RawMessage{"qualificationStatus": json.RawMessage(`"QUALIFIED"`)}
	if !isQualificationOnlyPatch(only) {
		t.Fatal("qualification-only body should route to qualification patch")
	}

	profile := map[string]json.RawMessage{
		"qualificationStatus": json.RawMessage(`"QUALIFIED"`),
		"source":              json.RawMessage(`"Facebook"`),
		"leadScore":           json.RawMessage(`40`),
	}
	if isQualificationOnlyPatch(profile) {
		t.Fatal("profile edit with empty name must not route to qualification-only patch")
	}
}

func TestOptionalEmailAndPhone(t *testing.T) {
	v := &ValidationError{}
	if optionalEmail(v, "email", "") != nil {
		t.Fatal("blank email should be omitted")
	}
	if optionalEmail(v, "email", "not-an-email") == nil || !v.HasErrors() {
		t.Fatal("invalid email should fail")
	}
	v = &ValidationError{}
	got := optionalEmail(v, "email", "  Lead@Example.COM ")
	if v.HasErrors() || got == nil || *got != "lead@example.com" {
		t.Fatalf("valid email: %#v err=%v", got, v.Errors)
	}
	v = &ValidationError{}
	if optionalPhone(v, "phone", "12") == nil || !v.HasErrors() {
		t.Fatal("short phone should fail")
	}
	v = &ValidationError{}
	if optionalPhone(v, "phone", "+977 9800000000") == nil || v.HasErrors() {
		t.Fatal("valid phone should pass")
	}
}
