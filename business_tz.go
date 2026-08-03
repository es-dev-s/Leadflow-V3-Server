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
