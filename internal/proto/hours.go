package proto

import (
	"errors"
	"math/big"
	"regexp"
	"strings"
	"time"
)

// Human hours and subscription plans (protocol 4, design §12.1). A
// person logs their own time on an issue for a day (sfx log); an admin
// records what a flat-rate plan costs a month and whose usage it pays
// for, and cost reports split that across the work beside the
// list-price equivalent.
const (
	OpHoursLog    = "hours.log"    // HoursLogArgs → HoursLogResult
	OpHoursDelete = "hours.delete" // HoursDeleteArgs → HoursDeleteResult; one's own entry, or an admin's
	OpHours       = "hours"        // HoursArgs → HoursResult
	OpPlanSet     = "plan.set"     // PlanSetArgs → PlanSetResult; admins only
	OpPlans       = "plans"        // PlansArgs → PlansResult
)

// HoursLogArgs logs the caller's Seconds on the issue ID for the day On
// (2006-01-02, UTC), empty for the server's today. Idem, an idempotency
// key, makes a retry return the first entry.
type HoursLogArgs struct {
	ID      string `json:"id"`
	Seconds int64  `json:"seconds"`
	On      string `json:"on,omitempty"`
	Note    string `json:"note,omitempty"`
	Idem    string `json:"idem,omitempty"`
}

// HoursLogResult is the new entry's id and the day it was logged for.
type HoursLogResult struct {
	ID string `json:"id"`
	On string `json:"on"`
}

// HoursDeleteArgs undoes the entry ID.
type HoursDeleteArgs struct {
	ID string `json:"id"`
}

// HoursDeleteResult is the entry undone.
type HoursDeleteResult struct {
	ID string `json:"id"`
}

// HoursArgs lists entries, newest day first: those on Issue, those By a
// principal, either or both. Limit is how many (0 takes the server's
// default, 50; at most 500).
type HoursArgs struct {
	Issue string `json:"issue,omitempty"`
	By    string `json:"by,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

// HoursEntry is one entry: Principal's Seconds on Issue on the day On
// (2006-01-02), logged at At.
type HoursEntry struct {
	ID        string    `json:"id"`
	Issue     string    `json:"issue"`
	Principal string    `json:"principal"`
	On        string    `json:"on"`
	Seconds   int64     `json:"seconds"`
	Note      string    `json:"note,omitempty"`
	At        time.Time `json:"at"`
}

// HoursResult is a page of entries and how many more matched.
type HoursResult struct {
	Entries []HoursEntry `json:"entries"`
	More    int          `json:"more,omitempty"`
}

// PersonHours is the time one principal logged.
type PersonHours struct {
	Principal string `json:"principal"`
	Seconds   int64  `json:"seconds"`
}

// MaxFee bounds a plan's fee: a million US dollars a seat a month, in
// micro-dollars.
const MaxFee = MaxRate

// PlanSetArgs sets a plan's terms from the month From (2006-01) on,
// until a later From of the same name: Fee micro-dollars per seat per
// month, Seats seats, and the Principals whose usage it covers. No fee or
// no seats ends the plan.
type PlanSetArgs struct {
	Name       string   `json:"name"`
	From       string   `json:"from"`
	Fee        int64    `json:"fee"`
	Seats      int      `json:"seats"`
	Principals []string `json:"principals,omitempty"`
}

// PlanSetResult says whether the terms were added, replaced (same name
// and from) or already so (unchanged).
type PlanSetResult struct {
	Change string `json:"change"`
}

// PlansArgs lists every plan; it takes nothing.
type PlansArgs struct{}

// Plan is a plan's terms from From (2006-01) on, and who last set them.
type Plan struct {
	Name       string    `json:"name"`
	From       string    `json:"from"`
	Fee        int64     `json:"fee"`
	Seats      int       `json:"seats"`
	Principals []string  `json:"principals"`
	SetBy      string    `json:"set_by"`
	SetAt      time.Time `json:"set_at"`
}

// PlansResult lists plans by name, then from.
type PlansResult struct {
	Plans []Plan `json:"plans"`
}

// hoursPattern is a duration of hours, minutes and seconds, in that
// order, each optional: 1.5h, 90m, 1h30m, 45m30s.
var hoursPattern = regexp.MustCompile(`^([0-9]{1,6}(\.[0-9]{1,6})?h)?([0-9]{1,6}(\.[0-9]{1,6})?m)?([0-9]{1,6}s)?$`)

// ParseHours reads the time a person logs: hours, minutes and seconds in
// Go's syntax and that order (1.5h, 90m, 1h30m), a whole number of
// seconds from a minute to 24 hours.
func ParseHours(s string) (time.Duration, error) {
	bad := errors.New("not a time from 1m to 24h such as 1.5h, 90m or 1h30m")
	if s == "" || !hoursPattern.MatchString(s) {
		return 0, bad
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < time.Minute || d > 24*time.Hour || d%time.Second != 0 {
		return 0, bad
	}
	return d, nil
}

// Hours renders seconds as decimal hours, rounded to the hundredth with
// halves up and without trailing zeros: "1.5h", "0.25h", "12h". Time
// above zero that rounds to nothing is "<0.01h".
func Hours(seconds int64) string {
	s := new(big.Rat).SetFrac64(seconds, 3600).FloatString(2)
	if s == "0.00" && seconds > 0 {
		return "<0.01h"
	}
	return strings.TrimSuffix(strings.TrimRight(s, "0"), ".") + "h"
}
