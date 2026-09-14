package billing

import (
	"testing"
	"time"
)

func TestMonthlyPeriodRetainsAnchorDay(t *testing.T) {
	loc := time.UTC
	anchor := time.Date(2024, 1, 31, 12, 0, 0, 0, loc)
	p, err := MonthlyPeriod(anchor, time.Date(2024, 2, 15, 0, 0, 0, 0, loc), loc)
	if err != nil {
		t.Fatal(err)
	}
	if p.From.Day() != 31 || p.To.Day() != 29 {
		t.Fatalf("%v", p)
	}
	p, err = MonthlyPeriod(anchor, time.Date(2024, 3, 1, 0, 0, 0, 0, loc), loc)
	if err != nil || p.From.Day() != 29 || p.To.Day() != 31 {
		t.Fatalf("%v %v", p, err)
	}
}
