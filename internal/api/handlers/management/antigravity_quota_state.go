package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const antigravityQuotaStateFileName = ".antigravity-quota-state.json"

type antigravityQuotaState struct {
	SavedAt          string                     `json:"saved_at,omitempty"`
	QuotaRefreshedAt string                     `json:"quota_refreshed_at,omitempty"`
	Replace          bool                       `json:"replace,omitempty"`
	Files            map[string]json.RawMessage `json:"files,omitempty"`
}

type antigravityQuotaGroup struct {
	ID                     string                   `json:"id"`
	Name                   string                   `json:"name"`
	Label                  string                   `json:"label"`
	DisplayName            string                   `json:"displayName"`
	Description            string                   `json:"description"`
	Models                 []string                 `json:"models"`
	ResetTime              string                   `json:"resetTime"`
	ResetTimeSnake         string                   `json:"reset_time"`
	RemainingFraction      *float64                 `json:"remainingFraction"`
	RemainingFractionSnake *float64                 `json:"remaining_fraction"`
	Buckets                []antigravityQuotaBucket `json:"buckets"`
}

type antigravityQuotaBucket struct {
	BucketID               string   `json:"bucketId"`
	DisplayName            string   `json:"displayName"`
	Window                 string   `json:"window"`
	ResetTime              string   `json:"resetTime"`
	ResetTimeSnake         string   `json:"reset_time"`
	RemainingFraction      *float64 `json:"remainingFraction"`
	RemainingFractionSnake *float64 `json:"remaining_fraction"`
}

type antigravityQuotaFileState struct {
	Status           string                  `json:"status"`
	Groups           []antigravityQuotaGroup `json:"groups"`
	AccountTierID    string                  `json:"account_tier_id,omitempty"`
	AccountTierName  string                  `json:"account_tier_name,omitempty"`
	AccountTierLabel string                  `json:"account_tier_label,omitempty"`
}

func (h *Handler) restoreAntigravityWeeklyPreferences() {
	if h == nil || h.authManager == nil {
		return
	}
	state, err := h.readAntigravityQuotaState()
	if err != nil {
		return
	}
	h.seedAntigravityWeeklyPreferences(state, time.Now())
}

// GetAntigravityQuotaState returns the last persisted Antigravity quota snapshot.
func (h *Handler) GetAntigravityQuotaState(c *gin.Context) {
	state, err := h.readAntigravityQuotaState()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if state.Files == nil {
		state.Files = map[string]json.RawMessage{}
	}
	h.seedAntigravityWeeklyPreferences(state, time.Now())
	c.JSON(http.StatusOK, state)
}

// PutAntigravityQuotaState persists Antigravity quota state and seeds in-memory weekly routing preference.
func (h *Handler) PutAntigravityQuotaState(c *gin.Context) {
	var state antigravityQuotaState
	if err := c.ShouldBindJSON(&state); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if state.Files == nil {
		state.Files = map[string]json.RawMessage{}
	}
	state.Files = h.filterAntigravityQuotaFilesToActiveAuths(state.Files)
	now := time.Now().UTC()
	state.SavedAt = now.Format(time.RFC3339)
	if strings.TrimSpace(state.QuotaRefreshedAt) == "" {
		state.QuotaRefreshedAt = state.SavedAt
	}
	h.quotaStateMu.Lock()
	defer h.quotaStateMu.Unlock()
	existing, readErr := h.readAntigravityQuotaState()
	if readErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": readErr.Error()})
		return
	}
	for fileName, raw := range state.Files {
		state.Files[fileName] = mergeAntigravityQuotaAttempt(existing.Files[fileName], raw)
	}
	if !state.Replace {
		for fileName, raw := range existing.Files {
			if _, updated := state.Files[fileName]; !updated {
				state.Files[fileName] = raw
			}
		}
	}
	if err := h.writeAntigravityQuotaState(state); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	seeded := h.seedAntigravityWeeklyPreferences(state, now)
	c.JSON(http.StatusOK, gin.H{
		"saved_at":            state.SavedAt,
		"quota_refreshed_at":  state.QuotaRefreshedAt,
		"files":               len(state.Files),
		"weekly_seeded_count": seeded,
	})
}

func (h *Handler) readAntigravityQuotaState() (antigravityQuotaState, error) {
	var state antigravityQuotaState
	path, err := h.antigravityQuotaStatePath()
	if err != nil {
		return state, err
	}
	data, err := os.ReadFile(path)
	// Early versions wrote this state into auth-dir. Keep that snapshot readable
	// while moving new writes beside auth-dir so it is never loaded as an auth.
	if errors.Is(err, os.ErrNotExist) {
		legacyPath, legacyErr := h.legacyAntigravityQuotaStatePath()
		if legacyErr == nil {
			if legacyData, legacyReadErr := os.ReadFile(legacyPath); legacyReadErr == nil {
				data = legacyData
				err = nil
			}
		}
	}
	if errors.Is(err, os.ErrNotExist) {
		state.Files = map[string]json.RawMessage{}
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		state.Files = map[string]json.RawMessage{}
		return state, nil
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, err
	}
	if state.Files == nil {
		state.Files = map[string]json.RawMessage{}
	}
	state.Files = h.filterAntigravityQuotaFilesToActiveAuths(state.Files)
	for fileName, raw := range state.Files {
		state.Files[fileName] = normalizePersistedAntigravityTransient(raw)
	}
	return state, nil
}

func normalizePersistedAntigravityTransient(raw json.RawMessage) json.RawMessage {
	var state map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &state) != nil {
		return raw
	}
	var status string
	_ = json.Unmarshal(state["status"], &status)
	if strings.EqualFold(strings.TrimSpace(status), "success") {
		if err := validatePersistedAntigravityQuotaSuccess(raw, time.Now()); err != nil {
			return markPersistedAntigravityQuotaIncomplete(state, err, raw)
		}
		return raw
	}
	if !strings.EqualFold(strings.TrimSpace(status), "loading") {
		return raw
	}
	nextStatus := "idle"
	if groups := state["groups"]; len(groups) > 0 && string(groups) != "null" && string(groups) != "[]" {
		nextStatus = "success"
	}
	state["status"], _ = json.Marshal(nextStatus)
	normalized, err := json.Marshal(state)
	if err != nil {
		return raw
	}
	if nextStatus == "success" {
		if err := validatePersistedAntigravityQuotaSuccess(normalized, time.Now()); err != nil {
			return markPersistedAntigravityQuotaIncomplete(state, err, raw)
		}
	}
	return normalized
}

func (h *Handler) writeAntigravityQuotaState(state antigravityQuotaState) error {
	path, err := h.antigravityQuotaStatePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// mergeAntigravityQuotaAttempt keeps the last successful quota payload while
// recording a newer loading/error attempt. Transient refresh state must never
// erase routing data that is still valid.
func mergeAntigravityQuotaAttempt(existing, incoming json.RawMessage) json.RawMessage {
	var next map[string]json.RawMessage
	if len(incoming) == 0 || json.Unmarshal(incoming, &next) != nil {
		return incoming
	}
	var status string
	_ = json.Unmarshal(next["status"], &status)
	if strings.EqualFold(strings.TrimSpace(status), "success") {
		if err := validatePersistedAntigravityQuotaSuccess(incoming, time.Now()); err == nil {
			return incoming
		} else {
			incoming = markPersistedAntigravityQuotaIncomplete(next, err, incoming)
			_ = json.Unmarshal(incoming, &next)
			status = "error"
		}
	}
	if len(existing) == 0 {
		return incoming
	}

	var previous map[string]json.RawMessage
	if json.Unmarshal(existing, &previous) != nil || len(previous["groups"]) == 0 || string(previous["groups"]) == "null" {
		return incoming
	}
	for _, key := range []string{"groups", "account_tier_id", "account_tier_name", "account_tier_label"} {
		if value := previous[key]; len(value) > 0 {
			next[key] = value
		}
	}
	if attemptedAt := next["refreshed_at"]; len(attemptedAt) > 0 {
		next["attempted_at"] = attemptedAt
	}
	if refreshedAt := previous["refreshed_at"]; len(refreshedAt) > 0 {
		next["refreshed_at"] = refreshedAt
	}
	merged, err := json.Marshal(next)
	if err != nil {
		return incoming
	}
	return merged
}

// persistAntigravityQuotaSnapshot stores a successful official quota response by
// auth filename, so refresh results remain available after a browser reload.
func (h *Handler) persistAntigravityQuotaSnapshot(auth *coreauth.Auth, payload gin.H) error {
	if h == nil || auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "antigravity") {
		return nil
	}
	fileName := antigravityQuotaSnapshotFileName(auth)
	if fileName == "" || fileName == "." {
		return errors.New("antigravity auth filename is missing")
	}
	groups, ok := payload["groups"]
	if !ok {
		return errors.New("official quota response has no groups")
	}
	snapshot := gin.H{
		"status":       "success",
		"groups":       groups,
		"refreshed_at": time.Now().UTC().Format(time.RFC3339),
	}
	for _, key := range []string{"account_tier_id", "account_tier_name", "account_tier_label"} {
		if value, ok := payload[key]; ok {
			snapshot[key] = value
		}
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	if err := validatePersistedAntigravityQuotaSuccess(raw, time.Now()); err != nil {
		return err
	}
	return h.persistAntigravityQuotaStateFile(fileName, raw)
}

func markPersistedAntigravityQuotaIncomplete(state map[string]json.RawMessage, validationErr error, fallback json.RawMessage) json.RawMessage {
	if state == nil {
		return fallback
	}
	state["status"], _ = json.Marshal("error")
	state["error"], _ = json.Marshal("incomplete quota snapshot: " + validationErr.Error())
	normalized, err := json.Marshal(state)
	if err != nil {
		return fallback
	}
	return normalized
}

func validatePersistedAntigravityQuotaSuccess(raw json.RawMessage, now time.Time) error {
	var state antigravityQuotaFileState
	if len(raw) == 0 || json.Unmarshal(raw, &state) != nil {
		return errors.New("quota snapshot is not valid JSON")
	}
	if !strings.EqualFold(strings.TrimSpace(state.Status), "success") {
		return nil
	}
	if len(state.Groups) == 0 {
		return errors.New("quota groups are empty")
	}

	type quotaWindowState struct {
		hasRemaining bool
		hasReset     bool
	}
	windows := make(map[string]quotaWindowState, 4)
	for _, group := range state.Groups {
		groupDescriptor := strings.ToLower(strings.Join(append([]string{
			group.ID, group.Name, group.Label, group.DisplayName, group.Description,
		}, group.Models...), " "))
		family := antigravityQuotaGroupFamily(group)
		consider := func(descriptor string, bucket antigravityQuotaBucket) {
			candidateFamily := family
			if candidateFamily == "" {
				candidateFamily = antigravityQuotaFamily(descriptor)
			}
			window := persistedAntigravityQuotaWindow(groupDescriptor+" "+strings.ToLower(descriptor), bucket, now)
			if candidateFamily == "" || window == "" {
				return
			}
			_, hasRemaining := parsePersistedAntigravityRemaining(bucket)
			_, hasReset := parsePersistedAntigravityResetTime(bucket)
			key := candidateFamily + "-" + window
			current := windows[key]
			current.hasRemaining = current.hasRemaining || hasRemaining
			current.hasReset = current.hasReset || hasReset
			windows[key] = current
		}
		consider(groupDescriptor, antigravityQuotaBucket{
			ResetTime: group.ResetTime, ResetTimeSnake: group.ResetTimeSnake,
			RemainingFraction: group.RemainingFraction, RemainingFractionSnake: group.RemainingFractionSnake,
		})
		for _, bucket := range group.Buckets {
			descriptor := strings.Join([]string{
				bucket.BucketID, bucket.DisplayName, bucket.Window,
			}, " ")
			consider(descriptor, bucket)
		}
	}

	for _, key := range []string{"gemini-short", "gemini-weekly", "claude-gpt-short", "claude-gpt-weekly"} {
		window, ok := windows[key]
		if !ok || !window.hasRemaining {
			return fmt.Errorf("missing %s quota", key)
		}
		if strings.HasSuffix(key, "-weekly") && !window.hasReset {
			return fmt.Errorf("missing %s reset time", key)
		}
	}
	return nil
}

func persistedAntigravityQuotaWindow(descriptor string, bucket antigravityQuotaBucket, now time.Time) string {
	descriptor = strings.ToLower(descriptor)
	switch {
	case strings.Contains(descriptor, "week"), strings.Contains(descriptor, "周"):
		return "weekly"
	case strings.Contains(descriptor, "short"), strings.Contains(descriptor, "5h"), strings.Contains(descriptor, "5 hour"), strings.Contains(descriptor, "5-hour"), strings.Contains(descriptor, "5 小时"), strings.Contains(descriptor, "5小时"):
		return "short"
	}
	if resetAt, ok := parsePersistedAntigravityResetTime(bucket); ok {
		if resetAt.Sub(now) >= 24*time.Hour {
			return "weekly"
		}
		return "short"
	}
	return ""
}

func antigravityQuotaSnapshotFileName(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}

	for _, candidate := range []string{
		auth.FileName,
		authAttribute(auth, "path"),
		authAttribute(auth, coreauth.AttributeVirtualSource),
	} {
		fileName := filepath.Base(strings.TrimSpace(candidate))
		if fileName != "" && fileName != "." {
			return fileName
		}
	}

	if id := strings.TrimSpace(auth.ID); strings.HasSuffix(strings.ToLower(id), ".json") {
		return filepath.Base(id)
	}
	return ""
}

func (h *Handler) persistAntigravityQuotaSnapshotFromAPICall(auth *coreauth.Auth, urlStr string, statusCode int, body []byte) error {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "antigravity") || len(body) == 0 {
		return nil
	}
	parsedURL, err := url.Parse(urlStr)
	if err != nil {
		return nil
	}
	path := strings.ToLower(parsedURL.Path)
	if !strings.Contains(path, "fetchavailablemodels") && !strings.Contains(path, "retrieveuserquotasummary") {
		return nil
	}
	if statusCode < http.StatusOK || statusCode >= http.StatusMultipleChoices {
		return h.persistAntigravityQuotaError(auth, statusCode, body)
	}
	var payload antigravityAvailableModelsResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return err
	}
	groups := completeAntigravityQuotaGroups(payload.Groups, payload.Models, time.Now())
	return h.persistAntigravityQuotaSnapshot(auth, gin.H{"groups": groups})
}

func completeAntigravityQuotaGroups(groups []antigravityAvailableModelsGroup, rawModels json.RawMessage, now time.Time) []antigravityAvailableModelsGroup {
	fallback := antigravityQuotaGroupsFromModels(rawModels, now)
	result := append([]antigravityAvailableModelsGroup(nil), groups...)
	if len(result) == 0 {
		result = append(result, fallback...)
		return completeInactiveAntigravityShortWindows(result, now)
	}
	if len(fallback) == 0 {
		return completeInactiveAntigravityShortWindows(result, now)
	}

	seen := make(map[string]struct{}, len(groups))
	for _, group := range groups {
		for _, key := range antigravityQuotaGroupKeys(group, now) {
			seen[key] = struct{}{}
		}
	}
	for _, group := range fallback {
		keys := antigravityQuotaGroupKeys(group, now)
		missing := len(keys) == 0
		for _, key := range keys {
			if _, ok := seen[key]; !ok {
				missing = true
				break
			}
		}
		if !missing {
			continue
		}
		result = append(result, group)
		for _, key := range keys {
			seen[key] = struct{}{}
		}
	}
	return completeInactiveAntigravityShortWindows(result, now)
}

// The official API omits both short windows for accounts that have not started
// a five-hour usage cycle. Weekly windows remain present, so represent the
// inactive short windows as fully available without inventing a reset time.
func completeInactiveAntigravityShortWindows(groups []antigravityAvailableModelsGroup, now time.Time) []antigravityAvailableModelsGroup {
	seen := make(map[string]struct{}, 4)
	for _, group := range groups {
		for _, key := range antigravityQuotaGroupKeys(group, now) {
			seen[key] = struct{}{}
		}
	}

	_, geminiWeekly := seen["gemini-weekly"]
	_, claudeWeekly := seen["claude-gpt-weekly"]
	_, geminiShort := seen["gemini-short"]
	_, claudeShort := seen["claude-gpt-short"]
	if !geminiWeekly || !claudeWeekly || geminiShort || claudeShort {
		return groups
	}

	remaining := 1.0
	for _, item := range []struct {
		id    string
		label string
	}{
		{id: "gemini-short", label: "Gemini"},
		{id: "claude-gpt-short", label: "Claude/GPT"},
	} {
		groups = append(groups, antigravityAvailableModelsGroup{
			ID:          item.id,
			Label:       item.label,
			DisplayName: item.label,
			Description: "5-hour window has not started",
			Buckets: []antigravityAvailableModelsBucket{{
				ID:                item.id,
				BucketID:          item.id,
				Label:             "5 小时",
				DisplayName:       "5 小时",
				Window:            "short",
				RemainingFraction: &remaining,
			}},
		})
	}
	return groups
}

func antigravityQuotaGroupKeys(group antigravityAvailableModelsGroup, now time.Time) []string {
	groupName := antigravityQuotaDisplayName(group.ID, group.Label, group.DisplayName, group.DisplayNameSnake)
	family := antigravityQuotaFamily(groupName)
	if family == "" {
		return nil
	}
	keys := make([]string, 0, len(group.Buckets))
	for _, bucket := range group.Buckets {
		bucketName := antigravityQuotaDisplayName(bucket.ID, bucket.BucketID, bucket.BucketIDSnake, bucket.Label, bucket.DisplayName, bucket.DisplayNameSnake, bucket.Window)
		resetAt, hasReset := parseAntigravityResetTime(bucketResetTime(bucket))
		scope := "short"
		if strings.Contains(strings.ToLower(groupName+" "+bucketName), "week") || (hasReset && isAntigravityWeeklyQuota(bucketName, resetAt, now)) {
			scope = "weekly"
		}
		keys = append(keys, family+"-"+scope)
	}
	if len(keys) == 0 {
		scope := "short"
		if strings.Contains(strings.ToLower(groupName), "week") {
			scope = "weekly"
		}
		keys = append(keys, family+"-"+scope)
	}
	return keys
}

func antigravityQuotaGroupsFromModels(raw json.RawMessage, now time.Time) []antigravityAvailableModelsGroup {
	type aggregate struct {
		group     antigravityAvailableModelsGroup
		remaining float64
		resetTime string
	}

	aggregates := make(map[string]*aggregate)
	order := make([]string, 0, 4)
	for _, model := range antigravityAvailableModels(raw) {
		if model.QuotaInfo == nil || model.QuotaInfo.RemainingFraction == nil {
			continue
		}
		modelName := antigravityQuotaDisplayName(model.ID, model.Name, model.DisplayName, model.DisplayNameV1, model.Model, model.APIProvider, model.ModelProvider)
		family := antigravityQuotaFamily(modelName)
		if family == "" {
			continue
		}
		resetAt, hasReset := parseAntigravityResetTime(model.QuotaInfo.ResetTime)
		scope := "short"
		if hasReset && isAntigravityWeeklyQuota(modelName, resetAt, now) {
			scope = "weekly"
		}
		key := family + "-" + scope
		item := aggregates[key]
		if item == nil {
			label := "Gemini"
			if family == "claude-gpt" {
				label = "Claude/GPT"
			}
			windowLabel := "5 小时"
			if scope == "weekly" {
				windowLabel = "周"
			}
			item = &aggregate{
				group: antigravityAvailableModelsGroup{
					ID:          key,
					Label:       label,
					DisplayName: label,
					Description: modelName,
				},
				remaining: *model.QuotaInfo.RemainingFraction,
				resetTime: strings.TrimSpace(model.QuotaInfo.ResetTime),
			}
			item.group.Buckets = []antigravityAvailableModelsBucket{{
				ID:          key,
				BucketID:    key,
				Label:       windowLabel,
				DisplayName: windowLabel,
				Window:      scope,
			}}
			aggregates[key] = item
			order = append(order, key)
		}
		if *model.QuotaInfo.RemainingFraction < item.remaining {
			item.remaining = *model.QuotaInfo.RemainingFraction
			item.resetTime = strings.TrimSpace(model.QuotaInfo.ResetTime)
		}
	}

	groups := make([]antigravityAvailableModelsGroup, 0, len(order))
	for _, key := range order {
		item := aggregates[key]
		item.group.Buckets[0].RemainingFraction = &item.remaining
		item.group.Buckets[0].ResetTime = item.resetTime
		groups = append(groups, item.group)
	}
	return groups
}

func (h *Handler) persistAntigravityQuotaError(auth *coreauth.Auth, statusCode int, body []byte) error {
	fileName := antigravityQuotaSnapshotFileName(auth)
	if fileName == "" || fileName == "." {
		return errors.New("antigravity auth filename is missing")
	}
	message := compactQuotaErrorBody(body)
	if message == "" {
		message = http.StatusText(statusCode)
	}
	raw, err := json.Marshal(gin.H{
		"status":       "error",
		"error":        fmt.Sprintf("official quota API HTTP %d: %s", statusCode, message),
		"refreshed_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	return h.persistAntigravityQuotaStateFile(fileName, raw)
}

func (h *Handler) persistAntigravityQuotaFailure(auth *coreauth.Auth, refreshErr error) error {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "antigravity") || refreshErr == nil {
		return nil
	}
	fileName := antigravityQuotaSnapshotFileName(auth)
	if fileName == "" || fileName == "." {
		return errors.New("antigravity auth filename is missing")
	}
	message := strings.TrimSpace(refreshErr.Error())
	if len(message) > 1000 {
		message = message[:1000] + "..."
	}
	raw, err := json.Marshal(gin.H{
		"status":       "error",
		"error":        message,
		"refreshed_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	return h.persistAntigravityQuotaStateFile(fileName, raw)
}

func (h *Handler) persistAntigravityQuotaStateFile(fileName string, raw json.RawMessage) error {
	if h == nil {
		return errors.New("management handler is unavailable")
	}
	if !h.isActiveAntigravityAuthFileName(fileName) {
		return nil
	}
	h.quotaStateMu.Lock()
	defer h.quotaStateMu.Unlock()

	state, err := h.readAntigravityQuotaState()
	if err != nil {
		return err
	}
	if state.Files == nil {
		state.Files = map[string]json.RawMessage{}
	}
	state.Files = h.filterAntigravityQuotaFilesToActiveAuths(state.Files)
	state.Files[fileName] = mergeAntigravityQuotaAttempt(state.Files[fileName], raw)
	now := time.Now().UTC().Format(time.RFC3339)
	state.SavedAt = now
	state.QuotaRefreshedAt = now
	if err := h.writeAntigravityQuotaState(state); err != nil {
		return err
	}
	h.seedAntigravityWeeklyPreferences(state, time.Now())
	return nil
}

func (h *Handler) antigravityQuotaStatePath() (string, error) {
	if h == nil || h.cfg == nil || strings.TrimSpace(h.cfg.AuthDir) == "" {
		return "", errors.New("auth dir is not configured")
	}
	return filepath.Join(filepath.Dir(filepath.Clean(h.cfg.AuthDir)), antigravityQuotaStateFileName), nil
}

func (h *Handler) legacyAntigravityQuotaStatePath() (string, error) {
	if h == nil || h.cfg == nil || strings.TrimSpace(h.cfg.AuthDir) == "" {
		return "", errors.New("auth dir is not configured")
	}
	return filepath.Join(h.cfg.AuthDir, antigravityQuotaStateFileName), nil
}

func (h *Handler) seedAntigravityWeeklyPreferences(state antigravityQuotaState, now time.Time) int {
	if h == nil || h.authManager == nil || len(state.Files) == 0 {
		return 0
	}
	authIDs := h.antigravityAuthIDByFileName()
	seeded := 0
	for fileName, raw := range state.Files {
		authID := authIDs[filepath.Base(strings.TrimSpace(fileName))]
		if authID == "" {
			continue
		}
		var fileState antigravityQuotaFileState
		if json.Unmarshal(raw, &fileState) != nil {
			continue
		}
		snapshots := antigravityWeeklyQuotaByFamily(raw, now)
		wasPinned := map[string]bool{
			"gemini":     coreauth.ManualWeeklyPriorityForPool(authID, "gemini"),
			"claude-gpt": coreauth.ManualWeeklyPriorityForPool(authID, "claude-gpt"),
		}
		shortQuotas := antigravityFiveHourQuotaByFamily(raw, now)
		if strings.EqualFold(fileState.Status, "success") {
			seeded += coreauth.ReplaceQuotaRoutingSnapshots(authID, snapshots, shortQuotas)
		} else {
			for family, snapshot := range snapshots {
				if coreauth.ObserveWeeklyQuotaSnapshot(authID, family, snapshot.ResetAt, snapshot.RemainingPercent) {
					seeded++
				}
			}
			for family, snapshot := range shortQuotas {
				if coreauth.ObserveFiveHourQuotaSnapshot(authID, family, snapshot.ResetAt, snapshot.RemainingPercent) {
					seeded++
				}
			}
		}
		if strings.EqualFold(fileState.Status, "success") {
			for _, family := range []string{"gemini", "claude-gpt"} {
				short, available := shortQuotas[family]
				if !available || short.RemainingPercent <= 2 {
					coreauth.SetManualWeeklyPriorityForPool(authID, family, false)
				}
			}
		}
		auth, ok := h.authManager.GetByID(authID)
		if !ok {
			continue
		}
		changed := false
		for _, family := range []string{"gemini", "claude-gpt"} {
			pinned := coreauth.ManualWeeklyPriorityForPool(authID, family)
			if wasPinned[family] != pinned {
				coreauth.PersistManualWeeklyPriorityForPool(auth, family, pinned)
				changed = true
			}
		}
		if changed {
			auth.UpdatedAt = time.Now()
			_, _ = h.authManager.Update(context.Background(), auth)
		}
	}
	return seeded
}

func (h *Handler) antigravityAuthIDByFileName() map[string]string {
	result := map[string]string{}
	for _, auth := range h.authManager.List() {
		if auth == nil {
			continue
		}
		if base := filepath.Base(strings.TrimSpace(auth.FileName)); base != "" && base != "." {
			result[base] = auth.ID
		}
		if base := filepath.Base(strings.TrimSpace(authAttribute(auth, "path"))); base != "" && base != "." {
			result[base] = auth.ID
		}
		if base := filepath.Base(strings.TrimSpace(authAttribute(auth, coreauth.AttributeVirtualSource))); base != "" && base != "." {
			result[base] = auth.ID
		}
	}
	return result
}

func (h *Handler) filterAntigravityQuotaFilesToActiveAuths(files map[string]json.RawMessage) map[string]json.RawMessage {
	if len(files) == 0 || h == nil || h.authManager == nil {
		return files
	}
	active := h.activeAntigravityAuthFileNames()
	if active == nil {
		return files
	}
	filtered := make(map[string]json.RawMessage, len(files))
	for fileName, raw := range files {
		base := filepath.Base(strings.TrimSpace(fileName))
		if _, ok := active[base]; ok {
			filtered[base] = raw
		}
	}
	return filtered
}

func (h *Handler) activeAntigravityAuthFileNames() map[string]struct{} {
	if h == nil || h.authManager == nil {
		return nil
	}
	result := map[string]struct{}{}
	for _, auth := range h.authManager.List() {
		if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "antigravity") {
			continue
		}
		for _, candidate := range []string{
			auth.FileName,
			authAttribute(auth, "path"),
			authAttribute(auth, coreauth.AttributeVirtualSource),
		} {
			base := filepath.Base(strings.TrimSpace(candidate))
			if base != "" && base != "." {
				result[base] = struct{}{}
			}
		}
	}
	return result
}

func (h *Handler) isActiveAntigravityAuthFileName(fileName string) bool {
	if h == nil || h.authManager == nil {
		return true
	}
	base := filepath.Base(strings.TrimSpace(fileName))
	if base == "" || base == "." {
		return false
	}
	active := h.activeAntigravityAuthFileNames()
	if active == nil {
		return true
	}
	_, ok := active[base]
	return ok
}

func antigravityWeeklyQuotaByFamily(raw json.RawMessage, now time.Time) map[string]coreauth.WeeklyQuotaSnapshotUpdate {
	result := make(map[string]coreauth.WeeklyQuotaSnapshotUpdate, 2)
	var state antigravityQuotaFileState
	if len(raw) == 0 || json.Unmarshal(raw, &state) != nil || len(state.Groups) == 0 {
		return result
	}
	for _, group := range state.Groups {
		family := antigravityQuotaGroupFamily(group)
		if family == "" {
			continue
		}
		consider := func(bucket antigravityQuotaBucket) {
			resetAt, ok := parsePersistedAntigravityResetTime(bucket)
			if !ok || !resetAt.After(now) || resetAt.Sub(now) > 8*24*time.Hour {
				return
			}
			window := antigravityQuotaBucketWindow(bucket)
			if window == "short" || (window == "" && resetAt.Sub(now) < 24*time.Hour) {
				return
			}
			remaining, ok := parsePersistedAntigravityRemaining(bucket)
			if !ok {
				return
			}
			current, exists := result[family]
			if !exists || resetAt.Before(current.ResetAt) {
				result[family] = coreauth.WeeklyQuotaSnapshotUpdate{ResetAt: resetAt, RemainingPercent: remaining}
			}
		}
		consider(antigravityQuotaBucket{
			BucketID: group.ID, DisplayName: firstNonEmpty(group.DisplayName, group.Label, group.Name),
			ResetTime: group.ResetTime, ResetTimeSnake: group.ResetTimeSnake,
			RemainingFraction: group.RemainingFraction, RemainingFractionSnake: group.RemainingFractionSnake,
		})
		for _, bucket := range group.Buckets {
			consider(bucket)
		}
	}
	return result
}

func antigravityFiveHourQuotaByFamily(raw json.RawMessage, now time.Time) map[string]coreauth.WeeklyQuotaSnapshotUpdate {
	result := make(map[string]coreauth.WeeklyQuotaSnapshotUpdate, 2)
	var state antigravityQuotaFileState
	if len(raw) == 0 || json.Unmarshal(raw, &state) != nil || len(state.Groups) == 0 {
		return result
	}
	for _, group := range state.Groups {
		family := antigravityQuotaGroupFamily(group)
		if family == "" {
			continue
		}
		consider := func(bucket antigravityQuotaBucket) {
			resetAt, ok := parsePersistedAntigravityResetTime(bucket)
			if !ok || !resetAt.After(now) {
				return
			}
			window := antigravityQuotaBucketWindow(bucket)
			if window == "weekly" || (window == "" && resetAt.Sub(now) >= 24*time.Hour) {
				return
			}
			remaining, ok := parsePersistedAntigravityRemaining(bucket)
			if !ok {
				return
			}
			current, exists := result[family]
			if !exists || remaining < current.RemainingPercent {
				result[family] = coreauth.WeeklyQuotaSnapshotUpdate{ResetAt: resetAt, RemainingPercent: remaining}
			}
		}
		consider(antigravityQuotaBucket{
			BucketID: group.ID, DisplayName: firstNonEmpty(group.DisplayName, group.Label, group.Name),
			ResetTime: group.ResetTime, ResetTimeSnake: group.ResetTimeSnake,
			RemainingFraction: group.RemainingFraction, RemainingFractionSnake: group.RemainingFractionSnake,
		})
		for _, bucket := range group.Buckets {
			consider(bucket)
		}
	}
	return result
}

func antigravityQuotaBucketWindow(bucket antigravityQuotaBucket) string {
	descriptor := strings.ToLower(strings.Join([]string{bucket.Window, bucket.BucketID, bucket.DisplayName}, " "))
	switch {
	case strings.Contains(descriptor, "weekly"), strings.Contains(descriptor, "week"), strings.Contains(descriptor, "周"):
		return "weekly"
	case strings.Contains(descriptor, "five-hour"), strings.Contains(descriptor, "5-hour"),
		strings.Contains(descriptor, "5 hour"), strings.Contains(descriptor, "short"), strings.Contains(descriptor, "5小时"):
		return "short"
	default:
		return ""
	}
}

func antigravityQuotaGroupFamily(group antigravityQuotaGroup) string {
	descriptor := strings.ToLower(strings.Join(append([]string{
		group.ID, group.Name, group.Label, group.DisplayName, group.Description,
	}, group.Models...), " "))
	switch {
	case strings.Contains(descriptor, "gemini"):
		return "gemini"
	case strings.Contains(descriptor, "claude") || strings.Contains(descriptor, "gpt"):
		return "claude-gpt"
	default:
		return ""
	}
}

func parsePersistedAntigravityResetTime(bucket antigravityQuotaBucket) (time.Time, bool) {
	raw := strings.TrimSpace(bucket.ResetTime)
	if raw == "" {
		raw = strings.TrimSpace(bucket.ResetTimeSnake)
	}
	if raw == "" {
		return time.Time{}, false
	}
	if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return parsed, true
	}
	if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		return parsed, true
	}
	return time.Time{}, false
}

func parsePersistedAntigravityRemaining(bucket antigravityQuotaBucket) (float64, bool) {
	value := bucket.RemainingFraction
	if value == nil {
		value = bucket.RemainingFractionSnake
	}
	if value == nil {
		return 0, false
	}
	remaining := *value
	if remaining <= 1 {
		remaining *= 100
	}
	if remaining < 0 {
		remaining = 0
	} else if remaining > 100 {
		remaining = 100
	}
	return remaining, true
}
