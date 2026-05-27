package management

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/tidwall/gjson"
)

const defaultAccountPoolPGSchema = "cliproxy"

func accountPoolPGSettings() (dsn string, schema string, enabled bool) {
	dsn = strings.TrimSpace(os.Getenv("PGSTORE_DSN"))
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv("pgstore_dsn"))
	}
	if dsn == "" {
		return "", "", false
	}
	schema = strings.TrimSpace(os.Getenv("PGSTORE_SCHEMA"))
	if schema == "" {
		schema = strings.TrimSpace(os.Getenv("pgstore_schema"))
	}
	if schema == "" {
		schema = defaultAccountPoolPGSchema
	}
	return dsn, schema, true
}

func accountPoolPGEnabled() bool {
	_, _, enabled := accountPoolPGSettings()
	return enabled
}

func openAccountPoolPostgresDB(ctx context.Context) (*sql.DB, error) {
	dsn, schema, ok := accountPoolPGSettings()
	if !ok {
		return nil, nil
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open account pool postgres database: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	if _, err = db.ExecContext(ctx, fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %s`, quotePGIdentifier(schema))); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to create account pool postgres schema: %w", err)
	}
	if _, err = db.ExecContext(ctx, fmt.Sprintf(`SET search_path TO %s`, quotePGIdentifier(schema))); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to set account pool postgres search_path: %w", err)
	}
	if err = ensureAccountPoolPostgresSchema(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func ensureAccountPoolPostgresSchema(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS account_pool_entries (
			name TEXT NOT NULL PRIMARY KEY,
			content_hash TEXT NOT NULL,
			type TEXT,
			provider TEXT,
			email TEXT,
			folder TEXT NOT NULL DEFAULT '',
			size INTEGER NOT NULL DEFAULT 0,
			data TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			check_result TEXT NOT NULL DEFAULT '',
			check_content_hash TEXT NOT NULL DEFAULT '',
			check_updated_at TEXT NOT NULL DEFAULT '',
			account_started_at TEXT NOT NULL DEFAULT '',
			account_stopped_at TEXT NOT NULL DEFAULT '',
			account_lifetime_seconds BIGINT NOT NULL DEFAULT 0,
			account_lifetime_active_since TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_account_pool_entries_email ON account_pool_entries(email)`,
		`CREATE INDEX IF NOT EXISTS idx_account_pool_entries_type ON account_pool_entries(type)`,
		`CREATE INDEX IF NOT EXISTS idx_account_pool_entries_updated_at ON account_pool_entries(updated_at)`,
		`CREATE INDEX IF NOT EXISTS idx_account_pool_entries_folder ON account_pool_entries(folder)`,
		`CREATE INDEX IF NOT EXISTS idx_account_pool_entries_content_hash ON account_pool_entries(content_hash)`,
		`CREATE TABLE IF NOT EXISTS account_pool_folders (
			folder TEXT NOT NULL PRIMARY KEY,
			source_model TEXT,
			source_info TEXT,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS account_pool_usage_records (
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
		)`,
		`CREATE INDEX IF NOT EXISTS idx_account_pool_usage_records_requested_at ON account_pool_usage_records(requested_at)`,
		`CREATE INDEX IF NOT EXISTS idx_account_pool_usage_records_auth_id ON account_pool_usage_records(auth_id)`,
		`CREATE INDEX IF NOT EXISTS idx_account_pool_usage_records_auth_index ON account_pool_usage_records(auth_index)`,
		`CREATE INDEX IF NOT EXISTS idx_account_pool_usage_records_service_email ON account_pool_usage_records(service_email)`,
		`CREATE TABLE IF NOT EXISTS account_pool_usage_summaries (
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
		)`,
	}
	for _, stmt := range stmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("failed to initialize account pool postgres schema: %w", err)
		}
	}
	columns := []struct {
		name string
		def  string
	}{
		{"account_lifetime_seconds", "BIGINT NOT NULL DEFAULT 0"},
		{"account_lifetime_active_since", "TEXT NOT NULL DEFAULT ''"},
	}
	for _, column := range columns {
		if _, err := db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE account_pool_entries ADD COLUMN IF NOT EXISTS %s %s", quotePGIdentifier(column.name), column.def)); err != nil {
			return fmt.Errorf("failed to add account pool postgres column %s: %w", column.name, err)
		}
	}
	return nil
}

func quotePGIdentifier(value string) string {
	return `"` + strings.ReplaceAll(strings.TrimSpace(value), `"`, `""`) + `"`
}

func syncAccountPoolEntriesToPostgres(ctx context.Context, entries map[string][]byte) error {
	dsn, schema, ok := accountPoolPGSettings()
	if !ok {
		return nil
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("failed to open account pool postgres database: %w", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err = db.ExecContext(ctx, fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %s`, quotePGIdentifier(schema))); err != nil {
		return fmt.Errorf("failed to create account pool postgres schema: %w", err)
	}
	if _, err = db.ExecContext(ctx, fmt.Sprintf(`SET search_path TO %s`, quotePGIdentifier(schema))); err != nil {
		return fmt.Errorf("failed to set account pool postgres search_path: %w", err)
	}
	if err = ensureAccountPoolPostgresSchema(ctx, db); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin account pool postgres transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.ExecContext(ctx, `DELETE FROM account_pool_entries`); err != nil {
		return fmt.Errorf("failed to clear account pool postgres entries: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM account_pool_folders`); err != nil {
		return fmt.Errorf("failed to clear account pool postgres folders: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO account_pool_entries (
	name, content_hash, type, provider, email, folder, size, data, created_at, updated_at,
	check_result, check_content_hash, check_updated_at, account_started_at, account_stopped_at, account_lifetime_seconds, account_lifetime_active_since
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
ON CONFLICT(name) DO UPDATE SET
	content_hash = EXCLUDED.content_hash,
	type = EXCLUDED.type,
	provider = EXCLUDED.provider,
	email = EXCLUDED.email,
	folder = CASE WHEN EXCLUDED.folder != '' THEN EXCLUDED.folder ELSE account_pool_entries.folder END,
	size = EXCLUDED.size,
	data = EXCLUDED.data,
	check_result = CASE WHEN EXCLUDED.check_result != '' THEN EXCLUDED.check_result ELSE account_pool_entries.check_result END,
	check_content_hash = CASE WHEN EXCLUDED.check_content_hash != '' THEN EXCLUDED.check_content_hash ELSE account_pool_entries.check_content_hash END,
	check_updated_at = CASE WHEN EXCLUDED.check_updated_at != '' THEN EXCLUDED.check_updated_at ELSE account_pool_entries.check_updated_at END,
	account_started_at = CASE WHEN EXCLUDED.account_started_at != '' THEN EXCLUDED.account_started_at ELSE account_pool_entries.account_started_at END,
	account_stopped_at = CASE WHEN EXCLUDED.account_stopped_at != '' THEN EXCLUDED.account_stopped_at ELSE account_pool_entries.account_stopped_at END,
	account_lifetime_seconds = CASE WHEN EXCLUDED.account_lifetime_seconds > 0 THEN EXCLUDED.account_lifetime_seconds ELSE account_pool_entries.account_lifetime_seconds END,
	account_lifetime_active_since = CASE WHEN EXCLUDED.account_lifetime_active_since != '' THEN EXCLUDED.account_lifetime_active_since ELSE account_pool_entries.account_lifetime_active_since END,
	updated_at = EXCLUDED.updated_at
`)
	if err != nil {
		return fmt.Errorf("failed to prepare account pool postgres upsert: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	folders := make(map[string]struct{})
	for name, data := range entries {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		entry := buildAccountPoolDBEntry(name, data, now)
		folders[entry.Folder] = struct{}{}
		startedAt := strings.TrimSpace(gjson.GetBytes([]byte(entry.Data), "account_started_at").String())
		stoppedAt := strings.TrimSpace(gjson.GetBytes([]byte(entry.Data), "account_stopped_at").String())
		if _, err = stmt.ExecContext(ctx, entry.Name, entry.Hash, entry.Type, entry.Provider, entry.Email, entry.Folder, entry.Size, entry.Data, entry.CreatedAt, entry.UpdatedAt, "", "", "", startedAt, stoppedAt, int64(0), ""); err != nil {
			return fmt.Errorf("failed to upsert account pool postgres entry %s: %w", entry.Name, err)
		}
	}
	folderStmt, err := tx.PrepareContext(ctx, `
INSERT INTO account_pool_folders (folder, source_model, source_info, created_at, updated_at)
VALUES ($1,$2,$3,$4,$5)
ON CONFLICT(folder) DO UPDATE SET
	source_model = EXCLUDED.source_model,
	source_info = EXCLUDED.source_info,
	updated_at = EXCLUDED.updated_at
`)
	if err != nil {
		return fmt.Errorf("failed to prepare account pool postgres folder upsert: %w", err)
	}
	defer func() { _ = folderStmt.Close() }()
	for folder := range folders {
		if strings.TrimSpace(folder) == "" {
			continue
		}
		if _, err = folderStmt.ExecContext(ctx, folder, nil, nil, now, now); err != nil {
			return fmt.Errorf("failed to upsert account pool postgres folder %s: %w", folder, err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit account pool postgres sync: %w", err)
	}
	committed = true
	return nil
}

func upsertAccountPoolArchiveFilesToPostgres(ctx context.Context, files []accountPoolArchiveFile) error {
	dsn, schema, ok := accountPoolPGSettings()
	if !ok || len(files) == 0 {
		return nil
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("failed to open account pool postgres database: %w", err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err = db.ExecContext(ctx, fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %s`, quotePGIdentifier(schema))); err != nil {
		return fmt.Errorf("failed to create account pool postgres schema: %w", err)
	}
	if _, err = db.ExecContext(ctx, fmt.Sprintf(`SET search_path TO %s`, quotePGIdentifier(schema))); err != nil {
		return fmt.Errorf("failed to set account pool postgres search_path: %w", err)
	}
	if err = ensureAccountPoolPostgresSchema(ctx, db); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin account pool postgres import transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	now := time.Now().UTC().Format(time.RFC3339)
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO account_pool_entries (
	name, content_hash, type, provider, email, folder, size, data, created_at, updated_at,
	check_result, check_content_hash, check_updated_at, account_started_at, account_stopped_at, account_lifetime_seconds, account_lifetime_active_since
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
ON CONFLICT(name) DO UPDATE SET
	content_hash = EXCLUDED.content_hash,
	type = EXCLUDED.type,
	provider = EXCLUDED.provider,
	email = EXCLUDED.email,
	folder = CASE WHEN EXCLUDED.folder != '' THEN EXCLUDED.folder ELSE account_pool_entries.folder END,
	size = EXCLUDED.size,
	data = EXCLUDED.data,
	check_result = CASE WHEN EXCLUDED.content_hash = account_pool_entries.content_hash THEN account_pool_entries.check_result ELSE '' END,
	check_content_hash = CASE WHEN EXCLUDED.content_hash = account_pool_entries.content_hash THEN account_pool_entries.check_content_hash ELSE '' END,
	check_updated_at = CASE WHEN EXCLUDED.content_hash = account_pool_entries.content_hash THEN account_pool_entries.check_updated_at ELSE '' END,
	account_started_at = CASE WHEN EXCLUDED.account_started_at != '' THEN EXCLUDED.account_started_at ELSE account_pool_entries.account_started_at END,
	account_stopped_at = CASE WHEN EXCLUDED.account_stopped_at != '' THEN EXCLUDED.account_stopped_at ELSE account_pool_entries.account_stopped_at END,
	account_lifetime_seconds = CASE WHEN EXCLUDED.account_lifetime_seconds > 0 THEN EXCLUDED.account_lifetime_seconds ELSE account_pool_entries.account_lifetime_seconds END,
	account_lifetime_active_since = CASE WHEN EXCLUDED.account_lifetime_active_since != '' THEN EXCLUDED.account_lifetime_active_since ELSE account_pool_entries.account_lifetime_active_since END,
	updated_at = EXCLUDED.updated_at
`)
	if err != nil {
		return fmt.Errorf("failed to prepare account pool postgres import upsert: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	folders := make(map[string]struct{})
	for _, file := range files {
		name := normalizeAccountPoolEntryName(file.Name)
		if name == "" || isUnsafeAccountPoolEntryName(name) || !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		data := bytes.TrimSpace(file.Data)
		if len(data) == 0 {
			continue
		}
		entry := buildAccountPoolDBEntry(name, data, now)
		folders[entry.Folder] = struct{}{}
		startedAt := strings.TrimSpace(gjson.GetBytes([]byte(entry.Data), "account_started_at").String())
		stoppedAt := strings.TrimSpace(gjson.GetBytes([]byte(entry.Data), "account_stopped_at").String())
		if _, err = stmt.ExecContext(ctx, entry.Name, entry.Hash, entry.Type, entry.Provider, entry.Email, entry.Folder, entry.Size, entry.Data, entry.CreatedAt, entry.UpdatedAt, "", "", "", startedAt, stoppedAt, int64(0), ""); err != nil {
			return fmt.Errorf("failed to upsert account pool postgres import entry %s: %w", entry.Name, err)
		}
	}
	folderStmt, err := tx.PrepareContext(ctx, `
INSERT INTO account_pool_folders (folder, source_model, source_info, created_at, updated_at)
VALUES ($1,$2,$3,$4,$5)
ON CONFLICT(folder) DO UPDATE SET
	updated_at = EXCLUDED.updated_at
`)
	if err != nil {
		return fmt.Errorf("failed to prepare account pool postgres import folder upsert: %w", err)
	}
	defer func() { _ = folderStmt.Close() }()
	for folder := range folders {
		if strings.TrimSpace(folder) == "" {
			continue
		}
		if _, err = folderStmt.ExecContext(ctx, folder, nil, nil, now, now); err != nil {
			return fmt.Errorf("failed to upsert account pool postgres import folder %s: %w", folder, err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit account pool postgres import: %w", err)
	}
	committed = true
	return nil
}

func readAccountPoolEntriesFromPostgres(ctx context.Context) (map[string][]byte, error) {
	db, err := openAccountPoolPostgresDB(ctx)
	if err != nil {
		return nil, err
	}
	if db == nil {
		return nil, nil
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(ctx, `SELECT name, data, account_started_at, account_stopped_at, account_lifetime_seconds, account_lifetime_active_since FROM account_pool_entries ORDER BY lower(name)`)
	if err != nil {
		return nil, fmt.Errorf("failed to query account pool postgres entries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	entries := make(map[string][]byte)
	for rows.Next() {
		var name, data string
		var startedAt, stoppedAt, activeSince string
		var lifetimeSeconds int64
		if errScan := rows.Scan(&name, &data, &startedAt, &stoppedAt, &lifetimeSeconds, &activeSince); errScan != nil {
			return nil, fmt.Errorf("failed to scan account pool postgres entry: %w", errScan)
		}
		name = normalizeAccountPoolEntryName(name)
		if name == "" || isUnsafeAccountPoolEntryName(name) || !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "" {
			continue
		}
		entries[name] = mergeAccountPoolPGPersistentFields([]byte(data), strings.TrimSpace(startedAt), strings.TrimSpace(stoppedAt), lifetimeSeconds, strings.TrimSpace(activeSince))
	}
	if errRows := rows.Err(); errRows != nil {
		return nil, fmt.Errorf("failed to read account pool postgres entries: %w", errRows)
	}
	return entries, nil
}

func (h *Handler) accountPoolAutoCheckEntriesPostgres(ctx context.Context) ([]accountPoolAutoCheckEntry, error) {
	db, err := openAccountPoolPostgresDB(ctx)
	if err != nil {
		return nil, err
	}
	if db == nil {
		return nil, nil
	}
	defer func() { _ = db.Close() }()
	existingHashes := h.existingAuthContentHashes()
	existingIdentities := h.existingAuthAccountIdentities()
	rows, err := db.QueryContext(ctx, `SELECT name, data, content_hash, COALESCE(type, ''), COALESCE(provider, '') FROM account_pool_entries ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("failed to query account pool postgres auto check entries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	entries := make([]accountPoolAutoCheckEntry, 0)
	for rows.Next() {
		var name, data, hash, typeValue, provider string
		if errScan := rows.Scan(&name, &data, &hash, &typeValue, &provider); errScan != nil {
			return nil, fmt.Errorf("failed to scan account pool postgres auto check entry: %w", errScan)
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
			entries = append(entries, accountPoolAutoCheckEntry{Name: name, Data: append([]byte(nil), entryData...), ContentHash: rawContentHash, Provider: provider})
		}
	}
	if errRows := rows.Err(); errRows != nil {
		return nil, fmt.Errorf("failed to iterate account pool postgres auto check entries: %w", errRows)
	}
	return entries, nil
}

func (h *Handler) highQuotaAccountPoolCandidatesPostgres(ctx context.Context) ([]accountPoolAutoAppendCandidate, error) {
	db, err := openAccountPoolPostgresDB(ctx)
	if err != nil {
		return nil, err
	}
	if db == nil {
		return nil, nil
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(ctx, `SELECT name, data, content_hash, COALESCE(check_result, ''), COALESCE(check_content_hash, '') FROM account_pool_entries`)
	if err != nil {
		return nil, fmt.Errorf("failed to query account pool postgres high quota candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	candidates := make([]accountPoolAutoAppendCandidate, 0)
	for rows.Next() {
		var name, data, contentHash, rawCheck, checkContentHash string
		if errScan := rows.Scan(&name, &data, &contentHash, &rawCheck, &checkContentHash); errScan != nil {
			return nil, fmt.Errorf("failed to scan account pool postgres high quota candidate: %w", errScan)
		}
		name = normalizeAccountPoolEntryName(name)
		if name == "" || isUnsafeAccountPoolEntryName(name) || !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		entryData := bytes.TrimSpace([]byte(data))
		if len(entryData) == 0 {
			continue
		}
		rawContentHash := strings.TrimSpace(contentHash)
		if rawContentHash == "" {
			rawContentHash = hashAccountPoolContent(entryData)
		}
		if checkContentHash = strings.TrimSpace(checkContentHash); checkContentHash != "" && rawContentHash != "" && checkContentHash != rawContentHash {
			continue
		}
		quota, checkedAt, ok := highQuotaRemainingFromCheck(rawCheck)
		if !ok || quota <= accountPoolHighQuotaRemainingPercent {
			continue
		}
		authData := stripAccountPoolStateForAuthFile(entryData)
		candidates = append(candidates, accountPoolAutoAppendCandidate{Name: name, Data: authData, Hash: hashAccountPoolContent(authData), Quota: quota, CheckedAt: checkedAt})
	}
	if errRows := rows.Err(); errRows != nil {
		return nil, fmt.Errorf("failed to read account pool postgres high quota candidates: %w", errRows)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Quota != candidates[j].Quota {
			return candidates[i].Quota > candidates[j].Quota
		}
		if candidates[i].CheckedAt != candidates[j].CheckedAt {
			return candidates[i].CheckedAt > candidates[j].CheckedAt
		}
		return strings.ToLower(candidates[i].Name) < strings.ToLower(candidates[j].Name)
	})
	return candidates, nil
}

func mergeAccountPoolPGPersistentFields(data []byte, startedAt string, stoppedAt string, lifetimeSeconds int64, activeSince string) []byte {
	if len(data) == 0 {
		return data
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(data, &obj); err != nil || obj == nil {
		return data
	}
	if startedAt != "" {
		obj["account_started_at"] = startedAt
	}
	if stoppedAt != "" {
		obj["account_stopped_at"] = stoppedAt
	} else {
		delete(obj, "account_stopped_at")
	}
	if lifetimeSeconds >= 0 {
		obj["account_lifetime_seconds"] = lifetimeSeconds
	}
	if activeSince != "" {
		obj["account_lifetime_active_since"] = activeSince
	} else {
		delete(obj, "account_lifetime_active_since")
	}
	merged, err := json.Marshal(obj)
	if err != nil {
		return data
	}
	return merged
}

func readAccountPoolEntryFromPostgres(ctx context.Context, name string) ([]byte, error) {
	name = normalizeAccountPoolEntryName(name)
	if name == "" || isUnsafeAccountPoolEntryName(name) || !strings.HasSuffix(strings.ToLower(name), ".json") {
		return nil, fmt.Errorf("invalid account pool entry name")
	}
	db, err := openAccountPoolPostgresDB(ctx)
	if err != nil {
		return nil, err
	}
	if db == nil {
		return nil, nil
	}
	defer func() { _ = db.Close() }()
	var data, startedAt, stoppedAt, activeSince string
	var lifetimeSeconds int64
	if err := db.QueryRowContext(ctx, `SELECT data, account_started_at, account_stopped_at, account_lifetime_seconds, account_lifetime_active_since FROM account_pool_entries WHERE name = $1`, name).Scan(&data, &startedAt, &stoppedAt, &lifetimeSeconds, &activeSince); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to query account pool postgres entry %s: %w", name, err)
	}
	data = strings.TrimSpace(data)
	if data == "" {
		return nil, nil
	}
	return mergeAccountPoolPGPersistentFields([]byte(data), strings.TrimSpace(startedAt), strings.TrimSpace(stoppedAt), lifetimeSeconds, strings.TrimSpace(activeSince)), nil
}
