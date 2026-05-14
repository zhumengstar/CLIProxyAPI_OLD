package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	apimiddleware "github.com/router-for-me/CLIProxyAPI/v7/internal/api/middleware"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

const maxAccountPoolUsageRecords = 300

type accountPoolUsageRecord struct {
	ID            string `json:"id"`
	RequestedAt   string `json:"requested_at"`
	RequestID     string `json:"request_id,omitempty"`
	RequestPath   string `json:"request_path,omitempty"`
	SessionID     string `json:"session_id,omitempty"`
	NewAPIUserID  string `json:"newapi_user_id,omitempty"`
	Username      string `json:"username,omitempty"`
	Provider      string `json:"provider,omitempty"`
	Model         string `json:"model,omitempty"`
	Alias         string `json:"alias,omitempty"`
	ServiceEmail  string `json:"service_email,omitempty"`
	AuthID        string `json:"auth_id,omitempty"`
	AuthIndex     string `json:"auth_index,omitempty"`
	AuthType      string `json:"auth_type,omitempty"`
	Success       bool   `json:"success"`
	StatusCode    int    `json:"status_code,omitempty"`
	LatencyMS     int64  `json:"latency_ms,omitempty"`
	InputTokens   int64  `json:"input_tokens,omitempty"`
	OutputTokens  int64  `json:"output_tokens,omitempty"`
	TotalTokens   int64  `json:"total_tokens,omitempty"`
	RequestParams string `json:"request_params,omitempty"`
}

type accountPoolUsageSummary struct {
	Key          string `json:"key"`
	ServiceEmail string `json:"service_email,omitempty"`
	AuthID       string `json:"auth_id,omitempty"`
	AuthIndex    string `json:"auth_index,omitempty"`
	AuthType     string `json:"auth_type,omitempty"`
	Provider     string `json:"provider,omitempty"`
	Model        string `json:"model,omitempty"`
	Alias        string `json:"alias,omitempty"`
	Requests     int64  `json:"requests"`
	Successes    int64  `json:"successes"`
	Failures     int64  `json:"failures"`
	InputTokens  int64  `json:"input_tokens,omitempty"`
	OutputTokens int64  `json:"output_tokens,omitempty"`
	TotalTokens  int64  `json:"total_tokens,omitempty"`
	LastUsedAt   string `json:"last_used_at,omitempty"`
}

type accountPoolUsageRecorder struct {
	mu        sync.RWMutex
	records   []accountPoolUsageRecord
	summaries map[string]*accountPoolUsageSummary
	nextID    uint64
}

var accountPoolUsage = &accountPoolUsageRecorder{}

func init() {
	usage.RegisterPlugin(accountPoolUsage)
}

func (r *accountPoolUsageRecorder) HandleUsage(ctx context.Context, record usage.Record) {
	if r == nil {
		return
	}
	sessionID, userID, username, requestPath, requestParams := accountPoolRequestIdentity(ctx)
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
		ID:            strconv.FormatUint(r.nextID, 10),
		RequestedAt:   requestedAt.Format(time.RFC3339),
		RequestID:     logging.GetRequestID(ctx),
		RequestPath:   requestPath,
		SessionID:     sessionID,
		NewAPIUserID:  userID,
		Username:      username,
		Provider:      record.Provider,
		Model:         record.Model,
		Alias:         record.Alias,
		ServiceEmail:  record.Source,
		AuthID:        record.AuthID,
		AuthIndex:     record.AuthIndex,
		AuthType:      record.AuthType,
		Success:       !record.Failed,
		StatusCode:    statusCode,
		LatencyMS:     record.Latency.Milliseconds(),
		InputTokens:   record.Detail.InputTokens,
		OutputTokens:  record.Detail.OutputTokens,
		TotalTokens:   record.Detail.TotalTokens,
		RequestParams: requestParams,
	}
	key := accountPoolUsageSummaryKey(item)
	r.records = append(r.records, item)
	if overflow := len(r.records) - maxAccountPoolUsageRecords; overflow > 0 {
		copy(r.records, r.records[overflow:])
		r.records = r.records[:len(r.records)-overflow]
	}
	if r.summaries == nil {
		r.summaries = make(map[string]*accountPoolUsageSummary)
	}
	summary := r.summaries[key]
	if summary == nil {
		summary = &accountPoolUsageSummary{
			Key:          key,
			ServiceEmail: item.ServiceEmail,
			AuthID:       item.AuthID,
			AuthIndex:    item.AuthIndex,
			AuthType:     item.AuthType,
			Provider:     item.Provider,
			Model:        item.Model,
			Alias:        item.Alias,
		}
		r.summaries[key] = summary
	}
	summary.Requests++
	if item.Success {
		summary.Successes++
	} else {
		summary.Failures++
	}
	summary.InputTokens += item.InputTokens
	summary.OutputTokens += item.OutputTokens
	summary.TotalTokens += item.TotalTokens
	if summary.ServiceEmail == "" {
		summary.ServiceEmail = item.ServiceEmail
	}
	if summary.AuthID == "" {
		summary.AuthID = item.AuthID
	}
	if summary.AuthIndex == "" {
		summary.AuthIndex = item.AuthIndex
	}
	if summary.AuthType == "" {
		summary.AuthType = item.AuthType
	}
	if summary.Provider == "" {
		summary.Provider = item.Provider
	}
	if summary.Model == "" {
		summary.Model = item.Model
	}
	if summary.Alias == "" {
		summary.Alias = item.Alias
	}
	summary.LastUsedAt = item.RequestedAt
	r.mu.Unlock()
}

func (r *accountPoolUsageRecorder) List(limit int) []accountPoolUsageRecord {
	records, _ := r.ListPage(limit, 0)
	return records
}

func (r *accountPoolUsageRecorder) ListPage(limit int, offset int) ([]accountPoolUsageRecord, int) {
	if r == nil {
		return nil, 0
	}
	if limit <= 0 || limit > maxAccountPoolUsageRecords {
		limit = 80
	}
	if offset < 0 {
		offset = 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	total := len(r.records)
	if offset >= total {
		return []accountPoolUsageRecord{}, total
	}
	out := make([]accountPoolUsageRecord, 0, limit)
	skipped := 0
	for i := len(r.records) - 1; i >= 0 && len(out) < limit; i-- {
		if skipped < offset {
			skipped++
			continue
		}
		out = append(out, r.records[i])
	}
	return out, total
}

func (r *accountPoolUsageRecorder) Clear() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.records = nil
	r.summaries = nil
	r.mu.Unlock()
}

func (r *accountPoolUsageRecorder) Summaries() []accountPoolUsageSummary {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.summaries) == 0 {
		return nil
	}
	out := make([]accountPoolUsageSummary, 0, len(r.summaries))
	for _, item := range r.summaries {
		if item == nil {
			continue
		}
		out = append(out, *item)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		if out[i].TotalTokens != out[j].TotalTokens {
			return out[i].TotalTokens > out[j].TotalTokens
		}
		return out[i].Key < out[j].Key
	})
	return out
}

func accountPoolUsageSummaryKey(item accountPoolUsageRecord) string {
	if value := strings.ToLower(strings.TrimSpace(item.ServiceEmail)); value != "" {
		return "email:" + value
	}
	if value := strings.TrimSpace(item.AuthID); value != "" {
		return "auth_id:" + value
	}
	if value := strings.TrimSpace(item.AuthIndex); value != "" {
		return "auth_index:" + value
	}
	if value := strings.TrimSpace(item.SessionID); value != "" {
		return "session:" + value
	}
	return "unknown"
}

func accountPoolRequestIdentity(ctx context.Context) (sessionID string, userID string, username string, requestPath string, requestParams string) {
	ginCtx, ok := ctx.Value("gin").(*gin.Context)
	if !ok || ginCtx == nil || ginCtx.Request == nil {
		return "", "", "", "", ""
	}
	sessionID = strings.TrimSpace(ginCtx.GetHeader("X-Session-ID"))
	userID, username = parseNewAPISessionID(sessionID)
	if ginCtx.Request.URL != nil {
		requestPath = strings.TrimSpace(ginCtx.Request.URL.Path)
	}
	requestParams = sanitizeAccountPoolUsageRequestParams(apimiddleware.CapturedRequestBody(ginCtx))
	return sessionID, userID, username, requestPath, requestParams
}

func sanitizeAccountPoolUsageRequestParams(body []byte) string {
	body = []byte(strings.TrimSpace(string(body)))
	if len(body) == 0 {
		return ""
	}
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	sanitized := sanitizeUsageValue(payload)
	out, err := json.MarshalIndent(sanitized, "", "  ")
	if err != nil {
		return ""
	}
	return string(out)
}

func sanitizeUsageValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, child := range typed {
			if isUsageContextKey(key) {
				out[key] = emptyUsageContextValue(child)
				continue
			}
			if isUsageSecretKey(key) {
				out[key] = "[redacted]"
				continue
			}
			out[key] = sanitizeUsageValue(child)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, child := range typed {
			out[i] = sanitizeUsageValue(child)
		}
		return out
	default:
		return value
	}
}

func isUsageContextKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "messages", "input", "contents", "prompt", "prompts", "context", "contexts",
		"conversation", "conversation_history", "chat_history", "history", "system",
		"system_instruction", "instructions":
		return true
	default:
		return false
	}
}

func isUsageSecretKey(key string) bool {
	lower := strings.ToLower(strings.TrimSpace(key))
	return strings.Contains(lower, "token") ||
		strings.Contains(lower, "secret") ||
		strings.Contains(lower, "api_key") ||
		strings.Contains(lower, "apikey") ||
		strings.Contains(lower, "authorization")
}

func emptyUsageContextValue(value any) any {
	switch value.(type) {
	case []any:
		return []any{}
	case map[string]any:
		return map[string]any{}
	case string:
		return ""
	default:
		return nil
	}
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
	offset := 0
	if raw := strings.TrimSpace(c.Query("offset")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid offset: %s", raw)})
			return
		}
		offset = parsed
	}
	if raw := strings.TrimSpace(c.Query("page")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid page: %s", raw)})
			return
		}
		offset = (parsed - 1) * limit
	}
	records, total := accountPoolUsage.ListPage(limit, offset)
	c.JSON(http.StatusOK, gin.H{
		"records":   records,
		"summaries": accountPoolUsage.Summaries(),
		"total":     total,
		"limit":     limit,
		"offset":    offset,
	})
}

func (h *Handler) ClearAccountPoolUsageRecords(c *gin.Context) {
	accountPoolUsage.Clear()
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
