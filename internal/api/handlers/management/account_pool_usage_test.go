package management

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func TestAccountPoolUsageRecordsAreNotTrimmed(t *testing.T) {
	t.Parallel()

	recorder := &accountPoolUsageRecorder{}
	for i := 0; i < 305; i++ {
		recorder.HandleUsage(context.Background(), usage.Record{
			Provider:    "codex",
			Model:       "gpt-5.5",
			AuthID:      "auth",
			Source:      "account@example.com",
			RequestedAt: time.Unix(int64(i), 0),
			Detail: usage.Detail{
				InputTokens:  10,
				OutputTokens: 1,
				TotalTokens:  11,
			},
		})
	}

	records, total := recorder.ListPage(400, 0)
	if total != 305 {
		t.Fatalf("total = %d, want 305", total)
	}
	if len(records) != 305 {
		t.Fatalf("records = %d, want 305", len(records))
	}
	totals := recorder.Totals()
	if totals.Requests != 305 || totals.InputTokens != 3050 || totals.OutputTokens != 305 || totals.TotalTokens != 3355 {
		t.Fatalf("totals = %#v, want requests/input/output/total 305/3050/305/3355", totals)
	}
}

func TestAccountPoolRequestIdentityInfersPlainSessionUsername(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("X-Session-ID", "kinsovip")
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = req

	sessionID, userID, username, requestPath, _ := accountPoolRequestIdentity(context.WithValue(context.Background(), "gin", ginCtx))
	if sessionID != "kinsovip" {
		t.Fatalf("sessionID = %q, want kinsovip", sessionID)
	}
	if userID != "" {
		t.Fatalf("userID = %q, want empty", userID)
	}
	if username != "kinsovip" {
		t.Fatalf("username = %q, want kinsovip", username)
	}
	if requestPath != "/v1/chat/completions" {
		t.Fatalf("requestPath = %q, want /v1/chat/completions", requestPath)
	}
}

func TestAccountPoolRequestIdentityKeepsOpaqueSessionAsSessionOnly(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)
	req := httptest.NewRequest("POST", "/v1/responses", nil)
	req.Header.Set("X-Session-ID", "019e2ee8-7697-7cc1-81cb-5eb87c81b07d")
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = req

	sessionID, userID, username, _, _ := accountPoolRequestIdentity(context.WithValue(context.Background(), "gin", ginCtx))
	if sessionID != "019e2ee8-7697-7cc1-81cb-5eb87c81b07d" {
		t.Fatalf("sessionID = %q, want UUID session", sessionID)
	}
	if userID != "" || username != "" {
		t.Fatalf("userID/username = %q/%q, want empty/empty", userID, username)
	}
}
