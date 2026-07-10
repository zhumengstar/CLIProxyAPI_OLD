package management

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestListInvalidAuthFiles_ReadsParentAuthBak(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	baseDir := t.TempDir()
	authDir := filepath.Join(baseDir, "auths")
	authBakDir := filepath.Join(baseDir, "authbak")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("failed to create auth dir: %v", err)
	}
	if err := os.MkdirAll(authBakDir, 0o700); err != nil {
		t.Fatalf("failed to create authbak dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(authBakDir, "antigravity-user.json"), []byte(`{"type":"antigravity","email":"user@example.com","project_id":"p1"}`), 0o600); err != nil {
		t.Fatalf("failed to write invalid auth file: %v", err)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/auth-files/invalid", nil)
	h.ListInvalidAuthFiles(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	var payload struct {
		Files []map[string]any `json:"files"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("failed to decode payload: %v", err)
	}
	if len(payload.Files) != 1 {
		t.Fatalf("expected one invalid auth file, got %d", len(payload.Files))
	}
	if got := payload.Files[0]["email"]; got != "user@example.com" {
		t.Fatalf("unexpected email: %#v", got)
	}
}

func TestExportAuthFiles_ZipsActiveAndInvalidFiles(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	baseDir := t.TempDir()
	authDir := filepath.Join(baseDir, "auths")
	authBakDir := filepath.Join(baseDir, "authbak")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("failed to create auth dir: %v", err)
	}
	if err := os.MkdirAll(authBakDir, 0o700); err != nil {
		t.Fatalf("failed to create authbak dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "active.json"), []byte(`{"type":"antigravity","email":"active@example.com"}`), 0o600); err != nil {
		t.Fatalf("failed to write active auth file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(authBakDir, "invalid.json"), []byte(`{"type":"antigravity","email":"invalid@example.com"}`), 0o600); err != nil {
		t.Fatalf("failed to write invalid auth file: %v", err)
	}

	body := `{"files":[{"name":"active.json","source":"active"},{"name":"invalid.json","source":"invalid","directory":` + strconvQuote(authBakDir) + `}]}`
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/auth-files/export", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.ExportAuthFiles(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("failed to read zip: %v", err)
	}
	names := make(map[string]bool, len(zr.File))
	for _, file := range zr.File {
		names[file.Name] = true
	}
	for _, name := range []string{"active/active.json", "invalid/authbak/invalid.json"} {
		if !names[name] {
			t.Fatalf("expected zip entry %q, got %#v", name, names)
		}
	}
}

func TestExportAuthFiles_RejectsInvalidDirectory(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	baseDir := t.TempDir()
	authDir := filepath.Join(baseDir, "auths")
	secretDir := filepath.Join(baseDir, "secret")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("failed to create auth dir: %v", err)
	}
	if err := os.MkdirAll(secretDir, 0o700); err != nil {
		t.Fatalf("failed to create secret dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(secretDir, "secret.json"), []byte(`{"type":"antigravity"}`), 0o600); err != nil {
		t.Fatalf("failed to write secret file: %v", err)
	}

	body := `{"files":[{"name":"secret.json","source":"invalid","directory":` + strconvQuote(secretDir) + `}]}`
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/auth-files/export", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.ExportAuthFiles(ctx)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusNotFound, rec.Code, rec.Body.String())
	}
}

func strconvQuote(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
