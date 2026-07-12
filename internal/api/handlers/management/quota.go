package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const resetQuotaBatchPageSizeMax = 100

const antigravityQuotaTokenRefreshAttempts = 3
const antigravityQuotaResetTimeout = 2 * time.Minute

var antigravityRetrieveUserQuotaSummaryURLs = []string{
	"https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
	"https://daily-cloudcode-pa.sandbox.googleapis.com/v1internal:retrieveUserQuotaSummary",
	"https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary",
}

const antigravityLoadCodeAssistURL = "https://cloudcode-pa.googleapis.com/v1internal:loadCodeAssist"

var fetchAntigravityProjectID = sdkauth.FetchAntigravityProjectID

type antigravityAccountTier struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type antigravityCodeAssistResponse struct {
	CurrentTier *antigravityAccountTier `json:"currentTier"`
	PaidTier    *antigravityAccountTier `json:"paidTier"`
}

// Quota exceeded toggles
func (h *Handler) GetSwitchProject(c *gin.Context) {
	c.JSON(200, gin.H{"switch-project": h.cfg.QuotaExceeded.SwitchProject})
}
func (h *Handler) PutSwitchProject(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.QuotaExceeded.SwitchProject = v })
}

func (h *Handler) GetSwitchPreviewModel(c *gin.Context) {
	c.JSON(200, gin.H{"switch-preview-model": h.cfg.QuotaExceeded.SwitchPreviewModel})
}
func (h *Handler) PutSwitchPreviewModel(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.QuotaExceeded.SwitchPreviewModel = v })
}

// ResetQuota clears quota/cooldown routing state for one auth index.
func (h *Handler) ResetQuota(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req struct {
		AuthIndex string `json:"auth_index"`
	}
	if errBindJSON := c.ShouldBindJSON(&req); errBindJSON != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	authIndex := strings.TrimSpace(req.AuthIndex)
	if authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is required"})
		return
	}

	auth := h.authByIndex(authIndex)
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}

	updated, models, quotaPayload, errReset := h.resetQuotaForAuth(c.Request.Context(), auth)
	if errReset != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to reset quota: %v", errReset)})
		return
	}
	if updated == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}
	updated.EnsureIndex()

	resp := gin.H{
		"status":     "ok",
		"auth_index": updated.Index,
		"models":     models,
	}
	for key, value := range quotaPayload {
		resp[key] = value
	}
	c.JSON(http.StatusOK, resp)
}

// ResetQuotaBatch clears quota/cooldown routing state for up to 100 auth indexes.
func (h *Handler) ResetQuotaBatch(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req struct {
		AuthIndexes []string `json:"auth_indexes"`
		Provider    string   `json:"provider"`
		Page        int      `json:"page"`
		PageSize    int      `json:"page_size"`
	}
	if errBindJSON := c.ShouldBindJSON(&req); errBindJSON != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	auths, page, pageSize, total, hasMore := h.resetQuotaBatchTargets(req.AuthIndexes, req.Provider, req.Page, req.PageSize)
	if len(auths) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no auth indexes matched"})
		return
	}
	if len(auths) > resetQuotaBatchPageSizeMax {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("batch size cannot exceed %d", resetQuotaBatchPageSizeMax)})
		return
	}

	results := make([]gin.H, 0, len(auths))
	succeeded := 0
	failed := 0
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		auth.EnsureIndex()
		result := gin.H{
			"auth_index": auth.Index,
			"id":         auth.ID,
			"name":       auth.FileName,
			"provider":   strings.TrimSpace(auth.Provider),
		}
		updated, models, quotaPayload, errReset := h.resetQuotaForAuth(c.Request.Context(), auth)
		if errReset != nil {
			failed++
			result["status"] = "error"
			result["error"] = fmt.Sprintf("failed to reset quota: %v", errReset)
		} else if updated == nil {
			failed++
			result["status"] = "not_found"
			result["error"] = "auth not found"
		} else {
			succeeded++
			updated.EnsureIndex()
			result["status"] = "ok"
			result["auth_index"] = updated.Index
			result["models"] = models
			for key, value := range quotaPayload {
				result[key] = value
			}
		}
		results = append(results, result)
	}

	statusCode := http.StatusOK
	if succeeded == 0 && failed > 0 {
		statusCode = http.StatusInternalServerError
	}
	c.JSON(statusCode, gin.H{
		"status":    "ok",
		"page":      page,
		"page_size": pageSize,
		"total":     total,
		"has_more":  hasMore,
		"succeeded": succeeded,
		"failed":    failed,
		"results":   results,
	})
}

func (h *Handler) resetQuotaBatchTargets(authIndexes []string, provider string, page, pageSize int) ([]*coreauth.Auth, int, int, int, bool) {
	pageSize = normalizeResetQuotaPageSize(pageSize)
	if page <= 0 {
		page = 1
	}

	if len(authIndexes) > 0 {
		auths := make([]*coreauth.Auth, 0, len(authIndexes))
		seen := make(map[string]struct{}, len(authIndexes))
		for _, rawIndex := range authIndexes {
			authIndex := strings.TrimSpace(rawIndex)
			if authIndex == "" {
				continue
			}
			if _, ok := seen[authIndex]; ok {
				continue
			}
			seen[authIndex] = struct{}{}
			auth := h.authByIndex(authIndex)
			if auth == nil {
				continue
			}
			auths = append(auths, auth)
		}
		return auths, 1, len(auths), len(auths), false
	}

	provider = strings.ToLower(strings.TrimSpace(provider))
	matches := make([]*coreauth.Auth, 0)
	for _, auth := range h.authManager.List() {
		if auth == nil {
			continue
		}
		if provider != "" && !strings.EqualFold(strings.TrimSpace(auth.Provider), provider) {
			continue
		}
		auth.EnsureIndex()
		matches = append(matches, auth)
	}
	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].Index < matches[j].Index
	})

	total := len(matches)
	start := (page - 1) * pageSize
	if start >= total {
		return nil, page, pageSize, total, false
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	return matches[start:end], page, pageSize, total, end < total
}

func normalizeResetQuotaPageSize(pageSize int) int {
	if pageSize <= 0 {
		return resetQuotaBatchPageSizeMax
	}
	if pageSize > resetQuotaBatchPageSizeMax {
		return resetQuotaBatchPageSizeMax
	}
	return pageSize
}

func (h *Handler) resetQuotaForAuth(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, []string, gin.H, error) {
	if h == nil || h.authManager == nil || auth == nil {
		return nil, nil, nil, nil
	}

	operationCtx, cancel := context.WithTimeout(context.Background(), antigravityQuotaResetTimeout)
	defer cancel()

	updated, models, errReset := h.authManager.ResetQuota(operationCtx, auth.ID)
	if errReset != nil {
		_ = h.persistAntigravityQuotaFailure(auth, errReset)
		return nil, nil, nil, errReset
	}
	if updated == nil {
		return nil, nil, nil, nil
	}

	quotaPayload := gin.H{}
	if h.canFetchAntigravityOfficialQuota(updated) {
		payload, errFetch := h.fetchAntigravityOfficialQuota(operationCtx, updated)
		if errFetch != nil {
			_ = h.persistAntigravityQuotaFailure(auth, errFetch)
			return nil, nil, nil, errFetch
		}
		probeResults := h.activateMissingAntigravityWeeklyWindows(operationCtx, updated, payload)
		if len(probeResults) > 0 {
			if refreshedPayload, errRefetch := h.fetchAntigravityOfficialQuota(operationCtx, updated); errRefetch == nil {
				payload = refreshedPayload
			} else {
				probeResults["refetch"] = "failed: " + compactQuotaErrorBody([]byte(errRefetch.Error()))
			}
			payload["weekly_activation_probes"] = probeResults
		}
		quotaPayload = payload
		if errPersist := h.persistAntigravityQuotaSnapshot(auth, payload); errPersist != nil {
			return nil, nil, nil, fmt.Errorf("persist antigravity quota: %w", errPersist)
		}
	}

	if len(quotaPayload) > 0 {
		if modelIDs, ok := quotaPayload["model_ids"].([]string); ok && len(modelIDs) > 0 {
			models = modelIDs
		}
	}
	return updated, models, quotaPayload, nil
}

func (h *Handler) canFetchAntigravityOfficialQuota(auth *coreauth.Auth) bool {
	if h == nil || auth == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "antigravity") {
		return false
	}
	if tokenValueForAuth(auth) != "" {
		return true
	}
	return stringValue(auth.Metadata, "refresh_token") != ""
}

func (h *Handler) fetchAntigravityOfficialQuota(ctx context.Context, auth *coreauth.Auth) (gin.H, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	token, errToken := h.resolveAntigravityQuotaToken(ctx, auth)
	if errToken != nil {
		return nil, fmt.Errorf("antigravity token refresh failed: %w", errToken)
	}
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("antigravity access token missing")
	}
	projectID, errProject := h.resolveAntigravityProjectID(ctx, auth, token)
	if errProject != nil {
		return nil, fmt.Errorf("antigravity project_id discovery failed: %w", errProject)
	}

	requestBody, errMarshal := json.Marshal(gin.H{"project": projectID})
	if errMarshal != nil {
		return nil, errMarshal
	}
	httpClient := &http.Client{
		Timeout:   defaultAPICallTimeout,
		Transport: h.apiCallTransport(auth),
	}

	var lastErr error
	var lastStatusCode int
	var lastBody []byte
	var quotaURL string
	for _, endpoint := range antigravityRetrieveUserQuotaSummaryURLs {
		req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(requestBody))
		if errReq != nil {
			return nil, errReq
		}
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "antigravity/cli/1.0.8 darwin/arm64")

		resp, errDo := httpClient.Do(req)
		if errDo != nil {
			lastErr = errDo
			continue
		}

		respBody, errRead := io.ReadAll(resp.Body)
		if errClose := resp.Body.Close(); errClose != nil {
			_ = errClose
		}
		if errRead != nil {
			lastErr = errRead
			continue
		}

		observeAntigravityQuotaSnapshotFromAPICall(auth, endpoint, resp.StatusCode, respBody)
		if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
			lastStatusCode = resp.StatusCode
			lastBody = respBody
			quotaURL = endpoint
			break
		}

		lastStatusCode = resp.StatusCode
		lastBody = respBody
		lastErr = fmt.Errorf("official API HTTP %d: %s", resp.StatusCode, compactQuotaErrorBody(respBody))
		if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusNotFound {
			break
		}
	}
	if len(lastBody) == 0 {
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, fmt.Errorf("official quota response empty")
	}
	if lastStatusCode < http.StatusOK || lastStatusCode >= http.StatusMultipleChoices {
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, fmt.Errorf("official API HTTP %d: %s", lastStatusCode, compactQuotaErrorBody(lastBody))
	}

	var payload antigravityAvailableModelsResponse
	if errUnmarshal := json.Unmarshal(lastBody, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("official quota response parse failed: %w", errUnmarshal)
	}
	payload.Groups = completeAntigravityQuotaGroups(payload.Groups, payload.Models, time.Now())
	if len(payload.Groups) == 0 {
		return nil, errors.New("暂无额度数据")
	}

	models := antigravityAvailableModels(payload.Models)
	modelIDs := make([]string, 0, len(models))
	for _, model := range models {
		modelID := strings.TrimSpace(model.ID)
		if modelID == "" {
			modelID = strings.TrimSpace(model.Model)
		}
		if modelID != "" {
			modelIDs = append(modelIDs, modelID)
		}
	}

	result := gin.H{
		"official_quota": true,
		"status_code":    lastStatusCode,
		"quota_url":      quotaURL,
		"groups":         payload.Groups,
		"models":         payload.Models,
		"model_ids":      dedupeQuotaModelIDs(modelIDs),
		"quota_summary":  antigravityQuotaSummary(payload, time.Now()),
		"project_id":     projectID,
	}
	if tier := fetchAntigravityAccountTier(ctx, httpClient, token); tier != nil {
		result["account_tier_id"] = tier.ID
		result["account_tier_name"] = tier.Name
		result["account_tier_label"] = antigravityAccountTierLabel(tier)
	}
	return result, nil
}

func (h *Handler) resolveAntigravityProjectID(ctx context.Context, auth *coreauth.Auth, token string) (string, error) {
	if projectID := authProjectID(auth); projectID != "" {
		return projectID, nil
	}
	if auth == nil || strings.TrimSpace(token) == "" {
		return "", errors.New("project_id missing and access token unavailable")
	}
	httpClient := &http.Client{
		Timeout:   defaultAPICallTimeout,
		Transport: h.apiCallTransport(auth),
	}
	projectID, err := fetchAntigravityProjectID(ctx, strings.TrimSpace(token), httpClient)
	if err != nil {
		return "", err
	}
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return "", errors.New("project discovery returned an empty project_id")
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["project_id"] = projectID
	if h != nil && h.authManager != nil {
		now := time.Now()
		auth.UpdatedAt = now
		if _, errUpdate := h.authManager.Update(ctx, auth); errUpdate != nil {
			return "", fmt.Errorf("persist project_id: %w", errUpdate)
		}
	}
	return projectID, nil
}

func (h *Handler) resolveAntigravityQuotaToken(ctx context.Context, auth *coreauth.Auth) (string, error) {
	var lastErr error
	for attempt := 0; attempt < antigravityQuotaTokenRefreshAttempts; attempt++ {
		token, err := h.resolveTokenForAuth(ctx, auth)
		if err == nil {
			return token, nil
		}
		lastErr = err
		if !isTransientQuotaRefreshError(err) || attempt == antigravityQuotaTokenRefreshAttempts-1 {
			break
		}
		delay := time.Duration(attempt+1) * 250 * time.Millisecond
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
	}
	return "", lastErr
}

func isTransientQuotaRefreshError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary()) {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"tls handshake timeout",
		"connection reset",
		"connection refused",
		"temporary failure",
		"unexpected eof",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func fetchAntigravityAccountTier(ctx context.Context, client *http.Client, token string) *antigravityAccountTier {
	body := bytes.NewBufferString(`{"metadata":{"ideType":"ANTIGRAVITY"}}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, antigravityLoadCodeAssistURL, body)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "antigravity/cli/1.0.8 darwin/arm64")
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil
	}
	var payload antigravityCodeAssistResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil
	}
	if payload.PaidTier != nil && strings.TrimSpace(payload.PaidTier.ID) != "" {
		return payload.PaidTier
	}
	return payload.CurrentTier
}

func antigravityAccountTierLabel(tier *antigravityAccountTier) string {
	if tier == nil {
		return ""
	}
	value := strings.ToLower(strings.TrimSpace(tier.ID + " " + tier.Name))
	switch {
	case strings.Contains(value, "ultra"):
		return "Ultra"
	case strings.Contains(value, "pro"), strings.Contains(value, "paid"):
		return "Pro"
	case strings.Contains(value, "free"), strings.Contains(value, "standard"):
		return "Free"
	default:
		return firstNonEmptyPlain(tier.Name, tier.ID)
	}
}

func compactQuotaErrorBody(body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > 500 {
		return text[:500] + "..."
	}
	return text
}

func dedupeQuotaModelIDs(values []string) []string {
	if len(values) < 2 {
		return values
	}
	seen := make(map[string]struct{}, len(values))
	out := values[:0]
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func antigravityQuotaSummary(payload antigravityAvailableModelsResponse, now time.Time) []gin.H {
	summary := make([]gin.H, 0)
	for _, group := range payload.Groups {
		groupLabel := antigravityQuotaDisplayName(group.Label, group.DisplayName, group.DisplayNameSnake)
		family := antigravityQuotaFamily(group.ID + " " + groupLabel)
		for _, bucket := range group.Buckets {
			remainingFraction := bucketRemainingFraction(bucket)
			if remainingFraction == nil {
				continue
			}
			resetTime := bucketResetTime(bucket)
			resetAt, hasReset := parseAntigravityResetTime(resetTime)
			scope := "unknown"
			if hasReset && isAntigravityWeeklyQuota(antigravityQuotaDisplayName(bucket.ID, bucket.BucketID, bucket.BucketIDSnake, bucket.Label, bucket.DisplayName, bucket.DisplayNameSnake, bucket.Window), resetAt, now) {
				scope = "weekly"
			} else if strings.TrimSpace(bucket.Window) != "" || hasReset {
				scope = "short"
			}
			item := gin.H{
				"family":             family,
				"scope":              scope,
				"group_id":           strings.TrimSpace(group.ID),
				"group_label":        groupLabel,
				"bucket_id":          firstNonEmptyPlain(bucket.ID, bucket.BucketID, bucket.BucketIDSnake),
				"bucket_label":       antigravityQuotaDisplayName(bucket.Label, bucket.DisplayName, bucket.DisplayNameSnake),
				"window":             strings.TrimSpace(bucket.Window),
				"remaining_fraction": *remainingFraction,
				"remaining_percent":  *remainingFraction * 100,
				"reset_time":         resetTime,
			}
			if hasReset {
				item["reset_unix"] = resetAt.Unix()
				item["reset_in_seconds"] = int64(resetAt.Sub(now).Seconds())
			}
			summary = append(summary, item)
		}
	}
	for _, model := range antigravityAvailableModels(payload.Models) {
		if model.QuotaInfo == nil || model.QuotaInfo.RemainingFraction == nil {
			continue
		}
		modelLabel := antigravityQuotaDisplayName(model.ID, model.Name, model.DisplayName, model.DisplayNameV1, model.Model, model.APIProvider, model.ModelProvider)
		family := antigravityQuotaFamily(modelLabel)
		resetAt, hasReset := parseAntigravityResetTime(model.QuotaInfo.ResetTime)
		scope := "unknown"
		if hasReset && isAntigravityWeeklyQuota(modelLabel, resetAt, now) {
			scope = "weekly"
		} else if hasReset {
			scope = "short"
		}
		item := gin.H{
			"family":             family,
			"scope":              scope,
			"model_id":           strings.TrimSpace(model.ID),
			"model":              strings.TrimSpace(model.Model),
			"model_label":        antigravityQuotaDisplayName(model.Name, model.DisplayName, model.DisplayNameV1),
			"api_provider":       strings.TrimSpace(model.APIProvider),
			"model_provider":     strings.TrimSpace(model.ModelProvider),
			"remaining_fraction": *model.QuotaInfo.RemainingFraction,
			"remaining_percent":  *model.QuotaInfo.RemainingFraction * 100,
			"reset_time":         strings.TrimSpace(model.QuotaInfo.ResetTime),
		}
		if hasReset {
			item["reset_unix"] = resetAt.Unix()
			item["reset_in_seconds"] = int64(resetAt.Sub(now).Seconds())
		}
		summary = append(summary, item)
	}
	return summary
}

func firstNonEmptyPlain(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
