package management

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestHighQuotaRemainingFromCheckRequiresUsableStatusAndPercent(t *testing.T) {
	quota := 42.5
	raw, err := json.Marshal(accountPoolCheckResultPayload{
		Status:                "success",
		QuotaRemainingPercent: &quota,
		CheckedAt:             123,
		RealRequestOK:         true,
	})
	if err != nil {
		t.Fatalf("failed to marshal check result: %v", err)
	}

	gotQuota, checkedAt, ok := highQuotaRemainingFromCheck(string(raw))
	if !ok || gotQuota != quota || checkedAt != 123 {
		t.Fatalf("highQuotaRemainingFromCheck = (%v, %v, %v), want (%v, 123, true)", gotQuota, checkedAt, ok, quota)
	}

	failedQuota := 99.0
	failedRaw, err := json.Marshal(accountPoolCheckResultPayload{Status: "error", QuotaRemainingPercent: &failedQuota, RealRequestOK: false, RealRequestError: "401: unauthorized"})
	if err != nil {
		t.Fatalf("failed to marshal failed check result: %v", err)
	}
	if _, _, ok := highQuotaRemainingFromCheck(string(failedRaw)); ok {
		t.Fatal("expected failed check status not to be treated as high quota")
	}
	successButModelFailedRaw, err := json.Marshal(accountPoolCheckResultPayload{Status: "success", Message: "Check passed", QuotaRemainingPercent: &failedQuota, RealRequestOK: false})
	if err != nil {
		t.Fatalf("failed to marshal success with failed model request result: %v", err)
	}
	if _, _, ok := highQuotaRemainingFromCheck(string(successButModelFailedRaw)); ok {
		t.Fatal("expected success check without model request success not to be treated as high quota")
	}
	if _, _, ok := highQuotaRemainingFromCheck(`{"status":"success"}`); ok {
		t.Fatal("expected missing quota percent not to be treated as high quota")
	}
}

func TestNormalizeAccountPoolCheckResultRequiresModelRequestSuccess(t *testing.T) {
	quota := 84.0
	result, ok := normalizeAccountPoolCheckResultPayload(accountPoolCheckResultPayload{
		Status:                "success",
		Message:               "Check passed",
		Plan:                  "plus",
		QuotaRemainingPercent: &quota,
		RealRequestOK:         false,
		StatusCode:            200,
		CheckedAt:             123,
	}, time.Unix(100, 0))
	if !ok {
		t.Fatal("expected result to normalize")
	}
	if result.Status != "error" {
		t.Fatalf("status = %q, want error", result.Status)
	}
	if result.Message != "模型检测请求失败" {
		t.Fatalf("message = %q, want 模型检测请求失败", result.Message)
	}
	if result.Plan != "plus" || result.QuotaRemainingPercent == nil || *result.QuotaRemainingPercent != quota {
		t.Fatalf("expected plus quota details to be preserved, got %#v", result)
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
	newData := []byte(`{
		"type":"codex",
		"email":"new@example.com",
		"disabled": true,
		"status": "disabled",
		"status_message": "from pool state",
		"check_result": {"status":"success"},
		"account_stopped_at": "2026-05-20T00:00:00Z",
		"metadata": {"status":"disabled", "account_stopped_at":"2026-05-20T00:00:00Z"},
		"attributes": {"failed":"3"}
	}`)
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
	writtenData, err := os.ReadFile(filepath.Join(authDir, "new.json"))
	if err != nil {
		t.Fatalf("expected new high quota auth file to be written: %v", err)
	}
	for _, path := range []string{"disabled", "status", "status_message", "check_result", "account_stopped_at", "metadata.status", "metadata.account_stopped_at", "attributes.failed"} {
		if gjson.GetBytes(writtenData, path).Exists() {
			t.Fatalf("expected written auth file not to include pool state path %s: %s", path, string(writtenData))
		}
	}
	if got := gjson.GetBytes(writtenData, "email").String(); got != "new@example.com" {
		t.Fatalf("email = %q, want new@example.com", got)
	}
	if _, err := os.Stat(filepath.Join(authDir, "duplicate.json")); !os.IsNotExist(err) {
		t.Fatalf("expected duplicate content not to be appended as duplicate.json, stat err: %v", err)
	}
	if auths := manager.List(); len(auths) != 1 {
		t.Fatalf("expected exactly one new runtime auth, got %d", len(auths))
	}
}

func TestAppendHighQuotaAccountPoolEntriesSkipsDuplicateAccountIdentity(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	existingData := []byte(`{"type":"codex","email":"same@example.com","account_id":"acct-1","access_token":"old-token"}`)
	poolData := []byte(`{"type":"codex","email":"same@example.com","account_id":"acct-1","access_token":"new-token"}`)
	otherData := []byte(`{"type":"codex","email":"other@example.com","account_id":"acct-2","access_token":"other-token"}`)
	if err := os.WriteFile(filepath.Join(authDir, "existing.json"), existingData, 0o600); err != nil {
		t.Fatalf("failed to write existing auth file: %v", err)
	}
	if err := h.writeAccountPoolArchive(map[string][]byte{
		"pool/same.json":  poolData,
		"pool/other.json": otherData,
	}); err != nil {
		t.Fatalf("failed to seed account pool: %v", err)
	}
	high := 90.0
	patchAccountPoolCheckResult(t, h, "pool/same.json", poolData, high)
	patchAccountPoolCheckResult(t, h, "pool/other.json", otherData, high)

	added, skipped, err := h.appendHighQuotaAccountPoolEntries(context.Background())
	if err != nil {
		t.Fatalf("appendHighQuotaAccountPoolEntries failed: %v", err)
	}
	if added != 1 || skipped != 1 {
		t.Fatalf("append result = added %d skipped %d, want added 1 skipped 1", added, skipped)
	}
	if _, err := os.Stat(filepath.Join(authDir, "same.json")); !os.IsNotExist(err) {
		t.Fatalf("expected duplicate account identity not to be appended as same.json, stat err: %v", err)
	}
	if _, err := os.Stat(filepath.Join(authDir, "other.json")); err != nil {
		t.Fatalf("expected distinct account to be appended: %v", err)
	}
}

func TestAccountPoolAutoCheckEntriesSkipsExistingAuthIdentityBeforeProbe(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	existingData := []byte(`{"type":"codex","email":"same@example.com","account_id":"acct-1","access_token":"old-token"}`)
	poolSameData := []byte(`{"type":"codex","email":"same@example.com","account_id":"acct-1","access_token":"new-token"}`)
	poolOtherData := []byte(`{"type":"codex","email":"other@example.com","account_id":"acct-2","access_token":"other-token"}`)
	if err := os.WriteFile(filepath.Join(authDir, "existing.json"), existingData, 0o600); err != nil {
		t.Fatalf("failed to write existing auth file: %v", err)
	}
	if err := h.writeAccountPoolArchive(map[string][]byte{
		"pool/same.json":  poolSameData,
		"pool/other.json": poolOtherData,
	}); err != nil {
		t.Fatalf("failed to seed account pool: %v", err)
	}

	entries, err := h.accountPoolAutoCheckEntries()
	if err != nil {
		t.Fatalf("accountPoolAutoCheckEntries failed: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries length = %d, want 1: %#v", len(entries), entries)
	}
	if entries[0].Name != "pool/other.json" {
		t.Fatalf("entry name = %q, want pool/other.json", entries[0].Name)
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
		RealRequestOK:         true,
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
