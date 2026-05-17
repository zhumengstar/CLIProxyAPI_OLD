package management

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/antigravity"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	geminiAuth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/gemini"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/kimi"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	sdkhandlers "github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	_ "modernc.org/sqlite"
)

var lastRefreshKeys = []string{"last_refresh", "lastRefresh", "last_refreshed_at", "lastRefreshedAt"}

const (
	anthropicCallbackPort = 54545
	geminiCallbackPort    = 8085
	codexCallbackPort     = 1455
	geminiCLIEndpoint     = "https://cloudcode-pa.googleapis.com"
	geminiCLIVersion      = "v1internal"
)

type callbackForwarder struct {
	provider string
	server   *http.Server
	done     chan struct{}
}

var (
	callbackForwardersMu  sync.Mutex
	callbackForwarders    = make(map[int]*callbackForwarder)
	errAuthFileMustBeJSON = errors.New("auth file must be .json")
	errAuthArchiveNoJSON  = errors.New("archive contains no json auth files")
	errAuthFileNotFound   = errors.New("auth file not found")
)

type authUploadFailure struct {
	Name  string
	Error string
}

type accountPoolArchiveFile struct {
	Name   string
	Data   []byte
	Folder string
}

type accountPoolImportJobStatus string

const (
	accountPoolImportPending accountPoolImportJobStatus = "pending"
	accountPoolImportRunning accountPoolImportJobStatus = "running"
	accountPoolImportDone    accountPoolImportJobStatus = "done"
	accountPoolImportFailed  accountPoolImportJobStatus = "failed"
)

type accountPoolImportJob struct {
	ID        string                     `json:"id"`
	Status    accountPoolImportJobStatus `json:"status"`
	Total     int                        `json:"total"`
	Done      int                        `json:"done"`
	Imported  int                        `json:"imported"`
	Failed    int                        `json:"failed"`
	Skipped   int                        `json:"skipped"`
	Files     []string                   `json:"files"`
	Failures  []authUploadFailure        `json:"failures,omitempty"`
	Error     string                     `json:"error,omitempty"`
	CreatedAt string                     `json:"created_at"`
	UpdatedAt string                     `json:"updated_at"`
}

type accountPoolAuthWriteCandidate struct {
	SourceName string
	TargetName string
	TargetPath string
	Data       []byte
	Auth       *coreauth.Auth
}

type accountPoolPendingUpload struct {
	Name        string
	DisplayName string
	Data        []byte
}

type accountPoolDBEntry struct {
	Name      string `json:"name"`
	Data      string `json:"data"`
	Hash      string `json:"hash"`
	Type      string `json:"type,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Email     string `json:"email,omitempty"`
	Folder    string `json:"folder,omitempty"`
	Size      int    `json:"size"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type accountPoolDBFile struct {
	Version   int                           `json:"version"`
	UpdatedAt string                        `json:"updated_at"`
	Entries   map[string]accountPoolDBEntry `json:"entries"`
}

type accountPoolFolderInfo struct {
	Folder      string `json:"folder"`
	SourceModel string `json:"source_model,omitempty"`
	SourceInfo  string `json:"source_info,omitempty"`
	Count       int    `json:"count,omitempty"`
	Requests    int64  `json:"requests,omitempty"`
	TotalTokens int64  `json:"total_tokens,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
}

type accountPoolRepairStats struct {
	ConvertedSub2 int `json:"converted_sub2"`
	InferredCodex int `json:"inferred_codex"`
	LLMRepaired   int `json:"llm_repaired"`
	LLMFailed     int `json:"llm_failed"`
}

type accountPoolCheckResultPayload struct {
	Status                string   `json:"status"`
	Message               string   `json:"message,omitempty"`
	Plan                  string   `json:"plan,omitempty"`
	QuotaLines            []string `json:"quotaLines,omitempty"`
	QuotaRemainingPercent *float64 `json:"quotaRemainingPercent,omitempty"`
	StatusCode            int      `json:"statusCode,omitempty"`
	CheckedAt             int64    `json:"checkedAt,omitempty"`
}

type accountPoolCheckResultUpdate struct {
	Name        string                        `json:"name"`
	ContentHash string                        `json:"content_hash"`
	Result      accountPoolCheckResultPayload `json:"result"`
}

type accountPoolStoredCheckResult struct {
	Result      string
	ContentHash string
	UpdatedAt   string
}

var accountPoolDBMu sync.Mutex

func extractLastRefreshTimestamp(meta map[string]any) (time.Time, bool) {
	if len(meta) == 0 {
		return time.Time{}, false
	}
	for _, key := range lastRefreshKeys {
		if val, ok := meta[key]; ok {
			if ts, ok1 := parseLastRefreshValue(val); ok1 {
				return ts, true
			}
		}
	}
	return time.Time{}, false
}

func parseLastRefreshValue(v any) (time.Time, bool) {
	switch val := v.(type) {
	case string:
		s := strings.TrimSpace(val)
		if s == "" {
			return time.Time{}, false
		}
		layouts := []string{time.RFC3339, time.RFC3339Nano, "2006-01-02 15:04:05", "2006-01-02T15:04:05Z07:00"}
		for _, layout := range layouts {
			if ts, err := time.Parse(layout, s); err == nil {
				return ts.UTC(), true
			}
		}
		if unix, err := strconv.ParseInt(s, 10, 64); err == nil && unix > 0 {
			return time.Unix(unix, 0).UTC(), true
		}
	case float64:
		if val <= 0 {
			return time.Time{}, false
		}
		return time.Unix(int64(val), 0).UTC(), true
	case int64:
		if val <= 0 {
			return time.Time{}, false
		}
		return time.Unix(val, 0).UTC(), true
	case int:
		if val <= 0 {
			return time.Time{}, false
		}
		return time.Unix(int64(val), 0).UTC(), true
	case json.Number:
		if i, err := val.Int64(); err == nil && i > 0 {
			return time.Unix(i, 0).UTC(), true
		}
	}
	return time.Time{}, false
}

func isWebUIRequest(c *gin.Context) bool {
	raw := strings.TrimSpace(c.Query("is_webui"))
	if raw == "" {
		return false
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func startCallbackForwarder(port int, provider, targetBase string) (*callbackForwarder, error) {
	callbackForwardersMu.Lock()
	prev := callbackForwarders[port]
	if prev != nil {
		delete(callbackForwarders, port)
	}
	callbackForwardersMu.Unlock()

	if prev != nil {
		stopForwarderInstance(port, prev)
	}

	addr := fmt.Sprintf("0.0.0.0:%d", port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("failed to listen on %s: %w", addr, err)
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := targetBase
		if raw := r.URL.RawQuery; raw != "" {
			if strings.Contains(target, "?") {
				target = target + "&" + raw
			} else {
				target = target + "?" + raw
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, target, http.StatusFound)
	})

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      5 * time.Second,
	}
	done := make(chan struct{})

	go func() {
		if errServe := srv.Serve(ln); errServe != nil && !errors.Is(errServe, http.ErrServerClosed) {
			log.WithError(errServe).Warnf("callback forwarder for %s stopped unexpectedly", provider)
		}
		close(done)
	}()

	forwarder := &callbackForwarder{
		provider: provider,
		server:   srv,
		done:     done,
	}

	callbackForwardersMu.Lock()
	callbackForwarders[port] = forwarder
	callbackForwardersMu.Unlock()

	log.Infof("callback forwarder for %s listening on %s", provider, addr)

	return forwarder, nil
}

func stopCallbackForwarderInstance(port int, forwarder *callbackForwarder) {
	if forwarder == nil {
		return
	}
	callbackForwardersMu.Lock()
	if current := callbackForwarders[port]; current == forwarder {
		delete(callbackForwarders, port)
	}
	callbackForwardersMu.Unlock()

	stopForwarderInstance(port, forwarder)
}

func stopForwarderInstance(port int, forwarder *callbackForwarder) {
	if forwarder == nil || forwarder.server == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := forwarder.server.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.WithError(err).Warnf("failed to shut down callback forwarder on port %d", port)
	}

	select {
	case <-forwarder.done:
	case <-time.After(2 * time.Second):
	}

	log.Infof("callback forwarder on port %d stopped", port)
}

func (h *Handler) managementCallbackURL(path string) (string, error) {
	if h == nil || h.cfg == nil || h.cfg.Port <= 0 {
		return "", fmt.Errorf("server port is not configured")
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	scheme := "http"
	if h.cfg.TLS.Enable {
		scheme = "https"
	}
	return fmt.Sprintf("%s://127.0.0.1:%d%s", scheme, h.cfg.Port, path), nil
}

func (h *Handler) ListAuthFiles(c *gin.Context) {
	if h == nil {
		c.JSON(500, gin.H{"error": "handler not initialized"})
		return
	}
	includeHash := strings.EqualFold(strings.TrimSpace(c.Query("include_hash")), "true")
	if h.authManager == nil {
		h.listAuthFilesFromDisk(c, includeHash)
		return
	}
	auths := h.authManager.List()
	files := make([]gin.H, 0, len(auths))
	for _, auth := range auths {
		if entry := h.buildAuthFileEntry(auth, includeHash); entry != nil {
			files = append(files, entry)
		}
	}
	sort.Slice(files, func(i, j int) bool {
		nameI, _ := files[i]["name"].(string)
		nameJ, _ := files[j]["name"].(string)
		return strings.ToLower(nameI) < strings.ToLower(nameJ)
	})
	c.JSON(200, gin.H{"files": files})
}

// GetAuthFileModels returns the models supported by a specific auth file
func (h *Handler) GetAuthFileModels(c *gin.Context) {
	name := c.Query("name")
	if name == "" {
		c.JSON(400, gin.H{"error": "name is required"})
		return
	}

	// Try to find auth ID via authManager
	var authID string
	if h.authManager != nil {
		auths := h.authManager.List()
		for _, auth := range auths {
			if auth.FileName == name || auth.ID == name {
				authID = auth.ID
				break
			}
		}
	}

	if authID == "" {
		authID = name // fallback to filename as ID
	}

	// Get models from registry
	reg := registry.GetGlobalRegistry()
	models := reg.GetModelsForClient(authID)

	result := make([]gin.H, 0, len(models))
	for _, m := range models {
		entry := gin.H{
			"id": m.ID,
		}
		if m.DisplayName != "" {
			entry["display_name"] = m.DisplayName
		}
		if m.Type != "" {
			entry["type"] = m.Type
		}
		if m.OwnedBy != "" {
			entry["owned_by"] = m.OwnedBy
		}
		result = append(result, entry)
	}

	c.JSON(200, gin.H{"models": result})
}

// List auth files from disk when the auth manager is unavailable.
func (h *Handler) listAuthFilesFromDisk(c *gin.Context, includeHash bool) {
	entries, err := os.ReadDir(h.cfg.AuthDir)
	if err != nil {
		c.JSON(500, gin.H{"error": fmt.Sprintf("failed to read auth dir: %v", err)})
		return
	}
	files := make([]gin.H, 0)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		if info, errInfo := e.Info(); errInfo == nil {
			fileData := gin.H{"name": name, "size": info.Size(), "modtime": info.ModTime()}

			// Read file to get type field
			full := filepath.Join(h.cfg.AuthDir, name)
			if data, errRead := os.ReadFile(full); errRead == nil {
				typeValue := gjson.GetBytes(data, "type").String()
				emailValue := gjson.GetBytes(data, "email").String()
				fileData["type"] = typeValue
				fileData["email"] = emailValue
				if includeHash {
					fileData["content_hash"] = hashAccountPoolContent(data)
				}
				if pv := gjson.GetBytes(data, "priority"); pv.Exists() {
					switch pv.Type {
					case gjson.Number:
						fileData["priority"] = int(pv.Int())
					case gjson.String:
						if parsed, errAtoi := strconv.Atoi(strings.TrimSpace(pv.String())); errAtoi == nil {
							fileData["priority"] = parsed
						}
					}
				}
				if nv := gjson.GetBytes(data, "note"); nv.Exists() && nv.Type == gjson.String {
					if trimmed := strings.TrimSpace(nv.String()); trimmed != "" {
						fileData["note"] = trimmed
					}
				}
			}

			files = append(files, fileData)
		}
	}
	c.JSON(200, gin.H{"files": files})
}

func (h *Handler) buildAuthFileEntry(auth *coreauth.Auth, includeHash bool) gin.H {
	if auth == nil {
		return nil
	}
	auth.EnsureIndex()
	runtimeOnly := isRuntimeOnlyAuth(auth)
	if runtimeOnly && (auth.Disabled || auth.Status == coreauth.StatusDisabled) {
		return nil
	}
	path := strings.TrimSpace(authAttribute(auth, "path"))
	if path == "" && !runtimeOnly {
		return nil
	}
	name := strings.TrimSpace(auth.FileName)
	if name == "" {
		name = auth.ID
	}
	entry := gin.H{
		"id":             auth.ID,
		"auth_index":     auth.Index,
		"name":           name,
		"type":           strings.TrimSpace(auth.Provider),
		"provider":       strings.TrimSpace(auth.Provider),
		"label":          auth.Label,
		"status":         auth.Status,
		"status_message": auth.StatusMessage,
		"disabled":       auth.Disabled,
		"unavailable":    auth.Unavailable,
		"runtime_only":   runtimeOnly,
		"source":         "memory",
		"size":           int64(0),
	}
	entry["success"] = auth.Success
	entry["failed"] = auth.Failed
	entry["recent_requests"] = auth.RecentRequestsSnapshot(time.Now())
	if email := authEmail(auth); email != "" {
		entry["email"] = email
	}
	if accountType, account := auth.AccountInfo(); accountType != "" || account != "" {
		if accountType != "" {
			entry["account_type"] = accountType
		}
		if account != "" {
			entry["account"] = account
		}
	}
	if !auth.CreatedAt.IsZero() {
		entry["created_at"] = auth.CreatedAt
	}
	if !auth.UpdatedAt.IsZero() {
		entry["modtime"] = auth.UpdatedAt
		entry["updated_at"] = auth.UpdatedAt
	}
	if !auth.LastRefreshedAt.IsZero() {
		entry["last_refresh"] = auth.LastRefreshedAt
	}
	if !auth.NextRetryAfter.IsZero() {
		entry["next_retry_after"] = auth.NextRetryAfter
	}
	if path != "" {
		entry["path"] = path
		entry["source"] = "file"
		if info, err := os.Stat(path); err == nil {
			entry["size"] = info.Size()
			entry["modtime"] = info.ModTime()
			if includeHash {
				if data, errRead := os.ReadFile(path); errRead == nil {
					entry["content_hash"] = hashAccountPoolContent(data)
				} else {
					log.WithError(errRead).Warnf("failed to hash auth file %s", path)
				}
			}
		} else if os.IsNotExist(err) {
			// Hide credentials removed from disk but still lingering in memory.
			if !runtimeOnly && (auth.Disabled || auth.Status == coreauth.StatusDisabled || strings.EqualFold(strings.TrimSpace(auth.StatusMessage), "removed via management api")) {
				return nil
			}
			entry["source"] = "memory"
		} else {
			log.WithError(err).Warnf("failed to stat auth file %s", path)
		}
	}
	if claims := extractCodexIDTokenClaims(auth); claims != nil {
		entry["id_token"] = claims
	}
	// Expose priority from Attributes (set by synthesizer from JSON "priority" field).
	// Fall back to Metadata for auths registered via UploadAuthFile (no synthesizer).
	if p := strings.TrimSpace(authAttribute(auth, "priority")); p != "" {
		if parsed, err := strconv.Atoi(p); err == nil {
			entry["priority"] = parsed
		}
	} else if auth.Metadata != nil {
		if rawPriority, ok := auth.Metadata["priority"]; ok {
			switch v := rawPriority.(type) {
			case float64:
				entry["priority"] = int(v)
			case int:
				entry["priority"] = v
			case string:
				if parsed, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
					entry["priority"] = parsed
				}
			}
		}
	}
	// Expose note from Attributes (set by synthesizer from JSON "note" field).
	// Fall back to Metadata for auths registered via UploadAuthFile (no synthesizer).
	if note := strings.TrimSpace(authAttribute(auth, "note")); note != "" {
		entry["note"] = note
	} else if auth.Metadata != nil {
		if rawNote, ok := auth.Metadata["note"].(string); ok {
			if trimmed := strings.TrimSpace(rawNote); trimmed != "" {
				entry["note"] = trimmed
			}
		}
	}
	return entry
}

func extractCodexIDTokenClaims(auth *coreauth.Auth) gin.H {
	if auth == nil || auth.Metadata == nil {
		return nil
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return nil
	}
	idTokenRaw, ok := auth.Metadata["id_token"].(string)
	if !ok {
		return nil
	}
	idToken := strings.TrimSpace(idTokenRaw)
	if idToken == "" {
		return nil
	}
	claims, err := codex.ParseJWTToken(idToken)
	if err != nil || claims == nil {
		return nil
	}

	result := gin.H{}
	if v := strings.TrimSpace(claims.CodexAuthInfo.ChatgptAccountID); v != "" {
		result["chatgpt_account_id"] = v
	}
	if v := strings.TrimSpace(claims.CodexAuthInfo.ChatgptPlanType); v != "" {
		result["plan_type"] = v
	}
	if v := claims.CodexAuthInfo.ChatgptSubscriptionActiveStart; v != nil {
		result["chatgpt_subscription_active_start"] = v
	}
	if v := claims.CodexAuthInfo.ChatgptSubscriptionActiveUntil; v != nil {
		result["chatgpt_subscription_active_until"] = v
	}

	if len(result) == 0 {
		return nil
	}
	return result
}

func authEmail(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Metadata != nil {
		if v, ok := auth.Metadata["email"].(string); ok {
			return strings.TrimSpace(v)
		}
	}
	if auth.Attributes != nil {
		if v := strings.TrimSpace(auth.Attributes["email"]); v != "" {
			return v
		}
		if v := strings.TrimSpace(auth.Attributes["account_email"]); v != "" {
			return v
		}
	}
	return ""
}

func authAttribute(auth *coreauth.Auth, key string) string {
	if auth == nil || len(auth.Attributes) == 0 {
		return ""
	}
	return auth.Attributes[key]
}

func isRuntimeOnlyAuth(auth *coreauth.Auth) bool {
	if auth == nil || len(auth.Attributes) == 0 {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Attributes["runtime_only"]), "true")
}

func isUnsafeAuthFileName(name string) bool {
	if strings.TrimSpace(name) == "" {
		return true
	}
	if strings.ContainsAny(name, "/\\") {
		return true
	}
	if filepath.VolumeName(name) != "" {
		return true
	}
	return false
}

func isUnsafeAuthFileDeleteName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return true
	}
	if filepath.VolumeName(name) != "" || filepath.IsAbs(name) {
		return true
	}
	cleaned := filepath.Clean(filepath.FromSlash(name))
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return true
	}
	return false
}

func safeAuthFileDeletePath(authDir string, name string) (string, error) {
	if isUnsafeAuthFileDeleteName(name) {
		return "", fmt.Errorf("invalid name")
	}
	authDir = strings.TrimSpace(authDir)
	if resolved, errResolve := util.ResolveAuthDir(authDir); errResolve == nil && resolved != "" {
		authDir = resolved
	}
	authDir = filepath.Clean(authDir)
	if !filepath.IsAbs(authDir) {
		if abs, errAbs := filepath.Abs(authDir); errAbs == nil {
			authDir = abs
		}
	}
	targetPath := filepath.Join(authDir, filepath.Clean(filepath.FromSlash(strings.TrimSpace(name))))
	targetPath = filepath.Clean(targetPath)
	if rel, errRel := filepath.Rel(authDir, targetPath); errRel != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid name")
	}
	return targetPath, nil
}

// Download single auth file by name
func (h *Handler) DownloadAuthFile(c *gin.Context) {
	name := strings.TrimSpace(c.Query("name"))
	if isUnsafeAuthFileName(name) {
		c.JSON(400, gin.H{"error": "invalid name"})
		return
	}
	if !strings.HasSuffix(strings.ToLower(name), ".json") {
		c.JSON(400, gin.H{"error": "name must end with .json"})
		return
	}
	full := filepath.Join(h.cfg.AuthDir, name)
	data, err := os.ReadFile(full)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(404, gin.H{"error": "file not found"})
		} else {
			c.JSON(500, gin.H{"error": fmt.Sprintf("failed to read file: %v", err)})
		}
		return
	}
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", name))
	c.Data(200, "application/json", data)
}

// Upload auth file: multipart or raw JSON with ?name=
func (h *Handler) UploadAuthFile(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	ctx := c.Request.Context()

	fileHeaders, errMultipart := h.multipartAuthFileHeaders(c)
	if errMultipart != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid multipart form: %v", errMultipart)})
		return
	}
	if len(fileHeaders) > 0 {
		uploaded := make([]string, 0, len(fileHeaders))
		failed := make([]gin.H, 0)
		for _, file := range fileHeaders {
			names, failures, _, errUpload := h.storeUploadedAuthFiles(ctx, file, false)
			if errUpload != nil {
				failed = append(failed, gin.H{"name": uploadedFileDisplayName(file), "error": uploadErrorMessage(errUpload)})
				continue
			}
			uploaded = append(uploaded, names...)
			for _, failure := range failures {
				failed = append(failed, gin.H{"name": failure.Name, "error": failure.Error})
			}
		}
		if len(fileHeaders) == 1 && len(uploaded) == 1 && len(failed) == 0 {
			c.JSON(http.StatusOK, gin.H{"status": "ok"})
			return
		}
		if len(failed) > 0 {
			c.JSON(http.StatusMultiStatus, gin.H{
				"status":   "partial",
				"uploaded": len(uploaded),
				"files":    uploaded,
				"failed":   failed,
			})
			return
		}
		if len(uploaded) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "no json auth files uploaded"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "uploaded": len(uploaded), "files": uploaded})
		return
	}
	if c.ContentType() == "multipart/form-data" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no files uploaded"})
		return
	}
	name := strings.TrimSpace(c.Query("name"))
	if isUnsafeAuthFileName(name) {
		c.JSON(400, gin.H{"error": "invalid name"})
		return
	}
	if !strings.HasSuffix(strings.ToLower(name), ".json") {
		c.JSON(400, gin.H{"error": "name must end with .json"})
		return
	}
	data, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(400, gin.H{"error": "failed to read body"})
		return
	}
	if err = h.writeAuthFileWithArchive(ctx, filepath.Base(name), data, false); err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"status": "ok"})
}

// Delete auth files: single by name or all
func (h *Handler) DeleteAuthFile(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	ctx := c.Request.Context()
	if all := c.Query("all"); all == "true" || all == "1" || all == "*" {
		entries, err := os.ReadDir(h.cfg.AuthDir)
		if err != nil {
			c.JSON(500, gin.H{"error": fmt.Sprintf("failed to read auth dir: %v", err)})
			return
		}
		deleted := 0
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if !strings.HasSuffix(strings.ToLower(name), ".json") {
				continue
			}
			full := filepath.Join(h.cfg.AuthDir, name)
			if !filepath.IsAbs(full) {
				if abs, errAbs := filepath.Abs(full); errAbs == nil {
					full = abs
				}
			}
			if err = os.Remove(full); err == nil {
				if errDel := h.deleteTokenRecord(ctx, full); errDel != nil {
					c.JSON(500, gin.H{"error": errDel.Error()})
					return
				}
				deleted++
				h.removeAuthRuntime(full)
			}
		}
		c.JSON(200, gin.H{"status": "ok", "deleted": deleted})
		return
	}

	names, errNames := requestedAuthFileNamesForDelete(c)
	if errNames != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errNames.Error()})
		return
	}
	if len(names) == 0 {
		c.JSON(400, gin.H{"error": "invalid name"})
		return
	}
	if len(names) == 1 {
		if _, status, errDelete := h.deleteAuthFileByName(ctx, names[0]); errDelete != nil {
			c.JSON(status, gin.H{"error": errDelete.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
		return
	}

	deletedFiles := make([]string, 0, len(names))
	failed := make([]gin.H, 0)
	for _, name := range names {
		deletedName, _, errDelete := h.deleteAuthFileByName(ctx, name)
		if errDelete != nil {
			failed = append(failed, gin.H{"name": name, "error": errDelete.Error()})
			continue
		}
		deletedFiles = append(deletedFiles, deletedName)
	}
	if len(failed) > 0 {
		c.JSON(http.StatusMultiStatus, gin.H{
			"status":  "partial",
			"deleted": len(deletedFiles),
			"files":   deletedFiles,
			"failed":  failed,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "deleted": len(deletedFiles), "files": deletedFiles})
}

func (h *Handler) multipartAuthFileHeaders(c *gin.Context) ([]*multipart.FileHeader, error) {
	if h == nil || c == nil || c.ContentType() != "multipart/form-data" {
		return nil, nil
	}
	form, err := c.MultipartForm()
	if err != nil {
		return nil, err
	}
	if form == nil || len(form.File) == 0 {
		return nil, nil
	}

	keys := make([]string, 0, len(form.File))
	for key := range form.File {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	headers := make([]*multipart.FileHeader, 0)
	for _, key := range keys {
		headers = append(headers, form.File[key]...)
	}
	return headers, nil
}

func (h *Handler) storeUploadedAuthFiles(ctx context.Context, file *multipart.FileHeader, updateArchive bool) ([]string, []authUploadFailure, []accountPoolArchiveFile, error) {
	if file == nil {
		return nil, nil, nil, fmt.Errorf("no file uploaded")
	}
	name := filepath.Base(strings.TrimSpace(file.Filename))
	lowerName := strings.ToLower(name)
	if !strings.HasSuffix(lowerName, ".json") && !strings.HasSuffix(lowerName, ".zip") {
		return nil, nil, nil, errAuthFileMustBeJSON
	}
	src, err := file.Open()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to open uploaded file: %w", err)
	}
	defer src.Close()

	data, err := io.ReadAll(src)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to read uploaded file: %w", err)
	}
	if strings.HasSuffix(lowerName, ".zip") {
		return h.storeUploadedAuthZip(ctx, data, updateArchive)
	}
	if err := h.writeAuthFileWithArchive(ctx, name, data, updateArchive); err != nil {
		return nil, nil, nil, err
	}
	return []string{name}, nil, []accountPoolArchiveFile{{Name: name, Data: data}}, nil
}

func (h *Handler) readUploadedAccountPoolFiles(file *multipart.FileHeader) ([]string, []authUploadFailure, []accountPoolArchiveFile, error) {
	if file == nil {
		return nil, nil, nil, fmt.Errorf("no file uploaded")
	}
	src, err := file.Open()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to open uploaded file: %w", err)
	}
	defer src.Close()
	data, err := io.ReadAll(src)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to read uploaded file: %w", err)
	}
	return h.readAccountPoolUploadData(file.Filename, data)
}

func (h *Handler) readAccountPoolUploadData(fileName string, data []byte) ([]string, []authUploadFailure, []accountPoolArchiveFile, error) {
	rawName := normalizeUploadPath(fileName)
	name := normalizeAccountPoolEntryName(rawName)
	lowerName := strings.ToLower(name)
	if !isAccountPoolUploadName(lowerName) {
		return nil, nil, nil, errAuthFileMustBeJSON
	}
	folder := accountPoolFolderFromUploadPath(rawName)
	if isArchiveUploadName(lowerName) {
		return readAccountPoolFilesFromArchive(data, filepath.Base(name), folder)
	}
	if uploaded, failures, files, ok := readAccountPoolFilesFromSub2JSON(data, folder); ok {
		return uploaded, failures, files, nil
	}
	if _, err := h.buildAuthFromFileData(name, data); err != nil {
		return nil, nil, nil, err
	}
	entryName := accountPoolEntryNameForFolder(folder, filepath.Base(name))
	return []string{entryName}, nil, []accountPoolArchiveFile{{Name: entryName, Data: data, Folder: folder}}, nil
}

func isAccountPoolUploadName(name string) bool {
	return strings.HasSuffix(name, ".json") || isArchiveUploadName(name)
}

func isArchiveUploadName(name string) bool {
	return strings.HasSuffix(name, ".zip") ||
		strings.HasSuffix(name, ".tar") ||
		strings.HasSuffix(name, ".tar.gz") ||
		strings.HasSuffix(name, ".tgz") ||
		strings.HasSuffix(name, ".gz")
}

func normalizeUploadPath(name string) string {
	name = strings.TrimSpace(strings.ReplaceAll(name, "\\", "/"))
	name = path.Clean("/" + name)
	name = strings.TrimPrefix(name, "/")
	if name == "." || name == "" {
		return ""
	}
	return name
}

func normalizeAccountPoolEntryName(name string) string {
	name = normalizeUploadPath(name)
	if name == "" || strings.Contains(name, ":") {
		return ""
	}
	parts := strings.Split(name, "/")
	cleanParts := parts[:0]
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || part == "." || part == ".." {
			return ""
		}
		cleanParts = append(cleanParts, part)
	}
	return strings.Join(cleanParts, "/")
}

func isUnsafeAccountPoolEntryName(name string) bool {
	name = strings.TrimSpace(strings.ReplaceAll(name, "\\", "/"))
	normalized := normalizeAccountPoolEntryName(name)
	if normalized == "" || normalized != path.Clean(name) {
		return true
	}
	if filepath.VolumeName(name) != "" {
		return true
	}
	return false
}

func accountPoolArchiveEntryName(name string) string {
	return normalizeAccountPoolEntryName(name)
}

func accountPoolEntryNameForFolder(folder string, name string) string {
	name = normalizeAccountPoolEntryName(name)
	if name == "" {
		return ""
	}
	folder = normalizeAccountPoolFolder(folder)
	if folder == "" || folder == defaultAccountPoolFolder() || strings.HasPrefix(name, folder+"/") {
		return name
	}
	return normalizeAccountPoolEntryName(path.Join(folder, name))
}

func readAccountPoolFilesFromArchive(data []byte, name string, folder string) ([]string, []authUploadFailure, []accountPoolArchiveFile, error) {
	lowerName := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lowerName, ".zip"):
		return readAccountPoolFilesFromZip(data, folder)
	case strings.HasSuffix(lowerName, ".tar"):
		return readAccountPoolFilesFromTar(data, folder)
	case strings.HasSuffix(lowerName, ".tar.gz") || strings.HasSuffix(lowerName, ".tgz"):
		return readAccountPoolFilesFromGzipTar(data, folder)
	case strings.HasSuffix(lowerName, ".gz"):
		return readAccountPoolFileFromGzip(data, name, folder)
	default:
		return nil, nil, nil, errAuthFileMustBeJSON
	}
}

func readAccountPoolFilesFromZip(data []byte, folder string) ([]string, []authUploadFailure, []accountPoolArchiveFile, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("invalid zip archive: %w", err)
	}
	folder = normalizeAccountPoolFolder(folder)
	uploaded := make([]string, 0, len(reader.File))
	failures := make([]authUploadFailure, 0)
	archiveFiles := make([]accountPoolArchiveFile, 0, len(reader.File))
	latestIndexByName := make(map[string]int)
	for index, entry := range reader.File {
		if entry == nil || entry.FileInfo().IsDir() {
			continue
		}
		name := accountPoolArchiveEntryName(entry.Name)
		if !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		if isUnsafeAccountPoolEntryName(name) {
			failures = append(failures, authUploadFailure{Name: entry.Name, Error: "invalid name"})
			continue
		}
		latestIndexByName[name] = index
	}
	for index, entry := range reader.File {
		if entry == nil || entry.FileInfo().IsDir() {
			continue
		}
		name := accountPoolArchiveEntryName(entry.Name)
		if latestIndexByName[name] != index {
			continue
		}
		if !strings.HasSuffix(strings.ToLower(name), ".json") || isUnsafeAccountPoolEntryName(name) {
			continue
		}
		rc, errOpen := entry.Open()
		if errOpen != nil {
			failures = append(failures, authUploadFailure{Name: name, Error: fmt.Sprintf("failed to open zip entry: %v", errOpen)})
			continue
		}
		entryData, errRead := io.ReadAll(rc)
		errClose := rc.Close()
		if errRead != nil {
			failures = append(failures, authUploadFailure{Name: name, Error: fmt.Sprintf("failed to read zip entry: %v", errRead)})
			continue
		}
		if errClose != nil {
			failures = append(failures, authUploadFailure{Name: name, Error: fmt.Sprintf("failed to close zip entry: %v", errClose)})
			continue
		}
		if len(bytes.TrimSpace(entryData)) == 0 {
			continue
		}
		if !json.Valid(entryData) {
			failures = append(failures, authUploadFailure{Name: name, Error: "invalid auth file"})
			continue
		}
		if sub2Uploaded, sub2Failures, sub2Files, ok := readAccountPoolFilesFromSub2JSON(entryData, folder); ok {
			uploaded = append(uploaded, sub2Uploaded...)
			failures = append(failures, sub2Failures...)
			archiveFiles = append(archiveFiles, sub2Files...)
			continue
		}
		entryName := accountPoolEntryNameForFolder(folder, name)
		uploaded = append(uploaded, entryName)
		archiveFiles = append(archiveFiles, accountPoolArchiveFile{Name: entryName, Data: entryData, Folder: folder})
	}
	if len(uploaded) == 0 && len(failures) == 0 {
		return nil, nil, nil, errAuthArchiveNoJSON
	}
	return uploaded, failures, archiveFiles, nil
}

func readAccountPoolFilesFromGzipTar(data []byte, folder string) ([]string, []authUploadFailure, []accountPoolArchiveFile, error) {
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("invalid gzip archive: %w", err)
	}
	defer reader.Close()
	unzipped, err := io.ReadAll(reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to read gzip archive: %w", err)
	}
	return readAccountPoolFilesFromTar(unzipped, folder)
}

func readAccountPoolFileFromGzip(data []byte, uploadName string, folder string) ([]string, []authUploadFailure, []accountPoolArchiveFile, error) {
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("invalid gzip file: %w", err)
	}
	defer reader.Close()
	entryData, err := io.ReadAll(reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to read gzip file: %w", err)
	}
	name := strings.TrimSuffix(filepath.Base(uploadName), filepath.Ext(uploadName))
	if !strings.HasSuffix(strings.ToLower(name), ".json") {
		name += ".json"
	}
	return buildAccountPoolArchiveFilesFromRaw(map[string][]byte{name: entryData}, folder)
}

func readAccountPoolFilesFromTar(data []byte, folder string) ([]string, []authUploadFailure, []accountPoolArchiveFile, error) {
	reader := tar.NewReader(bytes.NewReader(data))
	files := make(map[string][]byte)
	failures := make([]authUploadFailure, 0)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, nil, fmt.Errorf("invalid tar archive: %w", err)
		}
		if header == nil || header.FileInfo().IsDir() {
			continue
		}
		name := accountPoolArchiveEntryName(header.Name)
		if !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		if isUnsafeAccountPoolEntryName(name) {
			failures = append(failures, authUploadFailure{Name: header.Name, Error: "invalid name"})
			continue
		}
		entryData, errRead := io.ReadAll(reader)
		if errRead != nil {
			failures = append(failures, authUploadFailure{Name: name, Error: fmt.Sprintf("failed to read tar entry: %v", errRead)})
			continue
		}
		files[name] = entryData
	}
	uploaded, moreFailures, archiveFiles, err := buildAccountPoolArchiveFilesFromRaw(files, folder)
	failures = append(failures, moreFailures...)
	return uploaded, failures, archiveFiles, err
}

func buildAccountPoolArchiveFilesFromRaw(files map[string][]byte, folder string) ([]string, []authUploadFailure, []accountPoolArchiveFile, error) {
	folder = normalizeAccountPoolFolder(folder)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	uploaded := make([]string, 0, len(names))
	failures := make([]authUploadFailure, 0)
	archiveFiles := make([]accountPoolArchiveFile, 0, len(names))
	for _, name := range names {
		entryData := files[name]
		if len(bytes.TrimSpace(entryData)) == 0 {
			continue
		}
		if !json.Valid(entryData) {
			failures = append(failures, authUploadFailure{Name: name, Error: "invalid auth file"})
			continue
		}
		if sub2Uploaded, sub2Failures, sub2Files, ok := readAccountPoolFilesFromSub2JSON(entryData, folder); ok {
			uploaded = append(uploaded, sub2Uploaded...)
			failures = append(failures, sub2Failures...)
			archiveFiles = append(archiveFiles, sub2Files...)
			continue
		}
		entryName := accountPoolEntryNameForFolder(folder, name)
		uploaded = append(uploaded, entryName)
		archiveFiles = append(archiveFiles, accountPoolArchiveFile{Name: entryName, Data: entryData, Folder: folder})
	}
	if len(uploaded) == 0 && len(failures) == 0 {
		return nil, nil, nil, errAuthArchiveNoJSON
	}
	return uploaded, failures, archiveFiles, nil
}

func readAccountPoolFilesFromSub2JSON(data []byte, folder string) ([]string, []authUploadFailure, []accountPoolArchiveFile, bool) {
	docs, err := parseSub2APIDocuments(string(data))
	if err != nil {
		return nil, nil, nil, false
	}
	accounts := flattenSub2APIAccounts(docs)
	if len(accounts) == 0 {
		return nil, nil, nil, false
	}
	folder = normalizeAccountPoolFolder(folder)
	stamp := time.Now().Unix()
	uploaded := make([]string, 0, len(accounts))
	failures := make([]authUploadFailure, 0)
	archiveFiles := make([]accountPoolArchiveFile, 0, len(accounts))
	for index, item := range accounts {
		file, warnings, errBuild := sub2APIAccountToAccountPoolArchiveFile(item.account, item.exportedAt, folder, index, stamp)
		if errBuild != nil {
			failures = append(failures, authUploadFailure{Name: fmt.Sprintf("account_%d", index+1), Error: errBuild.Error()})
			continue
		}
		for _, warning := range warnings {
			if strings.TrimSpace(warning) != "" {
				log.WithField("folder", folder).Warn(warning)
			}
		}
		uploaded = append(uploaded, file.Name)
		archiveFiles = append(archiveFiles, file)
	}
	return uploaded, failures, archiveFiles, true
}

func convertAccountPoolSub2Entries(entries map[string][]byte) (map[string][]byte, bool) {
	converted, changed, _ := convertAccountPoolSub2EntriesWithStats(entries)
	return converted, changed
}

func convertAccountPoolSub2EntriesWithStats(entries map[string][]byte) (map[string][]byte, bool, int) {
	if len(entries) == 0 {
		return entries, false, 0
	}
	var converted map[string][]byte
	changed := false
	convertedCount := 0
	for name, data := range entries {
		name = normalizeAccountPoolEntryName(name)
		if name == "" || isUnsafeAccountPoolEntryName(name) || !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		folder := accountPoolFolderFromEntryName(name)
		_, _, files, ok := readAccountPoolFilesFromSub2JSON(data, folder)
		if !ok || len(files) == 0 {
			continue
		}
		if converted == nil {
			converted = make(map[string][]byte, len(entries)+len(files))
			for entryName, entryData := range entries {
				converted[entryName] = entryData
			}
		}
		delete(converted, name)
		for _, file := range files {
			entryName := normalizeAccountPoolEntryName(file.Name)
			if entryName == "" || isUnsafeAccountPoolEntryName(entryName) || !strings.HasSuffix(strings.ToLower(entryName), ".json") {
				continue
			}
			removeDuplicateAccountPoolEntries(converted, entryName, file.Data)
			converted[entryName] = file.Data
			convertedCount++
		}
		changed = true
	}
	if !changed {
		return entries, false, 0
	}
	return dedupeAccountPoolEntries(converted), true, convertedCount
}

func repairAccountPoolUnsupportedEntries(entries map[string][]byte) (map[string][]byte, bool, accountPoolRepairStats) {
	converted, changedSub2, convertedSub2 := convertAccountPoolSub2EntriesWithStats(entries)
	stats := accountPoolRepairStats{ConvertedSub2: convertedSub2}
	repaired := changedSub2
	if converted == nil {
		converted = entries
	}
	next := make(map[string][]byte, len(converted))
	for name, data := range converted {
		name = normalizeAccountPoolEntryName(name)
		if name == "" || isUnsafeAccountPoolEntryName(name) || !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		repairedData, ok := inferAccountPoolCodexType(data)
		if ok {
			data = repairedData
			stats.InferredCodex++
			repaired = true
		}
		next[name] = data
	}
	if !repaired {
		return entries, false, stats
	}
	return dedupeAccountPoolEntries(next), true, stats
}

func inferAccountPoolCodexType(data []byte) ([]byte, bool) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || strings.TrimSpace(gjson.GetBytes(trimmed, "type").String()) != "" {
		return data, false
	}
	if strings.TrimSpace(gjson.GetBytes(trimmed, "refresh_token").String()) == "" &&
		strings.TrimSpace(gjson.GetBytes(trimmed, "access_token").String()) == "" &&
		strings.TrimSpace(gjson.GetBytes(trimmed, "id_token").String()) == "" {
		return data, false
	}
	var parsed map[string]any
	if err := json.Unmarshal(trimmed, &parsed); err != nil {
		return data, false
	}
	parsed["type"] = "codex"
	repaired, err := json.MarshalIndent(parsed, "", "  ")
	if err != nil {
		return data, false
	}
	return repaired, true
}

func defaultAccountPoolFolder() string {
	return "默认文件夹"
}

func accountPoolFolderFromUploadName(name string) string {
	base := filepath.Base(strings.TrimSpace(name))
	ext := filepath.Ext(base)
	if ext != "" {
		base = strings.TrimSuffix(base, ext)
	}
	return normalizeAccountPoolFolder(base)
}

func accountPoolFolderFromUploadPath(name string) string {
	name = normalizeUploadPath(name)
	if strings.Contains(name, "/") {
		return normalizeAccountPoolFolder(strings.Split(name, "/")[0])
	}
	lowerName := strings.ToLower(name)
	if isArchiveUploadName(lowerName) {
		return accountPoolFolderFromUploadName(name)
	}
	return defaultAccountPoolFolder()
}

func accountPoolFolderFromEntryName(name string) string {
	name = normalizeAccountPoolEntryName(name)
	if strings.Contains(name, "/") {
		return normalizeAccountPoolFolder(strings.Split(name, "/")[0])
	}
	return defaultAccountPoolFolder()
}

func accountPoolFolderFromStoredEntry(name string, folder string) string {
	folder = normalizeAccountPoolFolder(folder)
	name = normalizeAccountPoolEntryName(name)
	if strings.Contains(name, "/") {
		return normalizeAccountPoolFolder(strings.Split(name, "/")[0])
	}
	return defaultAccountPoolFolder()
}

var (
	accountPoolEmailDomainPattern = regexp.MustCompile(`(?i)@([a-z0-9.-]+\.[a-z]{2,})`)
	accountPoolDomainPattern      = regexp.MustCompile(`(?i)([a-z0-9-]+(?:\.[a-z0-9-]+)+)(?:_[0-9]+(?:_[0-9]+)?)?$`)
)

func inferAccountPoolFolderFromName(name string) string {
	name = normalizeAccountPoolEntryName(name)
	if name == "" {
		return ""
	}
	if strings.Contains(name, "/") {
		return normalizeAccountPoolFolder(strings.Split(name, "/")[0])
	}
	return defaultAccountPoolFolder()
}

func inferLegacyAccountPoolFolderFromFlatName(name string) string {
	name = normalizeAccountPoolEntryName(name)
	if name == "" || strings.Contains(name, "/") {
		return ""
	}
	base := strings.TrimSuffix(filepath.Base(name), filepath.Ext(name))
	base = strings.TrimSpace(base)
	if base == "" {
		return ""
	}
	if matches := accountPoolEmailDomainPattern.FindStringSubmatch(base); len(matches) > 1 {
		return normalizeAccountPoolFolder(matches[1])
	}
	if matches := accountPoolDomainPattern.FindStringSubmatch(base); len(matches) > 1 {
		return normalizeAccountPoolFolder(matches[1])
	}
	if prefix := strings.TrimSpace(strings.Split(base, "_")[0]); prefix != "" {
		return normalizeAccountPoolFolder(prefix)
	}
	return normalizeAccountPoolFolder(base)
}

func inferAccountPoolFolder(name string, data []byte) string {
	normalizedName := normalizeAccountPoolEntryName(name)
	if normalizedName != "" && !strings.Contains(normalizedName, "/") {
		return defaultAccountPoolFolder()
	}
	if folder := inferAccountPoolFolderFromName(normalizedName); folder != "" && folder != defaultAccountPoolFolder() {
		return folder
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) > 0 {
		if typeValue := strings.TrimSpace(gjson.GetBytes(trimmed, "type").String()); typeValue != "" {
			return normalizeAccountPoolFolder(typeValue)
		}
		if providerValue := strings.TrimSpace(gjson.GetBytes(trimmed, "provider").String()); providerValue != "" {
			return normalizeAccountPoolFolder(providerValue)
		}
	}
	return inferAccountPoolFolderFromName(name)
}

func normalizeAccountPoolFolder(folder string) string {
	folder = strings.TrimSpace(folder)
	if folder == "" || folder == "直接上传" {
		return defaultAccountPoolFolder()
	}
	folder = filepath.Base(folder)
	folder = strings.TrimSpace(folder)
	if folder == "" || folder == "." {
		return defaultAccountPoolFolder()
	}
	runes := []rune(folder)
	if len(runes) > 120 {
		folder = string(runes[:120])
	}
	return folder
}

func (h *Handler) storeUploadedAuthZip(ctx context.Context, data []byte, updateArchive bool) ([]string, []authUploadFailure, []accountPoolArchiveFile, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("invalid zip archive: %w", err)
	}

	type zipAuthJob struct {
		index int
		name  string
		entry *zip.File
	}
	type zipAuthResult struct {
		index       int
		name        string
		uploaded    bool
		archiveFile accountPoolArchiveFile
		failure     *authUploadFailure
	}

	jobs := make([]zipAuthJob, 0, len(reader.File))
	failures := make([]authUploadFailure, 0)
	latestIndexByName := make(map[string]int)
	for index, entry := range reader.File {
		if entry == nil || entry.FileInfo().IsDir() {
			continue
		}
		name := authArchiveEntryFileName(entry.Name)
		if !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		if isUnsafeAuthFileName(name) {
			failures = append(failures, authUploadFailure{Name: entry.Name, Error: "invalid name"})
			continue
		}
		jobs = append(jobs, zipAuthJob{index: index, name: name, entry: entry})
		latestIndexByName[name] = index
	}
	if len(jobs) == 0 && len(failures) == 0 {
		return nil, nil, nil, errAuthArchiveNoJSON
	}

	jobCh := make(chan zipAuthJob)
	resultCh := make(chan zipAuthResult, len(jobs))
	workerCount := authZipLoadConcurrency(len(jobs))
	var wg sync.WaitGroup
	wg.Add(workerCount)
	for range workerCount {
		go func() {
			defer wg.Done()
			for job := range jobCh {
				if latestIndexByName[job.name] != job.index {
					continue
				}
				result := zipAuthResult{index: job.index, name: job.name}
				rc, errOpen := job.entry.Open()
				if errOpen != nil {
					result.failure = &authUploadFailure{Name: job.name, Error: fmt.Sprintf("failed to open zip entry: %v", errOpen)}
					resultCh <- result
					continue
				}
				entryData, errRead := io.ReadAll(rc)
				errClose := rc.Close()
				if errRead != nil {
					result.failure = &authUploadFailure{Name: job.name, Error: fmt.Sprintf("failed to read zip entry: %v", errRead)}
					resultCh <- result
					continue
				}
				if errClose != nil {
					result.failure = &authUploadFailure{Name: job.name, Error: fmt.Sprintf("failed to close zip entry: %v", errClose)}
					resultCh <- result
					continue
				}
				if errWrite := h.writeAuthFileWithArchive(ctx, job.name, entryData, updateArchive); errWrite != nil {
					result.failure = &authUploadFailure{Name: job.name, Error: errWrite.Error()}
					resultCh <- result
					continue
				}
				result.uploaded = true
				result.archiveFile = accountPoolArchiveFile{Name: job.name, Data: entryData}
				resultCh <- result
			}
		}()
	}
	go func() {
		for _, job := range jobs {
			jobCh <- job
		}
		close(jobCh)
		wg.Wait()
		close(resultCh)
	}()

	results := make([]zipAuthResult, 0, len(jobs))
	for result := range resultCh {
		results = append(results, result)
	}
	sort.Slice(results, func(i, j int) bool {
		return results[i].index < results[j].index
	})

	uploaded := make([]string, 0, len(results))
	archiveFiles := make([]accountPoolArchiveFile, 0, len(results))
	for _, result := range results {
		if result.failure != nil {
			failures = append(failures, *result.failure)
			continue
		}
		if !result.uploaded {
			continue
		}
		uploaded = append(uploaded, result.name)
		archiveFiles = append(archiveFiles, result.archiveFile)
	}
	if len(uploaded) == 0 && len(failures) == 0 {
		return nil, nil, nil, errAuthArchiveNoJSON
	}
	return uploaded, failures, archiveFiles, nil
}

func authZipLoadConcurrency(count int) int {
	if count <= 1 {
		return 1
	}
	limit := runtime.GOMAXPROCS(0) * 2
	if limit < 4 {
		limit = 4
	}
	if limit > 16 {
		limit = 16
	}
	if count < limit {
		return count
	}
	return limit
}

func authArchiveEntryFileName(name string) string {
	name = strings.TrimSpace(strings.ReplaceAll(name, "\\", "/"))
	return path.Base(name)
}

func uploadedFileDisplayName(file *multipart.FileHeader) string {
	if file == nil {
		return ""
	}
	return filepath.Base(strings.TrimSpace(file.Filename))
}

func readAccountPoolPendingUploads(fileHeaders []*multipart.FileHeader) ([]accountPoolPendingUpload, error) {
	uploads := make([]accountPoolPendingUpload, 0, len(fileHeaders))
	for _, file := range fileHeaders {
		if file == nil {
			continue
		}
		src, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("failed to open uploaded file %s: %w", uploadedFileDisplayName(file), err)
		}
		data, errRead := io.ReadAll(src)
		errClose := src.Close()
		if errRead != nil {
			return nil, fmt.Errorf("failed to read uploaded file %s: %w", uploadedFileDisplayName(file), errRead)
		}
		if errClose != nil {
			return nil, fmt.Errorf("failed to close uploaded file %s: %w", uploadedFileDisplayName(file), errClose)
		}
		uploads = append(uploads, accountPoolPendingUpload{
			Name:        file.Filename,
			DisplayName: uploadedFileDisplayName(file),
			Data:        data,
		})
	}
	return uploads, nil
}

func uploadErrorMessage(err error) string {
	if errors.Is(err, errAuthFileMustBeJSON) {
		return "file must be .json or .zip"
	}
	if errors.Is(err, errAuthArchiveNoJSON) {
		return "archive contains no .json files"
	}
	return err.Error()
}

func (h *Handler) writeAuthFile(ctx context.Context, name string, data []byte) error {
	return h.writeAuthFileWithArchive(ctx, name, data, false)
}

func (h *Handler) writeAuthFileWithArchive(ctx context.Context, name string, data []byte, updateArchive bool) error {
	dst := filepath.Join(h.cfg.AuthDir, filepath.Base(name))
	if !filepath.IsAbs(dst) {
		if abs, errAbs := filepath.Abs(dst); errAbs == nil {
			dst = abs
		}
	}
	auth, err := h.buildAuthFromFileData(dst, data)
	if err != nil {
		return err
	}
	if errWrite := os.WriteFile(dst, data, 0o600); errWrite != nil {
		return fmt.Errorf("failed to write file: %w", errWrite)
	}
	if err := h.upsertAuthRecord(ctx, auth); err != nil {
		return err
	}
	if updateArchive {
		if err := h.upsertAccountPoolArchiveFile(filepath.Base(name), data); err != nil {
			log.WithError(err).Warnf("failed to update account pool archive for %s", filepath.Base(name))
		}
	}
	return nil
}

func (h *Handler) upsertAccountPoolArchiveFiles(files []accountPoolArchiveFile) error {
	if h == nil || h.cfg == nil {
		return fmt.Errorf("handler not initialized")
	}
	if len(files) == 0 {
		return nil
	}
	if _, err := os.Stat(h.accountPoolSQLitePath()); err == nil {
		accountPoolDBMu.Lock()
		defer accountPoolDBMu.Unlock()
		return h.upsertAccountPoolSQLiteFilesLocked(files)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("failed to stat account pool sqlite database: %w", err)
	}
	entries, err := h.readAccountPoolArchive()
	if err != nil {
		return err
	}
	for _, file := range files {
		name := normalizeAccountPoolEntryName(file.Name)
		if isUnsafeAccountPoolEntryName(name) || !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		if len(bytes.TrimSpace(file.Data)) == 0 {
			continue
		}
		removeDuplicateAccountPoolEntries(entries, name, file.Data)
		entries[name] = file.Data
	}
	return h.writeAccountPoolArchive(dedupeAccountPoolEntries(entries))
}

func (h *Handler) accountPoolArchivePath() string {
	return filepath.Join(h.cfg.AuthDir, "account-pool.zip")
}

func (h *Handler) accountPoolDatabasePath() string {
	return filepath.Join(h.cfg.AuthDir, "account-pool.db.json")
}

func (h *Handler) accountPoolSQLitePath() string {
	return filepath.Join(h.cfg.AuthDir, "account-pool.sqlite")
}

func (h *Handler) upsertAccountPoolArchiveFile(name string, data []byte) error {
	return h.upsertAccountPoolArchiveFiles([]accountPoolArchiveFile{{Name: name, Data: data}})
}

func (h *Handler) readAccountPoolArchive() (map[string][]byte, error) {
	accountPoolDBMu.Lock()
	defer accountPoolDBMu.Unlock()
	return h.readAccountPoolDatabaseLocked()
}

func (h *Handler) readAccountPoolArchiveEntry(name string) ([]byte, error) {
	name = normalizeAccountPoolEntryName(name)
	if isUnsafeAccountPoolEntryName(name) || !strings.HasSuffix(strings.ToLower(name), ".json") {
		return nil, fmt.Errorf("invalid account pool entry name")
	}
	if h == nil || h.cfg == nil {
		return nil, fmt.Errorf("handler not initialized")
	}
	if _, err := os.Stat(h.accountPoolSQLitePath()); err == nil {
		return h.readAccountPoolSQLiteEntryLocked(name)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to stat account pool sqlite database: %w", err)
	}

	accountPoolDBMu.Lock()
	defer accountPoolDBMu.Unlock()
	entries, err := h.readAccountPoolDatabaseLocked()
	if err != nil {
		return nil, err
	}
	data := bytes.TrimSpace(entries[name])
	if len(data) == 0 {
		return nil, nil
	}
	return append([]byte(nil), data...), nil
}

func (h *Handler) readAccountPoolDatabaseLocked() (map[string][]byte, error) {
	if h == nil || h.cfg == nil {
		return nil, fmt.Errorf("handler not initialized")
	}
	if err := os.MkdirAll(h.cfg.AuthDir, 0o700); err != nil {
		return nil, fmt.Errorf("failed to create auth dir: %w", err)
	}
	if _, err := os.Stat(h.accountPoolSQLitePath()); err == nil {
		if errEnsure := h.ensureAccountPoolSQLiteSub2Converted(); errEnsure != nil {
			return nil, errEnsure
		}
		entries, errRead := h.readAccountPoolSQLiteLocked()
		if errRead != nil {
			return nil, errRead
		}
		return h.migrateAccountPoolSub2EntriesLocked(entries)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("failed to stat account pool sqlite database: %w", err)
	}

	entries, errRead := h.readAccountPoolJSONDatabase()
	if errRead != nil {
		return nil, errRead
	}
	if len(entries) == 0 {
		entries, errRead = h.readLegacyAccountPoolZip()
		if errRead != nil {
			return nil, errRead
		}
	}
	entries, _ = convertAccountPoolSub2Entries(entries)
	if len(entries) > 0 {
		if errWrite := h.writeAccountPoolSQLiteLocked(entries); errWrite != nil {
			return nil, errWrite
		}
	}
	return entries, nil
}

func (h *Handler) migrateAccountPoolSub2EntriesLocked(entries map[string][]byte) (map[string][]byte, error) {
	converted, changed := convertAccountPoolSub2Entries(entries)
	if !changed {
		return entries, nil
	}
	if err := h.writeAccountPoolSQLiteLocked(converted); err != nil {
		return nil, fmt.Errorf("failed to migrate sub2api account pool entries: %w", err)
	}
	return converted, nil
}

func (h *Handler) ensureAccountPoolSQLiteSub2Converted() error {
	db, err := h.openAccountPoolSQLiteLocked()
	if err != nil {
		return err
	}
	defer func() {
		if errClose := db.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close account pool sqlite database")
		}
	}()

	var candidateCount int
	if err = db.QueryRow(`
SELECT COUNT(*)
FROM account_pool_entries
WHERE COALESCE(type, '') = ''
  AND COALESCE(provider, '') = ''
  AND data LIKE '%"accounts"%'
`).Scan(&candidateCount); err != nil {
		return fmt.Errorf("failed to inspect account pool sqlite sub2 rows: %w", err)
	}
	if candidateCount == 0 {
		return nil
	}
	entries, errRead := h.readAccountPoolSQLiteLocked()
	if errRead != nil {
		return errRead
	}
	converted, changed, _ := repairAccountPoolUnsupportedEntries(entries)
	if !changed {
		return nil
	}
	if errWrite := h.writeAccountPoolSQLiteLocked(converted); errWrite != nil {
		return fmt.Errorf("failed to persist converted sub2 account pool entries: %w", errWrite)
	}
	return nil
}

func (h *Handler) ensureAccountPoolSQLiteFoldersInferred() error {
	accountPoolDBMu.Lock()
	defer accountPoolDBMu.Unlock()

	db, err := h.openAccountPoolSQLiteLocked()
	if err != nil {
		return err
	}
	defer func() {
		if errClose := db.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close account pool sqlite database")
		}
	}()

	rows, err := db.Query(`SELECT name, data, COALESCE(folder, '') FROM account_pool_entries`)
	if err != nil {
		return fmt.Errorf("failed to query account pool folder inference candidates: %w", err)
	}
	type folderUpdate struct {
		name   string
		folder string
	}
	updates := make([]folderUpdate, 0)
	for rows.Next() {
		var name, data, folder string
		if errScan := rows.Scan(&name, &data, &folder); errScan != nil {
			_ = rows.Close()
			return fmt.Errorf("failed to scan account pool folder inference candidate: %w", errScan)
		}
		currentFolder := normalizeAccountPoolFolder(folder)
		normalizedName := normalizeAccountPoolEntryName(name)
		if accountPoolEntryNameHasDefaultFolderPrefix(normalizedName) && currentFolder != defaultAccountPoolFolder() {
			updates = append(updates, folderUpdate{name: name, folder: defaultAccountPoolFolder()})
			continue
		}
		if normalizedName != "" && !strings.Contains(normalizedName, "/") && currentFolder != defaultAccountPoolFolder() {
			updates = append(updates, folderUpdate{name: name, folder: defaultAccountPoolFolder()})
			continue
		}
		if currentFolder != "" && currentFolder != defaultAccountPoolFolder() {
			continue
		}
		inferred := inferAccountPoolFolder(name, []byte(data))
		if inferred == "" || inferred == defaultAccountPoolFolder() {
			continue
		}
		updates = append(updates, folderUpdate{name: name, folder: inferred})
	}
	if errClose := rows.Close(); errClose != nil {
		return fmt.Errorf("failed to close account pool folder inference rows: %w", errClose)
	}
	if errRows := rows.Err(); errRows != nil {
		return fmt.Errorf("failed to read account pool folder inference rows: %w", errRows)
	}
	if len(updates) == 0 {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin account pool folder inference transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			if errRollback := tx.Rollback(); errRollback != nil {
				log.WithError(errRollback).Debug("failed to rollback account pool folder inference transaction")
			}
		}
	}()
	stmt, err := tx.Prepare(`UPDATE account_pool_entries SET folder = ? WHERE name = ?`)
	if err != nil {
		return fmt.Errorf("failed to prepare account pool folder inference update: %w", err)
	}
	defer func() {
		if errClose := stmt.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close account pool folder inference statement")
		}
	}()
	now := time.Now().UTC().Format(time.RFC3339)
	folders := make(map[string]struct{}, len(updates))
	for _, update := range updates {
		if _, errExec := stmt.Exec(update.folder, update.name); errExec != nil {
			return fmt.Errorf("failed to update inferred account pool folder %s: %w", update.name, errExec)
		}
		folders[update.folder] = struct{}{}
	}
	if err = upsertAccountPoolFoldersTx(tx, folders, now); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit account pool folder inference: %w", err)
	}
	committed = true
	return nil
}

func accountPoolEntryNameHasDefaultFolderPrefix(name string) bool {
	name = normalizeAccountPoolEntryName(name)
	if !strings.Contains(name, "/") {
		return false
	}
	first := strings.Split(name, "/")[0]
	return normalizeAccountPoolFolder(first) == defaultAccountPoolFolder()
}

func (h *Handler) ensureAccountPoolSQLiteSingleAccountFoldersDefault() error {
	accountPoolDBMu.Lock()
	defer accountPoolDBMu.Unlock()

	db, err := h.openAccountPoolSQLiteLocked()
	if err != nil {
		return err
	}
	defer func() {
		if errClose := db.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close account pool sqlite database")
		}
	}()

	rows, err := db.Query(`SELECT name, COALESCE(folder, '') FROM account_pool_entries`)
	if err != nil {
		return fmt.Errorf("failed to query account pool single-folder candidates: %w", err)
	}
	type singleFolderEntry struct {
		name   string
		folder string
	}
	groups := make(map[string][]singleFolderEntry)
	existingNames := make(map[string]struct{})
	for rows.Next() {
		var name, folder string
		if errScan := rows.Scan(&name, &folder); errScan != nil {
			_ = rows.Close()
			return fmt.Errorf("failed to scan account pool single-folder candidate: %w", errScan)
		}
		name = normalizeAccountPoolEntryName(name)
		if name == "" {
			continue
		}
		existingNames[name] = struct{}{}
		entryFolder := accountPoolFolderFromStoredEntry(name, folder)
		groups[entryFolder] = append(groups[entryFolder], singleFolderEntry{name: name, folder: entryFolder})
	}
	if errClose := rows.Close(); errClose != nil {
		return fmt.Errorf("failed to close account pool single-folder rows: %w", errClose)
	}
	if errRows := rows.Err(); errRows != nil {
		return fmt.Errorf("failed to read account pool single-folder rows: %w", errRows)
	}

	type folderMove struct {
		from string
		to   string
	}
	moves := make([]folderMove, 0)
	for folder, entries := range groups {
		if folder == "" || folder == defaultAccountPoolFolder() || len(entries) != 1 {
			continue
		}
		from := entries[0].name
		to := uniqueDefaultAccountPoolEntryName(from, existingNames)
		if to == "" || to == from {
			continue
		}
		delete(existingNames, from)
		existingNames[to] = struct{}{}
		moves = append(moves, folderMove{from: from, to: to})
	}
	if len(moves) == 0 {
		return nil
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin account pool single-folder transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			if errRollback := tx.Rollback(); errRollback != nil {
				log.WithError(errRollback).Debug("failed to rollback account pool single-folder transaction")
			}
		}
	}()

	stmt, err := tx.Prepare(`UPDATE account_pool_entries SET name = ?, folder = ?, updated_at = ? WHERE name = ?`)
	if err != nil {
		return fmt.Errorf("failed to prepare account pool single-folder update: %w", err)
	}
	defer func() {
		if errClose := stmt.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close account pool single-folder statement")
		}
	}()
	now := time.Now().UTC().Format(time.RFC3339)
	for _, move := range moves {
		if _, errExec := stmt.Exec(move.to, defaultAccountPoolFolder(), now, move.from); errExec != nil {
			return fmt.Errorf("failed to move account pool single-folder entry %s: %w", move.from, errExec)
		}
	}
	if err = upsertAccountPoolFoldersTx(tx, map[string]struct{}{defaultAccountPoolFolder(): {}}, now); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit account pool single-folder migration: %w", err)
	}
	committed = true
	return nil
}

func uniqueDefaultAccountPoolEntryName(name string, existing map[string]struct{}) string {
	name = normalizeAccountPoolEntryName(name)
	if name == "" {
		return ""
	}
	base := path.Base(name)
	if base == "." || base == "/" || base == "" {
		return ""
	}
	target := accountPoolEntryNameForFolder(defaultAccountPoolFolder(), base)
	if target == "" {
		return ""
	}
	if _, ok := existing[target]; !ok || target == name {
		return target
	}
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 2; i < 10000; i++ {
		candidate := accountPoolEntryNameForFolder(defaultAccountPoolFolder(), fmt.Sprintf("%s_%d%s", stem, i, ext))
		if _, ok := existing[candidate]; !ok {
			return candidate
		}
	}
	return ""
}

func (h *Handler) readAccountPoolJSONDatabase() (map[string][]byte, error) {
	data, err := os.ReadFile(h.accountPoolDatabasePath())
	if err != nil {
		if os.IsNotExist(err) {
			return map[string][]byte{}, nil
		}
		return nil, fmt.Errorf("failed to read account pool json database: %w", err)
	}
	var db accountPoolDBFile
	if errUnmarshal := json.Unmarshal(data, &db); errUnmarshal != nil {
		return nil, fmt.Errorf("invalid account pool json database: %w", errUnmarshal)
	}
	entries := make(map[string][]byte, len(db.Entries))
	for name, entry := range db.Entries {
		if strings.TrimSpace(entry.Name) != "" {
			name = entry.Name
		}
		name = normalizeAccountPoolEntryName(name)
		if isUnsafeAccountPoolEntryName(name) || !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		entryData := bytes.TrimSpace([]byte(entry.Data))
		if len(entryData) == 0 {
			continue
		}
		entries[name] = append([]byte(nil), entryData...)
	}
	return dedupeAccountPoolEntries(entries), nil
}

func (h *Handler) readLegacyAccountPoolZip() (map[string][]byte, error) {
	archivePath := h.accountPoolArchivePath()
	data, err := os.ReadFile(archivePath)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string][]byte{}, nil
		}
		return nil, fmt.Errorf("failed to read account pool archive: %w", err)
	}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("invalid account pool archive: %w", err)
	}

	entries := make(map[string][]byte, len(reader.File))
	for _, entry := range reader.File {
		if entry == nil || entry.FileInfo().IsDir() {
			continue
		}
		name := accountPoolArchiveEntryName(entry.Name)
		if isUnsafeAccountPoolEntryName(name) || !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		rc, errOpen := entry.Open()
		if errOpen != nil {
			return nil, fmt.Errorf("failed to open account pool entry %s: %w", name, errOpen)
		}
		entryData, errRead := io.ReadAll(rc)
		errClose := rc.Close()
		if errRead != nil {
			return nil, fmt.Errorf("failed to read account pool entry %s: %w", name, errRead)
		}
		if errClose != nil {
			return nil, fmt.Errorf("failed to close account pool entry %s: %w", name, errClose)
		}
		if len(bytes.TrimSpace(entryData)) == 0 {
			continue
		}
		entries[name] = entryData
	}
	return entries, nil
}

func (h *Handler) writeAccountPoolArchive(entries map[string][]byte) error {
	accountPoolDBMu.Lock()
	defer accountPoolDBMu.Unlock()
	return h.writeAccountPoolDatabaseLocked(entries)
}

func (h *Handler) writeAccountPoolDatabaseLocked(entries map[string][]byte) error {
	entries = dedupeAccountPoolEntries(entries)
	if err := h.writeAccountPoolSQLiteLocked(entries); err != nil {
		return err
	}
	return h.writeAccountPoolZipMirrorLocked(entries)
}

func (h *Handler) openAccountPoolSQLiteLocked() (*sql.DB, error) {
	if h == nil || h.cfg == nil {
		return nil, fmt.Errorf("handler not initialized")
	}
	if err := os.MkdirAll(h.cfg.AuthDir, 0o700); err != nil {
		return nil, fmt.Errorf("failed to create auth dir: %w", err)
	}
	db, err := sql.Open("sqlite", h.accountPoolSQLitePath())
	if err != nil {
		return nil, fmt.Errorf("failed to open account pool sqlite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL; PRAGMA busy_timeout=5000;`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to configure account pool sqlite database: %w", err)
	}
	if _, err = db.Exec(`
CREATE TABLE IF NOT EXISTS account_pool_entries (
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
	check_updated_at TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_account_pool_entries_email ON account_pool_entries(email);
CREATE INDEX IF NOT EXISTS idx_account_pool_entries_type ON account_pool_entries(type);
CREATE INDEX IF NOT EXISTS idx_account_pool_entries_updated_at ON account_pool_entries(updated_at);
CREATE TABLE IF NOT EXISTS account_pool_folders (
	folder TEXT NOT NULL PRIMARY KEY,
	source_model TEXT,
	source_info TEXT,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to initialize account pool sqlite database: %w", err)
	}
	if err = ensureAccountPoolSQLiteSchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

func ensureAccountPoolSQLiteSchema(db *sql.DB) error {
	if err := migrateAccountPoolEntriesContentHashIndex(db); err != nil {
		return err
	}
	columns := []struct {
		table string
		name  string
		def   string
	}{
		{"account_pool_entries", "folder", "TEXT NOT NULL DEFAULT ''"},
		{"account_pool_entries", "check_result", "TEXT NOT NULL DEFAULT ''"},
		{"account_pool_entries", "check_content_hash", "TEXT NOT NULL DEFAULT ''"},
		{"account_pool_entries", "check_updated_at", "TEXT NOT NULL DEFAULT ''"},
	}
	for _, column := range columns {
		if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", column.table, column.name, column.def)); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
				continue
			}
			return fmt.Errorf("failed to add account pool sqlite column %s.%s: %w", column.table, column.name, err)
		}
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_account_pool_entries_folder ON account_pool_entries(folder);`); err != nil {
		return fmt.Errorf("failed to initialize account pool folder index: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_account_pool_entries_content_hash ON account_pool_entries(content_hash);`); err != nil {
		return fmt.Errorf("failed to initialize account pool content hash index: %w", err)
	}
	return nil
}

func migrateAccountPoolEntriesContentHashIndex(db *sql.DB) error {
	var schema string
	err := db.QueryRow(`SELECT COALESCE(sql, '') FROM sqlite_master WHERE type = 'table' AND name = 'account_pool_entries'`).Scan(&schema)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return fmt.Errorf("failed to inspect account pool sqlite schema: %w", err)
	}
	if !strings.Contains(strings.ToLower(schema), "content_hash text not null unique") {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin account pool sqlite migration: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			if errRollback := tx.Rollback(); errRollback != nil {
				log.WithError(errRollback).Debug("failed to rollback account pool sqlite migration")
			}
		}
	}()
	if _, err = tx.Exec(`ALTER TABLE account_pool_entries RENAME TO account_pool_entries_old_hash_unique`); err != nil {
		return fmt.Errorf("failed to rename account pool sqlite table: %w", err)
	}
	if _, err = tx.Exec(`ALTER TABLE account_pool_entries_old_hash_unique ADD COLUMN folder TEXT NOT NULL DEFAULT ''`); err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
			return fmt.Errorf("failed to add folder column to old account pool sqlite table: %w", err)
		}
	}
	if _, err = tx.Exec(`
CREATE TABLE account_pool_entries (
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
	check_updated_at TEXT NOT NULL DEFAULT ''
)`); err != nil {
		return fmt.Errorf("failed to create migrated account pool sqlite table: %w", err)
	}
	if _, err = tx.Exec(`
INSERT OR REPLACE INTO account_pool_entries (
	name, content_hash, type, provider, email, folder, size, data, created_at, updated_at, check_result, check_content_hash, check_updated_at
)
SELECT name, content_hash, type, provider, email, COALESCE(folder, ''), size, data, created_at, updated_at, '', '', ''
FROM account_pool_entries_old_hash_unique
WHERE name IS NOT NULL AND TRIM(name) != ''`); err != nil {
		return fmt.Errorf("failed to copy account pool sqlite entries: %w", err)
	}
	if _, err = tx.Exec(`DROP TABLE account_pool_entries_old_hash_unique`); err != nil {
		return fmt.Errorf("failed to drop old account pool sqlite table: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit account pool sqlite migration: %w", err)
	}
	committed = true
	return nil
}

func (h *Handler) readAccountPoolSQLiteLocked() (map[string][]byte, error) {
	db, err := h.openAccountPoolSQLiteLocked()
	if err != nil {
		return nil, err
	}
	defer func() {
		if errClose := db.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close account pool sqlite database")
		}
	}()

	rows, err := db.Query(`SELECT name, data FROM account_pool_entries ORDER BY lower(name)`)
	if err != nil {
		return nil, fmt.Errorf("failed to query account pool sqlite database: %w", err)
	}
	defer func() {
		if errClose := rows.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close account pool sqlite rows")
		}
	}()

	entries := make(map[string][]byte)
	for rows.Next() {
		var name, data string
		if errScan := rows.Scan(&name, &data); errScan != nil {
			return nil, fmt.Errorf("failed to scan account pool sqlite entry: %w", errScan)
		}
		name = normalizeAccountPoolEntryName(name)
		if isUnsafeAccountPoolEntryName(name) || !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		entryData := bytes.TrimSpace([]byte(data))
		if len(entryData) == 0 {
			continue
		}
		entries[name] = append([]byte(nil), entryData...)
	}
	if errRows := rows.Err(); errRows != nil {
		return nil, fmt.Errorf("failed to read account pool sqlite entries: %w", errRows)
	}
	return dedupeAccountPoolEntries(entries), nil
}

func (h *Handler) readAccountPoolSQLiteEntryLocked(name string) ([]byte, error) {
	db, err := h.openAccountPoolSQLiteLocked()
	if err != nil {
		return nil, err
	}
	defer func() {
		if errClose := db.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close account pool sqlite database")
		}
	}()

	var data string
	err = db.QueryRow(`SELECT data FROM account_pool_entries WHERE name = ?`, name).Scan(&data)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to query account pool sqlite entry %s: %w", name, err)
	}
	entryData := bytes.TrimSpace([]byte(data))
	if len(entryData) == 0 {
		return nil, nil
	}
	return append([]byte(nil), entryData...), nil
}

func (h *Handler) readAccountPoolFolderMapLocked() (map[string]string, error) {
	db, err := h.openAccountPoolSQLiteLocked()
	if err != nil {
		return nil, err
	}
	defer func() {
		if errClose := db.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close account pool sqlite database")
		}
	}()
	rows, err := db.Query(`SELECT name, folder FROM account_pool_entries`)
	if err != nil {
		return nil, fmt.Errorf("failed to query account pool folders: %w", err)
	}
	defer func() {
		if errClose := rows.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close account pool folder rows")
		}
	}()
	folders := make(map[string]string)
	for rows.Next() {
		var name, folder string
		if errScan := rows.Scan(&name, &folder); errScan != nil {
			return nil, fmt.Errorf("failed to scan account pool folder: %w", errScan)
		}
		folders[name] = normalizeAccountPoolFolder(folder)
	}
	if errRows := rows.Err(); errRows != nil {
		return nil, fmt.Errorf("failed to read account pool folders: %w", errRows)
	}
	return folders, nil
}

func (h *Handler) writeAccountPoolSQLiteLocked(entries map[string][]byte) error {
	db, err := h.openAccountPoolSQLiteLocked()
	if err != nil {
		return err
	}
	defer func() {
		if errClose := db.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close account pool sqlite database")
		}
	}()

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin account pool sqlite transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			if errRollback := tx.Rollback(); errRollback != nil {
				log.WithError(errRollback).Debug("failed to rollback account pool sqlite transaction")
			}
		}
	}()
	previousChecks := make(map[string]accountPoolStoredCheckResult)
	if rows, errRows := tx.Query(`SELECT name, COALESCE(check_result, ''), COALESCE(check_content_hash, ''), COALESCE(check_updated_at, '') FROM account_pool_entries`); errRows == nil {
		for rows.Next() {
			var name string
			var check accountPoolStoredCheckResult
			if errScan := rows.Scan(&name, &check.Result, &check.ContentHash, &check.UpdatedAt); errScan != nil {
				log.WithError(errScan).Debug("failed to scan previous account pool check result")
				continue
			}
			name = normalizeAccountPoolEntryName(name)
			if name != "" && strings.TrimSpace(check.Result) != "" {
				previousChecks[name] = check
			}
		}
		if errClose := rows.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close previous account pool check result rows")
		}
	} else {
		log.WithError(errRows).Debug("failed to load previous account pool check results")
	}
	if _, err = tx.Exec(`DELETE FROM account_pool_entries`); err != nil {
		return fmt.Errorf("failed to clear account pool sqlite database: %w", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	folders := make(map[string]struct{})
	stmt, err := tx.Prepare(`
INSERT INTO account_pool_entries (
	name, content_hash, type, provider, email, folder, size, data, created_at, updated_at, check_result, check_content_hash, check_updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(name) DO UPDATE SET
	content_hash=excluded.content_hash,
	type=excluded.type,
	provider=excluded.provider,
	email=excluded.email,
	folder=CASE WHEN excluded.folder != '' THEN excluded.folder ELSE account_pool_entries.folder END,
	size=excluded.size,
	data=excluded.data,
	check_result=CASE WHEN excluded.check_result != '' THEN excluded.check_result ELSE account_pool_entries.check_result END,
	check_content_hash=CASE WHEN excluded.check_content_hash != '' THEN excluded.check_content_hash ELSE account_pool_entries.check_content_hash END,
	check_updated_at=CASE WHEN excluded.check_updated_at != '' THEN excluded.check_updated_at ELSE account_pool_entries.check_updated_at END,
	updated_at=excluded.updated_at
`)
	if err != nil {
		return fmt.Errorf("failed to prepare account pool sqlite insert: %w", err)
	}
	defer func() {
		if errClose := stmt.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close account pool sqlite insert statement")
		}
	}()
	for name, data := range dedupeAccountPoolEntries(entries) {
		name = normalizeAccountPoolEntryName(name)
		entry := buildAccountPoolDBEntry(name, data, now)
		folders[entry.Folder] = struct{}{}
		check := previousChecks[name]
		if check.ContentHash != "" && check.ContentHash != entry.Hash {
			check = accountPoolStoredCheckResult{}
		}
		if _, err = stmt.Exec(
			entry.Name,
			entry.Hash,
			entry.Type,
			entry.Provider,
			entry.Email,
			entry.Folder,
			entry.Size,
			entry.Data,
			entry.CreatedAt,
			entry.UpdatedAt,
			check.Result,
			check.ContentHash,
			check.UpdatedAt,
		); err != nil {
			return fmt.Errorf("failed to insert account pool sqlite entry %s: %w", name, err)
		}
	}
	if err = upsertAccountPoolFoldersTx(tx, folders, now); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit account pool sqlite database: %w", err)
	}
	committed = true
	return nil
}

func (h *Handler) upsertAccountPoolSQLiteFilesLocked(files []accountPoolArchiveFile) error {
	db, err := h.openAccountPoolSQLiteLocked()
	if err != nil {
		return err
	}
	defer func() {
		if errClose := db.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close account pool sqlite database")
		}
	}()

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin account pool sqlite transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			if errRollback := tx.Rollback(); errRollback != nil {
				log.WithError(errRollback).Debug("failed to rollback account pool sqlite transaction")
			}
		}
	}()
	stmt, err := tx.Prepare(`
INSERT INTO account_pool_entries (
	name, content_hash, type, provider, email, folder, size, data, created_at, updated_at, check_result, check_content_hash, check_updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(name) DO UPDATE SET
	content_hash=excluded.content_hash,
	type=excluded.type,
	provider=excluded.provider,
	email=excluded.email,
	folder=CASE WHEN excluded.folder != '' THEN excluded.folder ELSE account_pool_entries.folder END,
	size=excluded.size,
	data=excluded.data,
	check_result=CASE WHEN excluded.check_result != '' THEN excluded.check_result ELSE account_pool_entries.check_result END,
	check_content_hash=CASE WHEN excluded.check_content_hash != '' THEN excluded.check_content_hash ELSE account_pool_entries.check_content_hash END,
	check_updated_at=CASE WHEN excluded.check_updated_at != '' THEN excluded.check_updated_at ELSE account_pool_entries.check_updated_at END,
	updated_at=excluded.updated_at
`)
	if err != nil {
		return fmt.Errorf("failed to prepare account pool sqlite upsert: %w", err)
	}
	defer func() {
		if errClose := stmt.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close account pool sqlite upsert statement")
		}
	}()
	now := time.Now().UTC().Format(time.RFC3339)
	folders := make(map[string]struct{})
	pendingEntries := make(map[string]accountPoolDBEntry, len(files))
	pendingIdentities := make(map[string]string, len(files))
	for _, file := range files {
		name := normalizeAccountPoolEntryName(file.Name)
		if isUnsafeAccountPoolEntryName(name) || !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		if len(bytes.TrimSpace(file.Data)) == 0 {
			continue
		}
		entry := buildAccountPoolDBEntry(name, file.Data, now)
		if strings.TrimSpace(file.Folder) != "" {
			entry.Folder = normalizeAccountPoolFolder(file.Folder)
		}
		for _, identity := range accountPoolIdentityKeys(file.Data) {
			if previousName := pendingIdentities[identity]; previousName != "" && !strings.EqualFold(previousName, name) {
				delete(pendingEntries, previousName)
			}
			pendingIdentities[identity] = name
		}
		pendingEntries[name] = entry
	}
	pendingIdentities = make(map[string]string, len(pendingEntries))
	for name, entry := range pendingEntries {
		for _, identity := range accountPoolIdentityKeys([]byte(entry.Data)) {
			pendingIdentities[identity] = name
		}
	}
	if len(pendingIdentities) > 0 {
		existingRows, errRows := tx.Query(`SELECT name, data FROM account_pool_entries`)
		if errRows != nil {
			return fmt.Errorf("failed to query account pool sqlite duplicates: %w", errRows)
		}
		duplicateNames := make(map[string]struct{})
		for existingRows.Next() {
			var existingName, existingData string
			if errScan := existingRows.Scan(&existingName, &existingData); errScan != nil {
				_ = existingRows.Close()
				return fmt.Errorf("failed to scan account pool sqlite duplicate: %w", errScan)
			}
			for _, existingIdentity := range accountPoolIdentityKeys([]byte(existingData)) {
				keepName := pendingIdentities[existingIdentity]
				if keepName != "" && !strings.EqualFold(existingName, keepName) {
					duplicateNames[existingName] = struct{}{}
					break
				}
			}
		}
		if errRows := existingRows.Close(); errRows != nil {
			return fmt.Errorf("failed to close account pool sqlite duplicate rows: %w", errRows)
		}
		if errRows := existingRows.Err(); errRows != nil {
			return fmt.Errorf("failed to read account pool sqlite duplicates: %w", errRows)
		}
		for duplicateName := range duplicateNames {
			if _, err = tx.Exec(`DELETE FROM account_pool_entries WHERE name = ?`, duplicateName); err != nil {
				return fmt.Errorf("failed to remove duplicate account pool sqlite entry %s: %w", duplicateName, err)
			}
		}
	}
	for _, file := range files {
		name := normalizeAccountPoolEntryName(file.Name)
		entry, ok := pendingEntries[name]
		if !ok {
			continue
		}
		if _, err = tx.Exec(`DELETE FROM account_pool_entries WHERE name = ?`, name); err != nil {
			return fmt.Errorf("failed to replace account pool sqlite entry %s: %w", name, err)
		}
		folders[entry.Folder] = struct{}{}
		if _, err = stmt.Exec(
			entry.Name,
			entry.Hash,
			entry.Type,
			entry.Provider,
			entry.Email,
			entry.Folder,
			entry.Size,
			entry.Data,
			entry.CreatedAt,
			entry.UpdatedAt,
			"",
			"",
			"",
		); err != nil {
			return fmt.Errorf("failed to upsert account pool sqlite entry %s: %w", name, err)
		}
	}
	if err = upsertAccountPoolFoldersTx(tx, folders, now); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit account pool sqlite upsert: %w", err)
	}
	committed = true
	entries, err := h.readAccountPoolSQLiteLocked()
	if err != nil {
		return err
	}
	if err := h.writeAccountPoolZipMirrorLocked(entries); err != nil {
		return err
	}
	return nil
}

func upsertAccountPoolFoldersTx(tx *sql.Tx, folders map[string]struct{}, now string) error {
	if tx == nil || len(folders) == 0 {
		return nil
	}
	stmt, err := tx.Prepare(`
INSERT INTO account_pool_folders (folder, created_at, updated_at)
VALUES (?, ?, ?)
ON CONFLICT(folder) DO UPDATE SET updated_at=excluded.updated_at
`)
	if err != nil {
		return fmt.Errorf("failed to prepare account pool folder upsert: %w", err)
	}
	defer func() {
		if errClose := stmt.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close account pool folder upsert statement")
		}
	}()
	names := make([]string, 0, len(folders))
	for folder := range folders {
		folder = normalizeAccountPoolFolder(folder)
		if folder != "" {
			names = append(names, folder)
		}
	}
	sort.Strings(names)
	for _, folder := range names {
		if _, err = stmt.Exec(folder, now, now); err != nil {
			return fmt.Errorf("failed to upsert account pool folder %s: %w", folder, err)
		}
	}
	return nil
}

func buildAccountPoolDBEntry(name string, data []byte, now string) accountPoolDBEntry {
	trimmed := bytes.TrimSpace(data)
	typeValue := strings.TrimSpace(gjson.GetBytes(trimmed, "type").String())
	emailValue := strings.TrimSpace(gjson.GetBytes(trimmed, "email").String())
	return accountPoolDBEntry{
		Name:      name,
		Data:      string(trimmed),
		Hash:      hashAccountPoolContent(trimmed),
		Type:      typeValue,
		Provider:  typeValue,
		Email:     emailValue,
		Folder:    inferAccountPoolFolder(name, trimmed),
		Size:      len(trimmed),
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func buildAccountPoolZip(entries map[string][]byte) ([]byte, error) {
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	normalizedEntries := make(map[string][]byte, len(entries))
	names := make([]string, 0, len(entries))
	seenNames := make(map[string]struct{}, len(entries))
	for name, data := range entries {
		name = normalizeAccountPoolEntryName(name)
		if isUnsafeAccountPoolEntryName(name) || !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		normalizedEntries[name] = data
		if _, ok := seenNames[name]; ok {
			continue
		}
		seenNames[name] = struct{}{}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetMode(0o600)
		part, errCreate := writer.CreateHeader(header)
		if errCreate != nil {
			_ = writer.Close()
			return nil, fmt.Errorf("failed to create account pool entry %s: %w", name, errCreate)
		}
		if _, errWrite := part.Write(normalizedEntries[name]); errWrite != nil {
			_ = writer.Close()
			return nil, fmt.Errorf("failed to write account pool entry %s: %w", name, errWrite)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("failed to close account pool archive: %w", err)
	}
	return buf.Bytes(), nil
}

func (h *Handler) writeAccountPoolZipMirrorLocked(entries map[string][]byte) error {
	if h == nil || h.cfg == nil {
		return fmt.Errorf("handler not initialized")
	}
	data, err := buildAccountPoolZip(entries)
	if err != nil {
		return err
	}
	if err := os.WriteFile(h.accountPoolArchivePath(), data, 0o600); err != nil {
		return fmt.Errorf("failed to write account pool zip mirror: %w", err)
	}
	return nil
}

func dedupeAccountPoolEntries(entries map[string][]byte) map[string][]byte {
	type candidate struct {
		name string
		data []byte
	}
	candidates := make([]candidate, 0, len(entries))
	for name, data := range entries {
		name = normalizeAccountPoolEntryName(name)
		if isUnsafeAccountPoolEntryName(name) || !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		candidates = append(candidates, candidate{name: name, data: data})
	}
	sort.Slice(candidates, func(i, j int) bool {
		return strings.ToLower(candidates[i].name) < strings.ToLower(candidates[j].name)
	})

	result := make(map[string][]byte, len(candidates))
	seen := make(map[string]string, len(candidates))
	for _, item := range candidates {
		keys := accountPoolIdentityKeys(item.data)
		duplicate := false
		for _, key := range keys {
			if existingName, ok := seen[key]; ok && !strings.EqualFold(existingName, item.name) {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		if len(keys) == 0 {
			keys = []string{"hash|" + hashAccountPoolContent(item.data)}
		}
		for _, key := range keys {
			seen[key] = item.name
		}
		result[item.name] = item.data
	}
	return result
}

func removeDuplicateAccountPoolEntries(entries map[string][]byte, keepName string, keepData []byte) {
	keepName = normalizeAccountPoolEntryName(keepName)
	for name, data := range entries {
		if strings.EqualFold(name, keepName) {
			continue
		}
		if accountPoolSameIdentity(keepData, data) {
			delete(entries, name)
		}
	}
}

func accountPoolSameIdentity(left []byte, right []byte) bool {
	leftKeys := accountPoolIdentityKeys(left)
	rightKeys := accountPoolIdentityKeys(right)
	if len(leftKeys) == 0 || len(rightKeys) == 0 {
		return hashAccountPoolContent(left) == hashAccountPoolContent(right)
	}
	seen := make(map[string]struct{}, len(leftKeys))
	for _, key := range leftKeys {
		seen[key] = struct{}{}
	}
	for _, key := range rightKeys {
		if _, ok := seen[key]; ok {
			return true
		}
	}
	return false
}

func accountPoolIdentityKeys(data []byte) []string {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil
	}
	provider := firstAccountPoolJSONValue(trimmed, "type", "provider", "service", "kind")
	email := strings.ToLower(firstAccountPoolJSONValue(trimmed,
		"email", "account_email", "service_email", "user_email", "login_email",
		"account.email", "user.email", "profile.email", "oauth.email",
	))
	subject := strings.ToLower(firstAccountPoolJSONValue(trimmed,
		"sub", "subject", "user_id", "account_id", "openai_account_id",
		"account.id", "user.id", "profile.sub", "profile.id",
	))
	clientID := strings.ToLower(firstAccountPoolJSONValue(trimmed, "client_id", "oauth.client_id", "credentials.client_id"))
	refreshToken := firstAccountPoolJSONValue(trimmed,
		"refresh_token", "oauth.refresh_token", "credentials.refresh_token", "token.refresh_token",
	)
	apiKey := firstAccountPoolJSONValue(trimmed, "api_key", "key", "token.api_key")

	prefix := strings.ToLower(strings.TrimSpace(provider))
	if prefix == "" {
		prefix = "unknown"
	}
	keys := make([]string, 0, 5)
	add := func(kind string, parts ...string) {
		values := make([]string, 0, len(parts))
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part == "" {
				return
			}
			values = append(values, part)
		}
		keys = append(keys, kind+"|"+prefix+"|"+strings.Join(values, "|"))
	}
	add("email", email)
	add("subject", subject)
	add("email_subject", email, subject)
	add("oauth", clientID, hashAccountPoolSecret(refreshToken))
	add("api_key", hashAccountPoolSecret(apiKey))
	if len(keys) == 0 {
		keys = append(keys, "hash|"+hashAccountPoolContent(trimmed))
	}
	return keys
}

func firstAccountPoolJSONValue(data []byte, paths ...string) string {
	for _, itemPath := range paths {
		value := strings.TrimSpace(gjson.GetBytes(data, itemPath).String())
		if value != "" {
			return value
		}
	}
	return ""
}

func hashAccountPoolSecret(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func hashAccountPoolContent(data []byte) string {
	var parsed any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	normalized := bytes.TrimSpace(data)
	if err := decoder.Decode(&parsed); err == nil {
		if encoded, errMarshal := json.Marshal(normalizeAccountPoolJSON(parsed)); errMarshal == nil {
			normalized = encoded
		}
	}
	sum := sha256.Sum256(normalized)
	return hex.EncodeToString(sum[:])
}

func normalizeAccountPoolJSON(value any) any {
	switch typed := value.(type) {
	case []any:
		for index, item := range typed {
			typed[index] = normalizeAccountPoolJSON(item)
		}
		return typed
	case map[string]any:
		normalized := make(map[string]any, len(typed))
		for key, item := range typed {
			normalized[key] = normalizeAccountPoolJSON(item)
		}
		return normalized
	default:
		return value
	}
}

func (h *Handler) DownloadAccountPoolArchive(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}
	entries, err := h.readAccountPoolArchive()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if c.Request != nil && c.Request.Method == http.MethodPost {
		names, errNames := requestedAuthFileNamesForDelete(c)
		if errNames != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": errNames.Error()})
			return
		}
		filtered := make(map[string][]byte, len(names))
		for _, name := range names {
			name = normalizeAccountPoolEntryName(name)
			if isUnsafeAccountPoolEntryName(name) || !strings.HasSuffix(strings.ToLower(name), ".json") {
				c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid name: %s", name)})
				return
			}
			if data, ok := entries[name]; ok {
				filtered[name] = data
			}
		}
		entries = filtered
	}
	data, err := buildAccountPoolZip(entries)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", buildAccountPoolArchiveName()))
	c.Data(http.StatusOK, "application/zip", data)
}

func (h *Handler) ListAccountPoolEntries(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}
	includeHash := strings.EqualFold(strings.TrimSpace(c.Query("include_hash")), "true")
	if _, err := os.Stat(h.accountPoolSQLitePath()); err == nil {
		if errEnsure := h.ensureAccountPoolSQLiteSub2Converted(); errEnsure != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": errEnsure.Error()})
			return
		}
		if errEnsureFolders := h.ensureAccountPoolSQLiteFoldersInferred(); errEnsureFolders != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": errEnsureFolders.Error()})
			return
		}
		if errEnsureSingleFolders := h.ensureAccountPoolSQLiteSingleAccountFoldersDefault(); errEnsureSingleFolders != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": errEnsureSingleFolders.Error()})
			return
		}
		files, folderInfos, errList := h.listAccountPoolSQLiteEntrySummaries(includeHash)
		if errList != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": errList.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"files": files, "folders": folderInfos})
		return
	} else if !os.IsNotExist(err) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to stat account pool sqlite database: %v", err)})
		return
	}
	entries, err := h.readAccountPoolArchive()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	folderByName, errFolders := h.readAccountPoolFolderMapLocked()
	if errFolders != nil {
		log.WithError(errFolders).Warn("failed to read account pool folder map")
		folderByName = map[string]string{}
	}
	folderInfos, errFolderInfos := h.listAccountPoolFolders()
	if errFolderInfos != nil {
		log.WithError(errFolderInfos).Warn("failed to read account pool folder info")
		folderInfos = nil
	}
	files := make([]gin.H, 0, len(entries))
	folderUsage := make(map[string]accountPoolUsageSummary)
	for name, data := range entries {
		folder := inferAccountPoolFolder(name, data)
		if storedFolder := normalizeAccountPoolFolder(folderByName[name]); storedFolder != "" && storedFolder != defaultAccountPoolFolder() {
			folder = storedFolder
		}
		entry := gin.H{
			"id":         name,
			"auth_id":    name,
			"auth_index": name,
			"name":       name,
			"size":       len(data),
			"source":     "account_pool",
			"folder":     folder,
		}
		if typeValue := strings.TrimSpace(gjson.GetBytes(data, "type").String()); typeValue != "" {
			entry["type"] = typeValue
			entry["provider"] = typeValue
		}
		if emailValue := strings.TrimSpace(gjson.GetBytes(data, "email").String()); emailValue != "" {
			entry["email"] = emailValue
		}
		summary := accountPoolUsage.SummaryForAccountPoolEntry(name, accountPoolUsageEmailFromEntry(data))
		applyAccountPoolUsageSummary(entry, summary)
		accumulateAccountPoolFolderUsage(folderUsage, folder, summary)
		if includeHash {
			entry["content_hash"] = hashAccountPoolContent(data)
		}
		files = append(files, entry)
	}
	applyAccountPoolFolderUsageSummaries(folderInfos, folderUsage)
	sort.Slice(files, func(i, j int) bool {
		nameI, _ := files[i]["name"].(string)
		nameJ, _ := files[j]["name"].(string)
		return strings.ToLower(nameI) < strings.ToLower(nameJ)
	})
	c.JSON(http.StatusOK, gin.H{"files": files, "folders": folderInfos})
}

func (h *Handler) RepairAccountPoolEntries(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}
	entries, err := h.readAccountPoolArchive()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	repairedEntries, changed, stats := repairAccountPoolUnsupportedEntries(entries)
	if llmEntries, llmChanged, llmStats := h.repairAccountPoolEntriesWithLLM(c.Request.Context(), repairedEntries); llmChanged {
		repairedEntries = llmEntries
		changed = true
		stats.LLMRepaired += llmStats.LLMRepaired
		stats.LLMFailed += llmStats.LLMFailed
	} else {
		stats.LLMFailed += llmStats.LLMFailed
	}
	if changed {
		if errWrite := h.writeAccountPoolArchive(repairedEntries); errWrite != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": errWrite.Error()})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"status":         "ok",
		"repaired":       changed,
		"converted_sub2": stats.ConvertedSub2,
		"inferred_codex": stats.InferredCodex,
		"llm_repaired":   stats.LLMRepaired,
		"llm_failed":     stats.LLMFailed,
	})
}

func (h *Handler) repairAccountPoolEntriesWithLLM(ctx context.Context, entries map[string][]byte) (map[string][]byte, bool, accountPoolRepairStats) {
	stats := accountPoolRepairStats{}
	if len(entries) == 0 {
		return entries, false, stats
	}
	model, ok := h.accountPoolRepairModel()
	if !ok {
		return entries, false, stats
	}
	next := make(map[string][]byte, len(entries))
	for name, data := range entries {
		next[name] = data
	}
	changed := false
	for name, data := range entries {
		name = normalizeAccountPoolEntryName(name)
		if name == "" || isUnsafeAccountPoolEntryName(name) || !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		if strings.TrimSpace(gjson.GetBytes(data, "type").String()) != "" {
			continue
		}
		repaired, err := h.repairAccountPoolJSONWithOwnModel(ctx, model, name, data)
		if err != nil {
			stats.LLMFailed++
			log.WithError(err).Warnf("failed to repair account pool entry %s with llm", name)
			continue
		}
		if len(bytes.TrimSpace(repaired)) == 0 || strings.TrimSpace(gjson.GetBytes(repaired, "type").String()) == "" {
			stats.LLMFailed++
			continue
		}
		removeDuplicateAccountPoolEntries(next, name, repaired)
		next[name] = repaired
		stats.LLMRepaired++
		changed = true
	}
	if !changed {
		return entries, false, stats
	}
	return dedupeAccountPoolEntries(next), true, stats
}

func (h *Handler) accountPoolRepairModel() (string, bool) {
	if h == nil || h.authManager == nil {
		return "", false
	}
	if h.authManager.HomeEnabled() {
		return "auto", true
	}
	model, err := registry.GetGlobalRegistry().GetFirstAvailableModel("openai")
	if err == nil && strings.TrimSpace(model) != "" {
		return strings.TrimSpace(model), true
	}
	model, err = registry.GetGlobalRegistry().GetFirstAvailableModel("")
	if err == nil && strings.TrimSpace(model) != "" {
		return strings.TrimSpace(model), true
	}
	return "", false
}

func (h *Handler) repairAccountPoolJSONWithOwnModel(ctx context.Context, model string, name string, data []byte) ([]byte, error) {
	if h == nil || h.authManager == nil {
		return nil, fmt.Errorf("auth manager is not available")
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, fmt.Errorf("repair model is not available")
	}
	raw := string(bytes.TrimSpace(data))
	if len(raw) > 120000 {
		raw = raw[:120000]
	}
	prompt := fmt.Sprintf(`Convert this account JSON into one valid CLIProxyAPI CPA auth JSON object.
Return JSON only, no markdown. Preserve all useful token fields. If it is an OpenAI/Codex OAuth account, set "type":"codex".
Required output shape when possible:
{"type":"codex","email":"","access_token":"","refresh_token":"","id_token":"","client_id":"","account_id":"","expired":"","last_refresh":""}
File name: %s
Input JSON:
%s`, name, raw)
	body := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": "You repair malformed auth JSON. Return exactly one JSON object and nothing else."},
			{"role": "user", "content": prompt},
		},
		"temperature": 0,
	}
	bodyData, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	var sdkCfg *sdkconfig.SDKConfig
	if h.cfg != nil {
		sdkCfg = &h.cfg.SDKConfig
	}
	apiHandler := sdkhandlers.NewBaseAPIHandlers(sdkCfg, h.authManager)
	respData, _, errMsg := apiHandler.ExecuteWithAuthManager(ctx, "openai", model, bodyData, "")
	if errMsg != nil {
		if errMsg.Error == nil {
			return nil, fmt.Errorf("own model repair failed with status %d", errMsg.StatusCode)
		}
		return nil, fmt.Errorf("own model repair failed: %w", errMsg.Error)
	}
	content := strings.TrimSpace(gjson.GetBytes(respData, "choices.0.message.content").String())
	if content == "" {
		return nil, fmt.Errorf("llm repair returned empty content")
	}
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		return nil, fmt.Errorf("llm repair returned invalid json: %w", err)
	}
	if strings.TrimSpace(fmt.Sprint(parsed["type"])) == "" &&
		(strings.TrimSpace(fmt.Sprint(parsed["refresh_token"])) != "" ||
			strings.TrimSpace(fmt.Sprint(parsed["access_token"])) != "" ||
			strings.TrimSpace(fmt.Sprint(parsed["id_token"])) != "") {
		parsed["type"] = "codex"
	}
	return json.MarshalIndent(parsed, "", "  ")
}

func normalizeAccountPoolCheckResultPayload(result accountPoolCheckResultPayload, now time.Time) (accountPoolCheckResultPayload, bool) {
	status := strings.ToLower(strings.TrimSpace(result.Status))
	switch status {
	case "success", "error", "unsupported", "idle":
	default:
		return accountPoolCheckResultPayload{}, false
	}
	if status == "idle" {
		return accountPoolCheckResultPayload{Status: status, CheckedAt: now.UnixMilli()}, true
	}
	if result.CheckedAt <= 0 {
		result.CheckedAt = now.UnixMilli()
	}
	result.Status = status
	result.Message = strings.TrimSpace(result.Message)
	if len(result.Message) > 1000 {
		result.Message = result.Message[:1000]
	}
	result.Plan = strings.TrimSpace(result.Plan)
	if len(result.Plan) > 120 {
		result.Plan = result.Plan[:120]
	}
	if result.StatusCode < 0 {
		result.StatusCode = 0
	}
	if len(result.QuotaLines) > 8 {
		result.QuotaLines = result.QuotaLines[:8]
	}
	for i, line := range result.QuotaLines {
		line = strings.TrimSpace(line)
		if len(line) > 240 {
			line = line[:240]
		}
		result.QuotaLines[i] = line
	}
	return result, true
}

func applyAccountPoolCheckResult(entry gin.H, rawResult string, checkContentHash string, checkUpdatedAt string, contentHash string) {
	rawResult = strings.TrimSpace(rawResult)
	if rawResult == "" {
		return
	}
	checkContentHash = strings.TrimSpace(checkContentHash)
	contentHash = strings.TrimSpace(contentHash)
	if checkContentHash != "" && contentHash != "" && checkContentHash != contentHash {
		return
	}
	var result accountPoolCheckResultPayload
	if err := json.Unmarshal([]byte(rawResult), &result); err != nil {
		return
	}
	result, ok := normalizeAccountPoolCheckResultPayload(result, time.Now())
	if !ok {
		return
	}
	entry["check_status"] = result.Status
	if result.Message != "" {
		entry["check_message"] = result.Message
	}
	if result.Plan != "" {
		entry["check_plan"] = result.Plan
	}
	if len(result.QuotaLines) > 0 {
		entry["check_quota_lines"] = result.QuotaLines
	}
	if result.QuotaRemainingPercent != nil {
		entry["check_quota_remaining_percent"] = *result.QuotaRemainingPercent
	}
	if result.StatusCode > 0 {
		entry["check_status_code"] = result.StatusCode
	}
	if result.CheckedAt > 0 {
		entry["check_checked_at"] = result.CheckedAt
	} else if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(checkUpdatedAt)); err == nil {
		entry["check_checked_at"] = parsed.UnixMilli()
	}
	if checkContentHash != "" {
		entry["check_content_hash"] = checkContentHash
	}
}

func (h *Handler) PatchAccountPoolCheckResults(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}
	var req struct {
		Results []accountPoolCheckResultUpdate `json:"results"`
		Updates []accountPoolCheckResultUpdate `json:"updates"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	updates := req.Results
	if len(updates) == 0 {
		updates = req.Updates
	}
	if len(updates) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "results are required"})
		return
	}

	accountPoolDBMu.Lock()
	defer accountPoolDBMu.Unlock()
	db, err := h.openAccountPoolSQLiteLocked()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer func() {
		if errClose := db.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close account pool sqlite database")
		}
	}()
	tx, err := db.Begin()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to begin account pool check result transaction: %v", err)})
		return
	}
	committed := false
	defer func() {
		if !committed {
			if errRollback := tx.Rollback(); errRollback != nil {
				log.WithError(errRollback).Debug("failed to rollback account pool check result transaction")
			}
		}
	}()
	stmt, err := tx.Prepare(`UPDATE account_pool_entries SET check_result = ?, check_content_hash = ?, check_updated_at = ? WHERE name = ?`)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to prepare account pool check result update: %v", err)})
		return
	}
	defer func() {
		if errClose := stmt.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close account pool check result statement")
		}
	}()
	now := time.Now().UTC()
	updated := 0
	skipped := 0
	missing := make([]string, 0)
	for _, update := range updates {
		name := normalizeAccountPoolEntryName(update.Name)
		if name == "" || isUnsafeAccountPoolEntryName(name) || !strings.HasSuffix(strings.ToLower(name), ".json") {
			skipped++
			continue
		}
		result, ok := normalizeAccountPoolCheckResultPayload(update.Result, now)
		if !ok {
			skipped++
			continue
		}
		resultJSON, errMarshal := json.Marshal(result)
		if errMarshal != nil {
			skipped++
			continue
		}
		res, errExec := stmt.Exec(string(resultJSON), strings.TrimSpace(update.ContentHash), now.Format(time.RFC3339), name)
		if errExec != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to update account pool check result %s: %v", name, errExec)})
			return
		}
		rowsAffected, _ := res.RowsAffected()
		if rowsAffected == 0 {
			missing = append(missing, name)
			continue
		}
		updated++
	}
	if err = tx.Commit(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to commit account pool check results: %v", err)})
		return
	}
	committed = true
	c.JSON(http.StatusOK, gin.H{"status": "ok", "updated": updated, "skipped": skipped, "missing": missing})
}

func (h *Handler) listAccountPoolSQLiteEntrySummaries(includeHash bool) ([]gin.H, []accountPoolFolderInfo, error) {
	db, err := h.openAccountPoolSQLiteLocked()
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if errClose := db.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close account pool sqlite database")
		}
	}()

	folderInfos, errFolders := listAccountPoolFoldersFromDB(db)
	if errFolders != nil {
		return nil, nil, errFolders
	}

	rows, err := db.Query(`
SELECT name, content_hash, COALESCE(type, ''), COALESCE(provider, ''), COALESCE(email, ''), COALESCE(folder, ''), size, COALESCE(check_result, ''), COALESCE(check_content_hash, ''), COALESCE(check_updated_at, '')
FROM account_pool_entries
ORDER BY lower(name)`)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to query account pool sqlite summaries: %w", err)
	}
	defer func() {
		if errClose := rows.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close account pool sqlite summary rows")
		}
	}()

	files := make([]gin.H, 0)
	folderUsage := make(map[string]accountPoolUsageSummary)
	for rows.Next() {
		var name, contentHash, typeValue, provider, email, folder, checkResult, checkContentHash, checkUpdatedAt string
		var size int
		if errScan := rows.Scan(&name, &contentHash, &typeValue, &provider, &email, &folder, &size, &checkResult, &checkContentHash, &checkUpdatedAt); errScan != nil {
			return nil, nil, fmt.Errorf("failed to scan account pool sqlite summary: %w", errScan)
		}
		name = normalizeAccountPoolEntryName(name)
		if name == "" || isUnsafeAccountPoolEntryName(name) || !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		entryFolder := accountPoolFolderFromStoredEntry(name, folder)
		entry := gin.H{
			"id":         name,
			"auth_id":    name,
			"auth_index": name,
			"name":       name,
			"size":       size,
			"source":     "account_pool",
			"folder":     entryFolder,
		}
		if typeValue != "" {
			entry["type"] = typeValue
		}
		if provider != "" {
			entry["provider"] = provider
		} else if typeValue != "" {
			entry["provider"] = typeValue
		}
		if email != "" {
			entry["email"] = email
		}
		summary := accountPoolUsage.SummaryForAccountPoolEntry(name, email)
		applyAccountPoolUsageSummary(entry, summary)
		accumulateAccountPoolFolderUsage(folderUsage, entryFolder, summary)
		if includeHash {
			entry["content_hash"] = contentHash
		}
		applyAccountPoolCheckResult(entry, checkResult, checkContentHash, checkUpdatedAt, contentHash)
		files = append(files, entry)
	}
	if errRows := rows.Err(); errRows != nil {
		return nil, nil, fmt.Errorf("failed to read account pool sqlite summaries: %w", errRows)
	}
	applyAccountPoolFolderUsageSummaries(folderInfos, folderUsage)
	return files, folderInfos, nil
}

func applyAccountPoolUsageSummary(entry gin.H, summary accountPoolUsageSummary) {
	if summary.Requests <= 0 &&
		summary.Successes <= 0 &&
		summary.Failures <= 0 &&
		summary.InputTokens <= 0 &&
		summary.OutputTokens <= 0 &&
		summary.CachedTokens <= 0 &&
		summary.CacheReadTokens <= 0 &&
		summary.CacheCreationTokens <= 0 &&
		summary.TotalTokens <= 0 {
		return
	}
	entry["usage_requests"] = summary.Requests
	entry["usage_successes"] = summary.Successes
	entry["usage_failures"] = summary.Failures
	entry["usage_input_tokens"] = summary.InputTokens
	entry["usage_output_tokens"] = summary.OutputTokens
	entry["usage_cached_tokens"] = summary.CachedTokens
	entry["usage_cache_read_tokens"] = summary.CacheReadTokens
	entry["usage_cache_creation_tokens"] = summary.CacheCreationTokens
	entry["usage_total_tokens"] = summary.TotalTokens
	if summary.LastUsedAt != "" {
		entry["usage_last_used_at"] = summary.LastUsedAt
	}
}

func accumulateAccountPoolFolderUsage(folderUsage map[string]accountPoolUsageSummary, folder string, summary accountPoolUsageSummary) {
	if folderUsage == nil {
		return
	}
	folder = normalizeAccountPoolFolder(folder)
	if folder == "" {
		folder = defaultAccountPoolFolder()
	}
	current := folderUsage[folder]
	mergeAccountPoolUsageSummary(&current, summary)
	folderUsage[folder] = current
}

func applyAccountPoolFolderUsageSummaries(folderInfos []accountPoolFolderInfo, folderUsage map[string]accountPoolUsageSummary) {
	if len(folderInfos) == 0 || len(folderUsage) == 0 {
		return
	}
	for index := range folderInfos {
		folder := normalizeAccountPoolFolder(folderInfos[index].Folder)
		summary := folderUsage[folder]
		folderInfos[index].Requests = summary.Requests
		folderInfos[index].TotalTokens = summary.TotalTokens
	}
}

func (h *Handler) listAccountPoolFolders() ([]accountPoolFolderInfo, error) {
	db, err := h.openAccountPoolSQLiteLocked()
	if err != nil {
		return nil, err
	}
	defer func() {
		if errClose := db.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close account pool sqlite database")
		}
	}()
	return listAccountPoolFoldersFromDB(db)
}

func listAccountPoolFoldersFromDB(db *sql.DB) ([]accountPoolFolderInfo, error) {
	rows, err := db.Query(`
SELECT e.name, COALESCE(e.folder, ''), COALESCE(f.source_model, ''), COALESCE(f.source_info, ''), COALESCE(f.created_at, ''), COALESCE(f.updated_at, '')
FROM account_pool_entries e
LEFT JOIN account_pool_folders f ON f.folder = e.folder
ORDER BY lower(e.name)`)
	if err != nil {
		return nil, fmt.Errorf("failed to query account pool folders: %w", err)
	}
	defer func() {
		if errClose := rows.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close account pool folder info rows")
		}
	}()
	foldersByName := make(map[string]*accountPoolFolderInfo)
	for rows.Next() {
		var item accountPoolFolderInfo
		var name string
		if errScan := rows.Scan(&name, &item.Folder, &item.SourceModel, &item.SourceInfo, &item.CreatedAt, &item.UpdatedAt); errScan != nil {
			return nil, fmt.Errorf("failed to scan account pool folder info: %w", errScan)
		}
		item.Folder = accountPoolFolderFromStoredEntry(name, item.Folder)
		if item.Folder == "" {
			continue
		}
		if existing, ok := foldersByName[item.Folder]; ok {
			existing.Count++
			if existing.SourceModel == "" {
				existing.SourceModel = item.SourceModel
			}
			if existing.SourceInfo == "" {
				existing.SourceInfo = item.SourceInfo
			}
			continue
		}
		item.Count = 1
		copyItem := item
		foldersByName[item.Folder] = &copyItem
	}
	if errRows := rows.Err(); errRows != nil {
		return nil, fmt.Errorf("failed to read account pool folder info: %w", errRows)
	}
	folders := make([]accountPoolFolderInfo, 0, len(foldersByName))
	for _, item := range foldersByName {
		folders = append(folders, *item)
	}
	sort.Slice(folders, func(i, j int) bool {
		return strings.ToLower(folders[i].Folder) < strings.ToLower(folders[j].Folder)
	})
	return folders, nil
}

func (h *Handler) PatchAccountPoolFolder(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}
	var req struct {
		Folder      string `json:"folder"`
		SourceModel string `json:"source_model"`
		SourceInfo  string `json:"source_info"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	folder := normalizeAccountPoolFolder(req.Folder)
	db, err := h.openAccountPoolSQLiteLocked()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer func() {
		if errClose := db.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close account pool sqlite database")
		}
	}()
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err = db.Exec(`
INSERT INTO account_pool_folders (folder, source_model, source_info, created_at, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(folder) DO UPDATE SET
	source_model=excluded.source_model,
	source_info=excluded.source_info,
	updated_at=excluded.updated_at`, folder, strings.TrimSpace(req.SourceModel), strings.TrimSpace(req.SourceInfo), now, now); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "folder": folder})
}

func (h *Handler) storeAccountPoolImportJob(job *accountPoolImportJob) {
	if h == nil || job == nil {
		return
	}
	h.accountPoolJobsMu.Lock()
	defer h.accountPoolJobsMu.Unlock()
	h.accountPoolJobs[job.ID] = cloneAccountPoolImportJob(job)
}

func (h *Handler) updateAccountPoolImportJob(id string, update func(*accountPoolImportJob)) {
	if h == nil {
		return
	}
	h.accountPoolJobsMu.Lock()
	defer h.accountPoolJobsMu.Unlock()
	job := h.accountPoolJobs[id]
	if job == nil {
		return
	}
	update(job)
	job.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
}

func (h *Handler) getAccountPoolImportJob(id string) *accountPoolImportJob {
	if h == nil {
		return nil
	}
	h.accountPoolJobsMu.Lock()
	defer h.accountPoolJobsMu.Unlock()
	return cloneAccountPoolImportJob(h.accountPoolJobs[id])
}

func cloneAccountPoolImportJob(job *accountPoolImportJob) *accountPoolImportJob {
	if job == nil {
		return nil
	}
	out := *job
	out.Files = append([]string(nil), job.Files...)
	out.Failures = append([]authUploadFailure(nil), job.Failures...)
	return &out
}

func (h *Handler) GetAccountPoolImport(c *gin.Context) {
	job := h.getAccountPoolImportJob(c.Param("id"))
	if job == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"job": job})
}

func (h *Handler) runAccountPoolImportJob(jobID string, uploads []accountPoolPendingUpload) {
	h.updateAccountPoolImportJob(jobID, func(job *accountPoolImportJob) {
		job.Status = accountPoolImportRunning
		job.Total = len(uploads)
	})

	archiveFiles := make([]accountPoolArchiveFile, 0, len(uploads))
	for _, upload := range uploads {
		names, failures, updates, errUpload := h.readAccountPoolUploadData(upload.Name, upload.Data)
		h.updateAccountPoolImportJob(jobID, func(job *accountPoolImportJob) {
			job.Done++
			if errUpload != nil {
				if errors.Is(errUpload, errAuthFileMustBeJSON) {
					job.Skipped++
					return
				}
				job.Failed++
				job.Failures = append(job.Failures, authUploadFailure{Name: upload.DisplayName, Error: uploadErrorMessage(errUpload)})
				return
			}
			job.Imported += len(names)
			job.Files = append(job.Files, names...)
			for _, failure := range failures {
				job.Failed++
				job.Failures = append(job.Failures, failure)
			}
		})
		if errUpload != nil {
			continue
		}
		archiveFiles = append(archiveFiles, updates...)
	}
	if len(archiveFiles) > 0 {
		if errArchive := h.upsertAccountPoolArchiveFiles(archiveFiles); errArchive != nil {
			h.updateAccountPoolImportJob(jobID, func(job *accountPoolImportJob) {
				job.Status = accountPoolImportFailed
				job.Error = errArchive.Error()
			})
			return
		}
	}
	h.updateAccountPoolImportJob(jobID, func(job *accountPoolImportJob) {
		if job.Imported == 0 && job.Failed > 0 {
			job.Status = accountPoolImportFailed
			job.Error = "all account pool files failed to import"
			return
		}
		if job.Imported == 0 && job.Skipped > 0 {
			job.Status = accountPoolImportFailed
			job.Error = "no importable account pool files found"
			return
		}
		job.Status = accountPoolImportDone
	})
}

func (h *Handler) UploadAccountPoolEntries(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}
	fileHeaders, errMultipart := h.multipartAuthFileHeaders(c)
	if errMultipart != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid multipart form: %v", errMultipart)})
		return
	}
	if len(fileHeaders) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no files uploaded"})
		return
	}
	if strings.EqualFold(strings.TrimSpace(c.Query("async")), "true") {
		uploads, errRead := readAccountPoolPendingUploads(fileHeaders)
		if errRead != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": errRead.Error()})
			return
		}
		now := time.Now().UTC()
		job := &accountPoolImportJob{
			ID:        fmt.Sprintf("account-pool-import-%d", now.UnixNano()),
			Status:    accountPoolImportPending,
			Total:     len(uploads),
			CreatedAt: now.Format(time.RFC3339),
			UpdatedAt: now.Format(time.RFC3339),
		}
		h.storeAccountPoolImportJob(job)
		go h.runAccountPoolImportJob(job.ID, uploads)
		c.JSON(http.StatusAccepted, gin.H{"job": job})
		return
	}
	uploaded := make([]string, 0, len(fileHeaders))
	failed := make([]gin.H, 0)
	archiveFiles := make([]accountPoolArchiveFile, 0, len(fileHeaders))
	for _, file := range fileHeaders {
		names, failures, updates, errUpload := h.readUploadedAccountPoolFiles(file)
		if errUpload != nil {
			if errors.Is(errUpload, errAuthFileMustBeJSON) {
				continue
			}
			failed = append(failed, gin.H{"name": uploadedFileDisplayName(file), "error": uploadErrorMessage(errUpload)})
			continue
		}
		uploaded = append(uploaded, names...)
		archiveFiles = append(archiveFiles, updates...)
		for _, failure := range failures {
			failed = append(failed, gin.H{"name": failure.Name, "error": failure.Error})
		}
	}
	if len(archiveFiles) > 0 {
		if errArchive := h.upsertAccountPoolArchiveFiles(archiveFiles); errArchive != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": errArchive.Error()})
			return
		}
	}
	if len(failed) > 0 {
		c.JSON(http.StatusMultiStatus, gin.H{
			"status":   "partial",
			"uploaded": len(uploaded),
			"files":    uploaded,
			"failed":   failed,
		})
		return
	}
	if len(uploaded) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no json account pool files uploaded"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "uploaded": len(uploaded), "files": uploaded})
}

func (h *Handler) WriteAccountPoolToAuthFiles(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req struct {
		Names []string `json:"names"`
		Mode  string   `json:"mode"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	names := uniqueAuthFileNames(req.Names)
	if len(names) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no account pool entries specified"})
		return
	}

	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode == "" {
		mode = "append"
	}
	if mode != "append" && mode != "overwrite" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "mode must be append or overwrite"})
		return
	}

	entries, err := h.readAccountPoolArchive()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	candidates := make([]accountPoolAuthWriteCandidate, 0, len(names))
	failed := make([]gin.H, 0)
	targets := make(map[string]string, len(names))
	for _, rawName := range names {
		name := normalizeAccountPoolEntryName(rawName)
		if isUnsafeAccountPoolEntryName(name) || !strings.HasSuffix(strings.ToLower(name), ".json") {
			failed = append(failed, gin.H{"name": rawName, "error": "invalid name"})
			continue
		}
		data, ok := entries[name]
		if !ok {
			failed = append(failed, gin.H{"name": name, "error": "account pool entry not found"})
			continue
		}
		targetName := filepath.Base(name)
		if previous, exists := targets[strings.ToLower(targetName)]; exists {
			failed = append(failed, gin.H{
				"name":  name,
				"error": fmt.Sprintf("duplicate auth file name %s also used by %s", targetName, previous),
			})
			continue
		}
		targets[strings.ToLower(targetName)] = name
		targetPath := filepath.Join(h.cfg.AuthDir, targetName)
		if !filepath.IsAbs(targetPath) {
			if abs, errAbs := filepath.Abs(targetPath); errAbs == nil {
				targetPath = abs
			}
		}
		auth, errBuild := h.buildAuthFromFileData(targetPath, data)
		if errBuild != nil {
			failed = append(failed, gin.H{"name": name, "error": errBuild.Error()})
			continue
		}
		candidates = append(candidates, accountPoolAuthWriteCandidate{
			SourceName: name,
			TargetName: targetName,
			TargetPath: targetPath,
			Data:       data,
			Auth:       auth,
		})
	}

	ctx := c.Request.Context()
	if mode == "overwrite" {
		if errDelete := h.deleteAllAuthFilesForOverwrite(ctx); errDelete != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": errDelete.Error()})
			return
		}
	}

	written, writeFailures := h.writeAccountPoolAuthCandidates(ctx, candidates)
	failed = append(failed, writeFailures...)
	if len(written) > 0 {
		auths := make([]*coreauth.Auth, 0, len(written))
		for _, candidate := range written {
			auths = append(auths, candidate.Auth)
		}
		h.authManager.UpsertMany(coreauth.WithSkipPersist(ctx), auths)
	}

	files := make([]string, 0, len(written))
	for _, candidate := range written {
		files = append(files, candidate.SourceName)
	}
	if len(failed) > 0 {
		c.JSON(http.StatusMultiStatus, gin.H{
			"status":   "partial",
			"uploaded": len(files),
			"files":    files,
			"failed":   failed,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "uploaded": len(files), "files": files})
}

func (h *Handler) deleteAllAuthFilesForOverwrite(ctx context.Context) error {
	if h == nil || h.authManager == nil {
		return nil
	}
	auths := h.authManager.List()
	ids := make([]string, 0, len(auths))
	syntheticIDs := make([]string, 0)
	for _, auth := range auths {
		if auth == nil || strings.TrimSpace(auth.ID) == "" {
			continue
		}
		if h.isAccountPoolSyntheticAuth(auth) {
			syntheticIDs = append(syntheticIDs, auth.ID)
			continue
		}
		ids = append(ids, auth.ID)
	}
	if len(ids) > 0 {
		if _, failures := h.authManager.DeleteAuthFiles(ctx, ids); len(failures) > 0 {
			messages := make([]string, 0, len(failures))
			for _, failure := range failures {
				if failure != nil {
					messages = append(messages, failure.Error())
				}
			}
			return fmt.Errorf("failed to delete existing auth files: %s", strings.Join(messages, "; "))
		}
	}

	if strings.TrimSpace(h.cfg.AuthDir) == "" {
		return nil
	}
	entries, err := os.ReadDir(h.cfg.AuthDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read auth dir: %w", err)
	}
	for _, entry := range entries {
		if entry == nil || entry.IsDir() {
			continue
		}
		name := entry.Name()
		lowerName := strings.ToLower(name)
		if !strings.HasSuffix(lowerName, ".json") || strings.HasPrefix(lowerName, "account-pool.") {
			continue
		}
		if errRemove := os.Remove(filepath.Join(h.cfg.AuthDir, name)); errRemove != nil && !os.IsNotExist(errRemove) {
			return fmt.Errorf("failed to remove stale auth file %s: %w", name, errRemove)
		}
	}
	if len(ids) > 0 {
		h.authManager.RemoveAuths(ids)
	}
	if len(syntheticIDs) > 0 {
		h.authManager.RemoveAuths(syntheticIDs)
	}
	return nil
}

func (h *Handler) writeAccountPoolAuthCandidates(ctx context.Context, candidates []accountPoolAuthWriteCandidate) ([]accountPoolAuthWriteCandidate, []gin.H) {
	if len(candidates) == 0 {
		return nil, nil
	}
	if err := os.MkdirAll(h.cfg.AuthDir, 0o700); err != nil {
		failed := make([]gin.H, 0, len(candidates))
		for _, candidate := range candidates {
			failed = append(failed, gin.H{"name": candidate.SourceName, "error": err.Error()})
		}
		return nil, failed
	}

	type writeResult struct {
		candidate accountPoolAuthWriteCandidate
		err       error
	}
	workers := authZipLoadConcurrency(len(candidates))
	jobs := make(chan accountPoolAuthWriteCandidate)
	results := make(chan writeResult, len(candidates))

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for candidate := range jobs {
				select {
				case <-ctx.Done():
					results <- writeResult{candidate: candidate, err: ctx.Err()}
					continue
				default:
				}
				err := os.WriteFile(candidate.TargetPath, candidate.Data, 0o600)
				if err != nil {
					err = fmt.Errorf("failed to write file: %w", err)
				}
				results <- writeResult{candidate: candidate, err: err}
			}
		}()
	}
	for _, candidate := range candidates {
		jobs <- candidate
	}
	close(jobs)
	wg.Wait()
	close(results)

	written := make([]accountPoolAuthWriteCandidate, 0, len(candidates))
	failed := make([]gin.H, 0)
	for result := range results {
		if result.err != nil {
			failed = append(failed, gin.H{"name": result.candidate.SourceName, "error": result.err.Error()})
			continue
		}
		written = append(written, result.candidate)
	}
	sort.Slice(written, func(i, j int) bool {
		return strings.ToLower(written[i].SourceName) < strings.ToLower(written[j].SourceName)
	})
	return written, failed
}

func (h *Handler) DownloadAccountPoolEntry(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}
	name := normalizeAccountPoolEntryName(c.Query("name"))
	if isUnsafeAccountPoolEntryName(name) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid name"})
		return
	}
	if !strings.HasSuffix(strings.ToLower(name), ".json") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name must end with .json"})
		return
	}

	data, err := h.readAccountPoolArchiveEntry(name)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if len(bytes.TrimSpace(data)) == 0 {
		data, name, err = h.findAccountPoolEntryByName(name)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}
	if len(bytes.TrimSpace(data)) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "account pool entry not found"})
		return
	}
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", name))
	c.Data(http.StatusOK, "application/json", data)
}

func (h *Handler) findAccountPoolEntryByName(name string) ([]byte, string, error) {
	name = normalizeAccountPoolEntryName(name)
	if name == "" {
		return nil, "", nil
	}
	entries, err := h.readAccountPoolArchive()
	if err != nil {
		return nil, "", err
	}
	if data := bytes.TrimSpace(entries[name]); len(data) > 0 {
		return append([]byte(nil), data...), name, nil
	}
	base := path.Base(name)
	type match struct {
		name string
		data []byte
	}
	matches := make([]match, 0, 2)
	for entryName, data := range entries {
		entryName = normalizeAccountPoolEntryName(entryName)
		if entryName == "" {
			continue
		}
		if strings.EqualFold(entryName, name) || strings.EqualFold(path.Base(entryName), base) || strings.HasSuffix(strings.ToLower(entryName), "/"+strings.ToLower(base)) {
			if trimmed := bytes.TrimSpace(data); len(trimmed) > 0 {
				matches = append(matches, match{name: entryName, data: append([]byte(nil), trimmed...)})
			}
		}
	}
	if len(matches) == 0 {
		return nil, "", nil
	}
	sort.Slice(matches, func(i, j int) bool {
		if len(matches[i].name) != len(matches[j].name) {
			return len(matches[i].name) < len(matches[j].name)
		}
		return strings.ToLower(matches[i].name) < strings.ToLower(matches[j].name)
	})
	return matches[0].data, matches[0].name, nil
}

func (h *Handler) WriteAccountPoolEntriesToAuthFiles(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	var req struct {
		Names     []string `json:"names"`
		Overwrite bool     `json:"overwrite"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	names := uniqueAuthFileNames(req.Names)
	if len(names) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "names are required"})
		return
	}

	ctx := c.Request.Context()
	if req.Overwrite {
		entries, errRead := os.ReadDir(h.cfg.AuthDir)
		if errRead != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to read auth dir: %v", errRead)})
			return
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
				continue
			}
			full := filepath.Join(h.cfg.AuthDir, e.Name())
			if errRemove := os.Remove(full); errRemove != nil && !os.IsNotExist(errRemove) {
				c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to delete auth file %s: %v", e.Name(), errRemove)})
				return
			}
			if errDel := h.deleteTokenRecord(ctx, full); errDel != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": errDel.Error()})
				return
			}
			h.removeAuthRuntime(full)
		}
	}

	uploaded := make([]string, 0, len(names))
	failed := make([]gin.H, 0)
	usedTargetNames := make(map[string]int, len(names))
	for _, requestedName := range names {
		name := normalizeAccountPoolEntryName(requestedName)
		if name == "" || isUnsafeAccountPoolEntryName(name) || !strings.HasSuffix(strings.ToLower(name), ".json") {
			failed = append(failed, gin.H{"name": requestedName, "error": "invalid name"})
			continue
		}
		data, resolvedName, errRead := h.findAccountPoolEntryByName(name)
		if errRead != nil {
			failed = append(failed, gin.H{"name": requestedName, "error": errRead.Error()})
			continue
		}
		if len(bytes.TrimSpace(data)) == 0 {
			failed = append(failed, gin.H{"name": requestedName, "error": "account pool entry not found"})
			continue
		}
		targetName := filepath.Base(resolvedName)
		if targetName == "." || targetName == "" {
			targetName = filepath.Base(name)
		}
		targetName = uniqueAccountPoolAuthFileName(targetName, resolvedName, usedTargetNames)
		if errWrite := h.writeAuthFileWithArchive(ctx, targetName, data, false); errWrite != nil {
			failed = append(failed, gin.H{"name": requestedName, "error": errWrite.Error()})
			continue
		}
		uploaded = append(uploaded, targetName)
	}
	if len(failed) > 0 {
		c.JSON(http.StatusMultiStatus, gin.H{
			"status":   "partial",
			"uploaded": len(uploaded),
			"files":    uploaded,
			"failed":   failed,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "uploaded": len(uploaded), "files": uploaded})
}

func uniqueAccountPoolAuthFileName(baseName, sourceName string, used map[string]int) string {
	baseName = filepath.Base(strings.TrimSpace(baseName))
	if baseName == "." || baseName == "" {
		baseName = "account.json"
	}
	if used == nil {
		return baseName
	}
	key := strings.ToLower(baseName)
	if used[key] == 0 {
		used[key] = 1
		return baseName
	}
	used[key]++
	ext := filepath.Ext(baseName)
	stem := strings.TrimSuffix(baseName, ext)
	prefix := sanitizeAccountPoolAuthFileStem(sourceName)
	if prefix != "" && !strings.EqualFold(prefix, stem) {
		stem = prefix + "_" + stem
	}
	nextName := fmt.Sprintf("%s_%d%s", stem, used[key], ext)
	for used[strings.ToLower(nextName)] > 0 {
		used[key]++
		nextName = fmt.Sprintf("%s_%d%s", stem, used[key], ext)
	}
	used[strings.ToLower(nextName)] = 1
	return nextName
}

func sanitizeAccountPoolAuthFileStem(name string) string {
	name = normalizeAccountPoolEntryName(name)
	name = strings.TrimSuffix(name, path.Ext(name))
	name = strings.Trim(name, "/")
	if name == "" {
		return ""
	}
	replacer := strings.NewReplacer("/", "_", "\\", "_", " ", "_", ":", "_", "*", "_", "?", "_", "\"", "_", "<", "_", ">", "_", "|", "_")
	stem := replacer.Replace(name)
	stem = strings.Trim(stem, "._-")
	if len(stem) > 80 {
		stem = stem[len(stem)-80:]
	}
	return stem
}

func (h *Handler) DeleteAccountPoolEntries(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}

	names, err := requestedAuthFileNamesForDelete(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if len(names) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no account pool entries specified"})
		return
	}

	entries, err := h.readAccountPoolArchive()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	deleted := make([]string, 0, len(names))
	deletedEntries := make(map[string][]byte, len(names))
	failed := make([]gin.H, 0)
	for _, name := range names {
		name = normalizeAccountPoolEntryName(name)
		if isUnsafeAccountPoolEntryName(name) || !strings.HasSuffix(strings.ToLower(name), ".json") {
			failed = append(failed, gin.H{"name": name, "error": "invalid name"})
			continue
		}
		if data, ok := entries[name]; ok {
			deletedEntries[name] = data
			delete(entries, name)
			deleted = append(deleted, name)
		}
	}

	if len(deleted) > 0 {
		if err := h.writeAccountPoolArchive(entries); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		accountPoolUsage.RemoveAccountPoolEntries(deleted, deletedEntries)
	}

	if len(failed) > 0 {
		c.JSON(http.StatusMultiStatus, gin.H{
			"status":  "partial",
			"deleted": len(deleted),
			"files":   deleted,
			"failed":  failed,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok", "deleted": len(deleted), "files": deleted})
}

func buildAccountPoolArchiveName() string {
	return fmt.Sprintf("account-pool-%s.zip", time.Now().Format("20060102-150405"))
}

func requestedAuthFileNamesForDelete(c *gin.Context) ([]string, error) {
	if c == nil {
		return nil, nil
	}
	names := uniqueAuthFileNames(c.QueryArray("name"))
	if len(names) > 0 {
		return names, nil
	}

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read body")
	}
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, nil
	}

	var objectBody struct {
		Name  string   `json:"name"`
		Names []string `json:"names"`
	}
	if body[0] == '[' {
		var arrayBody []string
		if err := json.Unmarshal(body, &arrayBody); err != nil {
			return nil, fmt.Errorf("invalid request body")
		}
		return uniqueAuthFileNames(arrayBody), nil
	}
	if err := json.Unmarshal(body, &objectBody); err != nil {
		return nil, fmt.Errorf("invalid request body")
	}

	out := make([]string, 0, len(objectBody.Names)+1)
	if strings.TrimSpace(objectBody.Name) != "" {
		out = append(out, objectBody.Name)
	}
	out = append(out, objectBody.Names...)
	return uniqueAuthFileNames(out), nil
}

func uniqueAuthFileNames(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

func (h *Handler) deleteAuthFileByName(ctx context.Context, name string) (string, int, error) {
	name = strings.TrimSpace(name)
	if isUnsafeAuthFileDeleteName(name) {
		return "", http.StatusBadRequest, fmt.Errorf("invalid name")
	}

	targetPath, errPath := safeAuthFileDeletePath(h.cfg.AuthDir, name)
	if errPath != nil {
		return "", http.StatusBadRequest, errPath
	}
	targetID := ""
	if targetAuth := h.findAuthForDelete(name); targetAuth != nil {
		targetID = strings.TrimSpace(targetAuth.ID)
		if path := strings.TrimSpace(authAttribute(targetAuth, "path")); path != "" {
			targetPath = path
		}
	}
	if !filepath.IsAbs(targetPath) {
		if abs, errAbs := filepath.Abs(targetPath); errAbs == nil {
			targetPath = abs
		}
	}
	if errRemove := os.Remove(targetPath); errRemove != nil {
		if os.IsNotExist(errRemove) {
			return name, http.StatusNotFound, errAuthFileNotFound
		}
		return name, http.StatusInternalServerError, fmt.Errorf("failed to remove file: %w", errRemove)
	}
	if errDeleteRecord := h.deleteTokenRecord(ctx, targetPath); errDeleteRecord != nil {
		return name, http.StatusInternalServerError, errDeleteRecord
	}
	if targetID != "" {
		h.removeAuthRuntime(targetID)
	} else {
		h.removeAuthRuntime(targetPath)
	}
	return name, http.StatusOK, nil
}

func (h *Handler) findAuthForDelete(name string) *coreauth.Auth {
	if h == nil || h.authManager == nil {
		return nil
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	cleanName := filepath.Clean(filepath.FromSlash(name))
	baseName := filepath.Base(cleanName)
	if auth, ok := h.authManager.GetByID(name); ok {
		return auth
	}
	if cleanName != name {
		if auth, ok := h.authManager.GetByID(cleanName); ok {
			return auth
		}
	}
	auths := h.authManager.List()
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		fileName := strings.TrimSpace(auth.FileName)
		cleanFileName := filepath.Clean(filepath.FromSlash(fileName))
		if fileName == name || cleanFileName == cleanName {
			return auth
		}
		authPath := strings.TrimSpace(authAttribute(auth, "path"))
		if filepath.Base(authPath) == baseName {
			if cleanName == baseName {
				return auth
			}
			if h != nil && h.cfg != nil {
				if expectedPath, errPath := safeAuthFileDeletePath(h.cfg.AuthDir, name); errPath == nil && filepath.Clean(authPath) == filepath.Clean(expectedPath) {
					return auth
				}
			}
		}
		if authPath != "" && filepath.Clean(authPath) == cleanName {
			return auth
		}
	}
	return nil
}

func (h *Handler) authIDForPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		if abs, errAbs := filepath.Abs(path); errAbs == nil {
			path = abs
		}
	}
	id := path
	if h != nil && h.cfg != nil {
		authDir := strings.TrimSpace(h.cfg.AuthDir)
		if resolvedAuthDir, errResolve := util.ResolveAuthDir(authDir); errResolve == nil && resolvedAuthDir != "" {
			authDir = resolvedAuthDir
		}
		if authDir != "" {
			authDir = filepath.Clean(authDir)
			if !filepath.IsAbs(authDir) {
				if abs, errAbs := filepath.Abs(authDir); errAbs == nil {
					authDir = abs
				}
			}
			if rel, errRel := filepath.Rel(authDir, path); errRel == nil && rel != "" {
				id = rel
			}
		}
	}
	// On Windows, normalize ID casing to avoid duplicate auth entries caused by case-insensitive paths.
	if runtime.GOOS == "windows" {
		id = strings.ToLower(id)
	}
	return id
}

func (h *Handler) registerAuthFromFile(ctx context.Context, path string, data []byte) error {
	if h.authManager == nil {
		return nil
	}
	auth, err := h.buildAuthFromFileData(path, data)
	if err != nil {
		return err
	}
	return h.upsertAuthRecord(ctx, auth)
}

func (h *Handler) buildAuthFromFileData(path string, data []byte) (*coreauth.Auth, error) {
	if path == "" {
		return nil, fmt.Errorf("auth path is empty")
	}
	if data == nil {
		var err error
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("failed to read auth file: %w", err)
		}
	}
	metadata := make(map[string]any)
	if err := json.Unmarshal(data, &metadata); err != nil {
		return nil, fmt.Errorf("invalid auth file: %w", err)
	}
	provider, _ := metadata["type"].(string)
	if provider == "" {
		provider = "unknown"
	}
	label := provider
	if email, ok := metadata["email"].(string); ok && email != "" {
		label = email
	}
	lastRefresh, hasLastRefresh := extractLastRefreshTimestamp(metadata)

	authID := h.authIDForPath(path)
	if authID == "" {
		authID = path
	}
	attr := map[string]string{
		"path":   path,
		"source": path,
	}
	auth := &coreauth.Auth{
		ID:         authID,
		Provider:   provider,
		FileName:   filepath.Base(path),
		Label:      label,
		Status:     coreauth.StatusActive,
		Attributes: attr,
		Metadata:   metadata,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	if hasLastRefresh {
		auth.LastRefreshedAt = lastRefresh
	}
	if h != nil && h.authManager != nil {
		if existing, ok := h.authManager.GetByID(authID); ok {
			auth.CreatedAt = existing.CreatedAt
			if !hasLastRefresh {
				auth.LastRefreshedAt = existing.LastRefreshedAt
			}
			auth.NextRefreshAfter = existing.NextRefreshAfter
			auth.Runtime = existing.Runtime
		}
	}
	coreauth.ApplyCustomHeadersFromMetadata(auth)
	return auth, nil
}

func (h *Handler) upsertAuthRecord(ctx context.Context, auth *coreauth.Auth) error {
	if h == nil || h.authManager == nil || auth == nil {
		return nil
	}
	if existing, ok := h.authManager.GetByID(auth.ID); ok {
		auth.CreatedAt = existing.CreatedAt
		_, err := h.authManager.Update(ctx, auth)
		return err
	}
	_, err := h.authManager.Register(ctx, auth)
	return err
}

// PatchAuthFileStatus toggles the disabled state of an auth file
func (h *Handler) PatchAuthFileStatus(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req struct {
		Name     string `json:"name"`
		Disabled *bool  `json:"disabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	if req.Disabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "disabled is required"})
		return
	}

	ctx := c.Request.Context()

	// Find auth by name or ID
	var targetAuth *coreauth.Auth
	if auth, ok := h.authManager.GetByID(name); ok {
		targetAuth = auth
	} else {
		auths := h.authManager.List()
		for _, auth := range auths {
			if auth.FileName == name {
				targetAuth = auth
				break
			}
		}
	}

	if targetAuth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}

	// Update disabled state
	targetAuth.Disabled = *req.Disabled
	if *req.Disabled {
		targetAuth.Status = coreauth.StatusDisabled
		targetAuth.StatusMessage = "disabled via management API"
	} else {
		targetAuth.Status = coreauth.StatusActive
		targetAuth.StatusMessage = ""
	}
	targetAuth.UpdatedAt = time.Now()

	if _, err := h.authManager.Update(ctx, targetAuth); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to update auth: %v", err)})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok", "disabled": *req.Disabled})
}

// PatchAuthFileFields updates editable fields (prefix, proxy_url, headers, priority, note) of an auth file.
func (h *Handler) PatchAuthFileFields(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req struct {
		Name     string            `json:"name"`
		Prefix   *string           `json:"prefix"`
		ProxyURL *string           `json:"proxy_url"`
		Headers  map[string]string `json:"headers"`
		Priority *int              `json:"priority"`
		Note     *string           `json:"note"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}

	ctx := c.Request.Context()

	// Find auth by name or ID
	var targetAuth *coreauth.Auth
	if auth, ok := h.authManager.GetByID(name); ok {
		targetAuth = auth
	} else {
		auths := h.authManager.List()
		for _, auth := range auths {
			if auth.FileName == name {
				targetAuth = auth
				break
			}
		}
	}

	if targetAuth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}

	changed := false
	if req.Prefix != nil {
		prefix := strings.TrimSpace(*req.Prefix)
		targetAuth.Prefix = prefix
		if targetAuth.Metadata == nil {
			targetAuth.Metadata = make(map[string]any)
		}
		if prefix == "" {
			delete(targetAuth.Metadata, "prefix")
		} else {
			targetAuth.Metadata["prefix"] = prefix
		}
		changed = true
	}
	if req.ProxyURL != nil {
		proxyURL := strings.TrimSpace(*req.ProxyURL)
		targetAuth.ProxyURL = proxyURL
		if targetAuth.Metadata == nil {
			targetAuth.Metadata = make(map[string]any)
		}
		if proxyURL == "" {
			delete(targetAuth.Metadata, "proxy_url")
		} else {
			targetAuth.Metadata["proxy_url"] = proxyURL
		}
		changed = true
	}
	if len(req.Headers) > 0 {
		existingHeaders := coreauth.ExtractCustomHeadersFromMetadata(targetAuth.Metadata)
		nextHeaders := make(map[string]string, len(existingHeaders))
		for k, v := range existingHeaders {
			nextHeaders[k] = v
		}
		headerChanged := false

		for key, value := range req.Headers {
			name := strings.TrimSpace(key)
			if name == "" {
				continue
			}
			val := strings.TrimSpace(value)
			attrKey := "header:" + name
			if val == "" {
				if _, ok := nextHeaders[name]; ok {
					delete(nextHeaders, name)
					headerChanged = true
				}
				if targetAuth.Attributes != nil {
					if _, ok := targetAuth.Attributes[attrKey]; ok {
						headerChanged = true
					}
				}
				continue
			}
			if prev, ok := nextHeaders[name]; !ok || prev != val {
				headerChanged = true
			}
			nextHeaders[name] = val
			if targetAuth.Attributes != nil {
				if prev, ok := targetAuth.Attributes[attrKey]; !ok || prev != val {
					headerChanged = true
				}
			} else {
				headerChanged = true
			}
		}

		if headerChanged {
			if targetAuth.Metadata == nil {
				targetAuth.Metadata = make(map[string]any)
			}
			if targetAuth.Attributes == nil {
				targetAuth.Attributes = make(map[string]string)
			}

			for key, value := range req.Headers {
				name := strings.TrimSpace(key)
				if name == "" {
					continue
				}
				val := strings.TrimSpace(value)
				attrKey := "header:" + name
				if val == "" {
					delete(nextHeaders, name)
					delete(targetAuth.Attributes, attrKey)
					continue
				}
				nextHeaders[name] = val
				targetAuth.Attributes[attrKey] = val
			}

			if len(nextHeaders) == 0 {
				delete(targetAuth.Metadata, "headers")
			} else {
				metaHeaders := make(map[string]any, len(nextHeaders))
				for k, v := range nextHeaders {
					metaHeaders[k] = v
				}
				targetAuth.Metadata["headers"] = metaHeaders
			}
			changed = true
		}
	}
	if req.Priority != nil || req.Note != nil {
		if targetAuth.Metadata == nil {
			targetAuth.Metadata = make(map[string]any)
		}
		if targetAuth.Attributes == nil {
			targetAuth.Attributes = make(map[string]string)
		}

		if req.Priority != nil {
			if *req.Priority == 0 {
				delete(targetAuth.Metadata, "priority")
				delete(targetAuth.Attributes, "priority")
			} else {
				targetAuth.Metadata["priority"] = *req.Priority
				targetAuth.Attributes["priority"] = strconv.Itoa(*req.Priority)
			}
		}
		if req.Note != nil {
			trimmedNote := strings.TrimSpace(*req.Note)
			if trimmedNote == "" {
				delete(targetAuth.Metadata, "note")
				delete(targetAuth.Attributes, "note")
			} else {
				targetAuth.Metadata["note"] = trimmedNote
				targetAuth.Attributes["note"] = trimmedNote
			}
		}
		changed = true
	}

	if !changed {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no fields to update"})
		return
	}

	targetAuth.UpdatedAt = time.Now()

	if _, err := h.authManager.Update(ctx, targetAuth); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to update auth: %v", err)})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func (h *Handler) removeAuthRuntime(id string) {
	if h == nil || h.authManager == nil {
		return
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	if auth, ok := h.authManager.GetByID(id); ok {
		h.authManager.Remove(auth.ID)
		return
	}
	authID := h.authIDForPath(id)
	if authID == "" {
		return
	}
	h.authManager.Remove(authID)
}

func (h *Handler) deleteTokenRecord(ctx context.Context, path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("auth path is empty")
	}
	store := h.tokenStoreWithBaseDir()
	if store == nil {
		return fmt.Errorf("token store unavailable")
	}
	return store.Delete(ctx, path)
}

func (h *Handler) tokenStoreWithBaseDir() coreauth.Store {
	if h == nil {
		return nil
	}
	store := h.tokenStore
	if store == nil {
		store = sdkAuth.GetTokenStore()
		h.tokenStore = store
	}
	if h.cfg != nil {
		if dirSetter, ok := store.(interface{ SetBaseDir(string) }); ok {
			dirSetter.SetBaseDir(h.cfg.AuthDir)
		}
	}
	return store
}

func (h *Handler) saveTokenRecord(ctx context.Context, record *coreauth.Auth) (string, error) {
	if record == nil {
		return "", fmt.Errorf("token record is nil")
	}
	store := h.tokenStoreWithBaseDir()
	if store == nil {
		return "", fmt.Errorf("token store unavailable")
	}
	if h.postAuthHook != nil {
		if err := h.postAuthHook(ctx, record); err != nil {
			return "", fmt.Errorf("post-auth hook failed: %w", err)
		}
	}
	return store.Save(ctx, record)
}

func (h *Handler) RequestAnthropicToken(c *gin.Context) {
	ctx := context.Background()
	ctx = PopulateAuthContext(ctx, c)

	fmt.Println("Initializing Claude authentication...")

	// Generate PKCE codes
	pkceCodes, err := claude.GeneratePKCECodes()
	if err != nil {
		log.Errorf("Failed to generate PKCE codes: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate PKCE codes"})
		return
	}

	// Generate random state parameter
	state, err := misc.GenerateRandomState()
	if err != nil {
		log.Errorf("Failed to generate state parameter: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate state parameter"})
		return
	}

	// Initialize Claude auth service
	anthropicAuth := claude.NewClaudeAuth(h.cfg)

	// Generate authorization URL (then override redirect_uri to reuse server port)
	authURL, state, err := anthropicAuth.GenerateAuthURL(state, pkceCodes)
	if err != nil {
		log.Errorf("Failed to generate authorization URL: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
		return
	}

	RegisterOAuthSession(state, "anthropic")

	isWebUI := isWebUIRequest(c)
	var forwarder *callbackForwarder
	if isWebUI {
		targetURL, errTarget := h.managementCallbackURL("/anthropic/callback")
		if errTarget != nil {
			log.WithError(errTarget).Error("failed to compute anthropic callback target")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "callback server unavailable"})
			return
		}
		var errStart error
		if forwarder, errStart = startCallbackForwarder(anthropicCallbackPort, "anthropic", targetURL); errStart != nil {
			log.WithError(errStart).Error("failed to start anthropic callback forwarder")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start callback server"})
			return
		}
	}

	go func() {
		if isWebUI {
			defer stopCallbackForwarderInstance(anthropicCallbackPort, forwarder)
		}

		// Helper: wait for callback file
		waitFile := filepath.Join(h.cfg.AuthDir, fmt.Sprintf(".oauth-anthropic-%s.oauth", state))
		waitForFile := func(path string, timeout time.Duration) (map[string]string, error) {
			deadline := time.Now().Add(timeout)
			for {
				if !IsOAuthSessionPending(state, "anthropic") {
					return nil, errOAuthSessionNotPending
				}
				if time.Now().After(deadline) {
					SetOAuthSessionError(state, "Timeout waiting for OAuth callback")
					return nil, fmt.Errorf("timeout waiting for OAuth callback")
				}
				data, errRead := os.ReadFile(path)
				if errRead == nil {
					var m map[string]string
					_ = json.Unmarshal(data, &m)
					_ = os.Remove(path)
					return m, nil
				}
				time.Sleep(500 * time.Millisecond)
			}
		}

		fmt.Println("Waiting for authentication callback...")
		// Wait up to 5 minutes
		resultMap, errWait := waitForFile(waitFile, 5*time.Minute)
		if errWait != nil {
			if errors.Is(errWait, errOAuthSessionNotPending) {
				return
			}
			authErr := claude.NewAuthenticationError(claude.ErrCallbackTimeout, errWait)
			log.Error(claude.GetUserFriendlyMessage(authErr))
			return
		}
		if errStr := resultMap["error"]; errStr != "" {
			oauthErr := claude.NewOAuthError(errStr, "", http.StatusBadRequest)
			log.Error(claude.GetUserFriendlyMessage(oauthErr))
			SetOAuthSessionError(state, "Bad request")
			return
		}
		if resultMap["state"] != state {
			authErr := claude.NewAuthenticationError(claude.ErrInvalidState, fmt.Errorf("expected %s, got %s", state, resultMap["state"]))
			log.Error(claude.GetUserFriendlyMessage(authErr))
			SetOAuthSessionError(state, "State code error")
			return
		}

		// Parse code (Claude may append state after '#')
		rawCode := resultMap["code"]
		code := strings.Split(rawCode, "#")[0]

		// Exchange code for tokens using internal auth service
		bundle, errExchange := anthropicAuth.ExchangeCodeForTokens(ctx, code, state, pkceCodes)
		if errExchange != nil {
			authErr := claude.NewAuthenticationError(claude.ErrCodeExchangeFailed, errExchange)
			log.Errorf("Failed to exchange authorization code for tokens: %v", authErr)
			SetOAuthSessionError(state, "Failed to exchange authorization code for tokens")
			return
		}

		// Create token storage
		tokenStorage := anthropicAuth.CreateTokenStorage(bundle)
		record := &coreauth.Auth{
			ID:       fmt.Sprintf("claude-%s.json", tokenStorage.Email),
			Provider: "claude",
			FileName: fmt.Sprintf("claude-%s.json", tokenStorage.Email),
			Storage:  tokenStorage,
			Metadata: map[string]any{"email": tokenStorage.Email},
		}
		savedPath, errSave := h.saveTokenRecord(ctx, record)
		if errSave != nil {
			log.Errorf("Failed to save authentication tokens: %v", errSave)
			SetOAuthSessionError(state, "Failed to save authentication tokens")
			return
		}

		fmt.Printf("Authentication successful! Token saved to %s\n", savedPath)
		if bundle.APIKey != "" {
			fmt.Println("API key obtained and saved")
		}
		fmt.Println("You can now use Claude services through this CLI")
		CompleteOAuthSession(state)
		CompleteOAuthSessionsByProvider("anthropic")
	}()

	c.JSON(200, gin.H{"status": "ok", "url": authURL, "state": state})
}

func (h *Handler) RequestGeminiCLIToken(c *gin.Context) {
	ctx := context.Background()
	ctx = PopulateAuthContext(ctx, c)
	proxyHTTPClient := util.SetProxy(&h.cfg.SDKConfig, &http.Client{})
	ctx = context.WithValue(ctx, oauth2.HTTPClient, proxyHTTPClient)

	// Optional project ID from query
	projectID := c.Query("project_id")

	fmt.Println("Initializing Google authentication...")

	// OAuth2 configuration using exported constants from internal/auth/gemini
	conf := &oauth2.Config{
		ClientID:     geminiAuth.ClientID,
		ClientSecret: geminiAuth.ClientSecret,
		RedirectURL:  fmt.Sprintf("http://localhost:%d/oauth2callback", geminiAuth.DefaultCallbackPort),
		Scopes:       geminiAuth.Scopes,
		Endpoint:     google.Endpoint,
	}

	// Build authorization URL and return it immediately
	state := fmt.Sprintf("gem-%d", time.Now().UnixNano())
	authURL := conf.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent"))

	RegisterOAuthSession(state, "gemini")

	isWebUI := isWebUIRequest(c)
	var forwarder *callbackForwarder
	if isWebUI {
		targetURL, errTarget := h.managementCallbackURL("/google/callback")
		if errTarget != nil {
			log.WithError(errTarget).Error("failed to compute gemini callback target")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "callback server unavailable"})
			return
		}
		var errStart error
		if forwarder, errStart = startCallbackForwarder(geminiCallbackPort, "gemini", targetURL); errStart != nil {
			log.WithError(errStart).Error("failed to start gemini callback forwarder")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start callback server"})
			return
		}
	}

	go func() {
		if isWebUI {
			defer stopCallbackForwarderInstance(geminiCallbackPort, forwarder)
		}

		// Wait for callback file written by server route
		waitFile := filepath.Join(h.cfg.AuthDir, fmt.Sprintf(".oauth-gemini-%s.oauth", state))
		fmt.Println("Waiting for authentication callback...")
		deadline := time.Now().Add(5 * time.Minute)
		var authCode string
		for {
			if !IsOAuthSessionPending(state, "gemini") {
				return
			}
			if time.Now().After(deadline) {
				log.Error("oauth flow timed out")
				SetOAuthSessionError(state, "OAuth flow timed out")
				return
			}
			if data, errR := os.ReadFile(waitFile); errR == nil {
				var m map[string]string
				_ = json.Unmarshal(data, &m)
				_ = os.Remove(waitFile)
				if errStr := m["error"]; errStr != "" {
					log.Errorf("Authentication failed: %s", errStr)
					SetOAuthSessionError(state, "Authentication failed")
					return
				}
				authCode = m["code"]
				if authCode == "" {
					log.Errorf("Authentication failed: code not found")
					SetOAuthSessionError(state, "Authentication failed: code not found")
					return
				}
				break
			}
			time.Sleep(500 * time.Millisecond)
		}

		// Exchange authorization code for token
		token, err := conf.Exchange(ctx, authCode)
		if err != nil {
			log.Errorf("Failed to exchange token: %v", err)
			SetOAuthSessionError(state, "Failed to exchange token")
			return
		}

		requestedProjectID := strings.TrimSpace(projectID)

		// Create token storage (mirrors internal/auth/gemini createTokenStorage)
		authHTTPClient := conf.Client(ctx, token)
		req, errNewRequest := http.NewRequestWithContext(ctx, "GET", "https://www.googleapis.com/oauth2/v1/userinfo?alt=json", nil)
		if errNewRequest != nil {
			log.Errorf("Could not get user info: %v", errNewRequest)
			SetOAuthSessionError(state, "Could not get user info")
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", token.AccessToken))

		resp, errDo := authHTTPClient.Do(req)
		if errDo != nil {
			log.Errorf("Failed to execute request: %v", errDo)
			SetOAuthSessionError(state, "Failed to execute request")
			return
		}
		defer func() {
			if errClose := resp.Body.Close(); errClose != nil {
				log.Printf("warn: failed to close response body: %v", errClose)
			}
		}()

		bodyBytes, _ := io.ReadAll(resp.Body)
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			log.Errorf("Get user info request failed with status %d: %s", resp.StatusCode, string(bodyBytes))
			SetOAuthSessionError(state, fmt.Sprintf("Get user info request failed with status %d", resp.StatusCode))
			return
		}

		email := gjson.GetBytes(bodyBytes, "email").String()
		if email != "" {
			fmt.Printf("Authenticated user email: %s\n", email)
		} else {
			fmt.Println("Failed to get user email from token")
		}

		// Marshal/unmarshal oauth2.Token to generic map and enrich fields
		var ifToken map[string]any
		jsonData, _ := json.Marshal(token)
		if errUnmarshal := json.Unmarshal(jsonData, &ifToken); errUnmarshal != nil {
			log.Errorf("Failed to unmarshal token: %v", errUnmarshal)
			SetOAuthSessionError(state, "Failed to unmarshal token")
			return
		}

		ifToken["token_uri"] = "https://oauth2.googleapis.com/token"
		ifToken["client_id"] = geminiAuth.ClientID
		ifToken["client_secret"] = geminiAuth.ClientSecret
		ifToken["scopes"] = geminiAuth.Scopes
		ifToken["universe_domain"] = "googleapis.com"

		ts := geminiAuth.GeminiTokenStorage{
			Token:     ifToken,
			ProjectID: requestedProjectID,
			Email:     email,
			Auto:      requestedProjectID == "",
		}

		// Initialize authenticated HTTP client via GeminiAuth to honor proxy settings
		gemAuth := geminiAuth.NewGeminiAuth()
		gemClient, errGetClient := gemAuth.GetAuthenticatedClient(ctx, &ts, h.cfg, &geminiAuth.WebLoginOptions{
			NoBrowser: true,
		})
		if errGetClient != nil {
			log.Errorf("failed to get authenticated client: %v", errGetClient)
			SetOAuthSessionError(state, "Failed to get authenticated client")
			return
		}
		fmt.Println("Authentication successful.")

		if strings.EqualFold(requestedProjectID, "ALL") {
			ts.Auto = false
			projects, errAll := onboardAllGeminiProjects(ctx, gemClient, &ts)
			if errAll != nil {
				log.Errorf("Failed to complete Gemini CLI onboarding: %v", errAll)
				SetOAuthSessionError(state, fmt.Sprintf("Failed to complete Gemini CLI onboarding: %v", errAll))
				return
			}
			if errVerify := ensureGeminiProjectsEnabled(ctx, gemClient, projects); errVerify != nil {
				log.Errorf("Failed to verify Cloud AI API status: %v", errVerify)
				SetOAuthSessionError(state, fmt.Sprintf("Failed to verify Cloud AI API status: %v", errVerify))
				return
			}
			ts.ProjectID = strings.Join(projects, ",")
			ts.Checked = true
		} else if strings.EqualFold(requestedProjectID, "GOOGLE_ONE") {
			ts.Auto = false
			if errSetup := performGeminiCLISetup(ctx, gemClient, &ts, ""); errSetup != nil {
				log.Errorf("Google One auto-discovery failed: %v", errSetup)
				SetOAuthSessionError(state, fmt.Sprintf("Google One auto-discovery failed: %v", errSetup))
				return
			}
			if strings.TrimSpace(ts.ProjectID) == "" {
				log.Error("Google One auto-discovery returned empty project ID")
				SetOAuthSessionError(state, "Google One auto-discovery returned empty project ID")
				return
			}
			isChecked, errCheck := checkCloudAPIIsEnabled(ctx, gemClient, ts.ProjectID)
			if errCheck != nil {
				log.Errorf("Failed to verify Cloud AI API status: %v", errCheck)
				SetOAuthSessionError(state, fmt.Sprintf("Failed to verify Cloud AI API status: %v", errCheck))
				return
			}
			ts.Checked = isChecked
			if !isChecked {
				log.Error("Cloud AI API is not enabled for the auto-discovered project")
				SetOAuthSessionError(state, fmt.Sprintf("Cloud AI API not enabled for project %s", ts.ProjectID))
				return
			}
		} else {
			if errEnsure := ensureGeminiProjectAndOnboard(ctx, gemClient, &ts, requestedProjectID); errEnsure != nil {
				log.Errorf("Failed to complete Gemini CLI onboarding: %v", errEnsure)
				SetOAuthSessionError(state, fmt.Sprintf("Failed to complete Gemini CLI onboarding: %v", errEnsure))
				return
			}

			if strings.TrimSpace(ts.ProjectID) == "" {
				log.Error("Onboarding did not return a project ID")
				SetOAuthSessionError(state, "Failed to resolve project ID")
				return
			}

			isChecked, errCheck := checkCloudAPIIsEnabled(ctx, gemClient, ts.ProjectID)
			if errCheck != nil {
				log.Errorf("Failed to verify Cloud AI API status: %v", errCheck)
				SetOAuthSessionError(state, fmt.Sprintf("Failed to verify Cloud AI API status: %v", errCheck))
				return
			}
			ts.Checked = isChecked
			if !isChecked {
				log.Error("Cloud AI API is not enabled for the selected project")
				SetOAuthSessionError(state, fmt.Sprintf("Cloud AI API not enabled for project %s", ts.ProjectID))
				return
			}
		}

		recordMetadata := map[string]any{
			"email":      ts.Email,
			"project_id": ts.ProjectID,
			"auto":       ts.Auto,
			"checked":    ts.Checked,
		}

		fileName := geminiAuth.CredentialFileName(ts.Email, ts.ProjectID, true)
		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "gemini",
			FileName: fileName,
			Storage:  &ts,
			Metadata: recordMetadata,
		}
		savedPath, errSave := h.saveTokenRecord(ctx, record)
		if errSave != nil {
			log.Errorf("Failed to save token to file: %v", errSave)
			SetOAuthSessionError(state, "Failed to save token to file")
			return
		}

		CompleteOAuthSession(state)
		CompleteOAuthSessionsByProvider("gemini")
		fmt.Printf("You can now use Gemini CLI services through this CLI; token saved to %s\n", savedPath)
	}()

	c.JSON(200, gin.H{"status": "ok", "url": authURL, "state": state})
}

func (h *Handler) RequestCodexToken(c *gin.Context) {
	ctx := context.Background()
	ctx = PopulateAuthContext(ctx, c)

	fmt.Println("Initializing Codex authentication...")

	// Generate PKCE codes
	pkceCodes, err := codex.GeneratePKCECodes()
	if err != nil {
		log.Errorf("Failed to generate PKCE codes: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate PKCE codes"})
		return
	}

	// Generate random state parameter
	state, err := misc.GenerateRandomState()
	if err != nil {
		log.Errorf("Failed to generate state parameter: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate state parameter"})
		return
	}

	// Initialize Codex auth service
	openaiAuth := codex.NewCodexAuth(h.cfg)

	// Generate authorization URL
	authURL, err := openaiAuth.GenerateAuthURL(state, pkceCodes)
	if err != nil {
		log.Errorf("Failed to generate authorization URL: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
		return
	}

	RegisterOAuthSession(state, "codex")

	isWebUI := isWebUIRequest(c)
	var forwarder *callbackForwarder
	if isWebUI {
		targetURL, errTarget := h.managementCallbackURL("/codex/callback")
		if errTarget != nil {
			log.WithError(errTarget).Error("failed to compute codex callback target")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "callback server unavailable"})
			return
		}
		var errStart error
		if forwarder, errStart = startCallbackForwarder(codexCallbackPort, "codex", targetURL); errStart != nil {
			log.WithError(errStart).Error("failed to start codex callback forwarder")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start callback server"})
			return
		}
	}

	go func() {
		if isWebUI {
			defer stopCallbackForwarderInstance(codexCallbackPort, forwarder)
		}

		// Wait for callback file
		waitFile := filepath.Join(h.cfg.AuthDir, fmt.Sprintf(".oauth-codex-%s.oauth", state))
		deadline := time.Now().Add(5 * time.Minute)
		var code string
		for {
			if !IsOAuthSessionPending(state, "codex") {
				return
			}
			if time.Now().After(deadline) {
				authErr := codex.NewAuthenticationError(codex.ErrCallbackTimeout, fmt.Errorf("timeout waiting for OAuth callback"))
				log.Error(codex.GetUserFriendlyMessage(authErr))
				SetOAuthSessionError(state, "Timeout waiting for OAuth callback")
				return
			}
			if data, errR := os.ReadFile(waitFile); errR == nil {
				var m map[string]string
				_ = json.Unmarshal(data, &m)
				_ = os.Remove(waitFile)
				if errStr := m["error"]; errStr != "" {
					oauthErr := codex.NewOAuthError(errStr, "", http.StatusBadRequest)
					log.Error(codex.GetUserFriendlyMessage(oauthErr))
					SetOAuthSessionError(state, "Bad Request")
					return
				}
				if m["state"] != state {
					authErr := codex.NewAuthenticationError(codex.ErrInvalidState, fmt.Errorf("expected %s, got %s", state, m["state"]))
					SetOAuthSessionError(state, "State code error")
					log.Error(codex.GetUserFriendlyMessage(authErr))
					return
				}
				code = m["code"]
				break
			}
			time.Sleep(500 * time.Millisecond)
		}

		log.Debug("Authorization code received, exchanging for tokens...")
		// Exchange code for tokens using internal auth service
		bundle, errExchange := openaiAuth.ExchangeCodeForTokens(ctx, code, pkceCodes)
		if errExchange != nil {
			authErr := codex.NewAuthenticationError(codex.ErrCodeExchangeFailed, errExchange)
			SetOAuthSessionError(state, "Failed to exchange authorization code for tokens")
			log.Errorf("Failed to exchange authorization code for tokens: %v", authErr)
			return
		}

		// Extract additional info for filename generation
		claims, _ := codex.ParseJWTToken(bundle.TokenData.IDToken)
		planType := ""
		hashAccountID := ""
		if claims != nil {
			planType = strings.TrimSpace(claims.CodexAuthInfo.ChatgptPlanType)
			if accountID := claims.GetAccountID(); accountID != "" {
				digest := sha256.Sum256([]byte(accountID))
				hashAccountID = hex.EncodeToString(digest[:])[:8]
			}
		}

		// Create token storage and persist
		tokenStorage := openaiAuth.CreateTokenStorage(bundle)
		fileName := codex.CredentialFileName(tokenStorage.Email, planType, hashAccountID, true)
		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "codex",
			FileName: fileName,
			Storage:  tokenStorage,
			Metadata: map[string]any{
				"email":      tokenStorage.Email,
				"account_id": tokenStorage.AccountID,
			},
		}
		savedPath, errSave := h.saveTokenRecord(ctx, record)
		if errSave != nil {
			SetOAuthSessionError(state, "Failed to save authentication tokens")
			log.Errorf("Failed to save authentication tokens: %v", errSave)
			return
		}
		fmt.Printf("Authentication successful! Token saved to %s\n", savedPath)
		if bundle.APIKey != "" {
			fmt.Println("API key obtained and saved")
		}
		fmt.Println("You can now use Codex services through this CLI")
		CompleteOAuthSession(state)
		CompleteOAuthSessionsByProvider("codex")
	}()

	c.JSON(200, gin.H{"status": "ok", "url": authURL, "state": state})
}

func (h *Handler) RequestAntigravityToken(c *gin.Context) {
	ctx := context.Background()
	ctx = PopulateAuthContext(ctx, c)

	fmt.Println("Initializing Antigravity authentication...")

	authSvc := antigravity.NewAntigravityAuth(h.cfg, nil)

	state, errState := misc.GenerateRandomState()
	if errState != nil {
		log.Errorf("Failed to generate state parameter: %v", errState)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate state parameter"})
		return
	}

	redirectURI := fmt.Sprintf("http://localhost:%d/oauth-callback", antigravity.CallbackPort)
	authURL := authSvc.BuildAuthURL(state, redirectURI)

	RegisterOAuthSession(state, "antigravity")

	isWebUI := isWebUIRequest(c)
	var forwarder *callbackForwarder
	if isWebUI {
		targetURL, errTarget := h.managementCallbackURL("/antigravity/callback")
		if errTarget != nil {
			log.WithError(errTarget).Error("failed to compute antigravity callback target")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "callback server unavailable"})
			return
		}
		var errStart error
		if forwarder, errStart = startCallbackForwarder(antigravity.CallbackPort, "antigravity", targetURL); errStart != nil {
			log.WithError(errStart).Error("failed to start antigravity callback forwarder")
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start callback server"})
			return
		}
	}

	go func() {
		if isWebUI {
			defer stopCallbackForwarderInstance(antigravity.CallbackPort, forwarder)
		}

		waitFile := filepath.Join(h.cfg.AuthDir, fmt.Sprintf(".oauth-antigravity-%s.oauth", state))
		deadline := time.Now().Add(5 * time.Minute)
		var authCode string
		for {
			if !IsOAuthSessionPending(state, "antigravity") {
				return
			}
			if time.Now().After(deadline) {
				log.Error("oauth flow timed out")
				SetOAuthSessionError(state, "OAuth flow timed out")
				return
			}
			if data, errReadFile := os.ReadFile(waitFile); errReadFile == nil {
				var payload map[string]string
				_ = json.Unmarshal(data, &payload)
				_ = os.Remove(waitFile)
				if errStr := strings.TrimSpace(payload["error"]); errStr != "" {
					log.Errorf("Authentication failed: %s", errStr)
					SetOAuthSessionError(state, "Authentication failed")
					return
				}
				if payloadState := strings.TrimSpace(payload["state"]); payloadState != "" && payloadState != state {
					log.Errorf("Authentication failed: state mismatch")
					SetOAuthSessionError(state, "Authentication failed: state mismatch")
					return
				}
				authCode = strings.TrimSpace(payload["code"])
				if authCode == "" {
					log.Error("Authentication failed: code not found")
					SetOAuthSessionError(state, "Authentication failed: code not found")
					return
				}
				break
			}
			time.Sleep(500 * time.Millisecond)
		}

		tokenResp, errToken := authSvc.ExchangeCodeForTokens(ctx, authCode, redirectURI)
		if errToken != nil {
			log.Errorf("Failed to exchange token: %v", errToken)
			SetOAuthSessionError(state, "Failed to exchange token")
			return
		}

		accessToken := strings.TrimSpace(tokenResp.AccessToken)
		if accessToken == "" {
			log.Error("antigravity: token exchange returned empty access token")
			SetOAuthSessionError(state, "Failed to exchange token")
			return
		}

		email, errInfo := authSvc.FetchUserInfo(ctx, accessToken)
		if errInfo != nil {
			log.Errorf("Failed to fetch user info: %v", errInfo)
			SetOAuthSessionError(state, "Failed to fetch user info")
			return
		}
		email = strings.TrimSpace(email)
		if email == "" {
			log.Error("antigravity: user info returned empty email")
			SetOAuthSessionError(state, "Failed to fetch user info")
			return
		}

		projectID := ""
		if accessToken != "" {
			fetchedProjectID, errProject := authSvc.FetchProjectID(ctx, accessToken)
			if errProject != nil {
				log.Warnf("antigravity: failed to fetch project ID: %v", errProject)
			} else {
				projectID = fetchedProjectID
				log.Infof("antigravity: obtained project ID %s", projectID)
			}
		}

		now := time.Now()
		metadata := map[string]any{
			"type":          "antigravity",
			"access_token":  tokenResp.AccessToken,
			"refresh_token": tokenResp.RefreshToken,
			"expires_in":    tokenResp.ExpiresIn,
			"timestamp":     now.UnixMilli(),
			"expired":       now.Add(time.Duration(tokenResp.ExpiresIn) * time.Second).Format(time.RFC3339),
		}
		if email != "" {
			metadata["email"] = email
		}
		if projectID != "" {
			metadata["project_id"] = projectID
		}

		fileName := antigravity.CredentialFileName(email)
		label := strings.TrimSpace(email)
		if label == "" {
			label = "antigravity"
		}

		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "antigravity",
			FileName: fileName,
			Label:    label,
			Metadata: metadata,
		}
		savedPath, errSave := h.saveTokenRecord(ctx, record)
		if errSave != nil {
			log.Errorf("Failed to save token to file: %v", errSave)
			SetOAuthSessionError(state, "Failed to save token to file")
			return
		}

		CompleteOAuthSession(state)
		CompleteOAuthSessionsByProvider("antigravity")
		fmt.Printf("Authentication successful! Token saved to %s\n", savedPath)
		if projectID != "" {
			fmt.Printf("Using GCP project: %s\n", projectID)
		}
		fmt.Println("You can now use Antigravity services through this CLI")
	}()

	c.JSON(200, gin.H{"status": "ok", "url": authURL, "state": state})
}

func (h *Handler) RequestKimiToken(c *gin.Context) {
	ctx := context.Background()
	ctx = PopulateAuthContext(ctx, c)

	fmt.Println("Initializing Kimi authentication...")

	state := fmt.Sprintf("kmi-%d", time.Now().UnixNano())
	// Initialize Kimi auth service
	kimiAuth := kimi.NewKimiAuth(h.cfg)

	// Generate authorization URL
	deviceFlow, errStartDeviceFlow := kimiAuth.StartDeviceFlow(ctx)
	if errStartDeviceFlow != nil {
		log.Errorf("Failed to generate authorization URL: %v", errStartDeviceFlow)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to generate authorization url"})
		return
	}
	authURL := deviceFlow.VerificationURIComplete
	if authURL == "" {
		authURL = deviceFlow.VerificationURI
	}

	RegisterOAuthSession(state, "kimi")

	go func() {
		fmt.Println("Waiting for authentication...")
		authBundle, errWaitForAuthorization := kimiAuth.WaitForAuthorization(ctx, deviceFlow)
		if errWaitForAuthorization != nil {
			SetOAuthSessionError(state, "Authentication failed")
			fmt.Printf("Authentication failed: %v\n", errWaitForAuthorization)
			return
		}

		// Create token storage
		tokenStorage := kimiAuth.CreateTokenStorage(authBundle)

		metadata := map[string]any{
			"type":          "kimi",
			"access_token":  authBundle.TokenData.AccessToken,
			"refresh_token": authBundle.TokenData.RefreshToken,
			"token_type":    authBundle.TokenData.TokenType,
			"scope":         authBundle.TokenData.Scope,
			"timestamp":     time.Now().UnixMilli(),
		}
		if authBundle.TokenData.ExpiresAt > 0 {
			expired := time.Unix(authBundle.TokenData.ExpiresAt, 0).UTC().Format(time.RFC3339)
			metadata["expired"] = expired
		}
		if strings.TrimSpace(authBundle.DeviceID) != "" {
			metadata["device_id"] = strings.TrimSpace(authBundle.DeviceID)
		}

		fileName := fmt.Sprintf("kimi-%d.json", time.Now().UnixMilli())
		record := &coreauth.Auth{
			ID:       fileName,
			Provider: "kimi",
			FileName: fileName,
			Label:    "Kimi User",
			Storage:  tokenStorage,
			Metadata: metadata,
		}
		savedPath, errSave := h.saveTokenRecord(ctx, record)
		if errSave != nil {
			log.Errorf("Failed to save authentication tokens: %v", errSave)
			SetOAuthSessionError(state, "Failed to save authentication tokens")
			return
		}

		fmt.Printf("Authentication successful! Token saved to %s\n", savedPath)
		fmt.Println("You can now use Kimi services through this CLI")
		CompleteOAuthSession(state)
		CompleteOAuthSessionsByProvider("kimi")
	}()

	c.JSON(200, gin.H{"status": "ok", "url": authURL, "state": state})
}

type projectSelectionRequiredError struct{}

func (e *projectSelectionRequiredError) Error() string {
	return "gemini cli: project selection required"
}

func ensureGeminiProjectAndOnboard(ctx context.Context, httpClient *http.Client, storage *geminiAuth.GeminiTokenStorage, requestedProject string) error {
	if storage == nil {
		return fmt.Errorf("gemini storage is nil")
	}

	trimmedRequest := strings.TrimSpace(requestedProject)
	if trimmedRequest == "" {
		projects, errProjects := fetchGCPProjects(ctx, httpClient)
		if errProjects != nil {
			return fmt.Errorf("fetch project list: %w", errProjects)
		}
		if len(projects) == 0 {
			return fmt.Errorf("no Google Cloud projects available for this account")
		}
		trimmedRequest = strings.TrimSpace(projects[0].ProjectID)
		if trimmedRequest == "" {
			return fmt.Errorf("resolved project id is empty")
		}
		storage.Auto = true
	} else {
		storage.Auto = false
	}

	if err := performGeminiCLISetup(ctx, httpClient, storage, trimmedRequest); err != nil {
		return err
	}

	if strings.TrimSpace(storage.ProjectID) == "" {
		storage.ProjectID = trimmedRequest
	}

	return nil
}

func onboardAllGeminiProjects(ctx context.Context, httpClient *http.Client, storage *geminiAuth.GeminiTokenStorage) ([]string, error) {
	projects, errProjects := fetchGCPProjects(ctx, httpClient)
	if errProjects != nil {
		return nil, fmt.Errorf("fetch project list: %w", errProjects)
	}
	if len(projects) == 0 {
		return nil, fmt.Errorf("no Google Cloud projects available for this account")
	}
	activated := make([]string, 0, len(projects))
	seen := make(map[string]struct{}, len(projects))
	for _, project := range projects {
		candidate := strings.TrimSpace(project.ProjectID)
		if candidate == "" {
			continue
		}
		if _, dup := seen[candidate]; dup {
			continue
		}
		if err := performGeminiCLISetup(ctx, httpClient, storage, candidate); err != nil {
			return nil, fmt.Errorf("onboard project %s: %w", candidate, err)
		}
		finalID := strings.TrimSpace(storage.ProjectID)
		if finalID == "" {
			finalID = candidate
		}
		activated = append(activated, finalID)
		seen[candidate] = struct{}{}
	}
	if len(activated) == 0 {
		return nil, fmt.Errorf("no Google Cloud projects available for this account")
	}
	return activated, nil
}

func ensureGeminiProjectsEnabled(ctx context.Context, httpClient *http.Client, projectIDs []string) error {
	for _, pid := range projectIDs {
		trimmed := strings.TrimSpace(pid)
		if trimmed == "" {
			continue
		}
		isChecked, errCheck := checkCloudAPIIsEnabled(ctx, httpClient, trimmed)
		if errCheck != nil {
			return fmt.Errorf("project %s: %w", trimmed, errCheck)
		}
		if !isChecked {
			return fmt.Errorf("project %s: Cloud AI API not enabled", trimmed)
		}
	}
	return nil
}

func performGeminiCLISetup(ctx context.Context, httpClient *http.Client, storage *geminiAuth.GeminiTokenStorage, requestedProject string) error {
	metadata := map[string]string{
		"ideType":    "IDE_UNSPECIFIED",
		"platform":   "PLATFORM_UNSPECIFIED",
		"pluginType": "GEMINI",
	}

	trimmedRequest := strings.TrimSpace(requestedProject)
	explicitProject := trimmedRequest != ""

	loadReqBody := map[string]any{
		"metadata": metadata,
	}
	if explicitProject {
		loadReqBody["cloudaicompanionProject"] = trimmedRequest
	}

	var loadResp map[string]any
	if errLoad := callGeminiCLI(ctx, httpClient, "loadCodeAssist", loadReqBody, &loadResp); errLoad != nil {
		return fmt.Errorf("load code assist: %w", errLoad)
	}

	tierID := "legacy-tier"
	if tiers, okTiers := loadResp["allowedTiers"].([]any); okTiers {
		for _, rawTier := range tiers {
			tier, okTier := rawTier.(map[string]any)
			if !okTier {
				continue
			}
			if isDefault, okDefault := tier["isDefault"].(bool); okDefault && isDefault {
				if id, okID := tier["id"].(string); okID && strings.TrimSpace(id) != "" {
					tierID = strings.TrimSpace(id)
					break
				}
			}
		}
	}

	projectID := trimmedRequest
	if projectID == "" {
		if id, okProject := loadResp["cloudaicompanionProject"].(string); okProject {
			projectID = strings.TrimSpace(id)
		}
		if projectID == "" {
			if projectMap, okProject := loadResp["cloudaicompanionProject"].(map[string]any); okProject {
				if id, okID := projectMap["id"].(string); okID {
					projectID = strings.TrimSpace(id)
				}
			}
		}
	}
	if projectID == "" {
		// Auto-discovery: try onboardUser without specifying a project
		// to let Google auto-provision one (matches Gemini CLI headless behavior
		// and Antigravity's FetchProjectID pattern).
		autoOnboardReq := map[string]any{
			"tierId":   tierID,
			"metadata": metadata,
		}

		autoCtx, autoCancel := context.WithTimeout(ctx, 30*time.Second)
		defer autoCancel()
		for attempt := 1; ; attempt++ {
			var onboardResp map[string]any
			if errOnboard := callGeminiCLI(autoCtx, httpClient, "onboardUser", autoOnboardReq, &onboardResp); errOnboard != nil {
				return fmt.Errorf("auto-discovery onboardUser: %w", errOnboard)
			}

			if done, okDone := onboardResp["done"].(bool); okDone && done {
				if resp, okResp := onboardResp["response"].(map[string]any); okResp {
					switch v := resp["cloudaicompanionProject"].(type) {
					case string:
						projectID = strings.TrimSpace(v)
					case map[string]any:
						if id, okID := v["id"].(string); okID {
							projectID = strings.TrimSpace(id)
						}
					}
				}
				break
			}

			log.Debugf("Auto-discovery: onboarding in progress, attempt %d...", attempt)
			select {
			case <-autoCtx.Done():
				return &projectSelectionRequiredError{}
			case <-time.After(2 * time.Second):
			}
		}

		if projectID == "" {
			return &projectSelectionRequiredError{}
		}
		log.Infof("Auto-discovered project ID via onboarding: %s", projectID)
	}

	onboardReqBody := map[string]any{
		"tierId":                  tierID,
		"metadata":                metadata,
		"cloudaicompanionProject": projectID,
	}

	storage.ProjectID = projectID

	for {
		var onboardResp map[string]any
		if errOnboard := callGeminiCLI(ctx, httpClient, "onboardUser", onboardReqBody, &onboardResp); errOnboard != nil {
			return fmt.Errorf("onboard user: %w", errOnboard)
		}

		if done, okDone := onboardResp["done"].(bool); okDone && done {
			responseProjectID := ""
			if resp, okResp := onboardResp["response"].(map[string]any); okResp {
				switch projectValue := resp["cloudaicompanionProject"].(type) {
				case map[string]any:
					if id, okID := projectValue["id"].(string); okID {
						responseProjectID = strings.TrimSpace(id)
					}
				case string:
					responseProjectID = strings.TrimSpace(projectValue)
				}
			}

			finalProjectID := projectID
			if responseProjectID != "" {
				if explicitProject && !strings.EqualFold(responseProjectID, projectID) {
					log.Infof("Gemini onboarding: requested project %s maps to backend project %s", projectID, responseProjectID)
					log.Infof("Using backend project ID: %s", responseProjectID)
				}
				finalProjectID = responseProjectID
			}

			storage.ProjectID = strings.TrimSpace(finalProjectID)
			if storage.ProjectID == "" {
				storage.ProjectID = strings.TrimSpace(projectID)
			}
			if storage.ProjectID == "" {
				return fmt.Errorf("onboard user completed without project id")
			}
			log.Infof("Onboarding complete. Using Project ID: %s", storage.ProjectID)
			return nil
		}

		log.Println("Onboarding in progress, waiting 5 seconds...")
		time.Sleep(5 * time.Second)
	}
}

func callGeminiCLI(ctx context.Context, httpClient *http.Client, endpoint string, body any, result any) error {
	endPointURL := fmt.Sprintf("%s/%s:%s", geminiCLIEndpoint, geminiCLIVersion, endpoint)
	if strings.HasPrefix(endpoint, "operations/") {
		endPointURL = fmt.Sprintf("%s/%s", geminiCLIEndpoint, endpoint)
	}

	var reader io.Reader
	if body != nil {
		rawBody, errMarshal := json.Marshal(body)
		if errMarshal != nil {
			return fmt.Errorf("marshal request body: %w", errMarshal)
		}
		reader = bytes.NewReader(rawBody)
	}

	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, endPointURL, reader)
	if errRequest != nil {
		return fmt.Errorf("create request: %w", errRequest)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", misc.GeminiCLIUserAgent(""))

	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		return fmt.Errorf("execute request: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("api request failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	if result == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}

	if errDecode := json.NewDecoder(resp.Body).Decode(result); errDecode != nil {
		return fmt.Errorf("decode response body: %w", errDecode)
	}

	return nil
}

func fetchGCPProjects(ctx context.Context, httpClient *http.Client) ([]interfaces.GCPProjectProjects, error) {
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, "https://cloudresourcemanager.googleapis.com/v1/projects", nil)
	if errRequest != nil {
		return nil, fmt.Errorf("could not create project list request: %w", errRequest)
	}

	resp, errDo := httpClient.Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("failed to execute project list request: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("response body close error: %v", errClose)
		}
	}()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("project list request failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
	}

	var projects interfaces.GCPProject
	if errDecode := json.NewDecoder(resp.Body).Decode(&projects); errDecode != nil {
		return nil, fmt.Errorf("failed to unmarshal project list: %w", errDecode)
	}

	return projects.Projects, nil
}

func checkCloudAPIIsEnabled(ctx context.Context, httpClient *http.Client, projectID string) (bool, error) {
	serviceUsageURL := "https://serviceusage.googleapis.com"
	requiredServices := []string{
		"cloudaicompanion.googleapis.com",
	}
	for _, service := range requiredServices {
		checkURL := fmt.Sprintf("%s/v1/projects/%s/services/%s", serviceUsageURL, projectID, service)
		req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, checkURL, nil)
		if errRequest != nil {
			return false, fmt.Errorf("failed to create request: %w", errRequest)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", misc.GeminiCLIUserAgent(""))
		resp, errDo := httpClient.Do(req)
		if errDo != nil {
			return false, fmt.Errorf("failed to execute request: %w", errDo)
		}

		if resp.StatusCode == http.StatusOK {
			bodyBytes, _ := io.ReadAll(resp.Body)
			if gjson.GetBytes(bodyBytes, "state").String() == "ENABLED" {
				_ = resp.Body.Close()
				continue
			}
		}
		_ = resp.Body.Close()

		enableURL := fmt.Sprintf("%s/v1/projects/%s/services/%s:enable", serviceUsageURL, projectID, service)
		req, errRequest = http.NewRequestWithContext(ctx, http.MethodPost, enableURL, strings.NewReader("{}"))
		if errRequest != nil {
			return false, fmt.Errorf("failed to create request: %w", errRequest)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", misc.GeminiCLIUserAgent(""))
		resp, errDo = httpClient.Do(req)
		if errDo != nil {
			return false, fmt.Errorf("failed to execute request: %w", errDo)
		}

		bodyBytes, _ := io.ReadAll(resp.Body)
		errMessage := string(bodyBytes)
		errMessageResult := gjson.GetBytes(bodyBytes, "error.message")
		if errMessageResult.Exists() {
			errMessage = errMessageResult.String()
		}
		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
			_ = resp.Body.Close()
			continue
		} else if resp.StatusCode == http.StatusBadRequest {
			_ = resp.Body.Close()
			if strings.Contains(strings.ToLower(errMessage), "already enabled") {
				continue
			}
		}
		_ = resp.Body.Close()
		return false, fmt.Errorf("project activation required: %s", errMessage)
	}
	return true, nil
}

func (h *Handler) GetAuthStatus(c *gin.Context) {
	state := strings.TrimSpace(c.Query("state"))
	if state == "" {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
		return
	}
	if err := ValidateOAuthState(state); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "invalid state"})
		return
	}

	_, status, ok := GetOAuthSession(state)
	if !ok {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
		return
	}
	if status != "" {
		c.JSON(http.StatusOK, gin.H{"status": "error", "error": status})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "wait"})
}

// PopulateAuthContext extracts request info and adds it to the context
func PopulateAuthContext(ctx context.Context, c *gin.Context) context.Context {
	info := &coreauth.RequestInfo{
		Query:   c.Request.URL.Query(),
		Headers: c.Request.Header,
	}
	return coreauth.WithRequestInfo(ctx, info)
}
