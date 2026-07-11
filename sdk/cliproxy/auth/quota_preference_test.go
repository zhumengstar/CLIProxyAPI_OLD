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
	for range 4 {
		got, err := selector.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, []*Auth{regular, preferred})
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != preferred.ID {
			t.Fatalf("picked %q, want %q", got.ID, preferred.ID)
		}
	}
}

func TestRoundRobinRotatesWithinSoonWeeklyQuotaPool(t *testing.T) {
	resetWeeklyQuotaPreferences(t)
	now := time.Now()
	lowerRemaining := &Auth{ID: "a-lower", Provider: "codex"}
	higherRemaining := &Auth{ID: "b-higher", Provider: "codex"}
	ObserveQuotaHeaders(lowerRemaining.ID, weeklyHeaders(now.Add(12*time.Hour), 40))
	ObserveQuotaHeaders(higherRemaining.ID, weeklyHeaders(now.Add(12*time.Hour), 10))

	selector := &RoundRobinSelector{}
	want := []string{lowerRemaining.ID, higherRemaining.ID, lowerRemaining.ID, higherRemaining.ID}
	for i := range want {
		got, err := selector.Pick(context.Background(), "codex", "gpt-5", cliproxyexecutor.Options{}, []*Auth{lowerRemaining, higherRemaining})
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != want[i] {
			t.Fatalf("pick %d = %q, want round-robin %q", i, got.ID, want[i])
		}
	}
}

func TestWeeklyQuotaPreferenceRequiresRemainingCapacity(t *testing.T) {
	resetWeeklyQuotaPreferences(t)
	now := time.Now()
	tooEmpty := &Auth{ID: "too-empty"}
	regular := &Auth{ID: "regular"}
	ObserveQuotaHeaders(tooEmpty.ID, weeklyHeaders(now.Add(12*time.Hour), 98))

	if got := preferExpiringWeeklyQuota([]*Auth{regular, tooEmpty}, now, "gemini-pro-agent"); len(got) != 2 {
		t.Fatalf("preferred len = %d, want ordinary candidate set", len(got))
	}
}

func TestWeeklyQuotaPreferenceUsesSecondaryBeforeReserveFallback(t *testing.T) {
	resetWeeklyQuotaPreferences(t)
	now := time.Now()
	secondary := &Auth{ID: "secondary"}
	reserve := &Auth{ID: "reserve"}
	ObserveQuotaHeaders(secondary.ID, weeklyHeaders(now.Add(60*time.Hour), 40))
	ObserveQuotaHeaders(reserve.ID, weeklyHeaders(now.Add(96*time.Hour), 40))

	for range 10 {
		got := preferExpiringWeeklyQuota([]*Auth{secondary, reserve}, now, "gemini-pro-agent")
		if len(got) != 1 {
			t.Fatalf("preferred len = %d, want 1", len(got))
		}
		if got[0].ID != secondary.ID {
			t.Fatalf("picked %q, want secondary pool %q", got[0].ID, secondary.ID)
		}
	}
}

func TestWeeklyQuotaPreferenceSeparatesModelFamilies(t *testing.T) {
	resetWeeklyQuotaPreferences(t)
	now := time.Now()
	auth := &Auth{ID: "dual-pool"}
	if got := ReplaceWeeklyQuotaSnapshots(auth.ID, map[string]WeeklyQuotaSnapshotUpdate{
		"gemini":     {ResetAt: now.Add(12 * time.Hour), RemainingPercent: 80},
		"claude-gpt": {ResetAt: now.Add(96 * time.Hour), RemainingPercent: 70},
	}); got != 2 {
		t.Fatalf("stored snapshots = %d, want 2", got)
	}

	_, geminiPool := weeklyPreferencePoolForAuth(auth, now, "gemini-3-pro")
	_, claudePool := weeklyPreferencePoolForAuth(auth, now, "claude-sonnet-4-6")
	if geminiPool != weeklyPreferencePoolUrgent || claudePool != weeklyPreferencePoolReserve {
		t.Fatalf("pools = gemini %v claude %v, want urgent/reserve", geminiPool, claudePool)
	}
}

func TestReplaceWeeklyQuotaSnapshotsClearsStaleFamily(t *testing.T) {
	resetWeeklyQuotaPreferences(t)
	now := time.Now()
	auth := &Auth{ID: "refresh-replace"}
	ReplaceWeeklyQuotaSnapshots(auth.ID, map[string]WeeklyQuotaSnapshotUpdate{
		"gemini":     {ResetAt: now.Add(12 * time.Hour), RemainingPercent: 80},
		"claude-gpt": {ResetAt: now.Add(12 * time.Hour), RemainingPercent: 80},
	})
	ReplaceWeeklyQuotaSnapshots(auth.ID, map[string]WeeklyQuotaSnapshotUpdate{
		"gemini": {ResetAt: now.Add(60 * time.Hour), RemainingPercent: 60},
	})

	_, geminiPool := weeklyPreferencePoolForAuth(auth, now, "gemini-3-pro")
	_, claudePool := weeklyPreferencePoolForAuth(auth, now, "gpt-oss-120b")
	if geminiPool != weeklyPreferencePoolSecondary || claudePool != weeklyPreferencePoolNone {
		t.Fatalf("pools after replace = gemini %v claude %v, want secondary/none", geminiPool, claudePool)
	}
}

func TestReplaceWeeklyQuotaSnapshotsClearsManualPriorityWhenExhausted(t *testing.T) {
	resetWeeklyQuotaPreferences(t)
	now := time.Now()
	auth := &Auth{ID: "manual-exhausted"}
	SetManualWeeklyPriority(auth.ID, true)

	ReplaceWeeklyQuotaSnapshots(auth.ID, map[string]WeeklyQuotaSnapshotUpdate{
		"gemini":     {ResetAt: now.Add(24 * time.Hour), RemainingPercent: 2},
		"claude-gpt": {ResetAt: now.Add(24 * time.Hour), RemainingPercent: 0},
	})

	if ManualWeeklyPriority(auth.ID) {
		t.Fatal("manual priority should be cleared when every weekly quota pool is exhausted")
	}
}

func TestReplaceWeeklyQuotaSnapshotsKeepsManualPriorityWithUsableQuota(t *testing.T) {
	resetWeeklyQuotaPreferences(t)
	now := time.Now()
	auth := &Auth{ID: "manual-usable"}
	SetManualWeeklyPriority(auth.ID, true)

	ReplaceWeeklyQuotaSnapshots(auth.ID, map[string]WeeklyQuotaSnapshotUpdate{
		"gemini":     {ResetAt: now.Add(24 * time.Hour), RemainingPercent: 2},
		"claude-gpt": {ResetAt: now.Add(24 * time.Hour), RemainingPercent: 3},
	})

	if !ManualWeeklyPriority(auth.ID) {
		t.Fatal("manual priority should remain while at least one weekly quota pool is usable")
	}
}

func TestReplaceWeeklyQuotaSnapshotsKeepsManualPriorityForIncompleteRefresh(t *testing.T) {
	resetWeeklyQuotaPreferences(t)
	now := time.Now()
	auth := &Auth{ID: "manual-incomplete"}
	SetManualWeeklyPriority(auth.ID, true)

	ReplaceWeeklyQuotaSnapshots(auth.ID, map[string]WeeklyQuotaSnapshotUpdate{
		"gemini": {ResetAt: now.Add(24 * time.Hour), RemainingPercent: 0},
	})

	if !ManualWeeklyPriority(auth.ID) {
		t.Fatal("manual priority should remain when refresh data is incomplete")
	}
}

func TestRestoreManualWeeklyPrioritySupportsMultipleAuths(t *testing.T) {
	first := &Auth{ID: "persisted-priority-first", Metadata: map[string]any{"manual_weekly_priority": true}}
	second := &Auth{ID: "persisted-priority-second", Metadata: map[string]any{"manual_weekly_priority": true}}
	t.Cleanup(func() {
		SetManualWeeklyPriority(first.ID, false)
		SetManualWeeklyPriority(second.ID, false)
	})

	if !RestoreManualWeeklyPriority(first) || !RestoreManualWeeklyPriority(second) {
		t.Fatal("expected both persisted manual priorities to be restored")
	}
	if !ManualWeeklyPriority(first.ID) || !ManualWeeklyPriority(second.ID) {
		t.Fatal("expected multiple auths to remain manually prioritized")
	}
}

func TestPersistManualWeeklyPriorityStoresDisabledState(t *testing.T) {
	auth := &Auth{ID: "persisted-priority-disabled"}
	PersistManualWeeklyPriority(auth, false)
	if enabled, ok := auth.Metadata["manual_weekly_priority"].(bool); !ok || enabled {
		t.Fatalf("expected persisted false state, got %#v", auth.Metadata)
	}
}

func TestWeeklyQuotaPreferenceAcceptsMoreThanTwoPercentRemaining(t *testing.T) {
	resetWeeklyQuotaPreferences(t)
	now := time.Now()
	preferred := &Auth{ID: "preferred"}
	regular := &Auth{ID: "regular"}
	ObserveQuotaHeaders(preferred.ID, weeklyHeaders(now.Add(12*time.Hour), 97.9))

	got := preferExpiringWeeklyQuota([]*Auth{regular, preferred}, now, "gemini-pro-agent")
	if len(got) != 1 || got[0].ID != preferred.ID {
		t.Fatalf("preferred = %#v, want only %q", got, preferred.ID)
	}
}

func TestSchedulerPrefersExpiringWeeklyQuota(t *testing.T) {
	resetWeeklyQuotaPreferences(t)
	now := time.Now()
	preferred := &Auth{ID: "preferred", Provider: "codex"}
	regular := &Auth{ID: "regular", Provider: "codex"}
	ObserveQuotaHeaders(preferred.ID, weeklyHeaders(now.Add(6*time.Hour), 60))

	view := readyView{flat: []*scheduledAuth{{auth: regular}, {auth: preferred}}}
	predicate := func(entry *scheduledAuth) bool { return entry != nil }
	got := view.pickRoundRobin(weightedWeeklyPreferredPredicate(view.flat, predicate, now, "gemini-pro-agent"))
	if got == nil || got.auth.ID != preferred.ID {
		t.Fatalf("picked %#v, want preferred", got)
	}
}

func TestSchedulerFiltersToSoonWeeklyQuotaPool(t *testing.T) {
	resetWeeklyQuotaPreferences(t)
	now := time.Now()
	lowerRemaining := &Auth{ID: "a-lower", Provider: "codex"}
	higherRemaining := &Auth{ID: "b-higher", Provider: "codex"}
	ObserveQuotaHeaders(lowerRemaining.ID, weeklyHeaders(now.Add(6*time.Hour), 40))
	ObserveQuotaHeaders(higherRemaining.ID, weeklyHeaders(now.Add(6*time.Hour), 10))

	view := readyView{flat: []*scheduledAuth{{auth: lowerRemaining}, {auth: higherRemaining}}}
	predicate := func(entry *scheduledAuth) bool { return entry != nil }
	picked := weightedWeeklyPreferredPredicate(view.flat, predicate, now, "gemini-pro-agent")
	for _, entry := range view.flat {
		if !picked(entry) {
			t.Fatalf("soon pool unexpectedly excluded %q", entry.auth.ID)
		}
	}
}

func TestSchedulerWeightsUrgentPoolByResetTime(t *testing.T) {
	resetWeeklyQuotaPreferences(t)
	now := time.Now()
	closer := &Auth{ID: "closer", Provider: "antigravity"}
	farther := &Auth{ID: "farther", Provider: "antigravity"}
	ObserveQuotaHeaders(closer.ID, weeklyHeaders(now.Add(2*time.Hour), 50))
	ObserveQuotaHeaders(farther.ID, weeklyHeaders(now.Add(20*time.Hour), 50))

	view := readyView{flat: []*scheduledAuth{{auth: closer}, {auth: farther}}}
	predicate, pool := weeklyPreferredPredicate(view.flat, nil, now, "gemini-pro-agent")
	if pool != weeklyPreferencePoolUrgent {
		t.Fatalf("pool = %v, want urgent", pool)
	}
	counts := map[string]int{}
	for range 90 {
		picked := view.pickSmoothWeightedRoundRobin(predicate, func(entry *scheduledAuth) int {
			return urgentWeeklyResetWeight(entry, now, "gemini-pro-agent")
		})
		if picked == nil {
			t.Fatal("expected a weighted pick")
		}
		counts[picked.auth.ID]++
	}
	if counts[closer.ID] <= counts[farther.ID] {
		t.Fatalf("closer picks = %d, farther picks = %d", counts[closer.ID], counts[farther.ID])
	}
	if counts[farther.ID] == 0 {
		t.Fatal("farther urgent auth must remain reachable")
	}
}

func TestSchedulerManualWeeklyPriorityOverridesStickySoon(t *testing.T) {
	resetWeeklyQuotaPreferences(t)
	now := time.Now()
	manual := &Auth{ID: "a-manual", Provider: "codex"}
	sticky := &Auth{ID: "b-sticky", Provider: "codex"}
	ObserveQuotaHeaders(manual.ID, weeklyHeaders(now.Add(6*time.Hour), 40))
	ObserveQuotaHeaders(sticky.ID, weeklyHeaders(now.Add(6*time.Hour), 10))
	SetManualWeeklyPriority(manual.ID, true)

	view := readyView{flat: []*scheduledAuth{{auth: manual}, {auth: sticky}}}
	state := &manualWeeklyRoutingState{}
	got := state.pick(view.flat, func(entry *scheduledAuth) bool { return entry != nil }, now, "gemini-pro-agent", "antigravity")
	if got == nil || got.auth.ID != manual.ID {
		t.Fatalf("picked %#v, want manual priority auth", got)
	}
}

func TestSchedulerRotatesMultipleManualPrioritiesWithTenSecondCadence(t *testing.T) {
	resetWeeklyQuotaPreferences(t)
	now := time.Now()
	first := &Auth{ID: "a-manual", Provider: "antigravity"}
	second := &Auth{ID: "b-manual", Provider: "antigravity"}
	for _, auth := range []*Auth{first, second} {
		ReplaceWeeklyQuotaSnapshots(auth.ID, map[string]WeeklyQuotaSnapshotUpdate{
			"gemini": {ResetAt: now.Add(12 * time.Hour), RemainingPercent: 80},
		})
		SetManualWeeklyPriority(auth.ID, true)
	}
	entries := []*scheduledAuth{{auth: second}, {auth: first}}
	predicate := func(entry *scheduledAuth) bool { return entry != nil }
	state := &manualWeeklyRoutingState{}

	if got := state.pick(entries, predicate, now, "gemini-3-pro", "antigravity"); got == nil || got.auth.ID != first.ID {
		t.Fatalf("first pick = %#v, want %q", got, first.ID)
	}
	if got := state.pick(entries, predicate, now.Add(time.Second), "gemini-3-pro", "antigravity"); got == nil || got.auth.ID != second.ID {
		t.Fatalf("second pick = %#v, want %q", got, second.ID)
	}
	if got := state.pick(entries, predicate, now.Add(2*time.Second), "gemini-3-pro", "antigravity"); got != nil {
		t.Fatalf("third pick inside cadence = %#v, want ordinary-routing fallback", got)
	}
	if got := state.pick(entries, predicate, now.Add(10*time.Second), "gemini-3-pro", "antigravity"); got == nil || got.auth.ID != first.ID {
		t.Fatalf("pick after cadence = %#v, want %q", got, first.ID)
	}
}

func TestSchedulerManualCadenceIsIndependentByModelFamily(t *testing.T) {
	resetWeeklyQuotaPreferences(t)
	now := time.Now()
	auth := &Auth{ID: "dual-manual", Provider: "antigravity"}
	ReplaceWeeklyQuotaSnapshots(auth.ID, map[string]WeeklyQuotaSnapshotUpdate{
		"gemini":     {ResetAt: now.Add(12 * time.Hour), RemainingPercent: 80},
		"claude-gpt": {ResetAt: now.Add(12 * time.Hour), RemainingPercent: 80},
	})
	SetManualWeeklyPriority(auth.ID, true)
	entries := []*scheduledAuth{{auth: auth}}
	predicate := func(entry *scheduledAuth) bool { return entry != nil }
	state := &manualWeeklyRoutingState{}

	if got := state.pick(entries, predicate, now, "gemini-3-pro", "antigravity"); got == nil {
		t.Fatal("gemini pool did not select pinned auth")
	}
	if got := state.pick(entries, predicate, now, "claude-sonnet-4-6", "antigravity"); got == nil {
		t.Fatal("claude/gpt pool should have an independent cadence")
	}
}

func TestManualWeeklyPriorityClearsOnlyUnavailablePool(t *testing.T) {
	resetWeeklyQuotaPreferences(t)
	now := time.Now()
	auth := &Auth{ID: "empty", Provider: "codex"}
	ObserveQuotaHeaders(auth.ID, weeklyHeaders(now.Add(6*time.Hour), 99))
	SetManualWeeklyPriority(auth.ID, true)

	state := &manualWeeklyRoutingState{}
	got := state.pick([]*scheduledAuth{{auth: auth}}, func(entry *scheduledAuth) bool { return entry != nil }, now, "gemini-pro-agent", "antigravity")
	if got != nil {
		t.Fatalf("picked %#v, want no manual priority auth", got)
	}
	if ManualWeeklyPriorityForPool(auth.ID, "gemini") {
		t.Fatal("gemini priority should be cleared after its quota is unavailable")
	}
	if !ManualWeeklyPriorityForPool(auth.ID, "claude-gpt") {
		t.Fatal("claude/gpt priority should remain independent")
	}
}

func TestManualWeeklyPrioritySurvivesOneExhaustedModelFamily(t *testing.T) {
	resetWeeklyQuotaPreferences(t)
	now := time.Now()
	auth := &Auth{ID: "one-family-left", Provider: "antigravity"}
	ReplaceWeeklyQuotaSnapshots(auth.ID, map[string]WeeklyQuotaSnapshotUpdate{
		"gemini":     {ResetAt: now.Add(12 * time.Hour), RemainingPercent: 0},
		"claude-gpt": {ResetAt: now.Add(12 * time.Hour), RemainingPercent: 70},
	})
	SetManualWeeklyPriority(auth.ID, true)
	state := &manualWeeklyRoutingState{}

	if got := state.pick([]*scheduledAuth{{auth: auth}}, func(entry *scheduledAuth) bool { return entry != nil }, now, "gemini-3-pro", "antigravity"); got != nil {
		t.Fatalf("exhausted gemini pool picked %#v", got)
	}
	if !ManualWeeklyPriority(auth.ID) {
		t.Fatal("manual priority was cleared even though claude/gpt quota remains")
	}
	if ManualWeeklyPriorityForPool(auth.ID, "gemini") {
		t.Fatal("exhausted gemini pool should no longer be pinned")
	}
	if !ManualWeeklyPriorityForPool(auth.ID, "claude-gpt") {
		t.Fatal("claude/gpt pool should remain pinned")
	}
}

func TestPersistPoolPriorityMigratesLegacyStateWithoutChangingOtherPool(t *testing.T) {
	resetWeeklyQuotaPreferences(t)
	auth := &Auth{
		ID:       "legacy-pool-migration",
		Metadata: map[string]any{manualWeeklyPriorityMetadataKey: true},
	}

	PersistManualWeeklyPriorityForPool(auth, "gemini", false)
	RestoreManualWeeklyPriority(auth)

	if ManualWeeklyPriorityForPool(auth.ID, "gemini") {
		t.Fatal("gemini priority should reflect the pool-specific update")
	}
	if !ManualWeeklyPriorityForPool(auth.ID, "claude-gpt") {
		t.Fatal("legacy claude/gpt priority should survive a gemini-only update")
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
		fiveHourQuotaPreferences.Range(func(key, _ any) bool {
			fiveHourQuotaPreferences.Delete(key)
			return true
		})
		manualWeeklyPriorityAuths.Range(func(key, _ any) bool {
			manualWeeklyPriorityAuths.Delete(key)
			return true
		})
	}
	clearPreferences()
	t.Cleanup(clearPreferences)
}

func TestFiveHourQuotaExhaustionRemovesAuthFromItsModelPool(t *testing.T) {
	resetWeeklyQuotaPreferences(t)
	now := time.Now()
	exhausted := &Auth{ID: "short-exhausted"}
	usable := &Auth{ID: "short-usable"}
	weekly := map[string]WeeklyQuotaSnapshotUpdate{
		"gemini": {ResetAt: now.Add(36 * time.Hour), RemainingPercent: 80},
	}
	ReplaceQuotaRoutingSnapshots(exhausted.ID, weekly, map[string]WeeklyQuotaSnapshotUpdate{
		"gemini": {ResetAt: now.Add(5 * time.Hour), RemainingPercent: 0},
	})
	ReplaceQuotaRoutingSnapshots(usable.ID, weekly, map[string]WeeklyQuotaSnapshotUpdate{
		"gemini": {ResetAt: now.Add(5 * time.Hour), RemainingPercent: 50},
	})

	view := readyView{flat: []*scheduledAuth{{auth: exhausted}, {auth: usable}}}
	picked := view.pickRoundRobin(weightedWeeklyPreferredPredicate(view.flat, nil, now, "gemini-3-pro"))
	if picked == nil || picked.auth.ID != usable.ID {
		t.Fatalf("picked %#v, want usable short-window auth", picked)
	}
}
