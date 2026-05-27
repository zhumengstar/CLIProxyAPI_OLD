package management

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
)

func accountPoolUsageFromPostgres(ctx context.Context, limit int, offset int, summaryOnly bool) ([]accountPoolUsageRecord, []accountPoolUsageSummary, accountPoolUsageTotals, int, bool, error) {
	db, err := openAccountPoolPostgresDB(ctx)
	if err != nil {
		return nil, nil, accountPoolUsageTotals{}, 0, false, err
	}
	if db == nil {
		return nil, nil, accountPoolUsageTotals{}, 0, false, nil
	}
	defer func() { _ = db.Close() }()
	if err = ensureAccountPoolPostgresSchema(ctx, db); err != nil {
		return nil, nil, accountPoolUsageTotals{}, 0, false, err
	}
	total := 0
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM account_pool_usage_records`).Scan(&total); err != nil {
		return nil, nil, accountPoolUsageTotals{}, 0, false, fmt.Errorf("failed to count postgres account pool usage records: %w", err)
	}
	summaries, err := accountPoolUsageSummariesFromPostgres(ctx, db)
	if err != nil {
		return nil, nil, accountPoolUsageTotals{}, 0, false, err
	}
	totals := accountPoolUsageTotalsFromSummaries(summaries)
	if summaryOnly {
		return []accountPoolUsageRecord{}, summaries, totals, total, true, nil
	}
	records, err := accountPoolUsageRecordsFromPostgres(ctx, db, limit, offset)
	if err != nil {
		return nil, nil, accountPoolUsageTotals{}, 0, false, err
	}
	return records, summaries, totals, total, true, nil
}

func persistAccountPoolUsageToPostgres(ctx context.Context, record accountPoolUsageRecord, summary accountPoolUsageSummary) (bool, error) {
	db, err := openAccountPoolPostgresDB(ctx)
	if err != nil {
		return false, err
	}
	if db == nil {
		return false, nil
	}
	defer func() { _ = db.Close() }()
	id, err := strconv.ParseUint(record.ID, 10, 64)
	if err != nil {
		return true, fmt.Errorf("invalid account pool usage id: %w", err)
	}
	success := 0
	if record.Success {
		success = 1
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return true, fmt.Errorf("failed to begin postgres account pool usage transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.ExecContext(ctx, `
INSERT INTO account_pool_usage_records (
	id, requested_at, request_id, request_path, session_id, newapi_user_id, username, provider,
	model, alias, service_email, auth_id, auth_index, auth_type, success, status_code, latency_ms,
	input_tokens, output_tokens, cached_tokens, cache_read_tokens, cache_creation_tokens, total_tokens,
	request_params
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24)
ON CONFLICT(id) DO UPDATE SET
	requested_at=EXCLUDED.requested_at,
	request_id=EXCLUDED.request_id,
	request_path=EXCLUDED.request_path,
	session_id=EXCLUDED.session_id,
	newapi_user_id=EXCLUDED.newapi_user_id,
	username=EXCLUDED.username,
	provider=EXCLUDED.provider,
	model=EXCLUDED.model,
	alias=EXCLUDED.alias,
	service_email=EXCLUDED.service_email,
	auth_id=EXCLUDED.auth_id,
	auth_index=EXCLUDED.auth_index,
	auth_type=EXCLUDED.auth_type,
	success=EXCLUDED.success,
	status_code=EXCLUDED.status_code,
	latency_ms=EXCLUDED.latency_ms,
	input_tokens=EXCLUDED.input_tokens,
	output_tokens=EXCLUDED.output_tokens,
	cached_tokens=EXCLUDED.cached_tokens,
	cache_read_tokens=EXCLUDED.cache_read_tokens,
	cache_creation_tokens=EXCLUDED.cache_creation_tokens,
	total_tokens=EXCLUDED.total_tokens,
	request_params=EXCLUDED.request_params`,
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
		return true, fmt.Errorf("failed to upsert postgres account pool usage record: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `
INSERT INTO account_pool_usage_summaries (
	key, service_email, auth_id, auth_index, auth_type, provider, model, alias, requests, successes,
	failures, input_tokens, output_tokens, cached_tokens, cache_read_tokens, cache_creation_tokens,
	total_tokens, last_used_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)
ON CONFLICT(key) DO UPDATE SET
	service_email=EXCLUDED.service_email,
	auth_id=EXCLUDED.auth_id,
	auth_index=EXCLUDED.auth_index,
	auth_type=EXCLUDED.auth_type,
	provider=EXCLUDED.provider,
	model=EXCLUDED.model,
	alias=EXCLUDED.alias,
	requests=EXCLUDED.requests,
	successes=EXCLUDED.successes,
	failures=EXCLUDED.failures,
	input_tokens=EXCLUDED.input_tokens,
	output_tokens=EXCLUDED.output_tokens,
	cached_tokens=EXCLUDED.cached_tokens,
	cache_read_tokens=EXCLUDED.cache_read_tokens,
	cache_creation_tokens=EXCLUDED.cache_creation_tokens,
	total_tokens=EXCLUDED.total_tokens,
	last_used_at=EXCLUDED.last_used_at`,
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
		return true, fmt.Errorf("failed to upsert postgres account pool usage summary: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return true, fmt.Errorf("failed to commit postgres account pool usage transaction: %w", err)
	}
	committed = true
	return true, nil
}

func accountPoolUsageRecordsFromPostgres(ctx context.Context, db *sql.DB, limit int, offset int) ([]accountPoolUsageRecord, error) {
	query := `
SELECT id::text, COALESCE(requested_at, ''), COALESCE(request_id, ''), COALESCE(request_path, ''), COALESCE(session_id, ''), COALESCE(newapi_user_id, ''), COALESCE(username, ''),
	COALESCE(provider, ''), COALESCE(model, ''), COALESCE(alias, ''), COALESCE(service_email, ''), COALESCE(auth_id, ''), COALESCE(auth_index, ''), COALESCE(auth_type, ''), COALESCE(success, 0), COALESCE(status_code, 0),
	COALESCE(latency_ms, 0), COALESCE(input_tokens, 0), COALESCE(output_tokens, 0), COALESCE(cached_tokens, 0), COALESCE(cache_read_tokens, 0), COALESCE(cache_creation_tokens, 0),
	COALESCE(total_tokens, 0), COALESCE(request_params, '')
FROM account_pool_usage_records
ORDER BY id DESC`
	args := []any{}
	if limit > 0 {
		query += " LIMIT $1 OFFSET $2"
		args = append(args, limit, offset)
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query postgres account pool usage records: %w", err)
	}
	defer func() { _ = rows.Close() }()
	records := make([]accountPoolUsageRecord, 0)
	for rows.Next() {
		var item accountPoolUsageRecord
		var successInt int
		if err = rows.Scan(
			&item.ID, &item.RequestedAt, &item.RequestID, &item.RequestPath, &item.SessionID, &item.NewAPIUserID, &item.Username,
			&item.Provider, &item.Model, &item.Alias, &item.ServiceEmail, &item.AuthID, &item.AuthIndex, &item.AuthType, &successInt, &item.StatusCode,
			&item.LatencyMS, &item.InputTokens, &item.OutputTokens, &item.CachedTokens, &item.CacheReadTokens, &item.CacheCreationTokens,
			&item.TotalTokens, &item.RequestParams,
		); err != nil {
			return nil, fmt.Errorf("failed to scan postgres account pool usage record: %w", err)
		}
		item.Success = successInt != 0
		records = append(records, item)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate postgres account pool usage records: %w", err)
	}
	return records, nil
}

func accountPoolUsageSummariesFromPostgres(ctx context.Context, db *sql.DB) ([]accountPoolUsageSummary, error) {
	rows, err := db.QueryContext(ctx, `
SELECT key, COALESCE(service_email, ''), COALESCE(auth_id, ''), COALESCE(auth_index, ''), COALESCE(auth_type, ''), COALESCE(provider, ''), COALESCE(model, ''), COALESCE(alias, ''),
	COALESCE(requests, 0), COALESCE(successes, 0), COALESCE(failures, 0), COALESCE(input_tokens, 0), COALESCE(output_tokens, 0), COALESCE(cached_tokens, 0), COALESCE(cache_read_tokens, 0),
	COALESCE(cache_creation_tokens, 0), COALESCE(total_tokens, 0), COALESCE(last_used_at, '')
FROM account_pool_usage_summaries
ORDER BY requests DESC, key ASC`)
	if err != nil {
		return nil, fmt.Errorf("failed to query postgres account pool usage summaries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	summaries := make([]accountPoolUsageSummary, 0)
	for rows.Next() {
		var item accountPoolUsageSummary
		if err = rows.Scan(
			&item.Key, &item.ServiceEmail, &item.AuthID, &item.AuthIndex, &item.AuthType, &item.Provider, &item.Model, &item.Alias,
			&item.Requests, &item.Successes, &item.Failures, &item.InputTokens, &item.OutputTokens, &item.CachedTokens, &item.CacheReadTokens,
			&item.CacheCreationTokens, &item.TotalTokens, &item.LastUsedAt,
		); err != nil {
			return nil, fmt.Errorf("failed to scan postgres account pool usage summary: %w", err)
		}
		item.TotalUSD = accountPoolUsageUSD(item.Model, item.InputTokens, item.OutputTokens, item.CachedTokens, item.CacheReadTokens, item.CacheCreationTokens)
		summaries = append(summaries, item)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to iterate postgres account pool usage summaries: %w", err)
	}
	return summaries, nil
}

func accountPoolUsageTotalsFromSummaries(summaries []accountPoolUsageSummary) accountPoolUsageTotals {
	var totals accountPoolUsageTotals
	for _, item := range summaries {
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
