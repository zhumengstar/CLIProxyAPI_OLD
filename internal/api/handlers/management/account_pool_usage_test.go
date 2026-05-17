package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestAccountPoolUsageClearRemovesSummaries(t *testing.T) {
	recorder := &accountPoolUsageRecorder{
		records: []accountPoolUsageRecord{
			{ID: "1", ServiceEmail: "user@example.com", TotalTokens: 10},
		},
		summaries: map[string]*accountPoolUsageSummary{
			"email:user@example.com": {
				Key:          "email:user@example.com",
				ServiceEmail: "user@example.com",
				Requests:     1,
				TotalTokens:  10,
			},
		},
	}

	recorder.Clear()

	if len(recorder.records) != 0 {
		t.Fatalf("records length = %d, want 0", len(recorder.records))
	}
	if len(recorder.summaries) != 0 {
		t.Fatalf("summaries length = %d, want 0", len(recorder.summaries))
	}
}

func TestAccountPoolUsageListPageLimitZeroReturnsAll(t *testing.T) {
	recorder := &accountPoolUsageRecorder{}
	for i := 1; i <= 350; i++ {
		recorder.records = append(recorder.records, accountPoolUsageRecord{ID: strconv.Itoa(i)})
	}

	records, total := recorder.ListPage(0, 0)

	if total != 350 {
		t.Fatalf("total = %d, want 350", total)
	}
	if len(records) != 350 {
		t.Fatalf("records length = %d, want 350", len(records))
	}
}

func TestAccountPoolUsageTotalsUsesSummaries(t *testing.T) {
	recorder := &accountPoolUsageRecorder{
		summaries: map[string]*accountPoolUsageSummary{
			"email:a@example.com": {
				Requests:            2,
				Successes:           1,
				Failures:            1,
				InputTokens:         10,
				OutputTokens:        20,
				CachedTokens:        30,
				CacheReadTokens:     40,
				CacheCreationTokens: 50,
				TotalTokens:         60,
			},
			"email:b@example.com": {
				Requests:     3,
				Successes:    3,
				InputTokens:  7,
				OutputTokens: 8,
				TotalTokens:  9,
			},
		},
	}

	totals := recorder.Totals()

	if totals.Requests != 5 || totals.Successes != 4 || totals.Failures != 1 {
		t.Fatalf("request totals = (%d, %d, %d), want (5, 4, 1)", totals.Requests, totals.Successes, totals.Failures)
	}
	if totals.InputTokens != 17 || totals.OutputTokens != 28 || totals.TotalTokens != 69 {
		t.Fatalf("token totals = (%d, %d, %d), want (17, 28, 69)", totals.InputTokens, totals.OutputTokens, totals.TotalTokens)
	}
}

func TestAccountPoolRequestIdentityReadsOneAPIUsernameHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ginCtx.Request.Header.Set("X-Session-ID", "session-1")
	ginCtx.Request.Header.Set("X_OneAPI_User_ID", "42")
	ginCtx.Request.Header.Set("X_OneAPI_User_Name", "admin")

	sessionID, userID, username, _, _ := accountPoolRequestIdentity(context.WithValue(context.Background(), "gin", ginCtx))

	if sessionID != "session-1" || userID != "42" || username != "admin" {
		t.Fatalf("identity = (%q, %q, %q), want (session-1, 42, admin)", sessionID, userID, username)
	}
}

func TestAccountPoolRequestIdentityReadsCodexTurnMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ginCtx.Request.Header.Set("X-Session-ID", "019e3102-5e26-79f2-b3db-fa827155c090")
	ginCtx.Request.Header.Set("X-Codex-Turn-Metadata", `{"user":{"id":42,"username":"admin"},"turn_id":"turn-1"}`)

	sessionID, userID, username, _, _ := accountPoolRequestIdentity(context.WithValue(context.Background(), "gin", ginCtx))

	if sessionID != "019e3102-5e26-79f2-b3db-fa827155c090" || userID != "42" || username != "admin" {
		t.Fatalf("identity = (%q, %q, %q), want (019e3102..., 42, admin)", sessionID, userID, username)
	}
}

func TestAccountPoolUsageIdentityForSessionBackfillsUsername(t *testing.T) {
	recorder := &accountPoolUsageRecorder{
		records: []accountPoolUsageRecord{
			{SessionID: "session-1", NewAPIUserID: "42", Username: "admin"},
		},
	}

	userID, username := recorder.identityForSession("session-1")

	if userID != "42" || username != "admin" {
		t.Fatalf("identity = (%q, %q), want (42, admin)", userID, username)
	}
}
