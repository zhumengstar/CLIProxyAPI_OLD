package auth

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	weeklyWindowMinimum             = 6 * 24 * time.Hour
	weeklyRefreshUrgent             = 24 * time.Hour
	weeklyRefreshSoon               = 48 * time.Hour
	weeklySecondaryRefresh          = 72 * time.Hour
	weeklyRemainingMin              = 2.0
	manualPriorityInterval          = 10 * time.Second
	manualWeeklyPriorityMetadataKey = "manual_weekly_priority"
	manualGeminiPriorityMetadataKey = "manual_weekly_priority_gemini"
	manualClaudePriorityMetadataKey = "manual_weekly_priority_claude_gpt"
)

type weeklyQuotaPreference struct {
	resetAt          time.Time
	remainingPercent float64
}

var weeklyQuotaPreferences sync.Map
var fiveHourQuotaPreferences sync.Map
var manualWeeklyPriorityAuths sync.Map
var weeklyQuotaPreferencesMu sync.RWMutex

type weeklyPreferencePool int

const (
	weeklyPreferencePoolNone weeklyPreferencePool = iota
	weeklyPreferencePoolUrgent
	weeklyPreferencePoolSoon
	weeklyPreferencePoolSecondary
	weeklyPreferencePoolReserve
)

// WeeklyQuotaRoutingSnapshot is the read-only weekly quota state used by account routing.
type WeeklyQuotaRoutingSnapshot struct {
	ResetAt          time.Time `json:"reset_at"`
	RemainingPercent float64   `json:"remaining_percent"`
	Pool             string    `json:"pool"`
	ManualPriority   bool      `json:"manual_priority,omitempty"`
}

// WeeklyQuotaSnapshotUpdate is one model family's latest weekly quota value.
// ReplaceWeeklyQuotaSnapshots applies all families for an auth as one atomic update.
type WeeklyQuotaSnapshotUpdate struct {
	ResetAt          time.Time
	RemainingPercent float64
}

// SetManualWeeklyPriority preserves the legacy account-wide behavior by updating both pools.
func SetManualWeeklyPriority(authID string, enabled bool) bool {
	changed := false
	for _, family := range []string{"gemini", "claude-gpt"} {
		changed = SetManualWeeklyPriorityForPool(authID, family, enabled) || changed
	}
	return changed
}

// SetManualWeeklyPriorityForPool places an auth at the front of one model pool.
func SetManualWeeklyPriorityForPool(authID, modelFamily string, enabled bool) bool {
	authID = strings.TrimSpace(authID)
	family := weeklyQuotaFamilyKey(modelFamily)
	if authID == "" || family == "default" {
		return false
	}
	key := weeklyQuotaPreferenceKey(authID, family)
	if enabled {
		manualWeeklyPriorityAuths.Store(key, time.Now())
		return true
	}
	_, existed := manualWeeklyPriorityAuths.LoadAndDelete(key)
	return existed
}

// ManualWeeklyPriority reports whether either model pool is manually pinned.
func ManualWeeklyPriority(authID string) bool {
	return ManualWeeklyPriorityForPool(authID, "gemini") || ManualWeeklyPriorityForPool(authID, "claude-gpt")
}

// ManualWeeklyPriorityForPool reports whether an auth is pinned in one model pool.
func ManualWeeklyPriorityForPool(authID, modelFamily string) bool {
	authID = strings.TrimSpace(authID)
	family := weeklyQuotaFamilyKey(modelFamily)
	if authID == "" || family == "default" {
		return false
	}
	_, ok := manualWeeklyPriorityAuths.Load(weeklyQuotaPreferenceKey(authID, family))
	return ok
}

// RestoreManualWeeklyPriority restores a persisted pin when an auth is loaded.
func RestoreManualWeeklyPriority(auth *Auth) bool {
	if auth == nil || auth.Metadata == nil {
		return false
	}
	poolValues := make(map[string]bool, 2)
	poolValuesFound := false
	for _, family := range []string{"gemini", "claude-gpt"} {
		if enabled, ok := auth.Metadata[manualPriorityMetadataKeyForPool(family)].(bool); ok {
			poolValuesFound = true
			poolValues[family] = enabled
		}
	}
	if poolValuesFound {
		restored := false
		for _, family := range []string{"gemini", "claude-gpt"} {
			enabled := poolValues[family]
			SetManualWeeklyPriorityForPool(auth.ID, family, enabled)
			restored = restored || enabled
		}
		return restored
	}
	if enabled, ok := auth.Metadata[manualWeeklyPriorityMetadataKey].(bool); ok {
		SetManualWeeklyPriority(auth.ID, enabled)
		return enabled
	}
	return false
}

// PersistManualWeeklyPriority updates the auth metadata that is written by the token store.
func PersistManualWeeklyPriority(auth *Auth, enabled bool) {
	PersistManualWeeklyPriorityForPool(auth, "gemini", enabled)
	PersistManualWeeklyPriorityForPool(auth, "claude-gpt", enabled)
	if auth != nil {
		auth.Metadata[manualWeeklyPriorityMetadataKey] = enabled
	}
}

// PersistManualWeeklyPriorityForPool stores one pool's pin independently.
func PersistManualWeeklyPriorityForPool(auth *Auth, modelFamily string, enabled bool) {
	if auth == nil {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	_, hasGemini := auth.Metadata[manualGeminiPriorityMetadataKey]
	_, hasClaudeGPT := auth.Metadata[manualClaudePriorityMetadataKey]
	if !hasGemini && !hasClaudeGPT {
		if legacy, ok := auth.Metadata[manualWeeklyPriorityMetadataKey].(bool); ok {
			auth.Metadata[manualGeminiPriorityMetadataKey] = legacy
			auth.Metadata[manualClaudePriorityMetadataKey] = legacy
		}
	}
	key := manualPriorityMetadataKeyForPool(modelFamily)
	if key != "" {
		auth.Metadata[key] = enabled
	}
}

func manualPriorityMetadataKeyForPool(modelFamily string) string {
	switch weeklyQuotaFamilyKey(modelFamily) {
	case "gemini":
		return manualGeminiPriorityMetadataKey
	case "claude-gpt":
		return manualClaudePriorityMetadataKey
	default:
		return ""
	}
}

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
	storeWeeklyQuotaPreference(authID, "", preference)
}

// ObserveWeeklyQuotaSnapshot records a provider-reported weekly quota snapshot for routing.
func ObserveWeeklyQuotaSnapshot(authID string, modelFamily string, resetAt time.Time, remainingPercent float64) bool {
	authID = strings.TrimSpace(authID)
	if authID == "" || !resetAt.After(time.Now()) {
		return false
	}
	if remainingPercent < 0 {
		remainingPercent = 0
	} else if remainingPercent > 100 {
		remainingPercent = 100
	}
	storeWeeklyQuotaPreference(authID, modelFamily, weeklyQuotaPreference{
		resetAt:          resetAt,
		remainingPercent: remainingPercent,
	})
	return true
}

// ObserveFiveHourQuotaSnapshot records a provider-reported short-window quota snapshot.
func ObserveFiveHourQuotaSnapshot(authID string, modelFamily string, resetAt time.Time, remainingPercent float64) bool {
	authID = strings.TrimSpace(authID)
	if authID == "" || !resetAt.After(time.Now()) {
		return false
	}
	if remainingPercent < 0 {
		remainingPercent = 0
	} else if remainingPercent > 100 {
		remainingPercent = 100
	}
	fiveHourQuotaPreferences.Store(weeklyQuotaPreferenceKey(authID, modelFamily), weeklyQuotaPreference{
		resetAt:          resetAt,
		remainingPercent: remainingPercent,
	})
	return true
}

// ReplaceQuotaRoutingSnapshots atomically replaces both long- and short-window
// routing state after one complete successful refresh.
func ReplaceQuotaRoutingSnapshots(authID string, weekly, fiveHour map[string]WeeklyQuotaSnapshotUpdate) int {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return 0
	}
	now := time.Now()
	weeklyQuotaPreferencesMu.Lock()
	defer weeklyQuotaPreferencesMu.Unlock()
	for _, family := range []string{"gemini", "claude-gpt", "default"} {
		key := weeklyQuotaPreferenceKey(authID, family)
		weeklyQuotaPreferences.Delete(key)
		fiveHourQuotaPreferences.Delete(key)
	}
	weeklyQuotaPreferences.Delete(authID)

	stored := storeQuotaSnapshotUpdates(&weeklyQuotaPreferences, authID, weekly, now)
	stored += storeQuotaSnapshotUpdates(&fiveHourQuotaPreferences, authID, fiveHour, now)
	for family, snapshot := range fiveHour {
		if snapshot.RemainingPercent <= weeklyRemainingMin {
			manualWeeklyPriorityAuths.Delete(weeklyQuotaPreferenceKey(authID, family))
		}
	}
	return stored
}

func storeQuotaSnapshotUpdates(target *sync.Map, authID string, snapshots map[string]WeeklyQuotaSnapshotUpdate, now time.Time) int {
	stored := 0
	for family, snapshot := range snapshots {
		family = weeklyQuotaFamilyKey(family)
		if family == "default" || !snapshot.ResetAt.After(now) {
			continue
		}
		remaining := snapshot.RemainingPercent
		if remaining < 0 {
			remaining = 0
		} else if remaining > 100 {
			remaining = 100
		}
		target.Store(weeklyQuotaPreferenceKey(authID, family), weeklyQuotaPreference{
			resetAt: snapshot.ResetAt, remainingPercent: remaining,
		})
		stored++
	}
	return stored
}

// ReplaceWeeklyQuotaSnapshots atomically replaces model-specific quota routing
// state for an auth. Readers either observe the old snapshot or the complete new
// snapshot, never a partially refreshed pair of model pools.
func ReplaceWeeklyQuotaSnapshots(authID string, snapshots map[string]WeeklyQuotaSnapshotUpdate) int {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return 0
	}
	now := time.Now()
	weeklyQuotaPreferencesMu.Lock()
	defer weeklyQuotaPreferencesMu.Unlock()

	for _, family := range []string{"gemini", "claude-gpt", "default"} {
		weeklyQuotaPreferences.Delete(weeklyQuotaPreferenceKey(authID, family))
	}
	weeklyQuotaPreferences.Delete(authID)

	stored := 0
	for family, snapshot := range snapshots {
		family = weeklyQuotaFamilyKey(family)
		if family == "default" || !snapshot.ResetAt.After(now) {
			continue
		}
		remaining := snapshot.RemainingPercent
		if remaining < 0 {
			remaining = 0
		} else if remaining > 100 {
			remaining = 100
		}
		weeklyQuotaPreferences.Store(weeklyQuotaPreferenceKey(authID, family), weeklyQuotaPreference{
			resetAt:          snapshot.ResetAt,
			remainingPercent: remaining,
		})
		if remaining <= weeklyRemainingMin {
			manualWeeklyPriorityAuths.Delete(weeklyQuotaPreferenceKey(authID, family))
		}
		stored++
	}
	return stored
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

func weeklyPreferencePoolForAuth(auth *Auth, now time.Time, model string) (weeklyQuotaPreference, weeklyPreferencePool) {
	if auth == nil {
		return weeklyQuotaPreference{}, weeklyPreferencePoolNone
	}
	if short, known := loadFiveHourQuotaPreference(auth.ID, model, now); known && short.remainingPercent <= weeklyRemainingMin {
		return weeklyQuotaPreference{}, weeklyPreferencePoolNone
	}
	preference, ok := loadWeeklyQuotaPreference(auth.ID, model, now)
	if !ok {
		return weeklyQuotaPreference{}, weeklyPreferencePoolNone
	}
	if preference.remainingPercent <= weeklyRemainingMin {
		return weeklyQuotaPreference{}, weeklyPreferencePoolNone
	}
	untilReset := preference.resetAt.Sub(now)
	switch {
	case untilReset <= weeklyRefreshUrgent:
		return preference, weeklyPreferencePoolUrgent
	case untilReset <= weeklyRefreshSoon:
		return preference, weeklyPreferencePoolSoon
	case untilReset <= weeklySecondaryRefresh:
		return preference, weeklyPreferencePoolSecondary
	default:
		return preference, weeklyPreferencePoolReserve
	}
}

func loadFiveHourQuotaPreference(authID, model string, now time.Time) (weeklyQuotaPreference, bool) {
	authID = strings.TrimSpace(authID)
	family := weeklyQuotaFamilyKey(model)
	if authID == "" || family == "default" {
		return weeklyQuotaPreference{}, false
	}
	weeklyQuotaPreferencesMu.RLock()
	defer weeklyQuotaPreferencesMu.RUnlock()
	raw, ok := fiveHourQuotaPreferences.Load(weeklyQuotaPreferenceKey(authID, family))
	if !ok {
		return weeklyQuotaPreference{}, false
	}
	preference, ok := raw.(weeklyQuotaPreference)
	if !ok || !preference.resetAt.After(now) {
		return weeklyQuotaPreference{}, false
	}
	return preference, true
}

// WeeklyQuotaRoutingSnapshotForAuth returns the current routing bucket for management views.
func WeeklyQuotaRoutingSnapshotForAuth(auth *Auth, now time.Time, model string) (WeeklyQuotaRoutingSnapshot, bool) {
	preference, pool := weeklyPreferencePoolForAuth(auth, now, model)
	if pool == weeklyPreferencePoolNone {
		if auth != nil && ManualWeeklyPriorityForPool(auth.ID, model) && !hasUsableWeeklyQuotaForPool(auth.ID, model, now) {
			SetManualWeeklyPriorityForPool(auth.ID, model, false)
		}
		return WeeklyQuotaRoutingSnapshot{}, false
	}
	return WeeklyQuotaRoutingSnapshot{
		ResetAt:          preference.resetAt,
		RemainingPercent: preference.remainingPercent,
		Pool:             weeklyPreferencePoolName(pool),
		ManualPriority:   ManualWeeklyPriorityForPool(auth.ID, model),
	}, true
}

func weeklyPreferencePoolName(pool weeklyPreferencePool) string {
	switch pool {
	case weeklyPreferencePoolUrgent:
		return "24h-priority"
	case weeklyPreferencePoolSoon:
		return "48h-priority"
	case weeklyPreferencePoolSecondary:
		return "48-72h"
	case weeklyPreferencePoolReserve:
		return "reserve"
	default:
		return "unfetched"
	}
}

func preferExpiringWeeklyQuota(auths []*Auth, now time.Time, model string) []*Auth {
	preferred, _ := preferExpiringWeeklyQuotaWithMode(auths, now, model)
	return preferred
}

func preferExpiringWeeklyQuotaWithMode(auths []*Auth, now time.Time, model string) ([]*Auth, bool) {
	pools := make(map[weeklyPreferencePool][]*Auth, 4)
	for _, auth := range auths {
		_, pool := weeklyPreferencePoolForAuth(auth, now, model)
		if pool != weeklyPreferencePoolNone {
			pools[pool] = append(pools[pool], auth)
		}
	}
	for _, pool := range weeklyPreferencePoolsInOrder() {
		if len(pools[pool]) > 0 {
			return pools[pool], false
		}
	}
	return auths, false
}

func weightedWeeklyPreferredPredicate(entries []*scheduledAuth, predicate func(*scheduledAuth) bool, now time.Time, model string) func(*scheduledAuth) bool {
	predicate = fiveHourQuotaEligiblePredicate(predicate, now, model)
	counts := make(map[weeklyPreferencePool]int, 4)
	for _, entry := range entries {
		if entry == nil || entry.auth == nil || (predicate != nil && !predicate(entry)) {
			continue
		}
		_, pool := weeklyPreferencePoolForAuth(entry.auth, now, model)
		if pool != weeklyPreferencePoolNone {
			counts[pool]++
		}
	}
	for _, pool := range weeklyPreferencePoolsInOrder() {
		if counts[pool] > 0 {
			return weeklyPoolPredicate(predicate, now, model, pool)
		}
	}
	return predicate
}

func fiveHourQuotaEligiblePredicate(predicate func(*scheduledAuth) bool, now time.Time, model string) func(*scheduledAuth) bool {
	return func(entry *scheduledAuth) bool {
		if entry == nil || entry.auth == nil || (predicate != nil && !predicate(entry)) {
			return false
		}
		short, known := loadFiveHourQuotaPreference(entry.auth.ID, model, now)
		return !known || short.remainingPercent > weeklyRemainingMin
	}
}

type manualWeeklyRoutingState struct {
	cursors    map[string]int
	lastPicked map[string]time.Time
}

func (s *manualWeeklyRoutingState) pick(entries []*scheduledAuth, predicate func(*scheduledAuth) bool, now time.Time, model, scope string) *scheduledAuth {
	type candidate struct {
		entry *scheduledAuth
		pool  weeklyPreferencePool
	}
	candidates := make([]candidate, 0, len(entries))
	bestPool := weeklyPreferencePoolNone
	for _, entry := range entries {
		if entry == nil || entry.auth == nil || (predicate != nil && !predicate(entry)) {
			continue
		}
		if !ManualWeeklyPriorityForPool(entry.auth.ID, model) {
			continue
		}
		_, pool := weeklyPreferencePoolForAuth(entry.auth, now, model)
		if pool == weeklyPreferencePoolNone {
			if !hasUsableWeeklyQuotaForPool(entry.auth.ID, model, now) {
				SetManualWeeklyPriorityForPool(entry.auth.ID, model, false)
			}
			continue
		}
		if bestPool == weeklyPreferencePoolNone || pool < bestPool {
			bestPool = pool
		}
		candidates = append(candidates, candidate{entry: entry, pool: pool})
	}
	if len(candidates) == 0 {
		return nil
	}
	eligible := candidates[:0]
	for _, candidate := range candidates {
		if candidate.pool == bestPool {
			eligible = append(eligible, candidate)
		}
	}
	sort.Slice(eligible, func(i, j int) bool { return eligible[i].entry.auth.ID < eligible[j].entry.auth.ID })
	if s.cursors == nil {
		s.cursors = make(map[string]int)
	}
	if s.lastPicked == nil {
		s.lastPicked = make(map[string]time.Time)
	}
	family := weeklyQuotaFamilyKey(model)
	cursorKey := strings.TrimSpace(scope) + "\x00" + family + "\x00" + strconv.Itoa(int(bestPool))
	start := s.cursors[cursorKey] % len(eligible)
	for offset := 0; offset < len(eligible); offset++ {
		index := (start + offset) % len(eligible)
		entry := eligible[index].entry
		cadenceKey := entry.auth.ID + "\x00" + family
		if last := s.lastPicked[cadenceKey]; !last.IsZero() && now.Sub(last) < manualPriorityInterval {
			continue
		}
		s.cursors[cursorKey] = index + 1
		s.lastPicked[cadenceKey] = now
		return entry
	}
	return nil
}

func weeklyPreferencePoolsInOrder() []weeklyPreferencePool {
	return []weeklyPreferencePool{
		weeklyPreferencePoolUrgent,
		weeklyPreferencePoolSoon,
		weeklyPreferencePoolSecondary,
		weeklyPreferencePoolReserve,
	}
}

func hasUsableWeeklyQuota(authID string, now time.Time) bool {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return false
	}
	weeklyQuotaPreferencesMu.RLock()
	defer weeklyQuotaPreferencesMu.RUnlock()
	for _, family := range []string{"gemini", "claude-gpt", "default"} {
		preference, ok := loadWeeklyQuotaPreferenceKey(weeklyQuotaPreferenceKey(authID, family), now)
		if ok && preference.remainingPercent > weeklyRemainingMin {
			return true
		}
	}
	if preference, ok := loadWeeklyQuotaPreferenceKey(authID, now); ok && preference.remainingPercent > weeklyRemainingMin {
		return true
	}
	return false
}

func hasUsableWeeklyQuotaForPool(authID, modelFamily string, now time.Time) bool {
	authID = strings.TrimSpace(authID)
	family := weeklyQuotaFamilyKey(modelFamily)
	if authID == "" || family == "default" {
		return false
	}
	weeklyQuotaPreferencesMu.RLock()
	defer weeklyQuotaPreferencesMu.RUnlock()
	preference, ok := loadWeeklyQuotaPreferenceKey(weeklyQuotaPreferenceKey(authID, family), now)
	return ok && preference.remainingPercent > weeklyRemainingMin
}

func weeklyPoolPredicate(predicate func(*scheduledAuth) bool, now time.Time, model string, target weeklyPreferencePool) func(*scheduledAuth) bool {
	return func(entry *scheduledAuth) bool {
		if predicate != nil && !predicate(entry) {
			return false
		}
		if entry == nil {
			return false
		}
		_, pool := weeklyPreferencePoolForAuth(entry.auth, now, model)
		return pool == target
	}
}

func storeWeeklyQuotaPreference(authID string, modelFamily string, preference weeklyQuotaPreference) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return
	}
	weeklyQuotaPreferencesMu.Lock()
	defer weeklyQuotaPreferencesMu.Unlock()
	weeklyQuotaPreferences.Store(weeklyQuotaPreferenceKey(authID, modelFamily), preference)
	if strings.TrimSpace(modelFamily) == "" {
		weeklyQuotaPreferences.Store(authID, preference)
	}
}

func loadWeeklyQuotaPreference(authID string, model string, now time.Time) (weeklyQuotaPreference, bool) {
	authID = strings.TrimSpace(authID)
	if authID == "" {
		return weeklyQuotaPreference{}, false
	}

	weeklyQuotaPreferencesMu.RLock()
	defer weeklyQuotaPreferencesMu.RUnlock()
	if family := weeklyQuotaFamilyKey(model); family != "" && family != "default" {
		if preference, ok := loadWeeklyQuotaPreferenceKey(weeklyQuotaPreferenceKey(authID, family), now); ok {
			return preference, true
		}
	}
	if preference, ok := loadWeeklyQuotaPreferenceKey(weeklyQuotaPreferenceKey(authID, "default"), now); ok {
		return preference, true
	}
	if preference, ok := loadWeeklyQuotaPreferenceKey(authID, now); ok {
		return preference, true
	}
	return weeklyQuotaPreference{}, false
}

func loadWeeklyQuotaPreferenceKey(key string, now time.Time) (weeklyQuotaPreference, bool) {
	raw, ok := weeklyQuotaPreferences.Load(key)
	if !ok {
		return weeklyQuotaPreference{}, false
	}
	preference, ok := raw.(weeklyQuotaPreference)
	if !ok || !preference.resetAt.After(now) {
		return weeklyQuotaPreference{}, false
	}
	return preference, true
}

func weeklyQuotaPreferenceKey(authID string, modelFamily string) string {
	family := weeklyQuotaFamilyKey(modelFamily)
	if family == "" {
		family = "default"
	}
	return strings.TrimSpace(authID) + "\x00" + family
}

func weeklyQuotaFamilyKey(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	switch {
	case strings.Contains(model, "gemini"):
		return "gemini"
	case strings.Contains(model, "claude") || strings.Contains(model, "gpt"):
		return "claude-gpt"
	case model == "":
		return "default"
	default:
		return "default"
	}
}
