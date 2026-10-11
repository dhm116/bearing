package boot

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"
)

var isoDuration = regexp.MustCompile(`^P(?:(\d+)W)?(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)S)?)?$`)

// minSchedule keeps a typo from asking a source to sync every second.
const minSchedule = time.Minute

// ParseSchedule reads the ISO 8601 duration of a Source's schedule, such as
// PT1H or P1D. Weeks, days, hours, minutes and seconds are accepted; years
// and months are not, since they have no fixed length. Empty is zero: the
// source takes webhooks and manual syncs only. Anything shorter than a minute
// is refused.
func ParseSchedule(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	m := isoDuration.FindStringSubmatch(s)
	if m == nil || s == "P" || s[len(s)-1] == 'T' {
		return 0, fmt.Errorf("%q is not an ISO 8601 duration of weeks, days, hours, minutes or seconds, such as PT1H", s)
	}
	unit := [...]time.Duration{7 * 24 * time.Hour, 24 * time.Hour, time.Hour, time.Minute, time.Second}
	var d time.Duration
	for i, u := range unit {
		if m[i+1] == "" {
			continue
		}
		n, err := strconv.ParseInt(m[i+1], 10, 32)
		if err != nil {
			return 0, errors.New("a number in the duration is too large")
		}
		d += time.Duration(n) * u
		if d < 0 || d > 366*24*time.Hour {
			return 0, errors.New("the duration is longer than a year")
		}
	}
	if d < minSchedule {
		return 0, fmt.Errorf("%q is shorter than %s", s, minSchedule)
	}
	return d, nil
}
