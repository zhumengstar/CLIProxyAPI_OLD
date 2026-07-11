package management

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type manualPriorityStore struct {
	memoryAuthStore
	failSave bool
}

func (s *manualPriorityStore) Save(ctx context.Context, auth *coreauth.Auth) (string, error) {
	if s.failSave {
		return "", errors.New("save failed")
	}
	return s.memoryAuthStore.Save(ctx, auth)
}

func TestPatchAuthFileManualPriorityPersistsBeforeUpdatingMemory(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := &manualPriorityStore{}
	manager := coreauth.NewManager(store, nil, nil)
	auth := &coreauth.Auth{
		ID:       "pin-test.json",
		FileName: "pin-test.json",
		Provider: "antigravity",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"type": "antigravity"},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	t.Cleanup(func() { coreauth.SetManualWeeklyPriorityForPool(auth.ID, "gemini", false) })

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/manual-priority", strings.NewReader(`{"name":"pin-test.json","pool":"gemini","enabled":true}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.PatchAuthFileManualPriority(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !coreauth.ManualWeeklyPriorityForPool(auth.ID, "gemini") {
		t.Fatal("manual priority was not applied to runtime memory")
	}
	stored := store.items[auth.ID]
	if stored == nil {
		t.Fatal("auth was not persisted")
	}
	coreauth.SetManualWeeklyPriorityForPool(auth.ID, "gemini", false)
	coreauth.RestoreManualWeeklyPriority(stored)
	if !coreauth.ManualWeeklyPriorityForPool(auth.ID, "gemini") {
		t.Fatal("persisted metadata did not restore manual priority")
	}
	if !strings.Contains(rec.Body.String(), `"persisted":true`) || !strings.Contains(rec.Body.String(), `"memory_applied":true`) {
		t.Fatalf("response does not confirm persistence and memory state: %s", rec.Body.String())
	}
}

func TestPatchAuthFileManualPrioritySaveFailureLeavesMemoryUnchanged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := &manualPriorityStore{}
	manager := coreauth.NewManager(store, nil, nil)
	auth := &coreauth.Auth{
		ID:       "pin-fail.json",
		FileName: "pin-fail.json",
		Provider: "antigravity",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"type": "antigravity"},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("register auth: %v", err)
	}
	t.Cleanup(func() { coreauth.SetManualWeeklyPriorityForPool(auth.ID, "gemini", false) })
	store.failSave = true

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/auth-files/manual-priority", strings.NewReader(`{"name":"pin-fail.json","pool":"gemini","enabled":true}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.PatchAuthFileManualPriority(c)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if coreauth.ManualWeeklyPriorityForPool(auth.ID, "gemini") {
		t.Fatal("manual priority changed in memory after persistence failed")
	}
	current, ok := manager.GetByID(auth.ID)
	if !ok || current == nil {
		t.Fatal("auth disappeared from manager")
	}
	coreauth.RestoreManualWeeklyPriority(current)
	if coreauth.ManualWeeklyPriorityForPool(auth.ID, "gemini") {
		t.Fatal("manager metadata changed after persistence failed")
	}
}
