package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	emailPattern = regexp.MustCompile(`^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}$`)
)

type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

type ValidationError struct {
	Errors []FieldError `json:"errors"`
}

func (v *ValidationError) Error() string {
	return "validation failed"
}

func (v *ValidationError) Add(field, message string) {
	v.Errors = append(v.Errors, FieldError{Field: field, Message: message})
}

func (v *ValidationError) HasErrors() bool {
	return len(v.Errors) > 0
}

func decodeJSON(r *http.Request, dst any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return errors.New("empty JSON body")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return nil
}

func requireString(v *ValidationError, field, value string, min, max int) string {
	value = strings.TrimSpace(value)
	if value == "" {
		v.Add(field, field+" is required")
		return value
	}
	n := utf8.RuneCountInString(value)
	if n < min {
		v.Add(field, field+" must be at least "+itoa(min)+" characters")
	}
	if max > 0 && n > max {
		v.Add(field, field+" must be at most "+itoa(max)+" characters")
	}
	return value
}

// optionalBoundedString trims value; empty is allowed, max length is enforced.
func optionalBoundedString(v *ValidationError, field, value string, max int) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return value
	}
	n := utf8.RuneCountInString(value)
	if max > 0 && n > max {
		v.Add(field, field+" must be at most "+itoa(max)+" characters")
	}
	return value
}

func optionalEmail(v *ValidationError, field, value string) *string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return nil
	}
	if !emailPattern.MatchString(value) {
		v.Add(field, "email format is invalid")
	}
	return &value
}

func optionalPhone(v *ValidationError, field, value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	digits := 0
	for _, r := range value {
		if r >= '0' && r <= '9' {
			digits++
		}
	}
	if digits < 5 {
		v.Add(field, "phone must include at least 5 digits")
	}
	return &value
}

func requireEmail(v *ValidationError, field, value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		v.Add(field, "email is required")
		return value
	}
	if !emailPattern.MatchString(value) {
		v.Add(field, "email format is invalid")
	}
	return value
}

func requirePassword(v *ValidationError, field, value string) string {
	if strings.TrimSpace(value) == "" {
		v.Add(field, "password is required")
		return value
	}
	n := utf8.RuneCountInString(value)
	if n < 8 {
		v.Add(field, "password must be at least 8 characters")
	}
	if n > 128 {
		v.Add(field, "password must be at most 128 characters")
	}
	hasLetter := false
	hasDigit := false
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			hasLetter = true
		}
		if r >= '0' && r <= '9' {
			hasDigit = true
		}
	}
	if n >= 8 && (!hasLetter || !hasDigit) {
		v.Add(field, "password must include at least one letter and one number")
	}
	return value
}

func requireRole(v *ValidationError, field, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		v.Add(field, "role is required")
		return value
	}
	if !isValidRole(value) {
		v.Add(field, "role is invalid")
	}
	return value
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func asValidation(err error) (*ValidationError, bool) {
	var v *ValidationError
	if errors.As(err, &v) {
		return v, true
	}
	return nil, false
}
