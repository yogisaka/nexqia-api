// Package schedule contains pure scheduling logic: projecting recurring
// schedule patterns into concrete sessions, computing capacity status, and
// detecting overlaps. It has no database or HTTP dependencies.
package schedule

import (
	"sort"
	"time"
)

// Pattern is a recurring weekly schedule rule defined by a physician.
// EffectiveTo is the zero value (time.Time{}) when the pattern has no end
// date. Start and End use the "HH:MM" format.
type Pattern struct {
	ID            string
	PhysicianID   string
	DepartmentID  string
	RoomID        string
	DayOfWeek     time.Weekday
	Start         string // "HH:MM"
	End           string // "HH:MM"
	QuotaJKN      int    // 0 = unlimited
	SlotQuota     int    // 0 = unlimited
	EffectiveFrom time.Time
	EffectiveTo   time.Time // zero = open-ended
	Active        bool
}

// StoredSession is a persisted concrete session that overrides the
// projection for a given (pattern, date) pair.
type StoredSession struct {
	ScheduleID  string // matches Pattern.ID
	Date        time.Time
	PhysicianID string
	RoomID      string
	Start       string // "HH:MM"
	End         string // "HH:MM"
	QuotaJKN    int
	SlotQuota   int
	Status      string // "", "leave", "substituted", "cancelled"
	LeaveID     string
	Notes       string
}

// Session is a concrete session for a specific date, either projected from a
// pattern (Projected=true) or loaded from storage (Projected=false).
type Session struct {
	ScheduleID  string
	Date        time.Time
	PhysicianID string
	RoomID      string
	Start       string // "HH:MM"
	End         string // "HH:MM"
	QuotaJKN    int
	SlotQuota   int
	Status      string
	LeaveID     string
	Notes       string
	Projected   bool
}

// Capacity status values.
const (
	StatusAvailable   = "available"
	StatusNearFull    = "near_full"
	StatusFull        = "full"
	StatusLeave       = "leave"
	StatusSubstituted = "substituted"
	StatusCancelled   = "cancelled"
)

// Capacity modes.
const (
	ModeCombined = "combined"
	ModeSplit    = "split"
)

func zero(t time.Time) bool {
	return t.IsZero()
}

func dateBetween(d, from, to time.Time) bool {
	day := func(t time.Time) time.Time {
		y, m, dd := t.Date()
		return time.Date(y, m, dd, 0, 0, 0, 0, t.Location())
	}
	d = day(d)
	if !zero(from) && d.Before(day(from)) {
		return false
	}
	if !zero(to) && d.After(day(to)) {
		return false
	}
	return true
}

// Project returns one Session per (active pattern, date) pair for every date
// in [from, to] whose weekday matches the pattern and that falls inside the
// pattern's effective range. A StoredSession matching (pattern ID, date)
// overrides the projected values and is marked Projected=false. The result is
// ordered by date, then start time.
func Project(patterns []Pattern, stored []StoredSession, from, to time.Time) []Session {
	storedByID := make(map[string]StoredSession, len(stored))
	for _, s := range stored {
		storedByID[key(s.ScheduleID, s.Date)] = s
	}

	var out []Session
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		d = dayStart(d)
		for _, p := range patterns {
			if !p.Active || p.DayOfWeek != d.Weekday() {
				continue
			}
			if !dateBetween(d, p.EffectiveFrom, p.EffectiveTo) {
				continue
			}
			if s, ok := storedByID[key(p.ID, d)]; ok {
				out = append(out, Session{
					ScheduleID:  s.ScheduleID,
					Date:        d,
					PhysicianID: s.PhysicianID,
					RoomID:      s.RoomID,
					Start:       s.Start,
					End:         s.End,
					QuotaJKN:    s.QuotaJKN,
					SlotQuota:   s.SlotQuota,
					Status:      s.Status,
					LeaveID:     s.LeaveID,
					Notes:       s.Notes,
					Projected:   false,
				})
				continue
			}
			out = append(out, Session{
				ScheduleID:  p.ID,
				Date:        d,
				PhysicianID: p.PhysicianID,
				RoomID:      p.RoomID,
				Start:       p.Start,
				End:         p.End,
				QuotaJKN:    p.QuotaJKN,
				SlotQuota:   p.SlotQuota,
				Projected:   true,
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Date.Equal(out[j].Date) {
			return out[i].Date.Before(out[j].Date)
		}
		return out[i].Start < out[j].Start
	})
	return out
}

func key(scheduleID string, date time.Time) string {
	return scheduleID + "|" + dayStart(date).Format("2006-01-02")
}

// dayStart normalizes a time to midnight of the same calendar date, so that
// dates from different sources (DB timestamps vs. loop dates) match.
func dayStart(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// Capacity returns the JKN, non-JKN and total quotas for a session under the
// given mode. Zero quotas mean unlimited.
//
// Mode combined: total = SlotQuota; JKN capacity = QuotaJKN, non-JKN is the
// remainder of the combined quota. Mode split: JKN compared against QuotaJKN,
// non-JKN against SlotQuota; total is the sum of both pools.
func Capacity(s Session, mode string) (jkn, nonJKN, total int) {
	if mode == ModeSplit {
		jkn = s.QuotaJKN
		nonJKN = s.SlotQuota
		return jkn, nonJKN, jkn + nonJKN
	}
	jkn = s.QuotaJKN
	if s.SlotQuota > 0 {
		total = s.SlotQuota
		nonJKN = total - jkn
		if nonJKN < 0 {
			nonJKN = 0
		}
	}
	return jkn, nonJKN, total
}

// Status resolves the display status of a session. Explicit session status
// (leave/substituted/cancelled) wins. Otherwise the capacity status is
// computed against registered counts with the near-full percentage threshold
// (0 < nearFullPct <= 100). Quota 0 means unlimited, which is always
// available.
//
// Mode combined: the total quota is SlotQuota and registered is the sum of
// both counters. full when registered >= total; near_full when
// registered*100 >= total*nearFullPct.
//
// Mode split: JKN is compared against QuotaJKN, others against SlotQuota.
// full when BOTH pools are full; near_full when either pool reaches the
// threshold. An unlimited pool (quota 0) is never full.
func Status(registeredJKN, registeredOther int, s Session, mode string, nearFullPct int) string {
	switch s.Status {
	case StatusLeave, StatusSubstituted, StatusCancelled:
		return s.Status
	}

	if mode == ModeSplit {
		jknFull := s.QuotaJKN > 0 && registeredJKN >= s.QuotaJKN
		otherFull := s.SlotQuota > 0 && registeredOther >= s.SlotQuota
		if jknFull && otherFull {
			return StatusFull
		}
		if reachesThreshold(registeredJKN, s.QuotaJKN, nearFullPct) ||
			reachesThreshold(registeredOther, s.SlotQuota, nearFullPct) {
			return StatusNearFull
		}
		return StatusAvailable
	}

	total := s.SlotQuota
	if total <= 0 {
		return StatusAvailable
	}
	registered := registeredJKN + registeredOther
	if registered >= total {
		return StatusFull
	}
	if registered*100 >= total*nearFullPct {
		return StatusNearFull
	}
	return StatusAvailable
}

// reachesThreshold reports whether registered >= quota*nearFullPct/100 using
// integer math. Unlimited quota (0) never reaches a threshold.
func reachesThreshold(registered, quota, nearFullPct int) bool {
	if quota <= 0 {
		return false
	}
	return registered*100 >= quota*nearFullPct
}

// Overlaps reports whether two concrete sessions conflict: same date,
// overlapping time range, and either the same (non-empty) room or the same
// physician. Sessions on leave or cancelled do not count.
func Overlaps(a, b Session) bool {
	if a.Status == StatusLeave || a.Status == StatusCancelled ||
		b.Status == StatusLeave || b.Status == StatusCancelled {
		return false
	}
	if !a.Date.Equal(b.Date) {
		return false
	}
	if !(a.Start < b.End && b.Start < a.End) {
		return false
	}
	sameRoom := a.RoomID != "" && a.RoomID == b.RoomID
	samePhysician := a.PhysicianID == b.PhysicianID && a.PhysicianID != ""
	return sameRoom || samePhysician
}

// PatternOverlap reports whether two recurring patterns conflict: same
// weekday, overlapping time range, overlapping effective ranges, either the
// same room or the same physician, and both active.
func PatternOverlap(a, b Pattern) bool {
	if !a.Active || !b.Active {
		return false
	}
	if a.DayOfWeek != b.DayOfWeek {
		return false
	}
	if !(a.Start < b.End && b.Start < a.End) {
		return false
	}
	if !rangesOverlap(a.EffectiveFrom, a.EffectiveTo, b.EffectiveFrom, b.EffectiveTo) {
		return false
	}
	sameRoom := a.RoomID != "" && a.RoomID == b.RoomID
	samePhysician := a.PhysicianID == b.PhysicianID && a.PhysicianID != ""
	return sameRoom || samePhysician
}

func rangesOverlap(aFrom, aTo, bFrom, bTo time.Time) bool {
	if !zero(bFrom) && !zero(aTo) && aTo.Before(bFrom) {
		return false
	}
	if !zero(aFrom) && !zero(bTo) && bTo.Before(aFrom) {
		return false
	}
	return true
}
