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

// createdAtBusinessDateSQL is the Kathmandu calendar day of createdAt — the
// same day the leads list uses. Do not wrap AT TIME ZONE 'UTC' first: date-
// picker values are stored at KTM midnight (UTC 18:15 the previous day) and
// that extra conversion puts them on the prior graph day.
func businessTZName() string {
	name := quoteTimeZoneName(businessLocation().String())
	if name == "Local" {
		return defaultBusinessTZ
	}
	return name
}

func createdAtBusinessDateSQL() string {
	tz := businessTZName()
	return `(l."createdAt" AT TIME ZONE '` + tz + `')::date`
}

func leadCreatedAtInBusinessDateRangeSQL(startDateExpr, endExclusiveDateExpr string) string {
	tz := businessTZName()
	return `l."createdAt" >= (` + startDateExpr + `::timestamp AT TIME ZONE '` + tz + `')` +
		` AND l."createdAt" < (` + endExclusiveDateExpr + `::timestamp AT TIME ZONE '` + tz + `')`
}

func currentBusinessDateSQL() string {
	tz := businessTZName()
	return `(CURRENT_TIMESTAMP AT TIME ZONE '` + tz + `')::date`
}
