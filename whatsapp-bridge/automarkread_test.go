package main

import (
	"testing"
	"time"
)

func TestNextAutoRunStaysInWindow(t *testing.T) {
	cfg := &autoMarkReadConfig{WindowStart: "07:00", WindowEnd: "22:00", MinMinutes: 30, MaxMinutes: 60}
	day := time.Date(2026, 10, 5, 0, 0, 0, 0, time.Local)
	cases := []struct {
		from        string
		minGap      time.Duration // lower bound from `from`
		wantNextDay bool
	}{
		{"10:00", 30 * time.Minute, false}, // normal: +30..60 min
		{"21:45", 0, true},                 // would land after 22:00 -> next day 07:00..07:20
		{"23:30", 0, true},                 // after window -> next day
		{"03:00", 0, false},                // before window -> today 07:00..07:20
	}
	for _, c := range cases {
		from := atClock(day, c.from)
		for i := 0; i < 500; i++ {
			next := nextAutoRun(cfg, from)
			start, end := atClock(next, "07:00"), atClock(next, "22:00")
			if next.Before(start) || !next.Before(end) {
				t.Fatalf("from %s: next %s outside window", c.from, next)
			}
			if next.Sub(from) < c.minGap {
				t.Fatalf("from %s: next %s too soon", c.from, next)
			}
			if c.from == "10:00" && next.Sub(from) > 60*time.Minute {
				t.Fatalf("from 10:00: next %s too late", next)
			}
			if gotNextDay := next.Day() != from.Day(); gotNextDay != c.wantNextDay {
				t.Fatalf("from %s: next %s, wantNextDay=%v", c.from, next, c.wantNextDay)
			}
			if c.from == "03:00" && next.After(atClock(day, "07:20")) {
				t.Fatalf("from 03:00: next %s later than 07:20", next)
			}
		}
	}
}
