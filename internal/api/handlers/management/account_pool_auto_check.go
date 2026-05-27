package management

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const accountPoolAutoCheckConcurrency = 1

var accountPoolAutoCheckRunning atomic.Bool

type accountPoolAutoCheckEntry struct {
	Name        string
	Data        []byte
	ContentHash string
	Provider    string
}

type accountPoolQuotaCheckResponse struct {
	StatusCode int
	Body       string
}

type accountPoolQuotaDetail struct {
	Label     string   `json:"label"`
	Remaining string   `json:"remaining"`
	Reset     string   `json:"reset"`
	Percent   *float64 `json:"percent,omitempty"`
}

func (h *Handler) startAccountPoolAutoCheck() {
	if h == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(accountPoolAutoCheckInterval)
		defer ticker.Stop()
		for range ticker.C {
			ctx, cancel := context.WithTimeout(context.Background(), accountPoolAutoCheckInterval)
			updated, checked, failed, err := h.autoCheckAccountPool(ctx)
			cancel()
			if err != nil {
				log.WithError(err).Warn("account pool auto check failed")
				continue
			}
			if checked > 0 {
				log.Infof("account pool auto check completed: checked=%d updated=%d failed=%d", checked, updated, failed)
			}
		}
	}()
}

func (h *Handler) autoCheckAccountPool(ctx context.Context) (int, int, int, error) {
	if h == nil || h.cfg == nil || strings.TrimSpace(h.cfg.AuthDir) == "" {
		return 0, 0, 0, nil
	}
	if !accountPoolAutoCheckRunning.CompareAndSwap(false, true) {
		return 0, 0, 0, nil
	}
	defer accountPoolAutoCheckRunning.Store(false)

	entries, err := h.accountPoolAutoCheckEntries()
	if err != nil || len(entries) == 0 {
		return 0, 0, 0, err
	}

	jobs := make(chan accountPoolAutoCheckEntry)
	results := make(chan accountPoolCheckResultUpdate, len(entries))
	workers := accountPoolAutoCheckConcurrency
	if len(entries) < workers {
		workers = len(entries)
	}
	for i := 0; i < workers; i++ {
		go func() {
			for entry := range jobs {
				result := h.checkAccountPoolEntryQuota(ctx, entry)
				select {
				case results <- accountPoolCheckResultUpdate{Name: entry.Name, ContentHash: entry.ContentHash, Result: result}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	sent := 0
	for _, entry := range entries {
		select {
		case <-ctx.Done():
			close(jobs)
			return 0, sent, 0, ctx.Err()
		case jobs <- entry:
			sent++
		}
	}
	close(jobs)

	updates := make([]accountPoolCheckResultUpdate, 0, sent)
	failed := 0
	for i := 0; i < sent; i++ {
		select {
		case <-ctx.Done():
			return 0, sent, failed, ctx.Err()
		case update := <-results:
			if update.Result.Status == "error" {
				failed++
			}
			updates = append(updates, update)
		}
	}

	updated, _, _, err := h.persistAccountPoolCheckResults(updates, time.Now().UTC())
	if err != nil {
		return updated, sent, failed, err
	}
	appended, appendSkipped, appendErr := h.appendHighQuotaAccountPoolEntries(ctx)
	if appendErr != nil {
		return updated, sent, failed, appendErr
	}
	if appended > 0 {
		log.Infof("account pool auto check appended high-quota entries: added=%d skipped=%d", appended, appendSkipped)
	}
	return updated, sent, failed, nil
}

func (h *Handler) accountPoolAutoCheckEntries() ([]accountPoolAutoCheckEntry, error) {
	if accountPoolPGEnabled() {
		return h.accountPoolAutoCheckEntriesPostgres(context.Background())
	}
	accountPoolDBMu.Lock()
	defer accountPoolDBMu.Unlock()
	db, err := h.openAccountPoolSQLiteLocked()
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() {
		if errClose := db.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close account pool auto check database")
		}
	}()
	existingHashes := h.existingAuthContentHashes()
	existingIdentities := h.existingAuthAccountIdentities()
	rows, err := db.Query(`SELECT name, data, content_hash, COALESCE(type, ''), COALESCE(provider, '') FROM account_pool_entries ORDER BY name`)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to query account pool auto check entries: %w", err)
	}
	defer func() {
		if errClose := rows.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close account pool auto check rows")
		}
	}()

	entries := make([]accountPoolAutoCheckEntry, 0)
	for rows.Next() {
		var name, data, hash, typeValue, provider string
		if errScan := rows.Scan(&name, &data, &hash, &typeValue, &provider); errScan != nil {
			return nil, fmt.Errorf("failed to scan account pool auto check entry: %w", errScan)
		}
		name = normalizeAccountPoolEntryName(name)
		if name == "" || isUnsafeAccountPoolEntryName(name) || !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		entryData := bytes.TrimSpace([]byte(data))
		if len(entryData) == 0 || accountPoolEntryDisabled(entryData) {
			continue
		}
		rawContentHash := strings.TrimSpace(hash)
		if rawContentHash == "" {
			rawContentHash = hashAccountPoolContent(entryData)
		}
		if rawContentHash != "" {
			if _, exists := existingHashes[rawContentHash]; exists {
				continue
			}
		}
		if identity := accountPoolAuthIdentityFromData(entryData); identity != "" {
			if _, exists := existingIdentities[identity]; exists {
				continue
			}
		}
		provider = strings.ToLower(strings.TrimSpace(firstNonEmptyStringValue(provider, typeValue, gjson.GetBytes(entryData, "provider").String(), gjson.GetBytes(entryData, "type").String())))
		switch provider {
		case "antigravity", "claude", "codex", "gemini-cli", "kimi":
			entries = append(entries, accountPoolAutoCheckEntry{Name: name, Data: append([]byte(nil), entryData...), ContentHash: strings.TrimSpace(hash), Provider: provider})
		}
	}
	if errRows := rows.Err(); errRows != nil {
		return nil, fmt.Errorf("failed to iterate account pool auto check entries: %w", errRows)
	}
	return entries, nil
}

func (h *Handler) checkAccountPoolEntryQuota(ctx context.Context, entry accountPoolAutoCheckEntry) accountPoolCheckResultPayload {
	now := time.Now().UnixMilli()
	auth, err := h.buildAuthFromFileData(filepath.Join(h.cfg.AuthDir, ".account-pool", filepath.FromSlash(entry.Name)), entry.Data)
	if err != nil {
		return accountPoolCheckResultPayload{Status: "error", Message: err.Error(), CheckedAt: now}
	}
	auth.ID = entry.Name
	auth.FileName = entry.Name
	provider := strings.ToLower(strings.TrimSpace(firstNonEmptyStringValue(entry.Provider, auth.Provider)))

	quota, err := h.fetchAccountPoolQuota(ctx, auth, provider, entry.Data)
	if err != nil {
		return accountPoolCheckResultPayload{Status: "error", Message: err.Error(), StatusCode: accountPoolQuotaErrorStatus(err), CheckedAt: now}
	}

	lines, remaining := accountPoolQuotaSummary(provider, quota.Body)
	realRequestOK, realRequestErr := h.checkAccountPoolRealRequest(ctx, auth, provider, entry.Data)
	status := "success"
	message := "检测成功"
	statusCode := quota.StatusCode
	if !realRequestOK {
		status = "error"
		if realRequestErr != "" {
			message = "模型检测请求失败: " + realRequestErr
		} else {
			message = "模型检测请求失败"
		}
	}
	return accountPoolCheckResultPayload{
		Status:                status,
		Message:               message,
		Plan:                  accountPoolDetectedPlan(provider, auth, quota.Body),
		QuotaLines:            lines,
		QuotaRemainingPercent: remaining,
		RealRequestOK:         realRequestOK,
		RealRequestError:      realRequestErr,
		StatusCode:            statusCode,
		CheckedAt:             now,
	}
}

func (h *Handler) checkAccountPoolRealRequest(ctx context.Context, auth *coreauth.Auth, provider string, data []byte) (bool, string) {
	provider = strings.ToLower(strings.TrimSpace(provider))
	switch provider {
	case "codex":
		return h.checkAccountPoolCodexRealRequest(ctx, auth, data)
	default:
		// Other providers currently have quota endpoints that require authenticated
		// provider access; do not block their promotion on a Codex-specific probe.
		return true, ""
	}
}

func (h *Handler) checkAccountPoolCodexRealRequest(ctx context.Context, auth *coreauth.Auth, data []byte) (bool, string) {
	token, err := h.resolveTokenForAuth(ctx, auth)
	if err != nil {
		return false, "auth token refresh failed"
	}
	if strings.TrimSpace(token) == "" {
		return false, "auth token not found"
	}
	body := `{"model":"gpt-5.4-mini","input":[{"role":"user","content":[{"type":"input_text","text":"ping"}]}],"instructions":"Reply with pong only.","stream":true,"store":false}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", strings.NewReader(body))
	if err != nil {
		return false, err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal")
	req.Header.Set("Originator", "codex_cli_rs")
	if accountID := extractAccountPoolCodexAccountID(data); accountID != "" {
		req.Header.Set("Chatgpt-Account-Id", accountID)
	}
	client := &http.Client{Timeout: defaultAPICallTimeout, Transport: h.apiCallTransport(auth)}
	resp, err := client.Do(req)
	if err != nil {
		return false, err.Error()
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close account pool codex real request response")
		}
	}()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		msg := accountPoolAPIErrorMessage(string(respBody))
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return false, fmt.Sprintf("%d: %s", resp.StatusCode, msg)
	}
	return true, ""
}

func (h *Handler) fetchAccountPoolQuota(ctx context.Context, auth *coreauth.Auth, provider string, data []byte) (accountPoolQuotaCheckResponse, error) {
	switch provider {
	case "antigravity":
		projectID := firstNonEmptyStringValue(gjson.GetBytes(data, "project_id").String(), gjson.GetBytes(data, "installed.project_id").String(), gjson.GetBytes(data, "web.project_id").String(), "bamboo-precept-lgxtn")
		body := fmt.Sprintf(`{"project":%q}`, projectID)
		urls := []string{
			"https://daily-cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels",
			"https://daily-cloudcode-pa.sandbox.googleapis.com/v1internal:fetchAvailableModels",
			"https://cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels",
		}
		var lastErr error
		var lastStatus int
		for _, url := range urls {
			resp, err := h.callAccountPoolQuotaAPI(ctx, auth, "POST", url, map[string]string{
				"Authorization": "Bearer $TOKEN$",
				"Content-Type":  "application/json",
				"User-Agent":    "antigravity/1.11.5 windows/amd64",
			}, body)
			if err != nil {
				lastErr = err
				continue
			}
			if resp.StatusCode >= 200 && resp.StatusCode < 300 && gjson.Get(resp.Body, "models").Exists() {
				return resp, nil
			}
			lastStatus = resp.StatusCode
			lastErr = accountPoolQuotaHTTPError{status: resp.StatusCode, message: accountPoolAPIErrorMessage(resp.Body)}
		}
		if lastErr != nil {
			if lastStatus > 0 {
				return accountPoolQuotaCheckResponse{StatusCode: lastStatus}, lastErr
			}
			return accountPoolQuotaCheckResponse{}, lastErr
		}
		return accountPoolQuotaCheckResponse{}, fmt.Errorf("empty antigravity quota response")
	case "claude":
		return h.callAccountPoolQuotaAPI(ctx, auth, "GET", "https://api.anthropic.com/api/oauth/usage", map[string]string{
			"Authorization":  "Bearer $TOKEN$",
			"Content-Type":   "application/json",
			"anthropic-beta": "oauth-2025-04-20",
		}, "")
	case "codex":
		headers := map[string]string{
			"Authorization": "Bearer $TOKEN$",
			"Content-Type":  "application/json",
			"User-Agent":    "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal",
		}
		if accountID := extractAccountPoolCodexAccountID(data); accountID != "" {
			headers["Chatgpt-Account-Id"] = accountID
		}
		return h.callAccountPoolQuotaAPI(ctx, auth, "GET", "https://chatgpt.com/backend-api/wham/usage", headers, "")
	case "gemini-cli":
		projectID := accountPoolGeminiCLIProjectID(data)
		if projectID == "" {
			return accountPoolQuotaCheckResponse{}, fmt.Errorf("missing project id")
		}
		return h.callAccountPoolQuotaAPI(ctx, auth, "POST", "https://cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota", map[string]string{
			"Authorization": "Bearer $TOKEN$",
			"Content-Type":  "application/json",
		}, fmt.Sprintf(`{"project":%q}`, projectID))
	case "kimi":
		return h.callAccountPoolQuotaAPI(ctx, auth, "GET", "https://api.kimi.com/coding/v1/usages", map[string]string{
			"Authorization": "Bearer $TOKEN$",
		}, "")
	default:
		return accountPoolQuotaCheckResponse{}, fmt.Errorf("unsupported provider %q", provider)
	}
}

func (h *Handler) callAccountPoolQuotaAPI(ctx context.Context, auth *coreauth.Auth, method, url string, headers map[string]string, body string) (accountPoolQuotaCheckResponse, error) {
	token, err := h.resolveTokenForAuth(ctx, auth)
	if err != nil {
		return accountPoolQuotaCheckResponse{}, fmt.Errorf("auth token refresh failed")
	}
	if strings.TrimSpace(token) == "" {
		return accountPoolQuotaCheckResponse{}, fmt.Errorf("auth token not found")
	}
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return accountPoolQuotaCheckResponse{}, err
	}
	for key, value := range headers {
		req.Header.Set(key, strings.ReplaceAll(value, "$TOKEN$", token))
	}
	client := &http.Client{Timeout: defaultAPICallTimeout, Transport: h.apiCallTransport(auth)}
	resp, err := client.Do(req)
	if err != nil {
		return accountPoolQuotaCheckResponse{}, err
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close account pool quota response")
		}
	}()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return accountPoolQuotaCheckResponse{}, err
	}
	out := accountPoolQuotaCheckResponse{StatusCode: resp.StatusCode, Body: string(respBody)}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return out, accountPoolQuotaHTTPError{status: resp.StatusCode, message: accountPoolAPIErrorMessage(out.Body)}
	}
	return out, nil
}

type accountPoolQuotaHTTPError struct {
	status  int
	message string
}

func (e accountPoolQuotaHTTPError) Error() string {
	if e.message != "" {
		return fmt.Sprintf("%d: %s", e.status, e.message)
	}
	return fmt.Sprintf("%d: quota request failed", e.status)
}

func accountPoolQuotaErrorStatus(err error) int {
	var httpErr accountPoolQuotaHTTPError
	if err != nil && strings.Contains(err.Error(), "context deadline exceeded") {
		return http.StatusRequestTimeout
	}
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "timeout") {
		return http.StatusRequestTimeout
	}
	if err != nil && strings.Contains(err.Error(), "auth token") {
		return http.StatusBadRequest
	}
	if err != nil && strings.Contains(err.Error(), "missing project id") {
		return http.StatusBadRequest
	}
	if err != nil && strings.Contains(err.Error(), "unsupported provider") {
		return http.StatusNotFound
	}
	if errors.As(err, &httpErr) {
		return httpErr.status
	}
	return 0
}

func accountPoolAPIErrorMessage(body string) string {
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return "quota request failed"
	}
	for _, path := range []string{"error.message", "message", "error", "detail"} {
		if value := strings.TrimSpace(gjson.Get(trimmed, path).String()); value != "" {
			return value
		}
	}
	if len(trimmed) > 300 {
		return trimmed[:300]
	}
	return trimmed
}

func accountPoolQuotaSummary(provider, body string) ([]string, *float64) {
	details := make([]accountPoolQuotaDetail, 0)
	switch provider {
	case "claude":
		for _, key := range []string{"five_hour", "seven_day", "seven_day_oauth_apps", "seven_day_opus", "seven_day_sonnet", "seven_day_cowork", "iguana_necktie"} {
			item := gjson.Get(body, key)
			if !item.Exists() {
				continue
			}
			used := gjsonNumber(item, "used_percent", "usedPercent")
			remaining := percentPtr(100 - used)
			details = append(details, accountPoolQuotaDetail{Label: key, Remaining: formatPercentValue(*remaining), Reset: firstNonEmptyStringValue(gjson.Get(item.Raw, "reset_at").String(), gjson.Get(item.Raw, "resetAt").String()), Percent: remaining})
		}
	case "codex":
		addCodexWindow := func(label string, item gjson.Result) {
			if !item.Exists() {
				return
			}
			used := gjsonNumber(item, "used_percent", "usedPercent")
			remaining := percentPtr(100 - used)
			details = append(details, accountPoolQuotaDetail{Label: label, Remaining: formatPercentValue(*remaining), Reset: firstNonEmptyStringValue(gjson.Get(item.Raw, "reset_at").String(), gjson.Get(item.Raw, "resetAt").String(), gjson.Get(item.Raw, "reset_seconds").String()), Percent: remaining})
		}
		for _, path := range []string{"rate_limit.primary_window", "rateLimit.primaryWindow"} {
			addCodexWindow("five-hour", gjson.Get(body, path))
		}
		for _, path := range []string{"rate_limit.secondary_window", "rateLimit.secondaryWindow"} {
			addCodexWindow("weekly", gjson.Get(body, path))
		}
	case "gemini-cli":
		for _, bucket := range gjson.Get(body, "buckets").Array() {
			model := firstNonEmptyStringValue(gjson.Get(bucket.Raw, "modelId").String(), gjson.Get(bucket.Raw, "model_id").String(), "Gemini")
			fraction, ok := gjsonOptionalNumber(bucket, "remainingFraction", "remaining_fraction")
			var percent *float64
			remaining := "--"
			if ok {
				percent = percentPtr(fraction * 100)
				remaining = formatPercentValue(*percent)
			} else if amount, okAmount := gjsonOptionalNumber(bucket, "remainingAmount", "remaining_amount"); okAmount {
				remaining = fmt.Sprintf("%.0f", amount)
			}
			details = append(details, accountPoolQuotaDetail{Label: model, Remaining: remaining, Reset: firstNonEmptyStringValue(gjson.Get(bucket.Raw, "resetTime").String(), gjson.Get(bucket.Raw, "reset_time").String()), Percent: percent})
		}
	case "antigravity":
		models := gjson.Get(body, "models")
		models.ForEach(func(key, value gjson.Result) bool {
			fraction, ok := gjsonOptionalNumber(value, "remainingFraction", "remaining_fraction")
			if !ok {
				return true
			}
			percent := percentPtr(fraction * 100)
			details = append(details, accountPoolQuotaDetail{Label: key.String(), Remaining: formatPercentValue(*percent), Reset: firstNonEmptyStringValue(gjson.Get(value.Raw, "resetTime").String(), gjson.Get(value.Raw, "reset_time").String()), Percent: percent})
			return true
		})
	case "kimi":
		for _, row := range gjson.Get(body, "usages").Array() {
			label := firstNonEmptyStringValue(gjson.Get(row.Raw, "label").String(), gjson.Get(row.Raw, "name").String(), gjson.Get(row.Raw, "model").String(), "Kimi")
			used, usedOK := gjsonOptionalNumber(row, "used")
			limit, limitOK := gjsonOptionalNumber(row, "limit", "total")
			remaining := "--"
			var percent *float64
			if usedOK && limitOK && limit > 0 {
				percent = percentPtr((limit - used) / limit * 100)
				remaining = formatPercentValue(*percent)
			}
			details = append(details, accountPoolQuotaDetail{Label: label, Remaining: remaining, Reset: firstNonEmptyStringValue(gjson.Get(row.Raw, "resetAt").String(), gjson.Get(row.Raw, "reset_at").String(), gjson.Get(row.Raw, "resetHint").String()), Percent: percent})
		}
	}
	if len(details) == 0 {
		return nil, nil
	}
	sort.SliceStable(details, func(i, j int) bool {
		if details[i].Percent == nil {
			return false
		}
		if details[j].Percent == nil {
			return true
		}
		return *details[i].Percent < *details[j].Percent
	})
	if len(details) > 3 {
		details = details[:3]
	}
	lines := make([]string, 0, len(details))
	var minRemaining *float64
	for _, detail := range details {
		if detail.Percent != nil && (minRemaining == nil || *detail.Percent < *minRemaining) {
			value := *detail.Percent
			minRemaining = &value
		}
		raw, err := json.Marshal(detail)
		if err == nil {
			lines = append(lines, string(raw))
		}
	}
	return lines, minRemaining
}

func gjsonOptionalNumber(item gjson.Result, keys ...string) (float64, bool) {
	for _, key := range keys {
		value := gjson.Get(item.Raw, key)
		if value.Exists() {
			return value.Float(), true
		}
	}
	return 0, false
}

func gjsonNumber(item gjson.Result, keys ...string) float64 {
	value, _ := gjsonOptionalNumber(item, keys...)
	return value
}

func percentPtr(value float64) *float64 {
	if value < 0 {
		value = 0
	}
	if value > 100 {
		value = 100
	}
	return &value
}

func formatPercentValue(value float64) string {
	if value == float64(int64(value)) {
		return fmt.Sprintf("%.0f%%", value)
	}
	return fmt.Sprintf("%.1f%%", value)
}

func accountPoolDetectedPlan(provider string, auth *coreauth.Auth, body string) string {
	switch provider {
	case "codex":
		return firstNonEmptyStringValue(gjson.Get(body, "plan_type").String(), gjson.Get(body, "planType").String(), authStringMetadata(auth, "plan_type"), authStringMetadata(auth, "planType"))
	case "claude":
		return firstNonEmptyStringValue(gjson.Get(body, "plan_type").String(), gjson.Get(body, "planType").String(), authStringMetadata(auth, "plan_type"), authStringMetadata(auth, "planType"))
	}
	return ""
}

func accountPoolEntryDisabled(data []byte) bool {
	value := gjson.GetBytes(data, "disabled")
	if !value.Exists() {
		return false
	}
	if value.Type == gjson.True {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(value.String()), "true") || value.Int() != 0
}

func accountPoolGeminiCLIProjectID(data []byte) string {
	for _, candidate := range []string{gjson.GetBytes(data, "project_id").String(), gjson.GetBytes(data, "projectId").String(), gjson.GetBytes(data, "metadata.project_id").String(), gjson.GetBytes(data, "account").String()} {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		if open := strings.LastIndex(candidate, "("); open >= 0 {
			if close := strings.LastIndex(candidate, ")"); close > open {
				return strings.TrimSpace(candidate[open+1 : close])
			}
		}
		return candidate
	}
	return ""
}

func extractAccountPoolCodexAccountID(data []byte) string {
	for _, path := range []string{"id_token", "metadata.id_token", "attributes.id_token"} {
		if id := accountPoolCodexAccountIDFromToken(gjson.GetBytes(data, path).Value()); id != "" {
			return id
		}
	}
	return ""
}

func accountPoolCodexAccountIDFromToken(value any) string {
	switch typed := value.(type) {
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return ""
		}
		if gjson.Valid(trimmed) {
			if id := strings.TrimSpace(gjson.Get(trimmed, "chatgpt_account_id").String()); id != "" {
				return id
			}
		}
		parts := strings.Split(trimmed, ".")
		if len(parts) < 2 {
			return ""
		}
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			return ""
		}
		return strings.TrimSpace(gjson.GetBytes(payload, "chatgpt_account_id").String())
	case map[string]any:
		if id, ok := typed["chatgpt_account_id"].(string); ok {
			return strings.TrimSpace(id)
		}
	}
	return ""
}

func authStringMetadata(auth *coreauth.Auth, key string) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	if value, ok := auth.Metadata[key].(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

func firstNonEmptyStringValue(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
