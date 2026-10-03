package schedule

import (
	"testing"
	"time"
)

func d(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestProject(t *testing.T) {
	monday := d("2026-01-05") // Monday
	pattern := Pattern{
		ID: "p1", PhysicianID: "dr1", DepartmentID: "dep1", RoomID: "r1",
		DayOfWeek: time.Monday, Start: "08:00", End: "12:00",
		QuotaJKN: 5, SlotQuota: 10,
		EffectiveFrom: monday, Active: true,
	}
	stored := StoredSession{
		ScheduleID: "p1", Date: monday, PhysicianID: "dr-sub",
		RoomID: "r2", Start: "09:00", End: "11:00",
		QuotaJKN: 0, SlotQuota: 0, Status: "leave", Notes: "cuti",
	}

	t.Run("projects only matching weekday", func(t *testing.T) {
		got := Project([]Pattern{pattern}, nil, monday, monday.AddDate(0, 0, 6))
		// Window covers Mon 2026-01-05 .. Sun 2026-01-11; only Mon matches
		// DayOfWeek=time.Monday, so exactly 1 session on 2026-01-05.
		if len(got) != 1 {
			t.Fatalf("len=%d, want 1", len(got))
		}
		s := got[0]
		if !s.Date.Equal(monday) || !s.Projected || s.Start != "08:00" || s.End != "12:00" ||
			s.PhysicianID != "dr1" || s.RoomID != "r1" || s.QuotaJKN != 5 || s.SlotQuota != 10 {
			t.Fatalf("unexpected session: %+v", s)
		}
	})

	t.Run("stored session overrides projection", func(t *testing.T) {
		got := Project([]Pattern{pattern}, []StoredSession{stored}, monday, monday)
		if len(got) != 1 {
			t.Fatalf("len=%d, want 1", len(got))
		}
		s := got[0]
		if s.Projected {
			t.Fatal("Projected=true, want false for stored session")
		}
		if s.PhysicianID != "dr-sub" || s.RoomID != "r2" || s.Start != "09:00" ||
			s.Status != "leave" || s.Notes != "cuti" {
			t.Fatalf("stored values not applied: %+v", s)
		}
	})

	t.Run("effective_to inclusive boundary", func(t *testing.T) {
		p := pattern
		p.EffectiveTo = monday // inclusive: Monday itself is inside the range
		got := Project([]Pattern{p}, nil, monday, monday.AddDate(0, 0, 7))
		if len(got) != 1 {
			t.Fatalf("len=%d, want 1 (effective_to inclusive)", len(got))
		}
		p.EffectiveTo = monday.AddDate(0, 0, -1)
		got = Project([]Pattern{p}, nil, monday, monday)
		if len(got) != 0 {
			t.Fatalf("len=%d, want 0 (after effective_to)", len(got))
		}
	})

	t.Run("inactive pattern and sorted output", func(t *testing.T) {
		p2 := pattern
		p2.ID = "p2"
		p2.Active = false
		// Two active patterns on the same Monday: p1 at 08:00 sorts first.
		got := Project([]Pattern{pattern, p2}, nil, monday, monday)
		if len(got) != 1 {
			t.Fatalf("len=%d, want 1 (inactive skipped)", len(got))
		}
	})
	t.Run("two active patterns sorted by start time", func(t *testing.T) {
		// Patterns given in reverse start order; output must be sorted by
		// start time within the same date.
		late := pattern
		late.ID = "late"
		late.Start, late.End = "13:00", "15:00"
		early := pattern
		early.ID = "early"
		early.Start, early.End = "07:00", "09:00"
		got := Project([]Pattern{late, early}, nil, monday, monday)
		if len(got) != 2 {
			t.Fatalf("len=%d, want 2", len(got))
		}
		if got[0].ScheduleID != "early" || got[0].Start != "07:00" ||
			got[1].ScheduleID != "late" || got[1].Start != "13:00" {
			t.Fatalf("not sorted by start time: %s then %s", got[0].Start, got[1].Start)
		}
	})

	t.Run("output sorted across dates", func(t *testing.T) {
		// Window covers two Mondays; earlier date must come first. The two
		// sessions share the same start time, so date decides the order.
		nextMonday := monday.AddDate(0, 0, 7)
		later := pattern
		later.ID = "p-next"
		got := Project([]Pattern{later, pattern}, nil, monday, nextMonday)
		// Window 2026-01-05..2026-01-12 covers two Mondays x 2 patterns = 4.
		if len(got) != 4 {
			t.Fatalf("len=%d, want 4", len(got))
		}
		if !got[0].Date.Equal(monday) || !got[3].Date.Equal(nextMonday) {
			t.Fatalf("not sorted by date: %s before %s", got[0].Date, got[3].Date)
		}
	})
}

func TestCapacity(t *testing.T) {
	tests := []struct {
		name             string
		s                Session
		mode             string
		wantJKN, wantOth int
		wantTotal        int
	}{
		{
			name: "combined quota remainder",
			s:    Session{QuotaJKN: 4, SlotQuota: 10},
			mode: ModeCombined,
			// combined: total = SlotQuota = 10; JKN = 4; non-JKN remainder
			// = 10-4 = 6.
			wantJKN: 4, wantOth: 6, wantTotal: 10,
		},
		{
			name: "split independent pools",
			s:    Session{QuotaJKN: 4, SlotQuota: 10},
			mode: ModeSplit,
			// split: two independent pools, total is the sum 4+10 = 14.
			wantJKN: 4, wantOth: 10, wantTotal: 14,
		},
		{
			name: "combined zero slot quota unlimited",
			s:    Session{QuotaJKN: 3},
			mode: ModeCombined,
			// SlotQuota=0 = unlimited: total 0, non-JKN 0 (undefined pool).
			wantJKN: 3, wantOth: 0, wantTotal: 0,
		},
		{
			name: "combined jkn exceeds slot quota clamps non-jkn",
			s:    Session{QuotaJKN: 12, SlotQuota: 10},
			mode: ModeCombined,
			// total=10, remainder 10-12 negative → clamped to 0.
			wantJKN: 12, wantOth: 0, wantTotal: 10,
		},
		{
			name: "split zero quotas",
			s:    Session{},
			mode: ModeSplit,
			// both pools unlimited: totals are 0.
			wantJKN: 0, wantOth: 0, wantTotal: 0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			jkn, other, total := Capacity(tc.s, tc.mode)
			if jkn != tc.wantJKN || other != tc.wantOth || total != tc.wantTotal {
				t.Fatalf("got (%d,%d,%d), want (%d,%d,%d)", jkn, other, total, tc.wantJKN, tc.wantOth, tc.wantTotal)
			}
		})
	}
}

func TestStatus(t *testing.T) {
	tests := []struct {
		name           string
		regJKN, regOth int
		s              Session
		mode           string
		nearFullPct    int
		want           string
	}{
		{name: "leave wins", s: Session{Status: "leave"}, want: "leave"},
		{name: "substituted wins", s: Session{Status: "substituted"}, want: "substituted"},
		{name: "cancelled wins", s: Session{Status: "cancelled"}, want: "cancelled"},
		{
			name:        "combined at exact 80pct threshold",
			s:           Session{SlotQuota: 10},
			mode:        ModeCombined,
			nearFullPct: 80,
			regJKN:      3,
			regOth:      5,
			want:        "near_full",
			// combined, SlotQuota=10, registered=8: 8*100=800 >= 10*80=800
			// → exactly at threshold → near_full (not yet full: 8<10).
		},
		{
			name:        "combined full",
			s:           Session{SlotQuota: 10},
			mode:        ModeCombined,
			nearFullPct: 80,
			regJKN:      5,
			regOth:      5,
			want:        "full",
			// registered=10 >= total=10 → full.
		},
		{
			name:        "combined below threshold",
			s:           Session{SlotQuota: 10},
			mode:        ModeCombined,
			nearFullPct: 80,
			regJKN:      7,
			regOth:      0,
			want:        "available",
			// registered=7: 7*100=700 < 10*80=800 → available.
		},
		{
			name:        "combined unlimited quota",
			s:           Session{},
			mode:        ModeCombined,
			nearFullPct: 80,
			regJKN:      999,
			regOth:      999,
			want:        "available",
			// SlotQuota=0 = unlimited → always available regardless of count.
		},
		{
			name:        "split one pool full only",
			s:           Session{QuotaJKN: 5, SlotQuota: 10},
			mode:        ModeSplit,
			nearFullPct: 80,
			regJKN:      5,
			regOth:      5,
			want:        "near_full",
			// QuotaJKN=5, SlotQuota=10. JKN: 5>=5 full; others: 5<10 not full.
			// Not both full → not "full". Threshold: JKN 5*100=500 >= 5*80=400
			// → near_full.
		},
		{
			name:        "split both full",
			s:           Session{QuotaJKN: 5, SlotQuota: 10},
			mode:        ModeSplit,
			nearFullPct: 80,
			regJKN:      5,
			regOth:      10,
			want:        "full",
			// JKN 5>=5 and others 10>=10 → both full → full.
		},
		{
			name:        "split at exact threshold one pool",
			s:           Session{QuotaJKN: 5, SlotQuota: 10},
			mode:        ModeSplit,
			nearFullPct: 80,
			regJKN:      4,
			regOth:      5,
			want:        "near_full",
			// JKN 4<5 not full; others 5<10 not full. JKN 4*100=400 >= 5*80=400
			// (exact 80%) → near_full.
		},
		{
			name:        "split unlimited pools",
			s:           Session{},
			mode:        ModeSplit,
			nearFullPct: 80,
			regJKN:      100,
			regOth:      100,
			want:        "available",
			// Both quotas 0 = unlimited → never full/threshold → available.
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Status(tc.regJKN, tc.regOth, tc.s, tc.mode, tc.nearFullPct); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOverlaps(t *testing.T) {
	date := d("2026-01-05")
	s := func(id, phys, room, start, end, status string) Session {
		return Session{ScheduleID: id, Date: date, PhysicianID: phys, RoomID: room, Start: start, End: end, Status: status}
	}
	a := s("s1", "dr1", "r1", "08:00", "10:00", "")

	tests := []struct {
		name string
		b    Session
		want bool
	}{
		{name: "same room, overlapping time", b: s("s2", "dr2", "r1", "09:00", "11:00", ""), want: true},
		{name: "same physician, overlapping time", b: s("s2", "dr1", "r9", "09:00", "11:00", ""), want: true},
		{name: "touching intervals 08-10 vs 10-12", b: s("s2", "dr1", "r1", "10:00", "12:00", ""), want: false},
		{name: "disjoint time", b: s("s2", "dr1", "r1", "11:00", "12:00", ""), want: false},
		{name: "different date", b: Session{Date: date.AddDate(0, 0, 1), PhysicianID: "dr1", RoomID: "r1", Start: "09:00", End: "09:30"}, want: false},
		{name: "leave not counted", b: s("s2", "dr1", "r1", "09:00", "09:30", "leave"), want: false},
		{name: "cancelled not counted", b: s("s2", "dr1", "r1", "09:00", "09:30", "cancelled"), want: false},
		{name: "no shared room or physician", b: s("s2", "dr2", "r2", "09:00", "09:30", ""), want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Overlaps(a, tc.b); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPatternOverlap(t *testing.T) {
	from := d("2026-01-01")
	p := func(id, phys, room string, day time.Weekday, start, end string, to time.Time, active bool) Pattern {
		return Pattern{
			ID: id, PhysicianID: phys, RoomID: room, DayOfWeek: day,
			Start: start, End: end, EffectiveFrom: from, EffectiveTo: to, Active: active,
		}
	}
	open := time.Time{}
	a := p("p1", "dr1", "r1", time.Monday, "08:00", "12:00", open, true)

	tests := []struct {
		name string
		b    Pattern
		want bool
	}{
		{name: "same physician, overlapping time", b: p("p2", "dr1", "r9", time.Monday, "11:00", "13:00", open, true), want: true},
		{name: "same room, overlapping time", b: p("p2", "dr2", "r1", time.Monday, "09:00", "10:00", open, true), want: true},
		{name: "different weekday", b: p("p2", "dr1", "r1", time.Tuesday, "09:00", "09:30", open, true), want: false},
		{name: "disjoint time", b: p("p2", "dr1", "r1", time.Monday, "12:00", "14:00", open, true), want: false},
		{name: "inactive pattern", b: p("p2", "dr1", "r1", time.Monday, "09:00", "09:30", open, false), want: false},
		{
			name: "disjoint effective ranges",
			// a is open-ended; b ends 2025-12-31, one day before a starts
			// (2026-01-01) → no overlap.
			b:    p("p2", "dr1", "r1", time.Monday, "09:00", "09:30", d("2025-12-31"), true),
			want: false,
		},
		{
			name: "overlapping effective ranges",
			// b ends 2026-06-30, after a's start → ranges intersect.
			b:    p("p2", "dr1", "r1", time.Monday, "09:00", "09:30", d("2026-06-30"), true),
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PatternOverlap(a, tc.b); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
