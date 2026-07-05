package management

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const resetQuotaBatchPageSizeMax = 100

// Quota exceeded toggles
func (h *Handler) GetSwitchProject(c *gin.Context) {
	c.JSON(200, gin.H{"switch-project": h.cfg.QuotaExceeded.SwitchProject})
}
func (h *Handler) PutSwitchProject(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.QuotaExceeded.SwitchProject = v })
}

func (h *Handler) GetSwitchPreviewModel(c *gin.Context) {
	c.JSON(200, gin.H{"switch-preview-model": h.cfg.QuotaExceeded.SwitchPreviewModel})
}
func (h *Handler) PutSwitchPreviewModel(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.QuotaExceeded.SwitchPreviewModel = v })
}

// ResetQuota clears quota/cooldown routing state for one auth index.
func (h *Handler) ResetQuota(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req struct {
		AuthIndex string `json:"auth_index"`
	}
	if errBindJSON := c.ShouldBindJSON(&req); errBindJSON != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	authIndex := strings.TrimSpace(req.AuthIndex)
	if authIndex == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_index is required"})
		return
	}

	auth := h.authByIndex(authIndex)
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}

	updated, models, errReset := h.authManager.ResetQuota(c.Request.Context(), auth.ID)
	if errReset != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to reset quota: %v", errReset)})
		return
	}
	if updated == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}
	updated.EnsureIndex()

	c.JSON(http.StatusOK, gin.H{
		"status":     "ok",
		"auth_index": updated.Index,
		"models":     models,
	})
}

// ResetQuotaBatch clears quota/cooldown routing state for up to 100 auth indexes.
func (h *Handler) ResetQuotaBatch(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	var req struct {
		AuthIndexes []string `json:"auth_indexes"`
		Provider    string   `json:"provider"`
		Page        int      `json:"page"`
		PageSize    int      `json:"page_size"`
	}
	if errBindJSON := c.ShouldBindJSON(&req); errBindJSON != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	auths, page, pageSize, total, hasMore := h.resetQuotaBatchTargets(req.AuthIndexes, req.Provider, req.Page, req.PageSize)
	if len(auths) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no auth indexes matched"})
		return
	}
	if len(auths) > resetQuotaBatchPageSizeMax {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("batch size cannot exceed %d", resetQuotaBatchPageSizeMax)})
		return
	}

	results := make([]gin.H, 0, len(auths))
	succeeded := 0
	failed := 0
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		auth.EnsureIndex()
		result := gin.H{
			"auth_index": auth.Index,
			"id":         auth.ID,
			"name":       auth.FileName,
			"provider":   strings.TrimSpace(auth.Provider),
		}
		updated, models, errReset := h.authManager.ResetQuota(c.Request.Context(), auth.ID)
		if errReset != nil {
			failed++
			result["status"] = "error"
			result["error"] = fmt.Sprintf("failed to reset quota: %v", errReset)
		} else if updated == nil {
			failed++
			result["status"] = "not_found"
			result["error"] = "auth not found"
		} else {
			succeeded++
			updated.EnsureIndex()
			result["status"] = "ok"
			result["auth_index"] = updated.Index
			result["models"] = models
		}
		results = append(results, result)
	}

	statusCode := http.StatusOK
	if succeeded == 0 && failed > 0 {
		statusCode = http.StatusInternalServerError
	}
	c.JSON(statusCode, gin.H{
		"status":    "ok",
		"page":      page,
		"page_size": pageSize,
		"total":     total,
		"has_more":  hasMore,
		"succeeded": succeeded,
		"failed":    failed,
		"results":   results,
	})
}

func (h *Handler) resetQuotaBatchTargets(authIndexes []string, provider string, page, pageSize int) ([]*coreauth.Auth, int, int, int, bool) {
	pageSize = normalizeResetQuotaPageSize(pageSize)
	if page <= 0 {
		page = 1
	}

	if len(authIndexes) > 0 {
		auths := make([]*coreauth.Auth, 0, len(authIndexes))
		seen := make(map[string]struct{}, len(authIndexes))
		for _, rawIndex := range authIndexes {
			authIndex := strings.TrimSpace(rawIndex)
			if authIndex == "" {
				continue
			}
			if _, ok := seen[authIndex]; ok {
				continue
			}
			seen[authIndex] = struct{}{}
			auth := h.authByIndex(authIndex)
			if auth == nil {
				continue
			}
			auths = append(auths, auth)
		}
		return auths, 1, len(auths), len(auths), false
	}

	provider = strings.ToLower(strings.TrimSpace(provider))
	matches := make([]*coreauth.Auth, 0)
	for _, auth := range h.authManager.List() {
		if auth == nil {
			continue
		}
		if provider != "" && !strings.EqualFold(strings.TrimSpace(auth.Provider), provider) {
			continue
		}
		auth.EnsureIndex()
		matches = append(matches, auth)
	}
	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].Index < matches[j].Index
	})

	total := len(matches)
	start := (page - 1) * pageSize
	if start >= total {
		return nil, page, pageSize, total, false
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	return matches[start:end], page, pageSize, total, end < total
}

func normalizeResetQuotaPageSize(pageSize int) int {
	if pageSize <= 0 {
		return resetQuotaBatchPageSizeMax
	}
	if pageSize > resetQuotaBatchPageSizeMax {
		return resetQuotaBatchPageSizeMax
	}
	return pageSize
}
