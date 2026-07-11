package management

import (
	"encoding/json"
	"testing"
	"time"
)

func TestAntigravityQuotaWindowKeepsImminentWeeklyQuotaLongWindow(t *testing.T) {
	now := time.Date(2026, 7, 11, 8, 0, 0, 0, time.UTC)
	weeklyRemaining := 0.75
	shortRemaining := 0.9
	state := antigravityQuotaFileState{
		Status: "success",
		Groups: []antigravityQuotaGroup{{
			ID: "gemini",
			Buckets: []antigravityQuotaBucket{
				{BucketID: "gemini-short", Window: "short", ResetTime: now.Add(4 * time.Hour).Format(time.RFC3339), RemainingFraction: &shortRemaining},
				{BucketID: "gemini-weekly", Window: "weekly", ResetTime: now.Add(6 * time.Hour).Format(time.RFC3339), RemainingFraction: &weeklyRemaining},
			},
		}},
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}

	weekly := antigravityWeeklyQuotaByFamily(raw, now)
	if got := weekly["gemini"]; got.RemainingPercent != 75 || !got.ResetAt.Equal(now.Add(6*time.Hour)) {
		t.Fatalf("weekly snapshot = %#v, want imminent weekly quota", got)
	}
	short := antigravityFiveHourQuotaByFamily(raw, now)
	if got := short["gemini"]; got.RemainingPercent != 90 || !got.ResetAt.Equal(now.Add(4*time.Hour)) {
		t.Fatalf("short snapshot = %#v, want five-hour quota only", got)
	}
}
