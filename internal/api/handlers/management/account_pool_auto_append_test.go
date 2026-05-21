package management

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestHighQuotaRemainingFromCheckRequiresUsableStatusAndPercent(t *testing.T) {
	quota := 42.5
	raw, err := json.Marshal(accountPoolCheckResultPayload{
		Status:                "success",
		QuotaRemainingPercent: &quota,
		CheckedAt:             123,
	})
	if err != nil {
		t.Fatalf("failed to marshal check result: %v", err)
	}

	gotQuota, checkedAt, ok := highQuotaRemainingFromCheck(string(raw))

	if !ok || gotQuota != quota || checkedAt != 123 {
		t.Fatalf("highQuotaRemainingFromCheck = (%v, %v, %v), want (%v, 123, true)", gotQuota, checkedAt, ok, quota)
	}

	failedQuota := 99.0
	failedRaw, err := json.Marshal(accountPoolCheckResultPayload{Status: "error", QuotaRemainingPercent: &failedQuota})
	if err != nil {
		t.Fatalf("failed to marshal failed check result: %v", err)
	}
	if _, _, ok := highQuotaRemainingFromCheck(string(failedRaw)); ok {
		t.Fatal("expected failed check status not to be treated as high quota")
	}
	if _, _, ok := highQuotaRemainingFromCheck(`{"status":"success"}`); ok {
		t.Fatal("expected missing quota percent not to be treated as high quota")
	}
}

func TestHighQuotaAccountPoolCandidatesFiltersThresholdAndStaleChecks(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	authDir := t.TempDir()
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, coreauth.NewManager(nil, nil, nil))

	highData := []byte(`{"type":"codex","email":"high@example.com"}`)
	lowData := []byte(`{"type":"codex","email":"low@example.com"}`)
	staleData := []byte(`{"type":"codex","email":"stale@example.com"}`)
	if err := h.writeAccountPoolArchive(map[string][]byte{
		"folder/high.json":  highData,
		"folder/low.json":   lowData,
		"folder/stale.json": staleData,
	}); err != nil {
		t.Fatalf("failed to seed account pool: %v", err)
	}

	high := 80.0
	low := 5.0
	stale := 95.0
	patchAccountPoolCheckResult(t, h, "folder/high.json", highData, high)
	patchAccountPoolCheckResult(t, h, "folder/low.json", lowData, low)
	patchAccountPoolCheckResultWithHash(t, h, "folder/stale.json", stale, "stale-content-hash")

	candidates, err := h.highQuotaAccountPoolCandidates()
	if err != nil {
		t.Fatalf("highQuotaAccountPoolCandidates failed: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates length = %d, want 1: %#v", len(candidates), candidates)
	}
	if candidates[0].Name != "folder/high.json" || !bytes.Equal(candidates[0].Data, highData) || candidates[0].Quota != high {
		t.Fatalf("candidate = %#v, want high quota entry", candidates[0])
	}
}

func TestAppendHighQuotaAccountPoolEntriesSkipsDuplicateContent(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	existingData := []byte(`{"type":"codex","email":"dup@example.com"}`)
	newData := []byte(`{"type":"codex","email":"new@example.com"}`)
	if err := os.WriteFile(filepath.Join(authDir, "existing.json"), existingData, 0o600); err != nil {
		t.Fatalf("failed to write existing auth file: %v", err)
	}
	if err := h.writeAccountPoolArchive(map[string][]byte{
		"pool/duplicate.json": existingData,
		"pool/new.json":       newData,
	}); err != nil {
		t.Fatalf("failed to seed account pool: %v", err)
	}
	high := 90.0
	patchAccountPoolCheckResult(t, h, "pool/duplicate.json", existingData, high)
	patchAccountPoolCheckResult(t, h, "pool/new.json", newData, high)

	added, skipped, err := h.appendHighQuotaAccountPoolEntries(context.Background())
	if err != nil {
		t.Fatalf("appendHighQuotaAccountPoolEntries failed: %v", err)
	}
	if added != 1 || skipped != 1 {
		t.Fatalf("append result = added %d skipped %d, want added 1 skipped 1", added, skipped)
	}
	if _, err := os.Stat(filepath.Join(authDir, "new.json")); err != nil {
		t.Fatalf("expected new high quota auth file to be written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(authDir, "duplicate.json")); !os.IsNotExist(err) {
		t.Fatalf("expected duplicate content not to be appended as duplicate.json, stat err: %v", err)
	}
	if auths := manager.List(); len(auths) != 1 {
		t.Fatalf("expected exactly one new runtime auth, got %d", len(auths))
	}
}

func TestMarkAuthDeletedFromDiskHidesListButKeepsRuntimeRecord(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:       "live.json",
		FileName: "live.json",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"path": filepath.Join(authDir, "live.json"),
		},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("failed to register auth: %v", err)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	h.markAuthDeletedFromDisk(context.Background(), auth)

	if auths := manager.List(); len(auths) != 1 {
		t.Fatalf("expected runtime auth to remain after delete marker, got %d", len(auths))
	}
	updated, ok := manager.GetByID("live.json")
	if !ok {
		t.Fatal("expected runtime auth to still be addressable")
	}
	if !updated.Disabled || updated.Status != coreauth.StatusDisabled || updated.StatusMessage != "removed via management api" {
		t.Fatalf("unexpected marked auth state: disabled=%v status=%q msg=%q", updated.Disabled, updated.Status, updated.StatusMessage)
	}
	if got := h.buildAuthFileEntry(updated, false); got != nil {
		t.Fatalf("expected deleted auth to be hidden from management list, got %#v", got)
	}
}

func patchAccountPoolCheckResult(t *testing.T, h *Handler, name string, data []byte, quota float64) {
	t.Helper()
	patchAccountPoolCheckResultWithHash(t, h, name, quota, hashAccountPoolContent(data))
}

func patchAccountPoolCheckResultWithHash(t *testing.T, h *Handler, name string, quota float64, contentHash string) {
	t.Helper()
	accountPoolDBMu.Lock()
	defer accountPoolDBMu.Unlock()
	db, err := h.openAccountPoolSQLiteLocked()
	if err != nil {
		t.Fatalf("failed to open account pool sqlite database: %v", err)
	}
	defer db.Close()
	result, err := json.Marshal(accountPoolCheckResultPayload{
		Status:                "success",
		QuotaRemainingPercent: &quota,
		CheckedAt:             123,
	})
	if err != nil {
		t.Fatalf("failed to marshal check result: %v", err)
	}
	res, err := db.Exec(`UPDATE account_pool_entries SET check_result = ?, check_content_hash = ?, check_updated_at = ? WHERE name = ?`, string(result), contentHash, "2026-05-20T00:00:00Z", name)
	if err != nil {
		t.Fatalf("failed to patch check result: %v", err)
	}
	if rows, _ := res.RowsAffected(); rows != 1 {
		t.Fatalf("patched rows = %d, want 1 for %s", rows, name)
	}
}
