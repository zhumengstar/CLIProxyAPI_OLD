package usagepersist

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const defaultDir = "logs"

var (
	enabled atomic.Bool

	dirMu sync.RWMutex
	dir   = defaultDir

	writeMu sync.Mutex
)

func init() {
	coreusage.RegisterPlugin(&plugin{})
}

// SetEnabled toggles durable usage record persistence.
func SetEnabled(value bool) {
	enabled.Store(value)
}

// SetDirectory updates the directory where structured usage JSONL files are written.
func SetDirectory(value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = defaultDir
	}
	dirMu.Lock()
	dir = value
	dirMu.Unlock()
}

func Directory() string {
	dirMu.RLock()
	defer dirMu.RUnlock()
	return dir
}

type plugin struct{}

func (p *plugin) HandleUsage(ctx context.Context, record coreusage.Record) {
	if p == nil || !enabled.Load() {
		return
	}

	payload, err := json.Marshal(buildRecord(ctx, record))
	if err != nil {
		log.WithError(err).Warn("failed to marshal structured usage record")
		return
	}
	payload = append(payload, '\n')

	targetDir := Directory()
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		log.WithError(err).Warnf("failed to create usage persistence directory %s", targetDir)
		return
	}

	filename := filepath.Join(targetDir, "usage-records-"+time.Now().Format("2006-01-02")+".jsonl")
	writeMu.Lock()
	defer writeMu.Unlock()
	if err := appendFile(filename, payload); err != nil {
		log.WithError(err).Warnf("failed to write structured usage record %s", filename)
	}
}

type recordJSON struct {
	Timestamp       time.Time   `json:"timestamp"`
	Provider        string      `json:"provider"`
	Model           string      `json:"model"`
	Alias           string      `json:"alias"`
	Endpoint        string      `json:"endpoint"`
	AuthType        string      `json:"auth_type"`
	Email           string      `json:"email"`
	AuthID          string      `json:"auth_id"`
	AuthIndex       string      `json:"auth_index"`
	APIKey          string      `json:"api_key"`
	RequestID       string      `json:"request_id"`
	Source          string      `json:"source"`
	ReasoningEffort string      `json:"reasoning_effort"`
	LatencyMs       int64       `json:"latency_ms"`
	Tokens          tokenJSON   `json:"tokens"`
	Failed          bool        `json:"failed"`
	Fail            failJSON    `json:"fail"`
	ResponseHeaders http.Header `json:"response_headers,omitempty"`
}

type tokenJSON struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	ReasoningTokens     int64 `json:"reasoning_tokens"`
	CachedTokens        int64 `json:"cached_tokens"`
	CacheReadTokens     int64 `json:"cache_read_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
}

type failJSON struct {
	StatusCode int    `json:"status_code"`
	Body       string `json:"body"`
}

func buildRecord(ctx context.Context, record coreusage.Record) recordJSON {
	timestamp := record.RequestedAt
	if timestamp.IsZero() {
		timestamp = time.Now()
	}

	model := strings.TrimSpace(record.Model)
	if model == "" {
		model = "unknown"
	}
	alias := strings.TrimSpace(record.Alias)
	if alias == "" {
		alias = model
	}
	provider := strings.TrimSpace(record.Provider)
	if provider == "" {
		provider = "unknown"
	}
	authType := strings.TrimSpace(record.AuthType)
	if authType == "" {
		authType = "unknown"
	}

	tokens := tokenJSON{
		InputTokens:         record.Detail.InputTokens,
		OutputTokens:        record.Detail.OutputTokens,
		ReasoningTokens:     record.Detail.ReasoningTokens,
		CachedTokens:        record.Detail.CachedTokens,
		CacheReadTokens:     record.Detail.CacheReadTokens,
		CacheCreationTokens: record.Detail.CacheCreationTokens,
		TotalTokens:         record.Detail.TotalTokens,
	}
	if tokens.TotalTokens == 0 {
		tokens.TotalTokens = tokens.InputTokens + tokens.OutputTokens + tokens.ReasoningTokens
	}
	if tokens.TotalTokens == 0 {
		tokens.TotalTokens = tokens.InputTokens + tokens.OutputTokens + tokens.ReasoningTokens + tokens.CachedTokens
	}

	failed := record.Failed
	if !failed {
		failed = !resolveSuccess(ctx)
	}
	fail := resolveFail(ctx, record, failed)

	reasoningEffort := strings.TrimSpace(record.ReasoningEffort)
	if reasoningEffort == "" {
		reasoningEffort = coreusage.ReasoningEffortFromContext(ctx)
	}

	return recordJSON{
		Timestamp:       timestamp,
		Provider:        provider,
		Model:           model,
		Alias:           alias,
		Endpoint:        strings.TrimSpace(internallogging.GetEndpoint(ctx)),
		AuthType:        authType,
		Email:           strings.TrimSpace(record.Email),
		AuthID:          strings.TrimSpace(record.AuthID),
		AuthIndex:       strings.TrimSpace(record.AuthIndex),
		APIKey:          strings.TrimSpace(record.APIKey),
		RequestID:       strings.TrimSpace(internallogging.GetRequestID(ctx)),
		Source:          strings.TrimSpace(record.Source),
		ReasoningEffort: reasoningEffort,
		LatencyMs:       record.Latency.Milliseconds(),
		Tokens:          tokens,
		Failed:          failed,
		Fail:            fail,
		ResponseHeaders: record.ResponseHeaders,
	}
}

func resolveSuccess(ctx context.Context) bool {
	status := internallogging.GetResponseStatus(ctx)
	return status == 0 || status < http.StatusBadRequest
}

func resolveFail(ctx context.Context, record coreusage.Record, failed bool) failJSON {
	if !failed {
		return failJSON{StatusCode: http.StatusOK}
	}
	status := record.Fail.StatusCode
	if status <= 0 {
		status = internallogging.GetResponseStatus(ctx)
	}
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	return failJSON{
		StatusCode: status,
		Body:       strings.TrimSpace(record.Fail.Body),
	}
}

func appendFile(filename string, payload []byte) error {
	f, err := os.OpenFile(filename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		if errClose := f.Close(); errClose != nil {
			log.WithError(errClose).Warnf("failed to close structured usage record file %s", filename)
		}
	}()
	_, err = f.Write(payload)
	return err
}
