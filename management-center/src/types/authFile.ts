/**
 * 认证文件相关类型
 * 基于原项目 src/modules/auth-files.js
 */

import type { RecentRequestBucket } from '@/utils/recentRequests';

export type AuthFileType =
  | 'qwen'
  | 'kimi'
  | 'gemini'
  | 'gemini-cli'
  | 'aistudio'
  | 'claude'
  | 'codex'
  | 'antigravity'
  | 'iflow'
  | 'vertex'
  | 'empty'
  | 'unknown';

export interface AuthFileItem {
  name: string;
  type?: AuthFileType | string;
  provider?: string;
  size?: number;
  authIndex?: string | number | null;
  runtimeOnly?: boolean | string;
  disabled?: boolean;
  unavailable?: boolean;
  status?: string;
  statusMessage?: string;
  lastRefresh?: string | number;
  modified?: number;
  check_status?: string;
  checkStatus?: string;
  check_message?: string;
  checkMessage?: string;
  check_plan?: string;
  checkPlan?: string;
  check_quota_lines?: string[] | string;
  checkQuotaLines?: string[] | string;
  check_quota_remaining_percent?: number;
  checkQuotaRemainingPercent?: number;
  check_status_code?: number;
  checkStatusCode?: number;
  check_real_request_ok?: boolean;
  checkRealRequestOk?: boolean;
  check_real_request_error?: string;
  checkRealRequestError?: string;
  check_checked_at?: number | string;
  checkCheckedAt?: number | string;
  check_content_hash?: string;
  checkContentHash?: string;
  success?: unknown;
  failed?: unknown;
  recent_requests?: RecentRequestBucket[];
  recentRequests?: RecentRequestBucket[];
  account_cost?: number;
  account_started_at?: string;
  account_stopped_at?: string;
  account_lifetime_seconds?: number;
  source_channel?: string;
  accountCost?: number;
  accountStartedAt?: string;
  accountStoppedAt?: string;
  accountLifetimeSeconds?: number;
  sourceChannel?: string;
  [key: string]: unknown;
}

export interface AuthFilesResponse {
  files: AuthFileItem[];
  total?: number;
  folders?: Array<{
    folder: string;
    source_model?: string;
    source_info?: string;
    count?: number;
    requests?: number;
    total_tokens?: number;
    total_usd?: number;
    created_at?: string;
    updated_at?: string;
  }>;
}
