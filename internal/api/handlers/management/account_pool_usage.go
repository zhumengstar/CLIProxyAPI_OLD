package management

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

const maxAccountPoolUsageRecords = 300

type accountPoolUsageRecord struct {
	ID           string `json:"id"`
	RequestedAt  string `json:"requested_at"`
	RequestID    string `json:"request_id,omitempty"`
	RequestPath  string `json:"request_path,omitempty"`
	SessionID    string `json:"session_id,omitempty"`
	NewAPIUserID string `json:"newapi_user_id,omitempty"`
	Username     string `json:"username,omitempty"`
	Provider     string `json:"provider,omitempty"`
	Model        string `json:"model,omitempty"`
	Alias        string `json:"alias,omitempty"`
	ServiceEmail string `json:"service_email,omitempty"`
	AuthID       string `json:"auth_id,omitempty"`
	AuthIndex    string `json:"auth_index,omitempty"`
	AuthType     string `json:"auth_type,omitempty"`
	Success      bool   `json:"success"`
	StatusCode   int    `json:"status_code,omitempty"`
	LatencyMS    int64  `json:"latency_ms,omitempty"`
	InputTokens  int64  `json:"input_tokens,omitempty"`
	OutputTokens int64  `json:"output_tokens,omitempty"`
	TotalTokens  int64  `json:"total_tokens,omitempty"`
}

type accountPoolUsageRecorder struct {
	mu      sync.RWMutex
	records []accountPoolUsageRecord
	nextID  uint64
}

var accountPoolUsage = &accountPoolUsageRecorder{}

func init() {
	usage.RegisterPlugin(accountPoolUsage)
}

func (r *accountPoolUsageRecorder) HandleUsage(ctx context.Context, record usage.Record) {
	if r == nil {
		return
	}
	sessionID, userID, username, requestPath := accountPoolRequestIdentity(ctx)
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
		ID:           strconv.FormatUint(r.nextID, 10),
		RequestedAt:  requestedAt.Format(time.RFC3339),
		RequestID:    logging.GetRequestID(ctx),
		RequestPath:  requestPath,
		SessionID:    sessionID,
		NewAPIUserID: userID,
		Username:     username,
		Provider:     record.Provider,
		Model:        record.Model,
		Alias:        record.Alias,
		ServiceEmail: record.Source,
		AuthID:       record.AuthID,
		AuthIndex:    record.AuthIndex,
		AuthType:     record.AuthType,
		Success:      !record.Failed,
		StatusCode:   statusCode,
		LatencyMS:    record.Latency.Milliseconds(),
		InputTokens:  record.Detail.InputTokens,
		OutputTokens: record.Detail.OutputTokens,
		TotalTokens:  record.Detail.TotalTokens,
	}
	r.records = append(r.records, item)
	if overflow := len(r.records) - maxAccountPoolUsageRecords; overflow > 0 {
		copy(r.records, r.records[overflow:])
		r.records = r.records[:len(r.records)-overflow]
	}
	r.mu.Unlock()
}

func (r *accountPoolUsageRecorder) List(limit int) []accountPoolUsageRecord {
	if r == nil {
		return nil
	}
	if limit <= 0 || limit > maxAccountPoolUsageRecords {
		limit = 80
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	count := len(r.records)
	if limit < count {
		count = limit
	}
	out := make([]accountPoolUsageRecord, 0, count)
	for i := len(r.records) - 1; i >= 0 && len(out) < count; i-- {
		out = append(out, r.records[i])
	}
	return out
}

func (r *accountPoolUsageRecorder) Clear() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.records = nil
	r.mu.Unlock()
}

func accountPoolRequestIdentity(ctx context.Context) (sessionID string, userID string, username string, requestPath string) {
	ginCtx, ok := ctx.Value("gin").(*gin.Context)
	if !ok || ginCtx == nil || ginCtx.Request == nil {
		return "", "", "", ""
	}
	sessionID = strings.TrimSpace(ginCtx.GetHeader("X-Session-ID"))
	userID, username = parseNewAPISessionID(sessionID)
	if ginCtx.Request.URL != nil {
		requestPath = strings.TrimSpace(ginCtx.Request.URL.Path)
	}
	return sessionID, userID, username, requestPath
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
	limit := 80
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid limit: %s", raw)})
			return
		}
		limit = parsed
	}
	c.JSON(http.StatusOK, gin.H{"records": accountPoolUsage.List(limit)})
}

func (h *Handler) ClearAccountPoolUsageRecords(c *gin.Context) {
	accountPoolUsage.Clear()
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
