package management

import (
	"net/http"
	"testing"
	"time"
)

func TestAccountPoolAdvanceLifetimeAccumulatesOnlyActiveWindows(t *testing.T) {
	base := time.Date(2026, 5, 26, 1, 0, 0, 0, time.UTC)
	acc, activeSince, stoppedAt := accountPoolAdvanceLifetime(0, "", accountPoolCheckResultPayload{Status: "success", CheckedAt: base.UnixMilli()}, base)
	if acc != 0 || activeSince != base.Format(time.RFC3339) || stoppedAt != "" {
		t.Fatalf("success start = acc %d active %q stopped %q", acc, activeSince, stoppedAt)
	}

	failAt := base.Add(30 * time.Minute)
	acc, activeSince, stoppedAt = accountPoolAdvanceLifetime(acc, activeSince, accountPoolCheckResultPayload{Status: "error", CheckedAt: failAt.UnixMilli()}, failAt)
	if acc != int64((30*time.Minute).Seconds()) || activeSince != "" || stoppedAt != failAt.Format(time.RFC3339) {
		t.Fatalf("error stop = acc %d active %q stopped %q", acc, activeSince, stoppedAt)
	}

	later := failAt.Add(2 * time.Hour)
	acc2, activeSince2, _ := accountPoolAdvanceLifetime(acc, activeSince, accountPoolCheckResultPayload{Status: "failed", Message: "auth token not found", CheckedAt: later.UnixMilli()}, later)
	if acc2 != acc || activeSince2 != "" {
		t.Fatalf("inactive failure should not add time: acc %d active %q", acc2, activeSince2)
	}

	quotaAt := later.Add(10 * time.Minute)
	acc, activeSince, stoppedAt = accountPoolAdvanceLifetime(acc2, activeSince2, accountPoolCheckResultPayload{Status: "error", StatusCode: http.StatusTooManyRequests, Message: "quota exceeded", CheckedAt: quotaAt.UnixMilli()}, quotaAt)
	if acc != acc2 || activeSince != quotaAt.Format(time.RFC3339) || stoppedAt != "" {
		t.Fatalf("quota exceeded should be active: acc %d active %q stopped %q", acc, activeSince, stoppedAt)
	}

	usageLimitAt := quotaAt.Add(5 * time.Minute)
	acc, activeSince, stoppedAt = accountPoolAdvanceLifetime(acc, activeSince, accountPoolCheckResultPayload{Status: "error", StatusCode: http.StatusOK, Message: "模型检测请求失败: 429: The usage limit has been reached", CheckedAt: usageLimitAt.UnixMilli()}, usageLimitAt)
	if acc != acc2 || activeSince != quotaAt.Format(time.RFC3339) || stoppedAt != "" {
		t.Fatalf("usage limit reached should keep active segment: acc %d active %q stopped %q", acc, activeSince, stoppedAt)
	}

	end := quotaAt.Add(15 * time.Minute)
	acc, activeSince, stoppedAt = accountPoolAdvanceLifetime(acc, activeSince, accountPoolCheckResultPayload{Status: "error", Message: "401 unauthorized", CheckedAt: end.UnixMilli()}, end)
	want := int64((45 * time.Minute).Seconds())
	if acc != want || activeSince != "" || stoppedAt != end.Format(time.RFC3339) {
		t.Fatalf("second inactive stop = acc %d want %d active %q stopped %q", acc, want, activeSince, stoppedAt)
	}
}
