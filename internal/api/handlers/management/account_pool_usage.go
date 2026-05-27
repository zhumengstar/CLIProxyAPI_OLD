package management

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	apimiddleware "github.com/router-for-me/CLIProxyAPI/v7/internal/api/middleware"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const (
	maxAccountPoolUsageParamsLength = 16 * 1024
)

var accountPoolUsageUserIDHeaders = []string{
	"X-NewAPI-User-ID",
	"X-NewAPI-UserId",
	"X-NewAPI-User-Id",
	"NewAPI-User-ID",
	"NewAPI-UserId",
	"NewAPI-User-Id",
	"X-New-Api-User-Id",
	"X-New-Api-User-ID",
	"New-Api-User-Id",
	"New-Api-User-ID",
	"X-OneAPI-User-ID",
	"X-OneAPI-UserId",
	"X-OneAPI-User-Id",
	"OneAPI-User-ID",
	"OneAPI-UserId",
	"OneAPI-User-Id",
	"X-One-API-User-Id",
	"One-API-User-Id",
	"X-User-ID",
	"X-User-Id",
	"X-UserId",
	"X-Consumer-ID",
	"X-Consumer-Id",
	"X-Authenticated-User-ID",
	"X-Authenticated-User-Id",
}

var accountPoolUsageUsernameHeaders = []string{
	"X-NewAPI-Username",
	"X-NewAPI-UserName",
	"X-NewAPI-User-Name",
	"X-NewAPI-User",
	"X-NewAPI-Name",
	"NewAPI-Username",
	"NewAPI-UserName",
	"NewAPI-User-Name",
	"NewAPI-User",
	"NewAPI-Name",
	"X-New-Api-Username",
	"X-New-Api-UserName",
	"X-New-Api-User-Name",
	"X-New-Api-User",
	"X-New-Api-Name",
	"New-Api-Username",
	"New-Api-UserName",
	"New-Api-User-Name",
	"New-Api-User",
	"New-Api-Name",
	"X-OneAPI-Username",
	"X-OneAPI-UserName",
	"X-OneAPI-User-Name",
	"X-OneAPI-User",
	"OneAPI-Username",
	"OneAPI-UserName",
	"OneAPI-User-Name",
	"OneAPI-User",
	"X-One-API-Username",
	"X-One-API-UserName",
	"X-One-API-User-Name",
	"X-One-API-User",
	"One-API-Username",
	"One-API-UserName",
	"One-API-User-Name",
	"One-API-User",
	"X-Username",
	"X-User-Name",
	"X-UserName",
	"X-User",
	"X-Forwarded-User",
	"X-Authenticated-User",
	"X-Consumer-Username",
	"X-Consumer-User",
	"X-Login-Name",
	"X-Account-Name",
}

type accountPoolUsageRecord struct {
	ID                  string `json:"id"`
	RequestedAt         string `json:"requested_at"`
	RequestID           string `json:"request_id,omitempty"`
	RequestPath         string `json:"request_path,omitempty"`
	SessionID           string `json:"session_id,omitempty"`
	NewAPIUserID        string `json:"newapi_user_id,omitempty"`
	Username            string `json:"username,omitempty"`
	Provider            string `json:"provider,omitempty"`
	Model               string `json:"model,omitempty"`
	Alias               string `json:"alias,omitempty"`
	ServiceEmail        string `json:"service_email,omitempty"`
	AuthID              string `json:"auth_id,omitempty"`
	AuthIndex           string `json:"auth_index,omitempty"`
	AuthType            string `json:"auth_type,omitempty"`
	Success             bool   `json:"success"`
	StatusCode          int    `json:"status_code,omitempty"`
	LatencyMS           int64  `json:"latency_ms,omitempty"`
	InputTokens         int64  `json:"input_tokens,omitempty"`
	OutputTokens        int64  `json:"output_tokens,omitempty"`
	CachedTokens        int64  `json:"cached_tokens,omitempty"`
	CacheReadTokens     int64  `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int64  `json:"cache_creation_tokens,omitempty"`
	TotalTokens         int64  `json:"total_tokens,omitempty"`
	RequestParams       string `json:"request_params,omitempty"`
}

type accountPoolUsageSummary struct {
	Key                 string  `json:"key"`
	ServiceEmail        string  `json:"service_email,omitempty"`
	AuthID              string  `json:"auth_id,omitempty"`
	AuthIndex           string  `json:"auth_index,omitempty"`
	AuthType            string  `json:"auth_type,omitempty"`
	Provider            string  `json:"provider,omitempty"`
	Model               string  `json:"model,omitempty"`
	Alias               string  `json:"alias,omitempty"`
	Requests            int64   `json:"requests"`
	Successes           int64   `json:"successes"`
	Failures            int64   `json:"failures"`
	InputTokens         int64   `json:"input_tokens,omitempty"`
	OutputTokens        int64   `json:"output_tokens,omitempty"`
	CachedTokens        int64   `json:"cached_tokens,omitempty"`
	CacheReadTokens     int64   `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int64   `json:"cache_creation_tokens,omitempty"`
	TotalTokens         int64   `json:"total_tokens,omitempty"`
	TotalUSD            float64 `json:"total_usd,omitempty"`
	LastUsedAt          string  `json:"last_used_at,omitempty"`
}

type accountPoolUsageTotals struct {
	Requests            int64 `json:"requests"`
	Successes           int64 `json:"successes"`
	Failures            int64 `json:"failures"`
	InputTokens         int64 `json:"input_tokens,omitempty"`
	OutputTokens        int64 `json:"output_tokens,omitempty"`
	CachedTokens        int64 `json:"cached_tokens,omitempty"`
	CacheReadTokens     int64 `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int64 `json:"cache_creation_tokens,omitempty"`
	TotalTokens         int64 `json:"total_tokens,omitempty"`
}

type accountPoolUsageRecorder struct {
	mu          sync.RWMutex
	records     []accountPoolUsageRecord
	summaries   map[string]*accountPoolUsageSummary
	nextID      uint64
	dbPath      string
	schemaReady bool
}

var accountPoolUsage = &accountPoolUsageRecorder{}

func init() {
	usage.RegisterPlugin(accountPoolUsage)
}

func (r *accountPoolUsageRecorder) Configure(dbPath string) {
	if r == nil {
		return
	}
	dbPath = strings.TrimSpace(dbPath)
	if dbPath == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dbPath == dbPath {
		return
	}
	r.dbPath = dbPath
	r.schemaReady = false
	if err := r.loadLocked(); err != nil {
		log.WithError(err).Warn("failed to load account pool usage database")
	}
}

func (r *accountPoolUsageRecorder) HandleUsage(ctx context.Context, record usage.Record) {
	if r == nil {
		return
	}
	sessionID, userID, username, requestPath, requestParams := accountPoolRequestIdentity(ctx)
	if username == "" || userID == "" {
		cachedUserID, cachedUsername := r.identityForSession(sessionID)
		if userID == "" {
			userID = cachedUserID
		}
		if username == "" {
			username = cachedUsername
		}
	}
	requestedAt := record.RequestedAt
	if requestedAt.IsZero() {
		requestedAt = time.Now()
	}
	statusCode := 200
	if record.Failed {
		statusCode = record.Fail.StatusCode
	}

	r.mu.Lock()
	r.nextID++
	item := accountPoolUsageRecord{
		ID:                  strconv.FormatUint(r.nextID, 10),
		RequestedAt:         requestedAt.Format(time.RFC3339),
		RequestID:           logging.GetRequestID(ctx),
		RequestPath:         requestPath,
		SessionID:           sessionID,
		NewAPIUserID:        userID,
		Username:            username,
		Provider:            record.Provider,
		Model:               record.Model,
		Alias:               record.Alias,
		ServiceEmail:        record.Source,
		AuthID:              record.AuthID,
		AuthIndex:           record.AuthIndex,
		AuthType:            record.AuthType,
		Success:             !record.Failed,
		StatusCode:          statusCode,
		LatencyMS:           record.Latency.Milliseconds(),
		InputTokens:         record.Detail.InputTokens,
		OutputTokens:        record.Detail.OutputTokens,
		CachedTokens:        record.Detail.CachedTokens,
		CacheReadTokens:     record.Detail.CacheReadTokens,
		CacheCreationTokens: record.Detail.CacheCreationTokens,
		TotalTokens:         record.Detail.TotalTokens,
		RequestParams:       requestParams,
	}
	key := accountPoolUsageSummaryKey(item)
	r.records = append(r.records, item)
	if r.summaries == nil {
		r.summaries = make(map[string]*accountPoolUsageSummary)
	}
	summary := r.summaries[key]
	if summary == nil {
		summary = &accountPoolUsageSummary{
			Key:          key,
			ServiceEmail: item.ServiceEmail,
			AuthID:       item.AuthID,
			AuthIndex:    item.AuthIndex,
			AuthType:     item.AuthType,
			Provider:     item.Provider,
			Model:        item.Model,
			Alias:        item.Alias,
		}
		r.summaries[key] = summary
	}
	summary.Requests++
	if item.Success {
		summary.Successes++
	} else {
		summary.Failures++
	}
	summary.InputTokens += item.InputTokens
	summary.OutputTokens += item.OutputTokens
	summary.CachedTokens += item.CachedTokens
	summary.CacheReadTokens += item.CacheReadTokens
	summary.CacheCreationTokens += item.CacheCreationTokens
	summary.TotalTokens += item.TotalTokens
	summary.TotalUSD += accountPoolUsageUSD(item.Model, item.InputTokens, item.OutputTokens, item.CachedTokens, item.CacheReadTokens, item.CacheCreationTokens)
	if summary.ServiceEmail == "" {
		summary.ServiceEmail = item.ServiceEmail
	}
	if summary.AuthID == "" {
		summary.AuthID = item.AuthID
	}
	if summary.AuthIndex == "" {
		summary.AuthIndex = item.AuthIndex
	}
	if summary.AuthType == "" {
		summary.AuthType = item.AuthType
	}
	if summary.Provider == "" {
		summary.Provider = item.Provider
	}
	if summary.Model == "" {
		summary.Model = item.Model
	}
	if summary.Alias == "" {
		summary.Alias = item.Alias
	}
	summary.LastUsedAt = item.RequestedAt
	if err := r.persistUsageLocked(item, *summary); err != nil {
		log.WithError(err).Warn("failed to persist account pool usage record")
	}
	r.mu.Unlock()
}

func (r *accountPoolUsageRecorder) identityForSession(sessionID string) (userID string, username string) {
	if r == nil {
		return "", ""
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return "", ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for i := len(r.records) - 1; i >= 0; i-- {
		record := r.records[i]
		if strings.TrimSpace(record.SessionID) != sessionID {
			continue
		}
		if userID == "" {
			userID = strings.TrimSpace(record.NewAPIUserID)
		}
		if username == "" {
			username = strings.TrimSpace(record.Username)
		}
		if userID != "" && username != "" {
			return userID, username
		}
	}
	return userID, username
}

func (r *accountPoolUsageRecorder) refreshFromStoreIfNewer() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if strings.TrimSpace(r.dbPath) == "" {
		return
	}
	db, err := r.openDBLocked()
	if err != nil || db == nil {
		if err != nil {
			log.WithError(err).Debug("failed to open account pool usage database for refresh")
		}
		return
	}
	var persistedCount int
	var persistedMaxID uint64
	if err = db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(id), 0) FROM account_pool_usage_records`).Scan(&persistedCount, &persistedMaxID); err != nil {
		_ = db.Close()
		log.WithError(err).Debug("failed to inspect persisted account pool usage record count")
		return
	}
	_ = db.Close()
	if persistedCount <= len(r.records) && persistedMaxID <= r.nextID {
		return
	}
	if err = r.loadLocked(); err != nil {
		log.WithError(err).Warn("failed to refresh account pool usage database")
	}
}

func (r *accountPoolUsageRecorder) List(limit int) []accountPoolUsageRecord {
	records, _ := r.ListPage(limit, 0)
	return records
}

func (r *accountPoolUsageRecorder) ListPage(limit int, offset int) ([]accountPoolUsageRecord, int) {
	if r == nil {
		return nil, 0
	}
	r.refreshFromStoreIfNewer()
	if offset < 0 {
		offset = 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	total := len(r.records)
	if offset >= total {
		return []accountPoolUsageRecord{}, total
	}
	if limit <= 0 || limit > total-offset {
		limit = total - offset
	}
	out := make([]accountPoolUsageRecord, 0, limit)
	skipped := 0
	for i := len(r.records) - 1; i >= 0 && len(out) < limit; i-- {
		if skipped < offset {
			skipped++
			continue
		}
		out = append(out, r.records[i])
	}
	return out, total
}

func (r *accountPoolUsageRecorder) Clear() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.records = nil
	r.summaries = nil
	if err := r.clearRecordsLocked(); err != nil {
		log.WithError(err).Warn("failed to clear persisted account pool usage records")
	}
	r.mu.Unlock()
}

func (r *accountPoolUsageRecorder) RemoveAccountPoolEntries(names []string, entries map[string][]byte) {
	if r == nil || len(names) == 0 {
		return
	}
	matchers := accountPoolUsageEntryMatchers(names, entries)
	if len(matchers) == 0 {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for key := range matchers {
		delete(r.summaries, key)
	}
	if len(r.records) == 0 {
		return
	}
	filtered := r.records[:0]
	for _, record := range r.records {
		if accountPoolUsageRecordMatches(record, matchers) {
			continue
		}
		filtered = append(filtered, record)
	}
	r.records = filtered
	if err := r.removeAccountPoolEntriesLocked(matchers); err != nil {
		log.WithError(err).Warn("failed to remove persisted account pool usage entries")
	}
}

func accountPoolUsageEntryMatchers(names []string, entries map[string][]byte) map[string]struct{} {
	matchers := make(map[string]struct{}, len(names)*3)
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		matchers["auth_id:"+name] = struct{}{}
		matchers["auth_index:"+name] = struct{}{}
		if entry := entries[name]; len(entry) > 0 {
			if email := accountPoolUsageEmailFromEntry(entry); email != "" {
				matchers["email:"+email] = struct{}{}
			}
		}
	}
	return matchers
}

func accountPoolUsageEmailFromEntry(data []byte) string {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return ""
	}
	for _, key := range []string{"email", "service_email", "account_email"} {
		if value := strings.ToLower(strings.TrimSpace(fmt.Sprint(raw[key]))); value != "" && value != "<nil>" {
			return value
		}
	}
	for _, key := range []string{"account", "user", "profile", "metadata"} {
		nested, ok := raw[key].(map[string]any)
		if !ok {
			continue
		}
		if value := strings.ToLower(strings.TrimSpace(fmt.Sprint(nested["email"]))); value != "" && value != "<nil>" {
			return value
		}
	}
	return ""
}

func accountPoolUsageRecordMatches(record accountPoolUsageRecord, matchers map[string]struct{}) bool {
	for _, key := range []string{
		accountPoolUsageSummaryKey(record),
		"auth_id:" + strings.TrimSpace(record.AuthID),
		"auth_index:" + strings.TrimSpace(record.AuthIndex),
		"email:" + strings.ToLower(strings.TrimSpace(record.ServiceEmail)),
	} {
		if _, ok := matchers[key]; ok {
			return true
		}
	}
	return false
}

func (r *accountPoolUsageRecorder) Summaries() []accountPoolUsageSummary {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	summaries := r.recomputeSummariesLocked()
	if len(summaries) == 0 {
		return nil
	}
	out := make([]accountPoolUsageSummary, 0, len(summaries))
	for _, item := range summaries {
		if item == nil {
			continue
		}
		out = append(out, *item)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		if out[i].TotalTokens != out[j].TotalTokens {
			return out[i].TotalTokens > out[j].TotalTokens
		}
		return out[i].Key < out[j].Key
	})
	return out
}

func (r *accountPoolUsageRecorder) recomputeSummariesLocked() map[string]*accountPoolUsageSummary {
	if r == nil {
		return nil
	}
	if len(r.records) == 0 {
		return r.summaries
	}
	summaries := make(map[string]*accountPoolUsageSummary)
	seenRecords := make(map[string]struct{}, len(r.records))
	for _, record := range r.records {
		recordKey := strings.TrimSpace(record.ID)
		if recordKey == "" {
			recordKey = strings.Join([]string{
				record.RequestedAt,
				record.RequestID,
				record.SessionID,
				record.ServiceEmail,
				record.AuthID,
				record.AuthIndex,
				strconv.FormatInt(record.InputTokens, 10),
				strconv.FormatInt(record.OutputTokens, 10),
				strconv.FormatInt(record.TotalTokens, 10),
			}, "|")
		}
		if _, ok := seenRecords[recordKey]; ok {
			continue
		}
		seenRecords[recordKey] = struct{}{}
		key := accountPoolUsageSummaryKey(record)
		summary := summaries[key]
		if summary == nil {
			summary = &accountPoolUsageSummary{
				Key:          key,
				ServiceEmail: record.ServiceEmail,
				AuthID:       record.AuthID,
				AuthIndex:    record.AuthIndex,
				AuthType:     record.AuthType,
				Provider:     record.Provider,
				Model:        record.Model,
				Alias:        record.Alias,
			}
			summaries[key] = summary
		}
		mergeAccountPoolUsageSummary(summary, accountPoolUsageSummaryFromRecord(record))
	}
	return summaries
}

func (r *accountPoolUsageRecorder) SummaryForAccountPoolEntry(name string, email string) accountPoolUsageSummary {
	if r == nil {
		return accountPoolUsageSummary{}
	}
	candidates := accountPoolUsageEntrySummaryKeys(name, email)
	if len(candidates) == 0 {
		return accountPoolUsageSummary{}
	}
	matchers := make(map[string]struct{}, len(candidates))
	for _, key := range candidates {
		matchers[key] = struct{}{}
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	var total accountPoolUsageSummary
	seenRecords := make(map[string]struct{}, len(r.records))
	for _, record := range r.records {
		if !accountPoolUsageRecordMatches(record, matchers) {
			continue
		}
		recordKey := strings.TrimSpace(record.ID)
		if recordKey == "" {
			recordKey = strings.Join([]string{
				record.RequestedAt,
				record.RequestID,
				record.SessionID,
				record.ServiceEmail,
				record.AuthID,
				record.AuthIndex,
				strconv.FormatInt(record.InputTokens, 10),
				strconv.FormatInt(record.OutputTokens, 10),
				strconv.FormatInt(record.TotalTokens, 10),
			}, "|")
		}
		if _, ok := seenRecords[recordKey]; ok {
			continue
		}
		seenRecords[recordKey] = struct{}{}
		mergeAccountPoolUsageSummary(&total, accountPoolUsageSummaryFromRecord(record))
	}
	if total.Key == "" && len(candidates) > 0 {
		total.Key = candidates[0]
	}
	return total
}

func accountPoolUsageSummaryFromRecord(item accountPoolUsageRecord) accountPoolUsageSummary {
	summary := accountPoolUsageSummary{
		Key:                 accountPoolUsageSummaryKey(item),
		ServiceEmail:        item.ServiceEmail,
		AuthID:              item.AuthID,
		AuthIndex:           item.AuthIndex,
		AuthType:            item.AuthType,
		Provider:            item.Provider,
		Model:               item.Model,
		Alias:               item.Alias,
		Requests:            1,
		InputTokens:         item.InputTokens,
		OutputTokens:        item.OutputTokens,
		CachedTokens:        item.CachedTokens,
		CacheReadTokens:     item.CacheReadTokens,
		CacheCreationTokens: item.CacheCreationTokens,
		TotalTokens:         item.TotalTokens,
		TotalUSD:            accountPoolUsageUSD(item.Model, item.InputTokens, item.OutputTokens, item.CachedTokens, item.CacheReadTokens, item.CacheCreationTokens),
		LastUsedAt:          item.RequestedAt,
	}
	if item.Success {
		summary.Successes = 1
	} else {
		summary.Failures = 1
	}
	return summary
}

func (r *accountPoolUsageRecorder) Totals() accountPoolUsageTotals {
	if r == nil {
		return accountPoolUsageTotals{}
	}
	r.refreshFromStoreIfNewer()
	r.mu.RLock()
	defer r.mu.RUnlock()
	var totals accountPoolUsageTotals
	for _, item := range r.recomputeSummariesLocked() {
		if item == nil {
			continue
		}
		totals.Requests += item.Requests
		totals.Successes += item.Successes
		totals.Failures += item.Failures
		totals.InputTokens += item.InputTokens
		totals.OutputTokens += item.OutputTokens
		totals.CachedTokens += item.CachedTokens
		totals.CacheReadTokens += item.CacheReadTokens
		totals.CacheCreationTokens += item.CacheCreationTokens
		totals.TotalTokens += item.TotalTokens
	}
	return totals
}

func accountPoolUsageEntrySummaryKeys(name string, email string) []string {
	keys := make([]string, 0, 8)
	seen := make(map[string]struct{}, 8)
	add := func(key string) {
		key = strings.TrimSpace(key)
		if key == "" {
			return
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	for _, value := range accountPoolUsageIdentifierVariants(name) {
		add("auth_id:" + value)
		add("auth_index:" + value)
	}
	if value := strings.ToLower(strings.TrimSpace(email)); value != "" {
		add("email:" + value)
	}
	return keys
}

func accountPoolUsageIdentifierVariants(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	normalized := strings.ReplaceAll(value, "\\", "/")
	base := path.Base(normalized)
	if base == "." || base == "/" {
		base = ""
	}
	variants := []string{value, normalized, base}
	out := make([]string, 0, len(variants))
	seen := make(map[string]struct{}, len(variants))
	for _, variant := range variants {
		variant = strings.TrimSpace(variant)
		if variant == "" {
			continue
		}
		if _, ok := seen[variant]; ok {
			continue
		}
		seen[variant] = struct{}{}
		out = append(out, variant)
	}
	return out
}

func boolToInt64(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

func mergeAccountPoolUsageSummary(target *accountPoolUsageSummary, source accountPoolUsageSummary) {
	if target == nil {
		return
	}
	if target.Key == "" {
		target.Key = source.Key
	}
	if target.ServiceEmail == "" {
		target.ServiceEmail = source.ServiceEmail
	}
	if target.AuthID == "" {
		target.AuthID = source.AuthID
	}
	if target.AuthIndex == "" {
		target.AuthIndex = source.AuthIndex
	}
	if target.AuthType == "" {
		target.AuthType = source.AuthType
	}
	if target.Provider == "" {
		target.Provider = source.Provider
	}
	if target.Model == "" {
		target.Model = source.Model
	} else if source.Model != "" && target.Model != source.Model {
		target.Model = "mixed"
	}
	if target.Alias == "" {
		target.Alias = source.Alias
	} else if source.Alias != "" && target.Alias != source.Alias {
		target.Alias = "mixed"
	}
	target.Requests += source.Requests
	target.Successes += source.Successes
	target.Failures += source.Failures
	target.InputTokens += source.InputTokens
	target.OutputTokens += source.OutputTokens
	target.CachedTokens += source.CachedTokens
	target.CacheReadTokens += source.CacheReadTokens
	target.CacheCreationTokens += source.CacheCreationTokens
	target.TotalTokens += source.TotalTokens
	target.TotalUSD += source.TotalUSD
	if compareUsageTime(source.LastUsedAt, target.LastUsedAt) > 0 {
		target.LastUsedAt = source.LastUsedAt
	}
}

func accountPoolUsageUSD(model string, inputTokens, outputTokens, cachedTokens, cacheReadTokens, cacheCreationTokens int64) float64 {
	price := accountPoolGPTModelPrice(model)
	cacheTokens := cachedTokens
	if cacheTokens <= 0 {
		cacheTokens = cacheReadTokens
	}
	uncachedInputTokens := inputTokens - cacheTokens
	if uncachedInputTokens < 0 {
		uncachedInputTokens = 0
	}
	return (float64(uncachedInputTokens)*price.inputPerMillion +
		float64(cacheTokens)*price.cacheReadPerMillion +
		float64(cacheCreationTokens)*price.cacheWritePerMillion +
		float64(outputTokens)*price.outputPerMillion) / 1_000_000
}

type accountPoolModelPrice struct {
	inputPerMillion      float64
	outputPerMillion     float64
	cacheReadPerMillion  float64
	cacheWritePerMillion float64
}

func accountPoolGPTModelPrice(model string) accountPoolModelPrice {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "gpt-5.5", "mixed":
		return accountPoolModelPrice{inputPerMillion: 5, outputPerMillion: 30, cacheReadPerMillion: 0.5}
	case "gpt-5.2", "gpt-5.3-codex":
		return accountPoolModelPrice{inputPerMillion: 1.75, outputPerMillion: 14, cacheReadPerMillion: 0.175}
	case "gpt-5.4":
		return accountPoolModelPrice{inputPerMillion: 2.5, outputPerMillion: 15, cacheReadPerMillion: 0.25}
	case "gpt-5.4-mini":
		return accountPoolModelPrice{inputPerMillion: 0.75, outputPerMillion: 4.5, cacheReadPerMillion: 0.075}
	case "gpt-image-2":
		return accountPoolModelPrice{inputPerMillion: 5, outputPerMillion: 10, cacheReadPerMillion: 1.25}
	default:
		// Default to the current primary GPT model pricing used by NewAPI.
		return accountPoolModelPrice{inputPerMillion: 5, outputPerMillion: 30, cacheReadPerMillion: 0.5}
	}
}

func compareUsageTime(left string, right string) int {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	if left == "" && right == "" {
		return 0
	}
	if left == "" {
		return -1
	}
	if right == "" {
		return 1
	}
	leftTime, leftErr := time.Parse(time.RFC3339, left)
	rightTime, rightErr := time.Parse(time.RFC3339, right)
	if leftErr == nil && rightErr == nil {
		if leftTime.After(rightTime) {
			return 1
		}
		if leftTime.Before(rightTime) {
			return -1
		}
		return 0
	}
	return strings.Compare(left, right)
}

func accountPoolUsageSummaryKey(item accountPoolUsageRecord) string {
	if value := strings.ToLower(strings.TrimSpace(item.ServiceEmail)); value != "" {
		return "email:" + value
	}
	if value := strings.TrimSpace(item.AuthID); value != "" {
		return "auth_id:" + value
	}
	if value := strings.TrimSpace(item.AuthIndex); value != "" {
		return "auth_index:" + value
	}
	if value := strings.TrimSpace(item.SessionID); value != "" {
		return "session:" + value
	}
	return "unknown"
}

func (r *accountPoolUsageRecorder) openDBLocked() (*sql.DB, error) {
	if strings.TrimSpace(r.dbPath) == "" {
		return nil, nil
	}
	if err := os.MkdirAll(filepath.Dir(r.dbPath), 0o700); err != nil {
		return nil, fmt.Errorf("failed to create account pool usage database dir: %w", err)
	}
	db, err := sql.Open("sqlite", r.dbPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open account pool usage database: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL; PRAGMA busy_timeout=5000;`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to configure account pool usage database: %w", err)
	}
	if _, err = db.Exec(`
CREATE TABLE IF NOT EXISTS account_pool_usage_records (
	id INTEGER NOT NULL PRIMARY KEY,
	requested_at TEXT NOT NULL,
	request_id TEXT,
	request_path TEXT,
	session_id TEXT,
	newapi_user_id TEXT,
	username TEXT,
	provider TEXT,
	model TEXT,
	alias TEXT,
	service_email TEXT,
	auth_id TEXT,
	auth_index TEXT,
	auth_type TEXT,
	success INTEGER NOT NULL DEFAULT 0,
	status_code INTEGER NOT NULL DEFAULT 0,
	latency_ms INTEGER NOT NULL DEFAULT 0,
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	cached_tokens INTEGER NOT NULL DEFAULT 0,
	cache_read_tokens INTEGER NOT NULL DEFAULT 0,
	cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
	total_tokens INTEGER NOT NULL DEFAULT 0,
	request_params TEXT
);
CREATE INDEX IF NOT EXISTS idx_account_pool_usage_records_requested_at ON account_pool_usage_records(requested_at);
CREATE INDEX IF NOT EXISTS idx_account_pool_usage_records_auth_id ON account_pool_usage_records(auth_id);
CREATE INDEX IF NOT EXISTS idx_account_pool_usage_records_auth_index ON account_pool_usage_records(auth_index);
CREATE INDEX IF NOT EXISTS idx_account_pool_usage_records_service_email ON account_pool_usage_records(service_email);
CREATE TABLE IF NOT EXISTS account_pool_usage_summaries (
	key TEXT NOT NULL PRIMARY KEY,
	service_email TEXT,
	auth_id TEXT,
	auth_index TEXT,
	auth_type TEXT,
	provider TEXT,
	model TEXT,
	alias TEXT,
	requests INTEGER NOT NULL DEFAULT 0,
	successes INTEGER NOT NULL DEFAULT 0,
	failures INTEGER NOT NULL DEFAULT 0,
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	cached_tokens INTEGER NOT NULL DEFAULT 0,
	cache_read_tokens INTEGER NOT NULL DEFAULT 0,
	cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
	total_tokens INTEGER NOT NULL DEFAULT 0,
	last_used_at TEXT
);
`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to initialize account pool usage database: %w", err)
	}
	if !r.schemaReady {
		if err = ensureAccountPoolUsageColumns(db); err != nil {
			_ = db.Close()
			return nil, err
		}
		r.schemaReady = true
	}
	return db, nil
}

func ensureAccountPoolUsageColumns(db *sql.DB) error {
	columns := []struct {
		table string
		name  string
		def   string
	}{
		{"account_pool_usage_records", "cached_tokens", "INTEGER NOT NULL DEFAULT 0"},
		{"account_pool_usage_records", "cache_read_tokens", "INTEGER NOT NULL DEFAULT 0"},
		{"account_pool_usage_records", "cache_creation_tokens", "INTEGER NOT NULL DEFAULT 0"},
		{"account_pool_usage_summaries", "cached_tokens", "INTEGER NOT NULL DEFAULT 0"},
		{"account_pool_usage_summaries", "cache_read_tokens", "INTEGER NOT NULL DEFAULT 0"},
		{"account_pool_usage_summaries", "cache_creation_tokens", "INTEGER NOT NULL DEFAULT 0"},
	}
	for _, column := range columns {
		if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", column.table, column.name, column.def)); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
				continue
			}
			return fmt.Errorf("failed to add account pool usage column %s.%s: %w", column.table, column.name, err)
		}
	}
	return nil
}

func (r *accountPoolUsageRecorder) loadLocked() error {
	db, err := r.openDBLocked()
	if err != nil || db == nil {
		return err
	}
	defer func() {
		if errClose := db.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close account pool usage database")
		}
	}()

	var maxID uint64
	if err = db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM account_pool_usage_records`).Scan(&maxID); err != nil {
		return fmt.Errorf("failed to read account pool usage max id: %w", err)
	}
	r.nextID = maxID

	rows, err := db.Query(`
SELECT id, COALESCE(requested_at, ''), COALESCE(request_id, ''), COALESCE(request_path, ''), COALESCE(session_id, ''), COALESCE(newapi_user_id, ''), COALESCE(username, ''),
	COALESCE(provider, ''), COALESCE(model, ''), COALESCE(alias, ''), COALESCE(service_email, ''), COALESCE(auth_id, ''), COALESCE(auth_index, ''), COALESCE(auth_type, ''), COALESCE(success, 0), COALESCE(status_code, 0),
	COALESCE(latency_ms, 0), COALESCE(input_tokens, 0), COALESCE(output_tokens, 0), COALESCE(cached_tokens, 0), COALESCE(cache_read_tokens, 0), COALESCE(cache_creation_tokens, 0),
	COALESCE(total_tokens, 0), COALESCE(request_params, '')
FROM account_pool_usage_records
ORDER BY id ASC`)
	if err != nil {
		return fmt.Errorf("failed to query account pool usage records: %w", err)
	}
	records := make([]accountPoolUsageRecord, 0)
	for rows.Next() {
		var item accountPoolUsageRecord
		var id uint64
		var success int
		if errScan := rows.Scan(
			&id,
			&item.RequestedAt,
			&item.RequestID,
			&item.RequestPath,
			&item.SessionID,
			&item.NewAPIUserID,
			&item.Username,
			&item.Provider,
			&item.Model,
			&item.Alias,
			&item.ServiceEmail,
			&item.AuthID,
			&item.AuthIndex,
			&item.AuthType,
			&success,
			&item.StatusCode,
			&item.LatencyMS,
			&item.InputTokens,
			&item.OutputTokens,
			&item.CachedTokens,
			&item.CacheReadTokens,
			&item.CacheCreationTokens,
			&item.TotalTokens,
			&item.RequestParams,
		); errScan != nil {
			_ = rows.Close()
			return fmt.Errorf("failed to scan account pool usage record: %w", errScan)
		}
		item.ID = strconv.FormatUint(id, 10)
		item.Success = success != 0
		records = append(records, item)
	}
	if errRows := rows.Close(); errRows != nil {
		return fmt.Errorf("failed to close account pool usage record rows: %w", errRows)
	}
	r.records = records
	recomputedSummaries := make(map[string]*accountPoolUsageSummary)
	for _, item := range records {
		key := accountPoolUsageSummaryKey(item)
		summary := recomputedSummaries[key]
		if summary == nil {
			summary = &accountPoolUsageSummary{
				Key:          key,
				ServiceEmail: item.ServiceEmail,
				AuthID:       item.AuthID,
				AuthIndex:    item.AuthIndex,
				AuthType:     item.AuthType,
				Provider:     item.Provider,
				Model:        item.Model,
				Alias:        item.Alias,
			}
			recomputedSummaries[key] = summary
		}
		itemSummary := accountPoolUsageSummary{
			Key:                 key,
			ServiceEmail:        item.ServiceEmail,
			AuthID:              item.AuthID,
			AuthIndex:           item.AuthIndex,
			AuthType:            item.AuthType,
			Provider:            item.Provider,
			Model:               item.Model,
			Alias:               item.Alias,
			Requests:            1,
			Successes:           boolToInt64(item.Success),
			Failures:            boolToInt64(!item.Success),
			InputTokens:         item.InputTokens,
			OutputTokens:        item.OutputTokens,
			CachedTokens:        item.CachedTokens,
			CacheReadTokens:     item.CacheReadTokens,
			CacheCreationTokens: item.CacheCreationTokens,
			TotalTokens:         item.TotalTokens,
			TotalUSD:            accountPoolUsageUSD(item.Model, item.InputTokens, item.OutputTokens, item.CachedTokens, item.CacheReadTokens, item.CacheCreationTokens),
			LastUsedAt:          item.RequestedAt,
		}
		mergeAccountPoolUsageSummary(summary, itemSummary)
	}

	summaryRows, err := db.Query(`
SELECT key, COALESCE(service_email, ''), COALESCE(auth_id, ''), COALESCE(auth_index, ''), COALESCE(auth_type, ''), COALESCE(provider, ''), COALESCE(model, ''), COALESCE(alias, ''),
	COALESCE(requests, 0), COALESCE(successes, 0), COALESCE(failures, 0), COALESCE(input_tokens, 0), COALESCE(output_tokens, 0), COALESCE(cached_tokens, 0), COALESCE(cache_read_tokens, 0),
	COALESCE(cache_creation_tokens, 0), COALESCE(total_tokens, 0), COALESCE(last_used_at, '')
FROM account_pool_usage_summaries`)
	if err != nil {
		return fmt.Errorf("failed to query account pool usage summaries: %w", err)
	}
	summaries := make(map[string]*accountPoolUsageSummary)
	for summaryRows.Next() {
		item := accountPoolUsageSummary{}
		if errScan := summaryRows.Scan(
			&item.Key,
			&item.ServiceEmail,
			&item.AuthID,
			&item.AuthIndex,
			&item.AuthType,
			&item.Provider,
			&item.Model,
			&item.Alias,
			&item.Requests,
			&item.Successes,
			&item.Failures,
			&item.InputTokens,
			&item.OutputTokens,
			&item.CachedTokens,
			&item.CacheReadTokens,
			&item.CacheCreationTokens,
			&item.TotalTokens,
			&item.LastUsedAt,
		); errScan != nil {
			_ = summaryRows.Close()
			return fmt.Errorf("failed to scan account pool usage summary: %w", errScan)
		}
		summary := item
		if recomputed := recomputedSummaries[item.Key]; recomputed != nil {
			summary.TotalUSD = recomputed.TotalUSD
		}
		summaries[item.Key] = &summary
	}
	if errRows := summaryRows.Close(); errRows != nil {
		return fmt.Errorf("failed to close account pool usage summary rows: %w", errRows)
	}
	r.summaries = summaries
	return nil
}

func (r *accountPoolUsageRecorder) persistUsageLocked(record accountPoolUsageRecord, summary accountPoolUsageSummary) error {
	if ok, err := persistAccountPoolUsageToPostgres(context.Background(), record, summary); ok || err != nil {
		return err
	}
	db, err := r.openDBLocked()
	if err != nil || db == nil {
		return err
	}
	defer func() {
		if errClose := db.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close account pool usage database")
		}
	}()
	id, err := strconv.ParseUint(record.ID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid account pool usage id: %w", err)
	}
	success := 0
	if record.Success {
		success = 1
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin account pool usage transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			if errRollback := tx.Rollback(); errRollback != nil {
				log.WithError(errRollback).Debug("failed to rollback account pool usage transaction")
			}
		}
	}()
	if _, err = tx.Exec(`
INSERT OR REPLACE INTO account_pool_usage_records (
	id, requested_at, request_id, request_path, session_id, newapi_user_id, username, provider,
	model, alias, service_email, auth_id, auth_index, auth_type, success, status_code, latency_ms,
	input_tokens, output_tokens, cached_tokens, cache_read_tokens, cache_creation_tokens, total_tokens,
	request_params
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id,
		record.RequestedAt,
		record.RequestID,
		record.RequestPath,
		record.SessionID,
		record.NewAPIUserID,
		record.Username,
		record.Provider,
		record.Model,
		record.Alias,
		record.ServiceEmail,
		record.AuthID,
		record.AuthIndex,
		record.AuthType,
		success,
		record.StatusCode,
		record.LatencyMS,
		record.InputTokens,
		record.OutputTokens,
		record.CachedTokens,
		record.CacheReadTokens,
		record.CacheCreationTokens,
		record.TotalTokens,
		record.RequestParams,
	); err != nil {
		return fmt.Errorf("failed to insert account pool usage record: %w", err)
	}
	if _, err = tx.Exec(`
INSERT INTO account_pool_usage_summaries (
	key, service_email, auth_id, auth_index, auth_type, provider, model, alias, requests, successes,
	failures, input_tokens, output_tokens, cached_tokens, cache_read_tokens, cache_creation_tokens,
	total_tokens, last_used_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(key) DO UPDATE SET
	service_email=excluded.service_email,
	auth_id=excluded.auth_id,
	auth_index=excluded.auth_index,
	auth_type=excluded.auth_type,
	provider=excluded.provider,
	model=excluded.model,
	alias=excluded.alias,
	requests=excluded.requests,
	successes=excluded.successes,
	failures=excluded.failures,
	input_tokens=excluded.input_tokens,
	output_tokens=excluded.output_tokens,
	cached_tokens=excluded.cached_tokens,
	cache_read_tokens=excluded.cache_read_tokens,
	cache_creation_tokens=excluded.cache_creation_tokens,
	total_tokens=excluded.total_tokens,
	last_used_at=excluded.last_used_at`,
		summary.Key,
		summary.ServiceEmail,
		summary.AuthID,
		summary.AuthIndex,
		summary.AuthType,
		summary.Provider,
		summary.Model,
		summary.Alias,
		summary.Requests,
		summary.Successes,
		summary.Failures,
		summary.InputTokens,
		summary.OutputTokens,
		summary.CachedTokens,
		summary.CacheReadTokens,
		summary.CacheCreationTokens,
		summary.TotalTokens,
		summary.LastUsedAt,
	); err != nil {
		return fmt.Errorf("failed to upsert account pool usage summary: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit account pool usage transaction: %w", err)
	}
	committed = true
	return nil
}

func (r *accountPoolUsageRecorder) clearRecordsLocked() error {
	db, err := r.openDBLocked()
	if err != nil || db == nil {
		return err
	}
	defer func() {
		if errClose := db.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close account pool usage database")
		}
	}()
	if _, err = db.Exec(`DELETE FROM account_pool_usage_records`); err != nil {
		return fmt.Errorf("failed to clear account pool usage records: %w", err)
	}
	if _, err = db.Exec(`DELETE FROM account_pool_usage_summaries`); err != nil {
		return fmt.Errorf("failed to clear account pool usage summaries: %w", err)
	}
	return nil
}

func (r *accountPoolUsageRecorder) removeAccountPoolEntriesLocked(matchers map[string]struct{}) error {
	db, err := r.openDBLocked()
	if err != nil || db == nil {
		return err
	}
	defer func() {
		if errClose := db.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close account pool usage database")
		}
	}()
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin account pool usage cleanup transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			if errRollback := tx.Rollback(); errRollback != nil {
				log.WithError(errRollback).Debug("failed to rollback account pool usage cleanup transaction")
			}
		}
	}()
	for key := range matchers {
		if _, err = tx.Exec(`DELETE FROM account_pool_usage_summaries WHERE key = ?`, key); err != nil {
			return fmt.Errorf("failed to delete account pool usage summary: %w", err)
		}
		if strings.HasPrefix(key, "email:") {
			if _, err = tx.Exec(`DELETE FROM account_pool_usage_records WHERE lower(service_email) = ?`, strings.TrimPrefix(key, "email:")); err != nil {
				return fmt.Errorf("failed to delete account pool usage records by email: %w", err)
			}
			continue
		}
		if strings.HasPrefix(key, "auth_id:") {
			if _, err = tx.Exec(`DELETE FROM account_pool_usage_records WHERE auth_id = ?`, strings.TrimPrefix(key, "auth_id:")); err != nil {
				return fmt.Errorf("failed to delete account pool usage records by auth id: %w", err)
			}
			continue
		}
		if strings.HasPrefix(key, "auth_index:") {
			if _, err = tx.Exec(`DELETE FROM account_pool_usage_records WHERE auth_index = ?`, strings.TrimPrefix(key, "auth_index:")); err != nil {
				return fmt.Errorf("failed to delete account pool usage records by auth index: %w", err)
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit account pool usage cleanup: %w", err)
	}
	committed = true
	return nil
}

func accountPoolRequestIdentity(ctx context.Context) (sessionID string, userID string, username string, requestPath string, requestParams string) {
	ginCtx, ok := ctx.Value("gin").(*gin.Context)
	if !ok || ginCtx == nil || ginCtx.Request == nil {
		return "", "", "", "", ""
	}
	sessionID = strings.TrimSpace(ginCtx.GetHeader("X-Session-ID"))
	userID, username = parseNewAPISessionID(sessionID)
	if userID == "" {
		userID = firstAccountPoolUsageHeader(ginCtx, accountPoolUsageUserIDHeaders...)
	}
	if username == "" {
		username = firstAccountPoolUsageHeader(ginCtx, accountPoolUsageUsernameHeaders...)
	}
	if userID == "" || username == "" {
		metadataUserID, metadataUsername := accountPoolIdentityFromTurnMetadata(ginCtx.GetHeader("X-Codex-Turn-Metadata"))
		if userID == "" {
			userID = metadataUserID
		}
		if username == "" {
			username = metadataUsername
		}
	}
	if username == "" && isReadableAccountPoolUsageSessionName(sessionID) {
		username = sessionID
	}
	if username == "" {
		username = userID
	}
	if username == "" {
		log.Debugf(
			"account pool usage missing NewAPI username; session_id=%s header_names=%s",
			truncateAccountPoolUsageLogValue(sessionID),
			strings.Join(accountPoolUsageHeaderNames(ginCtx), ","),
		)
	}
	if ginCtx.Request.URL != nil {
		requestPath = strings.TrimSpace(ginCtx.Request.URL.Path)
	}
	requestParams = sanitizeAccountPoolUsageRequestParams(apimiddleware.CapturedRequestBody(ginCtx))
	return sessionID, userID, username, requestPath, requestParams
}

func accountPoolUsageHeaderNames(ctx *gin.Context) []string {
	if ctx == nil || ctx.Request == nil || len(ctx.Request.Header) == 0 {
		return nil
	}
	names := make([]string, 0, len(ctx.Request.Header))
	for name := range ctx.Request.Header {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func truncateAccountPoolUsageLogValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 12 {
		return value
	}
	return value[:8] + "..."
}

func firstAccountPoolUsageHeader(ctx *gin.Context, names ...string) string {
	if ctx == nil {
		return ""
	}
	normalizedNames := make(map[string]struct{}, len(names))
	for _, name := range names {
		if normalized := normalizeAccountPoolUsageHeaderName(name); normalized != "" {
			normalizedNames[normalized] = struct{}{}
		}
	}
	for _, name := range names {
		if value := strings.TrimSpace(ctx.GetHeader(name)); value != "" {
			return value
		}
	}
	for name, values := range ctx.Request.Header {
		if _, ok := normalizedNames[normalizeAccountPoolUsageHeaderName(name)]; !ok {
			continue
		}
		for _, value := range values {
			if value = strings.TrimSpace(value); value != "" {
				return value
			}
		}
	}
	return ""
}

func normalizeAccountPoolUsageHeaderName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	var builder strings.Builder
	builder.Grow(len(name))
	for _, char := range name {
		if char == '-' || char == '_' || char == ' ' {
			continue
		}
		builder.WriteRune(unicode.ToLower(char))
	}
	return builder.String()
}

func accountPoolIdentityFromTurnMetadata(raw string) (userID string, username string) {
	for _, candidate := range accountPoolUsageMetadataCandidates(raw) {
		if id, name := accountPoolIdentityFromJSON(candidate); id != "" || name != "" {
			return id, name
		}
	}
	return "", ""
}

func accountPoolUsageMetadataCandidates(raw string) [][]byte {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	candidates := [][]byte{[]byte(raw)}
	if decoded, err := url.QueryUnescape(raw); err == nil && decoded != raw {
		candidates = append(candidates, []byte(strings.TrimSpace(decoded)))
	}
	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		if decoded, err := encoding.DecodeString(raw); err == nil && len(decoded) > 0 {
			candidates = append(candidates, decoded)
		}
	}
	return candidates
}

func accountPoolIdentityFromJSON(data []byte) (userID string, username string) {
	data = []byte(strings.TrimSpace(string(data)))
	if len(data) == 0 || data[0] != '{' {
		return "", ""
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", ""
	}
	userID = firstAccountPoolMetadataString(payload,
		"user_id", "userid", "userId", "uid", "id", "newapi_user_id", "newapiUserId",
		"user.id", "user.user_id", "user.userId", "user.uid", "account.id", "account.user_id",
	)
	username = firstAccountPoolMetadataString(payload,
		"username", "user_name", "userName", "name", "login", "email", "newapi_username", "newapiUsername",
		"user.username", "user.user_name", "user.userName", "user.name", "user.login", "user.email",
		"account.username", "account.user_name", "account.name", "account.email",
	)
	return userID, username
}

func firstAccountPoolMetadataString(payload map[string]any, paths ...string) string {
	for _, path := range paths {
		if value := accountPoolMetadataValue(payload, strings.Split(path, ".")); value != "" {
			return value
		}
	}
	return ""
}

func accountPoolMetadataValue(value any, parts []string) string {
	if len(parts) == 0 {
		switch typed := value.(type) {
		case string:
			return strings.TrimSpace(typed)
		case float64:
			if typed == float64(int64(typed)) {
				return strconv.FormatInt(int64(typed), 10)
			}
		case json.Number:
			return strings.TrimSpace(typed.String())
		}
		return ""
	}
	current, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	want := normalizeAccountPoolUsageMetadataKey(parts[0])
	for key, child := range current {
		if normalizeAccountPoolUsageMetadataKey(key) == want {
			return accountPoolMetadataValue(child, parts[1:])
		}
	}
	return ""
}

func normalizeAccountPoolUsageMetadataKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	var builder strings.Builder
	builder.Grow(len(key))
	for _, char := range key {
		if char == '-' || char == '_' || char == ' ' {
			continue
		}
		builder.WriteRune(unicode.ToLower(char))
	}
	return builder.String()
}

func isReadableAccountPoolUsageSessionName(sessionID string) bool {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || strings.Contains(sessionID, ":") || isUUIDLikeAccountPoolUsageSessionID(sessionID) {
		return false
	}
	for _, char := range sessionID {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-' || char == '.' || char == '@' {
			continue
		}
		return false
	}
	return true
}

func isUUIDLikeAccountPoolUsageSessionID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, char := range value {
		switch i {
		case 8, 13, 18, 23:
			if char != '-' {
				return false
			}
		default:
			if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f' || char >= 'A' && char <= 'F') {
				return false
			}
		}
	}
	return true
}

func sanitizeAccountPoolUsageRequestParams(body []byte) string {
	body = []byte(strings.TrimSpace(string(body)))
	if len(body) == 0 {
		return ""
	}
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	sanitized := sanitizeUsageValue(payload)
	out, err := json.MarshalIndent(sanitized, "", "  ")
	if err != nil {
		return ""
	}
	if len(out) <= maxAccountPoolUsageParamsLength {
		return string(out)
	}
	return string(out[:maxAccountPoolUsageParamsLength]) + "\n...[truncated]"
}

func sanitizeUsageValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, child := range typed {
			if isUsageContextKey(key) {
				out[key] = emptyUsageContextValue(child)
				continue
			}
			if isUsageSecretKey(key) {
				out[key] = "[redacted]"
				continue
			}
			out[key] = sanitizeUsageValue(child)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, child := range typed {
			out[i] = sanitizeUsageValue(child)
		}
		return out
	default:
		return value
	}
}

func isUsageContextKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "messages", "input", "contents", "prompt", "prompts", "context", "contexts",
		"conversation", "conversation_history", "chat_history", "history", "system",
		"system_instruction", "instructions":
		return true
	default:
		return false
	}
}

func isUsageSecretKey(key string) bool {
	lower := strings.ToLower(strings.TrimSpace(key))
	return strings.Contains(lower, "token") ||
		strings.Contains(lower, "secret") ||
		strings.Contains(lower, "api_key") ||
		strings.Contains(lower, "apikey") ||
		strings.Contains(lower, "authorization")
}

func emptyUsageContextValue(value any) any {
	switch value.(type) {
	case []any:
		return []any{}
	case map[string]any:
		return map[string]any{}
	case string:
		return ""
	default:
		return nil
	}
}

func parseNewAPISessionID(sessionID string) (userID string, username string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return "", ""
	}
	const prefix = "newapi-user-"
	if !strings.HasPrefix(strings.ToLower(sessionID), prefix) {
		return "", ""
	}
	raw := strings.TrimSpace(sessionID[len(prefix):])
	userPart, namePart, ok := strings.Cut(raw, "+")
	if !ok {
		return strings.TrimSpace(userPart), ""
	}
	return strings.TrimSpace(userPart), strings.TrimSpace(namePart)
}

func (h *Handler) GetAccountPoolUsageRecords(c *gin.Context) {
	summaryOnly := parseBoolQuery(c.Query("summary_only")) || parseBoolQuery(c.Query("summaryOnly"))
	limit := 80
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid limit: %s", raw)})
			return
		}
		limit = parsed
	}
	offset := 0
	if raw := strings.TrimSpace(c.Query("offset")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid offset: %s", raw)})
			return
		}
		offset = parsed
	}
	if raw := strings.TrimSpace(c.Query("page")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid page: %s", raw)})
			return
		}
		if limit > 0 {
			offset = (parsed - 1) * limit
		}
	}
	if records, summaries, totals, total, ok, err := accountPoolUsageFromPostgres(c.Request.Context(), limit, offset, summaryOnly); err == nil && ok {
		if summaryOnly {
			c.JSON(http.StatusOK, gin.H{
				"records":   []accountPoolUsageRecord{},
				"summaries": summaries,
				"totals":    totals,
				"total":     0,
				"limit":     0,
				"offset":    0,
				"source":    "postgres",
			})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"records":   records,
			"summaries": summaries,
			"totals":    totals,
			"total":     total,
			"limit":     limit,
			"offset":    offset,
			"source":    "postgres",
		})
		return
	} else if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	accountPoolUsage.refreshFromStoreIfNewer()
	if summaryOnly {
		c.JSON(http.StatusOK, gin.H{
			"records":   []accountPoolUsageRecord{},
			"summaries": accountPoolUsage.Summaries(),
			"totals":    accountPoolUsage.Totals(),
			"total":     0,
			"limit":     0,
			"offset":    0,
		})
		return
	}
	records, total := accountPoolUsage.ListPage(limit, offset)
	c.JSON(http.StatusOK, gin.H{
		"records":   records,
		"summaries": accountPoolUsage.Summaries(),
		"totals":    accountPoolUsage.Totals(),
		"total":     total,
		"limit":     limit,
		"offset":    offset,
	})
}

func parseBoolQuery(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func (h *Handler) ClearAccountPoolUsageRecords(c *gin.Context) {
	accountPoolUsage.Clear()
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
