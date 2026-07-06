package auth

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	weeklyWindowMinimum = 6 * 24 * time.Hour
	weeklyRefreshSoon   = 48 * time.Hour
	weeklyRemainingMin  = 50.0
	weeklyPreferWeight  = 3
)

type weeklyQuotaPreference struct {
	resetAt          time.Time
	remainingPercent float64
}

var weeklyQuotaPreferences sync.Map

// ObserveQuotaHeaders records long-window quota information used by account selection.
func ObserveQuotaHeaders(authID string, headers http.Header) {
	authID = strings.TrimSpace(authID)
	if authID == "" || len(headers) == 0 {
		return
	}

	now := time.Now()
	preference, ok := parseWeeklyQuotaPreference(headers, now)
	if !ok {
		return
	}
	weeklyQuotaPreferences.Store(authID, preference)
}

func parseWeeklyQuotaPreference(headers http.Header, now time.Time) (weeklyQuotaPreference, bool) {
	var best weeklyQuotaPreference
	bestWindow := time.Duration(0)
	for _, window := range []string{"primary", "secondary"} {
		prefix := "x-codex-" + window + "-"
		windowMinutes, okWindow := parseHeaderFloat(headers, prefix+"window-minutes")
		usedPercent, okUsed := parseHeaderFloat(headers, prefix+"used-percent")
		resetUnix, okReset := parseHeaderInt64(headers, prefix+"reset-at")
		if !okWindow || !okUsed || !okReset {
			continue
		}
		windowDuration := time.Duration(windowMinutes * float64(time.Minute))
		resetAt := time.Unix(resetUnix, 0)
		if windowDuration < weeklyWindowMinimum || !resetAt.After(now) || windowDuration <= bestWindow {
			continue
		}
		remaining := 100 - usedPercent
		if remaining < 0 {
			remaining = 0
		} else if remaining > 100 {
			remaining = 100
		}
		best = weeklyQuotaPreference{resetAt: resetAt, remainingPercent: remaining}
		bestWindow = windowDuration
	}
	return best, bestWindow > 0
}

func parseHeaderFloat(headers http.Header, key string) (float64, bool) {
	raw := strings.TrimSpace(headers.Get(key))
	if raw == "" {
		return 0, false
	}
	value, err := strconv.ParseFloat(raw, 64)
	return value, err == nil
}

func parseHeaderInt64(headers http.Header, key string) (int64, bool) {
	raw := strings.TrimSpace(headers.Get(key))
	if raw == "" {
		return 0, false
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	return value, err == nil
}

func weeklyPreferenceForAuth(auth *Auth, now time.Time) (weeklyQuotaPreference, bool) {
	if auth == nil {
		return weeklyQuotaPreference{}, false
	}
	raw, ok := weeklyQuotaPreferences.Load(auth.ID)
	if !ok {
		return weeklyQuotaPreference{}, false
	}
	preference, ok := raw.(weeklyQuotaPreference)
	if !ok || !preference.resetAt.After(now) {
		weeklyQuotaPreferences.Delete(auth.ID)
		return weeklyQuotaPreference{}, false
	}
	if preference.resetAt.Sub(now) > weeklyRefreshSoon || preference.remainingPercent < weeklyRemainingMin {
		return weeklyQuotaPreference{}, false
	}
	return preference, true
}

func preferExpiringWeeklyQuota(auths []*Auth, now time.Time) []*Auth {
	preferred := make([]*Auth, 0, len(auths))
	ordinary := make([]*Auth, 0, len(auths))
	for _, candidate := range auths {
		if _, ok := weeklyPreferenceForAuth(candidate, now); ok {
			preferred = append(preferred, candidate)
			continue
		}
		ordinary = append(ordinary, candidate)
	}
	if len(preferred) == 0 {
		return auths
	}
	weighted := make([]*Auth, 0, len(preferred)*weeklyPreferWeight+len(ordinary))
	for i := 0; i < weeklyPreferWeight; i++ {
		weighted = append(weighted, preferred...)
	}
	weighted = append(weighted, ordinary...)
	return weighted
}

func hasWeeklyPreferredEntry(entries []*scheduledAuth, predicate func(*scheduledAuth) bool, now time.Time) bool {
	for _, entry := range entries {
		if entry == nil || entry.auth == nil || (predicate != nil && !predicate(entry)) {
			continue
		}
		if _, ok := weeklyPreferenceForAuth(entry.auth, now); ok {
			return true
		}
	}
	return false
}

func weeklyPreferredPredicate(predicate func(*scheduledAuth) bool, now time.Time) func(*scheduledAuth) bool {
	return func(entry *scheduledAuth) bool {
		if predicate != nil && !predicate(entry) {
			return false
		}
		if entry == nil {
			return false
		}
		_, ok := weeklyPreferenceForAuth(entry.auth, now)
		return ok
	}
}

func weeklyPreferredEntries(entries []*scheduledAuth, predicate func(*scheduledAuth) bool, now time.Time) []*scheduledAuth {
	preferred := make([]*scheduledAuth, 0, len(entries))
	ordinary := make([]*scheduledAuth, 0, len(entries))
	for _, entry := range entries {
		if entry == nil || (predicate != nil && !predicate(entry)) {
			continue
		}
		if _, ok := weeklyPreferenceForAuth(entry.auth, now); ok {
			preferred = append(preferred, entry)
			continue
		}
		ordinary = append(ordinary, entry)
	}
	if len(preferred) == 0 {
		return nil
	}
	weighted := make([]*scheduledAuth, 0, len(preferred)*weeklyPreferWeight+len(ordinary))
	for i := 0; i < weeklyPreferWeight; i++ {
		weighted = append(weighted, preferred...)
	}
	weighted = append(weighted, ordinary...)
	return weighted
}
