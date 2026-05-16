package management

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type sub2APIImportJobStatus string

const (
	sub2APIImportPending sub2APIImportJobStatus = "pending"
	sub2APIImportRunning sub2APIImportJobStatus = "running"
	sub2APIImportDone    sub2APIImportJobStatus = "done"
	sub2APIImportFailed  sub2APIImportJobStatus = "failed"
)

type sub2APIImportJob struct {
	ID        string                 `json:"id"`
	Status    sub2APIImportJobStatus `json:"status"`
	Total     int                    `json:"total"`
	Done      int                    `json:"done"`
	Imported  int                    `json:"imported"`
	Failed    int                    `json:"failed"`
	Files     []string               `json:"files"`
	Warnings  []string               `json:"warnings"`
	Error     string                 `json:"error,omitempty"`
	CreatedAt string                 `json:"created_at"`
	UpdatedAt string                 `json:"updated_at"`
}

type sub2APIImportRequest struct {
	Source string `json:"source"`
}

type sub2APIExport struct {
	ExportedAt string           `json:"exported_at"`
	Accounts   []sub2APIAccount `json:"accounts"`
}

type sub2APIAccount struct {
	Name        string             `json:"name"`
	Credentials sub2APICredentials `json:"credentials"`
	Extra       map[string]any     `json:"extra"`
	Raw         map[string]any     `json:"-"`
}

type sub2APICredentials struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	IDToken          string `json:"id_token"`
	ClientID         string `json:"client_id"`
	Email            string `json:"email"`
	ChatGPTAccountID string `json:"chatgpt_account_id"`
	ExpiresAt        int64  `json:"expires_at"`
}

type sub2APICPAAuthFile struct {
	IDToken              string `json:"id_token"`
	ClientID             string `json:"client_id"`
	AccessToken          string `json:"access_token"`
	RefreshToken         string `json:"refresh_token"`
	AccountID            string `json:"account_id"`
	LastRefresh          string `json:"last_refresh"`
	Email                string `json:"email"`
	Type                 string `json:"type"`
	Expired              string `json:"expired"`
	Password             string `json:"password,omitempty"`
	Phone                string `json:"phone,omitempty"`
	RegistrationStrategy string `json:"registration_strategy,omitempty"`
}

var sub2APISafeFilePartPattern = regexp.MustCompile(`[^\w.+@-]+`)

func (h *Handler) StartSub2APIImport(c *gin.Context) {
	var req sub2APIImportRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	source := strings.TrimSpace(req.Source)
	if source == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "source is required"})
		return
	}

	now := time.Now().UTC()
	job := &sub2APIImportJob{
		ID:        fmt.Sprintf("sub2api-%d", now.UnixNano()),
		Status:    sub2APIImportPending,
		CreatedAt: now.Format(time.RFC3339),
		UpdatedAt: now.Format(time.RFC3339),
	}
	h.storeSub2APIJob(job)

	go h.runSub2APIImport(context.Background(), job.ID, source)

	c.JSON(http.StatusAccepted, gin.H{"job": job})
}

func (h *Handler) GetSub2APIImport(c *gin.Context) {
	job := h.getSub2APIJob(c.Param("id"))
	if job == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"job": job})
}

func (h *Handler) storeSub2APIJob(job *sub2APIImportJob) {
	h.sub2APIJobsMu.Lock()
	defer h.sub2APIJobsMu.Unlock()
	h.sub2APIJobs[job.ID] = cloneSub2APIJob(job)
}

func (h *Handler) updateSub2APIJob(id string, update func(*sub2APIImportJob)) {
	h.sub2APIJobsMu.Lock()
	defer h.sub2APIJobsMu.Unlock()
	job := h.sub2APIJobs[id]
	if job == nil {
		return
	}
	update(job)
	job.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
}

func (h *Handler) getSub2APIJob(id string) *sub2APIImportJob {
	h.sub2APIJobsMu.Lock()
	defer h.sub2APIJobsMu.Unlock()
	return cloneSub2APIJob(h.sub2APIJobs[id])
}

func cloneSub2APIJob(job *sub2APIImportJob) *sub2APIImportJob {
	if job == nil {
		return nil
	}
	out := *job
	out.Files = append([]string(nil), job.Files...)
	out.Warnings = append([]string(nil), job.Warnings...)
	return &out
}

func (h *Handler) runSub2APIImport(ctx context.Context, jobID string, source string) {
	_ = ctx
	docs, err := parseSub2APIDocuments(source)
	if err != nil {
		h.updateSub2APIJob(jobID, func(job *sub2APIImportJob) {
			job.Status = sub2APIImportFailed
			job.Error = err.Error()
		})
		return
	}

	accounts := flattenSub2APIAccounts(docs)
	h.updateSub2APIJob(jobID, func(job *sub2APIImportJob) {
		job.Status = sub2APIImportRunning
		job.Total = len(accounts)
	})
	if len(accounts) == 0 {
		h.updateSub2APIJob(jobID, func(job *sub2APIImportJob) {
			job.Status = sub2APIImportFailed
			job.Error = "no sub2api accounts found"
		})
		return
	}

	stamp := time.Now().Unix()
	for index, item := range accounts {
		cpa, warnings := sub2APIAccountToCPA(item.account, item.exportedAt)
		nameSeed := firstSub2APINonEmpty(cpa.Phone, cpa.Email, cpa.AccountID, item.account.Name, fmt.Sprintf("account_%d", index+1))
		fileName := fmt.Sprintf("token_%s_%d.json", safeSub2APIFilePart(nameSeed), stamp+int64(index))
		data, errMarshal := json.MarshalIndent(cpa, "", "  ")
		if errMarshal != nil {
			h.updateSub2APIJob(jobID, func(job *sub2APIImportJob) {
				job.Done++
				job.Failed++
				job.Warnings = append(job.Warnings, fmt.Sprintf("%s: %v", fileName, errMarshal))
			})
			continue
		}
		if errWrite := h.upsertAccountPoolArchiveFile(filepath.Base(fileName), data); errWrite != nil {
			h.updateSub2APIJob(jobID, func(job *sub2APIImportJob) {
				job.Done++
				job.Failed++
				job.Warnings = append(job.Warnings, fmt.Sprintf("%s: %v", fileName, errWrite))
			})
			continue
		}
		h.updateSub2APIJob(jobID, func(job *sub2APIImportJob) {
			job.Done++
			job.Imported++
			job.Files = append(job.Files, fileName)
			job.Warnings = append(job.Warnings, warnings...)
		})
	}

	h.updateSub2APIJob(jobID, func(job *sub2APIImportJob) {
		if job.Imported == 0 && job.Failed > 0 {
			job.Status = sub2APIImportFailed
			job.Error = "all accounts failed to import"
			return
		}
		job.Status = sub2APIImportDone
	})
}

func parseSub2APIDocuments(source string) ([]sub2APIExport, error) {
	var raw any
	if err := json.Unmarshal([]byte(source), &raw); err != nil {
		return nil, fmt.Errorf("invalid sub2api json: %w", err)
	}
	var docs []sub2APIExport
	switch typed := raw.(type) {
	case []any:
		if looksLikeSub2APIAccountArray(typed) {
			docs = append(docs, sub2APIExport{Accounts: decodeSub2APIAccounts(typed)})
			return docs, nil
		}
		for _, item := range typed {
			doc, ok := decodeSub2APIExport(item)
			if ok {
				docs = append(docs, doc)
			}
		}
	case map[string]any:
		doc, ok := decodeSub2APIExport(typed)
		if ok {
			docs = append(docs, doc)
		}
	}
	if len(docs) == 0 {
		return nil, fmt.Errorf("json does not contain sub2api accounts")
	}
	return docs, nil
}

func looksLikeSub2APIAccountArray(items []any) bool {
	for _, item := range items {
		record, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if _, hasCredentials := record["credentials"].(map[string]any); hasCredentials {
			return true
		}
	}
	return false
}

func decodeSub2APIExport(value any) (sub2APIExport, bool) {
	record, ok := value.(map[string]any)
	if !ok {
		return sub2APIExport{}, false
	}
	if nested, ok := record["data"]; ok {
		if doc, ok := decodeSub2APIExport(nested); ok {
			return doc, true
		}
	}
	accountsRaw, ok := record["accounts"].([]any)
	if !ok {
		return sub2APIExport{}, false
	}
	return sub2APIExport{
		ExportedAt: stringFromAny(record["exported_at"]),
		Accounts:   decodeSub2APIAccounts(accountsRaw),
	}, true
}

func decodeSub2APIAccounts(items []any) []sub2APIAccount {
	accounts := make([]sub2APIAccount, 0, len(items))
	for _, item := range items {
		record, ok := item.(map[string]any)
		if !ok {
			continue
		}
		credentials, _ := record["credentials"].(map[string]any)
		extra, _ := record["extra"].(map[string]any)
		accounts = append(accounts, sub2APIAccount{
			Name: stringFromAny(record["name"]),
			Credentials: sub2APICredentials{
				AccessToken:      stringFromAny(credentials["access_token"]),
				RefreshToken:     stringFromAny(credentials["refresh_token"]),
				IDToken:          stringFromAny(credentials["id_token"]),
				ClientID:         stringFromAny(credentials["client_id"]),
				Email:            stringFromAny(credentials["email"]),
				ChatGPTAccountID: stringFromAny(credentials["chatgpt_account_id"]),
				ExpiresAt:        int64FromAny(credentials["expires_at"]),
			},
			Extra: extra,
			Raw:   record,
		})
	}
	return accounts
}

type sub2APIAccountItem struct {
	account    sub2APIAccount
	exportedAt string
}

func flattenSub2APIAccounts(docs []sub2APIExport) []sub2APIAccountItem {
	var out []sub2APIAccountItem
	for _, doc := range docs {
		for _, account := range doc.Accounts {
			out = append(out, sub2APIAccountItem{account: account, exportedAt: doc.ExportedAt})
		}
	}
	return out
}

func sub2APIAccountToCPA(account sub2APIAccount, exportedAt string) (sub2APICPAAuthFile, []string) {
	credentials := account.Credentials
	sourceCPA, _ := account.Extra["source_cpa"].(map[string]any)
	accessPayload := decodeSub2APIJWTPayload(credentials.AccessToken)
	idToken := firstSub2APINonEmpty(credentials.IDToken, stringFromAny(sourceCPA["id_token"]))
	idPayload := decodeSub2APIJWTPayload(idToken)
	phone := firstSub2APINonEmpty(
		stringFromAny(sourceCPA["phone"]),
		phoneLikeValue(credentials.Email),
		phoneLikeValue(account.Name),
	)
	tokenEmail := firstSub2APINonEmpty(
		stringFromAny(nestedMapValue(accessPayload, "https://api.openai.com/profile", "email")),
		stringFromAny(accessPayload["email"]),
		stringFromAny(nestedMapValue(idPayload, "https://api.openai.com/profile", "email")),
		stringFromAny(idPayload["email"]),
	)
	email := firstSub2APINonEmpty(
		nonPhoneValue(stringFromAny(sourceCPA["email"])),
		tokenEmail,
		nonPhoneValue(credentials.Email),
		nonPhoneValue(account.Name),
		phone,
	)
	accountID := firstSub2APINonEmpty(
		credentials.ChatGPTAccountID,
		authClaim(accessPayload, "chatgpt_account_id"),
		authClaim(idPayload, "chatgpt_account_id"),
		authClaim(accessPayload, "chatgpt_account_user_id"),
		authClaim(idPayload, "chatgpt_account_user_id"),
		authClaim(accessPayload, "account_id"),
		authClaim(idPayload, "account_id"),
		authClaim(accessPayload, "user_id"),
		authClaim(idPayload, "user_id"),
		stringFromAny(accessPayload["sub"]),
		stringFromAny(idPayload["sub"]),
	)
	lastRefresh := firstSub2APINonEmpty(epochToSub2APIIso(int64FromAny(accessPayload["iat"])), exportedAt, time.Now().UTC().Format(time.RFC3339))
	expiresAt := credentials.ExpiresAt
	if expiresAt == 0 {
		expiresAt = int64FromAny(accessPayload["exp"])
	}

	warnings := make([]string, 0)
	if credentials.AccessToken == "" {
		warnings = append(warnings, fmt.Sprintf("%s 缺少 access_token", account.Name))
	}
	if credentials.RefreshToken == "" {
		warnings = append(warnings, fmt.Sprintf("%s 缺少 refresh_token", account.Name))
	}
	if idToken == "" {
		warnings = append(warnings, fmt.Sprintf("%s 没有 id_token，CPA 文件会留空", account.Name))
	}

	cpa := sub2APICPAAuthFile{
		IDToken:      idToken,
		ClientID:     firstSub2APINonEmpty(credentials.ClientID, stringFromAny(accessPayload["client_id"])),
		AccessToken:  credentials.AccessToken,
		RefreshToken: credentials.RefreshToken,
		AccountID:    accountID,
		LastRefresh:  lastRefresh,
		Email:        email,
		Type:         "codex",
		Expired:      epochToSub2APIIso(expiresAt),
		Password:     stringFromAny(sourceCPA["password"]),
		Phone:        phone,
	}
	if strategy := stringFromAny(sourceCPA["registration_strategy"]); strategy != "" {
		cpa.RegistrationStrategy = strategy
	} else if phone != "" {
		cpa.RegistrationStrategy = "sms_first"
	}
	return cpa, warnings
}

func decodeSub2APIJWTPayload(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		if data, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return nil
		}
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil
	}
	return payload
}

func nestedMapValue(record map[string]any, parent string, key string) any {
	if record == nil {
		return nil
	}
	nested, _ := record[parent].(map[string]any)
	if nested == nil {
		return nil
	}
	return nested[key]
}

func authClaim(payload map[string]any, key string) string {
	return stringFromAny(nestedMapValue(payload, "https://api.openai.com/auth", key))
}

func stringFromAny(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return typed.String()
	case float64:
		if typed == float64(int64(typed)) {
			return fmt.Sprintf("%d", int64(typed))
		}
		return fmt.Sprintf("%v", typed)
	default:
		return ""
	}
}

func int64FromAny(value any) int64 {
	switch typed := value.(type) {
	case int64:
		return typed
	case int:
		return int64(typed)
	case float64:
		return int64(typed)
	case json.Number:
		out, _ := typed.Int64()
		return out
	case string:
		var out int64
		_, _ = fmt.Sscanf(strings.TrimSpace(typed), "%d", &out)
		return out
	default:
		return 0
	}
}

func epochToSub2APIIso(value int64) string {
	if value <= 0 {
		return ""
	}
	return time.Unix(value, 0).UTC().Format(time.RFC3339)
}

func phoneLikeValue(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "+") {
		return value
	}
	return ""
}

func nonPhoneValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "+") {
		return ""
	}
	return value
}

func firstSub2APINonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}

func safeSub2APIFilePart(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "account"
	}
	value = sub2APISafeFilePartPattern.ReplaceAllString(value, "_")
	if len(value) > 80 {
		value = value[:80]
	}
	if value == "" {
		return "account"
	}
	return value
}
