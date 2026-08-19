package main

import (
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

const defaultBusinessTZ = "Asia/Kathmandu"

var (
	businessLocOnce sync.Once
	businessLoc     *time.Location
)

// businessLocation is the wall-clock zone for LeadFlow date/time product fields
// (lead dates, first-response pickers). Defaults to Asia/Kathmandu (UTC+5:45).
// Override with BUSINESS_TZ if needed.
func quoteTimeZoneName(tz string) string {
	tz = strings.TrimSpace(tz)
	if tz == "" {
		return defaultBusinessTZ
	}
	for _, r := range tz {
		ok := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '/' || r == '_' || r == '+' || r == '-'
		if !ok {
			return defaultBusinessTZ
		}
	}
	return tz
}

func businessLocation() *time.Location {
	businessLocOnce.Do(func() {
		name := strings.TrimSpace(os.Getenv("BUSINESS_TZ"))
		if name == "" {
			name = defaultBusinessTZ
		}
		loc, err := time.LoadLocation(name)
		if err != nil {
			log.Printf("business_tz: load %q failed (%v); falling back to Local", name, err)
			businessLoc = time.Local
			return
		}
		businessLoc = loc
		log.Printf("business_tz: using %s", name)
	})
	return businessLoc
}

// createdAtBusinessDateSQL is the same calendar-day expression as list date filters:
// createdAt stored as UTC wall-clock timestamp without time zone.
func businessTZName() string {
	name := quoteTimeZoneName(businessLocation().String())
	if name == "Local" {
		return defaultBusinessTZ
	}
	return name
}

func createdAtBusinessDateSQL() string {
	tz := businessTZName()
	return `((l."createdAt" AT TIME ZONE 'UTC') AT TIME ZONE '` + tz + `')::date`
}

func currentBusinessDateSQL() string {
	tz := businessTZName()
	return `(CURRENT_TIMESTAMP AT TIME ZONE '` + tz + `')::date`
}
