package management

import "testing"

func TestSub2APIAccountToCPA(t *testing.T) {
	source := `{
		"exported_at": "2026-05-14T01:00:00Z",
		"accounts": [{
			"name": "user@example.com",
			"credentials": {
				"access_token": "access",
				"refresh_token": "refresh",
				"id_token": "id",
				"client_id": "client",
				"email": "user@example.com",
				"chatgpt_account_id": "acct",
				"expires_at": 1770000000
			}
		}]
	}`

	docs, err := parseSub2APIDocuments(source)
	if err != nil {
		t.Fatalf("parseSub2APIDocuments failed: %v", err)
	}
	items := flattenSub2APIAccounts(docs)
	if len(items) != 1 {
		t.Fatalf("expected one account, got %d", len(items))
	}
	cpa, warnings := sub2APIAccountToCPA(items[0].account, items[0].exportedAt)
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings, got %#v", warnings)
	}
	if cpa.AccessToken != "access" || cpa.RefreshToken != "refresh" || cpa.IDToken != "id" {
		t.Fatalf("unexpected tokens: %#v", cpa)
	}
	if cpa.Email != "user@example.com" || cpa.AccountID != "acct" || cpa.ClientID != "client" {
		t.Fatalf("unexpected identity fields: %#v", cpa)
	}
	if cpa.Type != "codex" || cpa.Expired == "" || cpa.LastRefresh != "2026-05-14T01:00:00Z" {
		t.Fatalf("unexpected metadata fields: %#v", cpa)
	}
}

func TestParseSub2APIDocumentsWrappedData(t *testing.T) {
	source := `{
		"skip_default_group_bind": true,
		"data": {
			"type": "sub2api",
			"version": "1",
			"exported_at": "2026-05-14T01:00:00Z",
			"accounts": [{
				"name": "wrapped@example.com",
				"credentials": {
					"access_token": "access",
					"refresh_token": "refresh",
					"id_token": "id",
					"client_id": "client",
					"email": "wrapped@example.com",
					"chatgpt_account_id": "acct",
					"expires_at": 1770000000
				}
			}]
		}
	}`

	docs, err := parseSub2APIDocuments(source)
	if err != nil {
		t.Fatalf("parseSub2APIDocuments failed: %v", err)
	}
	items := flattenSub2APIAccounts(docs)
	if len(items) != 1 {
		t.Fatalf("expected one wrapped account, got %d", len(items))
	}
	if items[0].account.Credentials.RefreshToken != "refresh" {
		t.Fatalf("unexpected wrapped credentials: %#v", items[0].account.Credentials)
	}
}
