package auth

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestRoundRobinPrefersExpiringWeeklyQuotaWithRemainingCapacity(t *testing.T) {
	resetWeeklyQuotaPreferences(t)
	now := time.Now()
	preferred := &Auth{ID: "preferred", Provider: "codex"}
	regular := &Auth{ID: "regular", Provider: "codex"}
	ObserveQuotaHeaders(preferred.ID, weeklyHeaders(now.Add(12*time.Hour), 25))

	selector := &RoundRobinSelector{}
	expected := []string{"preferred", "preferred", "preferred", "regular"}
	for i, want := range expected {
		got, err := selector.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, []*Auth{regular, preferred})
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != want {
			t.Fatalf("pick %d = %q, want %q", i, got.ID, want)
		}
	}
}

func TestWeeklyQuotaPreferenceRequiresSoonResetAndRemainingCapacity(t *testing.T) {
	resetWeeklyQuotaPreferences(t)
	now := time.Now()
	tooLate := &Auth{ID: "too-late"}
	tooEmpty := &Auth{ID: "too-empty"}
	ObserveQuotaHeaders(tooLate.ID, weeklyHeaders(now.Add(72*time.Hour), 10))
	ObserveQuotaHeaders(tooEmpty.ID, weeklyHeaders(now.Add(12*time.Hour), 90))

	if got := preferExpiringWeeklyQuota([]*Auth{tooLate, tooEmpty}, now); len(got) != 2 {
		t.Fatalf("preferred len = %d, want ordinary candidate set", len(got))
	}
}

func TestSchedulerPrefersExpiringWeeklyQuota(t *testing.T) {
	resetWeeklyQuotaPreferences(t)
	now := time.Now()
	preferred := &Auth{ID: "preferred", Provider: "codex"}
	regular := &Auth{ID: "regular", Provider: "codex"}
	ObserveQuotaHeaders(preferred.ID, weeklyHeaders(now.Add(6*time.Hour), 30))

	view := readyView{flat: []*scheduledAuth{{auth: regular}, {auth: preferred}}}
	predicate := func(entry *scheduledAuth) bool { return entry != nil }
	if !hasWeeklyPreferredEntry(view.flat, predicate, now) {
		t.Fatal("expected preferred scheduler entry")
	}
	got := view.pickRoundRobin(weeklyPreferredPredicate(predicate, now))
	if got == nil || got.auth.ID != preferred.ID {
		t.Fatalf("picked %#v, want preferred", got)
	}
}

func weeklyHeaders(resetAt time.Time, usedPercent float64) http.Header {
	headers := make(http.Header)
	headers.Set("x-codex-secondary-window-minutes", "10080")
	headers.Set("x-codex-secondary-used-percent", strconv.FormatFloat(usedPercent, 'f', -1, 64))
	headers.Set("x-codex-secondary-reset-at", strconv.FormatInt(resetAt.Unix(), 10))
	return headers
}

func resetWeeklyQuotaPreferences(t *testing.T) {
	t.Helper()
	clearPreferences := func() {
		weeklyQuotaPreferences.Range(func(key, _ any) bool {
			weeklyQuotaPreferences.Delete(key)
			return true
		})
	}
	clearPreferences()
	t.Cleanup(clearPreferences)
}
