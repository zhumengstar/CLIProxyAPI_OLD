package management

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestUploadAuthFile_BatchMultipart(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	files := []struct {
		name    string
		content string
	}{
		{name: "alpha.json", content: `{"type":"codex","email":"alpha@example.com"}`},
		{name: "beta.json", content: `{"type":"claude","email":"beta@example.com"}`},
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, file := range files {
		part, err := writer.CreateFormFile("file", file.name)
		if err != nil {
			t.Fatalf("failed to create multipart file: %v", err)
		}
		if _, err = part.Write([]byte(file.content)); err != nil {
			t.Fatalf("failed to write multipart content: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("failed to close multipart writer: %v", err)
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	ctx.Request = req

	h.UploadAuthFile(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected upload status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if got, ok := payload["uploaded"].(float64); !ok || int(got) != len(files) {
		t.Fatalf("expected uploaded=%d, got %#v", len(files), payload["uploaded"])
	}

	for _, file := range files {
		fullPath := filepath.Join(authDir, file.name)
		data, err := os.ReadFile(fullPath)
		if err != nil {
			t.Fatalf("expected uploaded file %s to exist: %v", file.name, err)
		}
		if string(data) != file.content {
			t.Fatalf("expected file %s content %q, got %q", file.name, file.content, string(data))
		}
	}

	auths := manager.List()
	if len(auths) != len(files) {
		t.Fatalf("expected %d auth entries, got %d", len(files), len(auths))
	}

	if _, err := os.Stat(filepath.Join(authDir, "account-pool.db.json")); !os.IsNotExist(err) {
		t.Fatalf("expected auth upload not to change account pool database, stat err: %v", err)
	}
	if _, err := os.Stat(filepath.Join(authDir, "account-pool.sqlite")); !os.IsNotExist(err) {
		t.Fatalf("expected auth upload not to change account pool sqlite database, stat err: %v", err)
	}
}

func TestDownloadAccountPoolArchive_ExportsDatabaseEntries(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, coreauth.NewManager(nil, nil, nil))

	deletedName := "deleted.json"
	deletedContent := []byte(`{"type":"codex","email":"deleted@example.com"}`)
	if err := h.upsertAccountPoolArchiveFile(deletedName, deletedContent); err != nil {
		t.Fatalf("failed to seed account pool archive: %v", err)
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/account-pool/download", nil)

	h.DownloadAccountPoolArchive(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if contentType := rec.Header().Get("Content-Type"); contentType != "application/zip" {
		t.Fatalf("expected application/zip content type, got %q", contentType)
	}

	reader, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatalf("failed to read response zip: %v", err)
	}
	entries := make(map[string][]byte, len(reader.File))
	for _, entry := range reader.File {
		rc, errOpen := entry.Open()
		if errOpen != nil {
			t.Fatalf("failed to open response zip entry %s: %v", entry.Name, errOpen)
		}
		data, errRead := io.ReadAll(rc)
		errClose := rc.Close()
		if errRead != nil {
			t.Fatalf("failed to read response zip entry %s: %v", entry.Name, errRead)
		}
		if errClose != nil {
			t.Fatalf("failed to close response zip entry %s: %v", entry.Name, errClose)
		}
		entries[entry.Name] = data
	}

	if got := string(entries[deletedName]); got != string(deletedContent) {
		t.Fatalf("expected account pool database entry to be exported, got %q", got)
	}
}

func TestDownloadAccountPoolEntry_ReadsArchivedDeletedFile(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, coreauth.NewManager(nil, nil, nil))

	name := "deleted.json"
	content := []byte(`{"type":"codex","email":"deleted@example.com"}`)
	if err := h.upsertAccountPoolArchiveFile(name, content); err != nil {
		t.Fatalf("failed to seed account pool archive: %v", err)
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/account-pool/download-entry?name="+url.QueryEscape(name), nil)

	h.DownloadAccountPoolEntry(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != string(content) {
		t.Fatalf("expected archived account pool content %q, got %q", content, got)
	}
}

func TestAccountPoolPreservesFolderPathsAndMovesDuplicateAccount(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, coreauth.NewManager(nil, nil, nil))

	if err := h.upsertAccountPoolArchiveFiles([]accountPoolArchiveFile{
		{Name: "old/alpha.json", Data: []byte(`{"type":"codex","email":"same@example.com","client_id":"client-a"}`), Folder: "old"},
		{Name: "old/beta.json", Data: []byte(`{"type":"codex","email":"beta@example.com","client_id":"client-b"}`), Folder: "old"},
	}); err != nil {
		t.Fatalf("failed to seed account pool entries: %v", err)
	}
	if err := h.upsertAccountPoolArchiveFiles([]accountPoolArchiveFile{
		{Name: "new/alpha.json", Data: []byte(`{"type":"codex","email":"same@example.com","client_id":"client-a"}`), Folder: "new"},
	}); err != nil {
		t.Fatalf("failed to upsert moved account pool entry: %v", err)
	}

	entries, err := h.readAccountPoolArchive()
	if err != nil {
		t.Fatalf("failed to read account pool entries: %v", err)
	}
	if _, ok := entries["old/alpha.json"]; ok {
		t.Fatalf("expected older duplicate account entry to be removed")
	}
	if _, ok := entries["new/alpha.json"]; !ok {
		t.Fatalf("expected latest duplicate account entry to be kept")
	}
	if _, ok := entries["old/beta.json"]; !ok {
		t.Fatalf("expected unrelated account entry to remain")
	}

	zipEntries := readTestZipEntries(t, h.accountPoolArchivePath())
	if _, ok := zipEntries["new/alpha.json"]; !ok {
		t.Fatalf("expected zip mirror to contain latest entry path")
	}
	if _, ok := zipEntries["old/beta.json"]; !ok {
		t.Fatalf("expected zip mirror to preserve folder path for unrelated entry")
	}
}

func TestDeleteAccountPoolEntries_RemovesOnlyArchiveEntries(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, coreauth.NewManager(nil, nil, nil))

	removeName := "remove.json"
	keepName := "keep.json"
	removeContent := []byte(`{"type":"codex","email":"remove@example.com"}`)
	keepContent := []byte(`{"type":"codex","email":"keep@example.com"}`)
	if err := h.upsertAccountPoolArchiveFile(removeName, removeContent); err != nil {
		t.Fatalf("failed to seed removable account pool archive entry: %v", err)
	}
	if err := h.upsertAccountPoolArchiveFile(keepName, keepContent); err != nil {
		t.Fatalf("failed to seed kept account pool archive entry: %v", err)
	}
	if err := os.WriteFile(filepath.Join(authDir, removeName), removeContent, 0o600); err != nil {
		t.Fatalf("failed to seed auth file: %v", err)
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(
		http.MethodDelete,
		"/v0/management/account-pool?name="+url.QueryEscape(removeName),
		nil,
	)

	h.DeleteAccountPoolEntries(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	entries, err := h.readAccountPoolArchive()
	if err != nil {
		t.Fatalf("failed to read account pool database: %v", err)
	}
	if _, ok := entries[removeName]; ok {
		t.Fatalf("expected account pool database entry %s to be removed", removeName)
	}
	if got := string(entries[keepName]); got != string(keepContent) {
		t.Fatalf("expected kept archive entry content %q, got %q", keepContent, got)
	}
	if _, err := os.Stat(filepath.Join(authDir, removeName)); err != nil {
		t.Fatalf("expected auth file to remain after account pool delete: %v", err)
	}
}

func TestWriteAccountPoolToAuthFiles_AppendsInBatch(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	files := []accountPoolArchiveFile{
		{Name: "folder/alpha.json", Data: []byte(`{"type":"codex","email":"alpha@example.com"}`), Folder: "folder"},
		{Name: "folder/beta.json", Data: []byte(`{"type":"claude","email":"beta@example.com"}`), Folder: "folder"},
	}
	if err := h.upsertAccountPoolArchiveFiles(files); err != nil {
		t.Fatalf("failed to seed account pool entries: %v", err)
	}

	body := bytes.NewBufferString(`{"mode":"append","names":["folder/alpha.json","folder/beta.json"]}`)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/account-pool/write-auth-files", body)
	ctx.Request.Header.Set("Content-Type", "application/json")

	h.WriteAccountPoolToAuthFiles(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	for _, name := range []string{"alpha.json", "beta.json"} {
		if _, err := os.Stat(filepath.Join(authDir, name)); err != nil {
			t.Fatalf("expected auth file %s to exist: %v", name, err)
		}
	}
	if got := len(manager.List()); got != 2 {
		t.Fatalf("expected 2 auth records, got %d", got)
	}
}

func TestWriteAccountPoolToAuthFiles_OverwriteDeletesExistingAuthFiles(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	oldContent := []byte(`{"type":"codex","email":"old@example.com"}`)
	if err := os.WriteFile(filepath.Join(authDir, "old.json"), oldContent, 0o600); err != nil {
		t.Fatalf("failed to seed old auth file: %v", err)
	}
	oldAuth, err := h.buildAuthFromFileData(filepath.Join(authDir, "old.json"), oldContent)
	if err != nil {
		t.Fatalf("failed to build old auth: %v", err)
	}
	manager.UpsertMany(context.Background(), []*coreauth.Auth{oldAuth})

	if err := h.upsertAccountPoolArchiveFiles([]accountPoolArchiveFile{
		{Name: "new.json", Data: []byte(`{"type":"claude","email":"new@example.com"}`)},
	}); err != nil {
		t.Fatalf("failed to seed account pool entry: %v", err)
	}

	body := bytes.NewBufferString(`{"mode":"overwrite","names":["new.json"]}`)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v0/management/account-pool/write-auth-files", body)
	ctx.Request.Header.Set("Content-Type", "application/json")

	h.WriteAccountPoolToAuthFiles(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(authDir, "old.json")); !os.IsNotExist(err) {
		t.Fatalf("expected old auth file to be removed, stat err: %v", err)
	}
	if _, err := os.Stat(filepath.Join(authDir, "new.json")); err != nil {
		t.Fatalf("expected new auth file to exist: %v", err)
	}
	auths := manager.List()
	if len(auths) != 1 || authEmail(auths[0]) != "new@example.com" {
		t.Fatalf("expected only new auth record, got %#v", auths)
	}
}

func readTestZipEntries(t *testing.T, archivePath string) map[string][]byte {
	t.Helper()

	data, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatalf("failed to read zip archive: %v", err)
	}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("failed to open zip archive: %v", err)
	}
	entries := make(map[string][]byte, len(reader.File))
	for _, entry := range reader.File {
		rc, errOpen := entry.Open()
		if errOpen != nil {
			t.Fatalf("failed to open zip entry %s: %v", entry.Name, errOpen)
		}
		entryData, errRead := io.ReadAll(rc)
		errClose := rc.Close()
		if errRead != nil {
			t.Fatalf("failed to read zip entry %s: %v", entry.Name, errRead)
		}
		if errClose != nil {
			t.Fatalf("failed to close zip entry %s: %v", entry.Name, errClose)
		}
		entries[entry.Name] = entryData
	}
	return entries
}

func TestUploadAuthFile_BatchMultipart_InvalidJSONDoesNotOverwriteExistingFile(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	existingName := "alpha.json"
	existingContent := `{"type":"codex","email":"alpha@example.com"}`
	if err := os.WriteFile(filepath.Join(authDir, existingName), []byte(existingContent), 0o600); err != nil {
		t.Fatalf("failed to seed existing auth file: %v", err)
	}

	files := []struct {
		name    string
		content string
	}{
		{name: existingName, content: `{"type":"codex"`},
		{name: "beta.json", content: `{"type":"claude","email":"beta@example.com"}`},
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, file := range files {
		part, err := writer.CreateFormFile("file", file.name)
		if err != nil {
			t.Fatalf("failed to create multipart file: %v", err)
		}
		if _, err = part.Write([]byte(file.content)); err != nil {
			t.Fatalf("failed to write multipart content: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("failed to close multipart writer: %v", err)
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	ctx.Request = req

	h.UploadAuthFile(ctx)

	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("expected upload status %d, got %d with body %s", http.StatusMultiStatus, rec.Code, rec.Body.String())
	}

	data, err := os.ReadFile(filepath.Join(authDir, existingName))
	if err != nil {
		t.Fatalf("expected existing auth file to remain readable: %v", err)
	}
	if string(data) != existingContent {
		t.Fatalf("expected existing auth file to remain %q, got %q", existingContent, string(data))
	}

	betaData, err := os.ReadFile(filepath.Join(authDir, "beta.json"))
	if err != nil {
		t.Fatalf("expected valid auth file to be created: %v", err)
	}
	if string(betaData) != files[1].content {
		t.Fatalf("expected beta auth file content %q, got %q", files[1].content, string(betaData))
	}
}

func TestUploadAuthFile_ZipMultipart(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	var archive bytes.Buffer
	zipWriter := zip.NewWriter(&archive)
	entries := []struct {
		name    string
		content string
	}{
		{name: "nested/alpha.json", content: `{"type":"codex","email":"alpha@example.com"}`},
		{name: "beta.json", content: `{"type":"claude","email":"beta@example.com"}`},
		{name: "notes.txt", content: "ignored"},
	}
	for _, entry := range entries {
		part, err := zipWriter.Create(entry.name)
		if err != nil {
			t.Fatalf("failed to create zip entry %s: %v", entry.name, err)
		}
		if _, err = part.Write([]byte(entry.content)); err != nil {
			t.Fatalf("failed to write zip entry %s: %v", entry.name, err)
		}
	}
	if err := zipWriter.Close(); err != nil {
		t.Fatalf("failed to close zip writer: %v", err)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "auths.zip")
	if err != nil {
		t.Fatalf("failed to create multipart file: %v", err)
	}
	if _, err = part.Write(archive.Bytes()); err != nil {
		t.Fatalf("failed to write multipart zip: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("failed to close multipart writer: %v", err)
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	ctx.Request = req

	h.UploadAuthFile(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected upload status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	for _, file := range []struct {
		name    string
		content string
	}{
		{name: "alpha.json", content: entries[0].content},
		{name: "beta.json", content: entries[1].content},
	} {
		data, err := os.ReadFile(filepath.Join(authDir, file.name))
		if err != nil {
			t.Fatalf("expected uploaded file %s to exist: %v", file.name, err)
		}
		if string(data) != file.content {
			t.Fatalf("expected file %s content %q, got %q", file.name, file.content, string(data))
		}
	}

	if _, err := os.Stat(filepath.Join(authDir, "notes.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected non-json zip entry to be ignored, stat err: %v", err)
	}
	if len(manager.List()) != 2 {
		t.Fatalf("expected 2 auth entries, got %d", len(manager.List()))
	}
}

func TestUploadAuthFile_ZipMultipartPartialFailure(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)

	var archive bytes.Buffer
	zipWriter := zip.NewWriter(&archive)
	for _, entry := range []struct {
		name    string
		content string
	}{
		{name: "good.json", content: `{"type":"codex","email":"good@example.com"}`},
		{name: "bad.json", content: `{"type":"codex"`},
	} {
		part, err := zipWriter.Create(entry.name)
		if err != nil {
			t.Fatalf("failed to create zip entry %s: %v", entry.name, err)
		}
		if _, err = part.Write([]byte(entry.content)); err != nil {
			t.Fatalf("failed to write zip entry %s: %v", entry.name, err)
		}
	}
	if err := zipWriter.Close(); err != nil {
		t.Fatalf("failed to close zip writer: %v", err)
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "auths.zip")
	if err != nil {
		t.Fatalf("failed to create multipart file: %v", err)
	}
	if _, err = part.Write(archive.Bytes()); err != nil {
		t.Fatalf("failed to write multipart zip: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("failed to close multipart writer: %v", err)
	}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/v0/management/auth-files", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	ctx.Request = req

	h.UploadAuthFile(ctx)

	if rec.Code != http.StatusMultiStatus {
		t.Fatalf("expected upload status %d, got %d with body %s", http.StatusMultiStatus, rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(authDir, "good.json")); err != nil {
		t.Fatalf("expected good auth file to be created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(authDir, "bad.json")); !os.IsNotExist(err) {
		t.Fatalf("expected invalid auth file not to be written, stat err: %v", err)
	}
}

func TestDeleteAuthFile_BatchQuery(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")
	gin.SetMode(gin.TestMode)

	authDir := t.TempDir()
	files := []string{"alpha.json", "beta.json"}
	for _, name := range files {
		if err := os.WriteFile(filepath.Join(authDir, name), []byte(`{"type":"codex"}`), 0o600); err != nil {
			t.Fatalf("failed to write auth file %s: %v", name, err)
		}
	}

	manager := coreauth.NewManager(nil, nil, nil)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, manager)
	h.tokenStore = &memoryAuthStore{}

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(
		http.MethodDelete,
		"/v0/management/auth-files?name="+url.QueryEscape(files[0])+"&name="+url.QueryEscape(files[1]),
		nil,
	)
	ctx.Request = req

	h.DeleteAuthFile(ctx)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected delete status %d, got %d with body %s", http.StatusOK, rec.Code, rec.Body.String())
	}

	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if got, ok := payload["deleted"].(float64); !ok || int(got) != len(files) {
		t.Fatalf("expected deleted=%d, got %#v", len(files), payload["deleted"])
	}

	for _, name := range files {
		if _, err := os.Stat(filepath.Join(authDir, name)); !os.IsNotExist(err) {
			t.Fatalf("expected auth file %s to be removed, stat err: %v", name, err)
		}
	}
}
