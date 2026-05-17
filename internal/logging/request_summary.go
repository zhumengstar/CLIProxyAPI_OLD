package logging

import (
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	RequestSummaryUsernameKey       = "REQUEST_SUMMARY_USERNAME"
	RequestSummaryUserIDKey         = "REQUEST_SUMMARY_USER_ID"
	RequestSummarySessionIDKey      = "REQUEST_SUMMARY_SESSION_ID"
	RequestSummaryModelKey          = "REQUEST_SUMMARY_MODEL"
	RequestSummaryRequestTypeKey    = "REQUEST_SUMMARY_REQUEST_TYPE"
	RequestSummaryUpstreamKey       = "REQUEST_SUMMARY_UPSTREAM"
	RequestSummaryMatchedAccountKey = "REQUEST_SUMMARY_MATCHED_ACCOUNT"
	RequestSummaryAttemptsKey       = "REQUEST_SUMMARY_ATTEMPTS"
	RequestSummaryFirstByteMSKey    = "REQUEST_SUMMARY_FIRST_BYTE_MS"
	RequestSummaryTotalTokensKey    = "REQUEST_SUMMARY_TOTAL_TOKENS"
)

func SetRequestSummaryValue(c *gin.Context, key string, value string) {
	if c == nil {
		return
	}
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return
	}
	c.Set(key, trimmed)
}

func GetRequestSummaryValue(c *gin.Context, key string) string {
	if c == nil {
		return ""
	}
	raw, exists := c.Get(key)
	if !exists {
		return ""
	}
	value, ok := raw.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}
