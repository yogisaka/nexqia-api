// internal/server/merchant_day_test.go
package server

import (
	"testing"
	"time"
)

func TestLocalDayStart(t *testing.T) {
	wib, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		t.Fatalf("load Asia/Jakarta: %v", err)
	}
	cases := []struct {
		name string
		now  time.Time
		loc  *time.Location
		want time.Time
	}{
		{"early morning WIB", time.Date(2026, 10, 6, 3, 0, 0, 0, wib), wib, time.Date(2026, 10, 6, 0, 0, 0, 0, wib)},
		{"UTC instant already next day in WIB", time.Date(2026, 10, 5, 20, 0, 0, 0, time.UTC), wib, time.Date(2026, 10, 6, 0, 0, 0, 0, wib)},
		{"UTC zone", time.Date(2026, 10, 5, 20, 0, 0, 0, time.UTC), time.UTC, time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)},
		{"exact midnight", time.Date(2026, 10, 6, 0, 0, 0, 0, wib), wib, time.Date(2026, 10, 6, 0, 0, 0, 0, wib)},
	}
	for _, tc := range cases {
		got := localDayStart(tc.now, tc.loc)
		if !got.Equal(tc.want) || got.Location().String() != tc.loc.String() {
			t.Errorf("%s: localDayStart = %v, want %v", tc.name, got, tc.want)
		}
	}
	// WIB midnight of 2026-10-06 is 2026-10-05 17:00 UTC.
	got := localDayStart(time.Date(2026, 10, 6, 3, 0, 0, 0, wib), wib)
	if want := time.Date(2026, 10, 5, 17, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("WIB midnight = %v, want %v", got.UTC(), want)
	}
}
