package management

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	apimiddleware "github.com/router-for-me/CLIProxyAPI/v7/internal/api/middleware"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const (
	maxAccountPoolUsageRecords      = 300
	maxAccountPoolUsageParamsLength = 16 * 1024
)

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
	Key                 string `json:"key"`
	ServiceEmail        string `json:"service_email,omitempty"`
	AuthID              string `json:"auth_id,omitempty"`
	AuthIndex           string `json:"auth_index,omitempty"`
	AuthType            string `json:"auth_type,omitempty"`
	Provider            string `json:"provider,omitempty"`
	Model               string `json:"model,omitempty"`
	Alias               string `json:"alias,omitempty"`
	Requests            int64  `json:"requests"`
	Successes           int64  `json:"successes"`
	Failures            int64  `json:"failures"`
	InputTokens         int64  `json:"input_tokens,omitempty"`
	OutputTokens        int64  `json:"output_tokens,omitempty"`
	CachedTokens        int64  `json:"cached_tokens,omitempty"`
	CacheReadTokens     int64  `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int64  `json:"cache_creation_tokens,omitempty"`
	TotalTokens         int64  `json:"total_tokens,omitempty"`
	LastUsedAt          string `json:"last_used_at,omitempty"`
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
	if overflow := len(r.records) - maxAccountPoolUsageRecords; overflow > 0 {
		copy(r.records, r.records[overflow:])
		r.records = r.records[:len(r.records)-overflow]
	}
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

func (r *accountPoolUsageRecorder) List(limit int) []accountPoolUsageRecord {
	records, _ := r.ListPage(limit, 0)
	return records
}

func (r *accountPoolUsageRecorder) ListPage(limit int, offset int) ([]accountPoolUsageRecord, int) {
	if r == nil {
		return nil, 0
	}
	if limit <= 0 || limit > maxAccountPoolUsageRecords {
		limit = 80
	}
	if offset < 0 {
		offset = 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	total := len(r.records)
	if offset >= total {
		return []accountPoolUsageRecord{}, total
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
	if len(r.summaries) == 0 {
		return nil
	}
	out := make([]accountPoolUsageSummary, 0, len(r.summaries))
	for _, item := range r.summaries {
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
SELECT id, requested_at, request_id, request_path, session_id, newapi_user_id, username,
	provider, model, alias, service_email, auth_id, auth_index, auth_type, success, status_code,
	latency_ms, input_tokens, output_tokens, cached_tokens, cache_read_tokens, cache_creation_tokens,
	total_tokens, request_params
FROM (
	SELECT * FROM account_pool_usage_records ORDER BY id DESC LIMIT ?
) ORDER BY id ASC`, maxAccountPoolUsageRecords)
	if err != nil {
		return fmt.Errorf("failed to query account pool usage records: %w", err)
	}
	records := make([]accountPoolUsageRecord, 0, maxAccountPoolUsageRecords)
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

	summaryRows, err := db.Query(`
SELECT key, service_email, auth_id, auth_index, auth_type, provider, model, alias,
	requests, successes, failures, input_tokens, output_tokens, cached_tokens, cache_read_tokens,
	cache_creation_tokens, total_tokens, last_used_at
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
		summaries[item.Key] = &summary
	}
	if errRows := summaryRows.Close(); errRows != nil {
		return fmt.Errorf("failed to close account pool usage summary rows: %w", errRows)
	}
	r.summaries = summaries
	return nil
}

func (r *accountPoolUsageRecorder) persistUsageLocked(record accountPoolUsageRecord, summary accountPoolUsageSummary) error {
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
	if _, err = tx.Exec(`DELETE FROM account_pool_usage_records WHERE id NOT IN (SELECT id FROM account_pool_usage_records ORDER BY id DESC LIMIT ?)`, maxAccountPoolUsageRecords); err != nil {
		return fmt.Errorf("failed to trim account pool usage records: %w", err)
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
	if ginCtx.Request.URL != nil {
		requestPath = strings.TrimSpace(ginCtx.Request.URL.Path)
	}
	requestParams = sanitizeAccountPoolUsageRequestParams(apimiddleware.CapturedRequestBody(ginCtx))
	return sessionID, userID, username, requestPath, requestParams
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
		if err != nil || parsed <= 0 {
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
		offset = (parsed - 1) * limit
	}
	if summaryOnly {
		c.JSON(http.StatusOK, gin.H{
			"records":   []accountPoolUsageRecord{},
			"summaries": accountPoolUsage.Summaries(),
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
