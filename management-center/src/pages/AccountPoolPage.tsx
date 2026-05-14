import { useCallback, useEffect, useMemo, useState, type ChangeEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { Card } from '@/components/ui/Card';
import { EmptyState } from '@/components/ui/EmptyState';
import { Input } from '@/components/ui/Input';
import { Select } from '@/components/ui/Select';
import { SelectionCheckbox } from '@/components/ui/SelectionCheckbox';
import { useAccountPoolCheckStore, useNotificationStore, type AccountCheckResult } from '@/stores';
import { authFilesApi, type AccountPoolUsageSummary } from '@/services/api';
import {
  ANTIGRAVITY_CONFIG,
  CLAUDE_CONFIG,
  CODEX_CONFIG,
  GEMINI_CLI_CONFIG,
  KIMI_CONFIG,
  type QuotaConfig,
} from '@/components/quota';
import type { AuthFileItem } from '@/types/authFile';
import { downloadBlob } from '@/utils/download';
import { formatUnixTimestamp } from '@/utils/format';
import { getStatusFromError, normalizePlanType } from '@/utils/quota';
import { createZipBlob } from '@/utils/zip';
import {
  ACCOUNT_POOL_UPDATED_EVENT,
  buildAccountPoolFileContentCache,
  deleteAccountPoolRecordsByName,
  readAccountPoolRecords,
  syncAccountPoolFromAuthFiles,
  uniqueAccountPoolRecords,
  type AccountPoolRecord,
} from '@/utils/accountPool';
import styles from './AccountPoolPage.module.scss';

const ACCOUNT_POOL_CHECK_CONCURRENCY_STORAGE_KEY = 'cli-proxy-account-pool-check-concurrency';
const MIN_ACCOUNT_POOL_CHECK_CONCURRENCY = 1;
const DEFAULT_ACCOUNT_POOL_CHECK_CONCURRENCY = 5;
const MIN_ACCOUNT_POOL_PAGE_SIZE = 1;
const MAX_ACCOUNT_POOL_PAGE_SIZE = 200;
const DEFAULT_ACCOUNT_POOL_PAGE_SIZE = 100;
const DEFAULT_ACCOUNT_POOL_SORT_MODE = 'quota_desc';
const DEFAULT_ACCOUNT_POOL_PLAN_FILTER = 'all';
const DEFAULT_ACCOUNT_POOL_CHECK_STATUS_FILTER = 'all';
const DEFAULT_ACCOUNT_POOL_QUOTA_FILTER = 'all';
const LOW_ACCOUNT_POOL_QUOTA_PERCENT = 20;
const ACCOUNT_POOL_CHECK_RETRY_ATTEMPTS = 2;
const ACCOUNT_POOL_CHECK_RETRY_DELAY_MS = 700;
type AccountPoolStatusCodeStats = {
  codes: Array<[number, number]>;
  unchecked: number;
  unsupported: number;
  unknownError: number;
};
const QUOTA_CONFIGS = [
  CLAUDE_CONFIG,
  ANTIGRAVITY_CONFIG,
  CODEX_CONFIG,
  GEMINI_CLI_CONFIG,
  KIMI_CONFIG,
] as Array<QuotaConfig<unknown, unknown>>;

const getFileType = (file: AuthFileItem): string => String(file.type || file.provider || 'unknown');

const getFileModifiedLabel = (file: AuthFileItem): string => {
  const value = file.modified ?? file['modtime'] ?? file['updated_at'];
  if (typeof value === 'number' && Number.isFinite(value)) {
    return formatUnixTimestamp(value < 1e12 ? value : Math.round(value / 1000));
  }
  if (typeof value === 'string' && value.trim()) return value;
  return '';
};

const parseDateValue = (value: unknown): number | null => {
  if (typeof value === 'number' && Number.isFinite(value)) {
    return value < 1e12 ? value * 1000 : value;
  }
  if (typeof value === 'string') {
    const trimmed = value.trim();
    if (!trimmed) return null;
    const numeric = Number(trimmed);
    if (Number.isFinite(numeric)) return numeric < 1e12 ? numeric * 1000 : numeric;
    const parsed = Date.parse(trimmed);
    return Number.isFinite(parsed) ? parsed : null;
  }
  return null;
};

const firstDateFromRecords = (records: Array<Record<string, unknown> | null>): number | null => {
  const keys = [
    'registered_at',
    'registeredAt',
    'registration_time',
    'registrationTime',
    'register_time',
    'registerTime',
    'signup_at',
    'signupAt',
    'sign_up_at',
    'signUpAt',
    'created_at',
    'createdAt',
    'account_created_at',
    'accountCreatedAt',
  ];
  for (const record of records) {
    if (!record) continue;
    for (const key of keys) {
      const value = parseDateValue(record[key]);
      if (value !== null) return value;
    }
  }
  return null;
};

const getNestedRecord = (value: unknown, key: string): Record<string, unknown> | null => {
  if (!value || typeof value !== 'object') return null;
  const nested = (value as Record<string, unknown>)[key];
  return nested && typeof nested === 'object' && !Array.isArray(nested)
    ? (nested as Record<string, unknown>)
    : null;
};

const getRegistrationTime = (
  file: AuthFileItem,
  fileContentCache: Record<string, string>,
  savedAtByName: Map<string, number>
): number | null => {
  const metadata =
    file.metadata && typeof file.metadata === 'object' && !Array.isArray(file.metadata)
      ? (file.metadata as Record<string, unknown>)
      : null;
  const attributes =
    file.attributes && typeof file.attributes === 'object' && !Array.isArray(file.attributes)
      ? (file.attributes as Record<string, unknown>)
      : null;
  const idToken =
    file.id_token && typeof file.id_token === 'object' && !Array.isArray(file.id_token)
      ? (file.id_token as Record<string, unknown>)
      : null;

  let parsedContent: Record<string, unknown> | null = null;
  const rawText = fileContentCache[file.name];
  if (rawText) {
    try {
      const parsed = JSON.parse(rawText) as unknown;
      if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
        parsedContent = parsed as Record<string, unknown>;
      }
    } catch {
      parsedContent = null;
    }
  }

  const detectedTime = firstDateFromRecords([
    file,
    metadata,
    attributes,
    idToken,
    parsedContent,
    getNestedRecord(parsedContent, 'account'),
    getNestedRecord(parsedContent, 'user'),
    getNestedRecord(parsedContent, 'metadata'),
    getNestedRecord(parsedContent, 'profile'),
  ]);
  if (detectedTime !== null) return detectedTime;

  const savedAt = savedAtByName.get(file.name);
  return typeof savedAt === 'number' && Number.isFinite(savedAt) && savedAt > 0 ? savedAt : null;
};

const getPlanValue = (
  file: AuthFileItem,
  fileContentCache: Record<string, string>,
  checkedPlan?: string
): string => {
  const normalizedCheckedPlan = normalizePlanType(checkedPlan);
  if (normalizedCheckedPlan) return normalizedCheckedPlan;

  const metadata =
    file.metadata && typeof file.metadata === 'object' && !Array.isArray(file.metadata)
      ? (file.metadata as Record<string, unknown>)
      : null;
  const attributes =
    file.attributes && typeof file.attributes === 'object' && !Array.isArray(file.attributes)
      ? (file.attributes as Record<string, unknown>)
      : null;
  const idToken =
    file.id_token && typeof file.id_token === 'object' && !Array.isArray(file.id_token)
      ? (file.id_token as Record<string, unknown>)
      : null;

  let parsedContent: Record<string, unknown> | null = null;
  const rawText = fileContentCache[file.name];
  if (rawText) {
    try {
      const parsed = JSON.parse(rawText) as unknown;
      if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
        parsedContent = parsed as Record<string, unknown>;
      }
    } catch {
      parsedContent = null;
    }
  }

  const keys = ['plan_type', 'planType', 'plan', 'tier', 'account_type', 'accountType'];
  const records = [
    file,
    metadata,
    attributes,
    idToken,
    parsedContent,
    getNestedRecord(parsedContent, 'account'),
    getNestedRecord(parsedContent, 'user'),
    getNestedRecord(parsedContent, 'metadata'),
    getNestedRecord(parsedContent, 'profile'),
  ];
  for (const record of records) {
    if (!record) continue;
    for (const key of keys) {
      const value = record[key];
      if (typeof value === 'string' && value.trim()) {
        return value.trim().toLowerCase();
      }
    }
  }
  return '';
};

const matchesPlanFilter = (
  file: AuthFileItem,
  fileContentCache: Record<string, string>,
  checkedPlan: string | undefined,
  planFilter: string
): boolean => {
  if (planFilter === DEFAULT_ACCOUNT_POOL_PLAN_FILTER) return true;
  const plan = getPlanValue(file, fileContentCache, checkedPlan);
  if (!plan) return false;
  if (planFilter === 'free') return plan.includes('free');
  if (planFilter === 'plus') return plan.includes('plus');
  if (planFilter === 'pro') return plan.includes('pro') || plan.includes('max');
  return true;
};

const getModifiedTime = (file: AuthFileItem): number | null => {
  const candidates = [file.modified, file['modtime'], file['updated_at'], file.updatedAt];
  for (const candidate of candidates) {
    const value = parseDateValue(candidate);
    if (value !== null) return value;
  }
  return null;
};

const getDetectedPlan = (value: unknown): string | undefined => {
  if (!value || typeof value !== 'object') return undefined;
  const record = value as Record<string, unknown>;
  const candidates = [
    record.planType,
    record.plan_type,
    record.plan,
    record.tierLabel,
    record.tier_label,
    record.tierId,
    record.tier_id,
  ];
  for (const candidate of candidates) {
    if (typeof candidate !== 'string') continue;
    const normalized = normalizePlanType(candidate);
    if (normalized) return normalized;
  }
  return undefined;
};

const getPlanLabel = (plan?: string): string => {
  const normalized = normalizePlanType(plan);
  if (!normalized) return '';
  if (normalized === 'free') return 'Free';
  if (normalized === 'plus') return 'Plus';
  if (normalized === 'pro') return 'Pro';
  if (normalized === 'team') return 'Team';
  if (normalized === 'prolite' || normalized === 'pro-lite' || normalized === 'pro_lite') {
    return 'Pro Lite';
  }
  return plan ?? normalized;
};

const isRecord = (value: unknown): value is Record<string, unknown> =>
  Boolean(value) && typeof value === 'object' && !Array.isArray(value);

const getStringValue = (value: unknown): string | undefined =>
  typeof value === 'string' && value.trim() ? value.trim() : undefined;

const getNumberValue = (value: unknown): number | null => {
  if (typeof value === 'number' && Number.isFinite(value)) return value;
  if (typeof value === 'string' && value.trim()) {
    const parsed = Number(value);
    return Number.isFinite(parsed) ? parsed : null;
  }
  return null;
};

const formatPercent = (value: number | null): string => {
  if (value === null) return '--';
  return `${Math.round(Math.max(0, Math.min(100, value)))}%`;
};

const resolveQuotaLabel = (record: Record<string, unknown>, t: ReturnType<typeof useTranslation>['t']): string => {
  const labelKey = getStringValue(record.labelKey);
  if (labelKey) return t(labelKey, record.labelParams as Record<string, string | number>);
  return getStringValue(record.label) ?? getStringValue(record.id) ?? 'Quota';
};

const buildQuotaDetail = (
  label: string,
  remaining: string,
  reset?: string,
  percent?: number
) => JSON.stringify({ label, remaining, reset: reset && reset !== '-' ? reset : '', percent });

const parseQuotaDetail = (line: string): {
  label: string;
  remaining: string;
  reset: string;
  percent?: number;
} => {
  try {
    const parsed = JSON.parse(line) as unknown;
    if (isRecord(parsed)) {
      return {
        label: getStringValue(parsed.label) ?? 'Quota',
        remaining: getStringValue(parsed.remaining) ?? '--',
        reset: getStringValue(parsed.reset) ?? '',
        percent: getNumberValue(parsed.percent) ?? undefined,
      };
    }
  } catch {
    // Older cached results used plain text; keep them readable.
  }
  const [labelPart, rest = '--'] = line.split(':');
  const [remainingPart, resetPart = ''] = rest.split('/');
  const percent = getNumberValue(remainingPart.replace('%', '').trim());
  return {
    label: labelPart.trim() || 'Quota',
    remaining: remainingPart.trim() || '--',
    reset: resetPart.trim(),
    percent: percent ?? undefined,
  };
};

const formatQuotaResetMeta = (
  t: ReturnType<typeof useTranslation>['t'],
  value: string
): string => {
  if (!value || value === '-') return '';
  if (value.includes('重置') || value.toLowerCase().includes('reset')) return value;
  return t('quota_management.reset_time_label', {
    time: value,
    defaultValue: `Reset time: ${value}`,
  });
};

const parseQuotaResetSortTime = (value: string): number | null => {
  const trimmed = value.trim();
  if (!trimmed) return null;

  const parseMonthDayTime = (input: string): number | null => {
    const match = input.match(
      /(\d{1,2})[/-](\d{1,2})(?:[T\s]+(\d{1,2})(?::(\d{1,2}))?(?::(\d{1,2}))?)?/
    );
    if (!match) return null;

    const month = Number(match[1]);
    const day = Number(match[2]);
    const hour = Number(match[3] ?? '0');
    const minute = Number(match[4] ?? '0');
    const second = Number(match[5] ?? '0');
    if (
      !Number.isFinite(month) ||
      !Number.isFinite(day) ||
      !Number.isFinite(hour) ||
      !Number.isFinite(minute) ||
      !Number.isFinite(second)
    ) {
      return null;
    }

    const now = new Date();
    const currentYear = now.getFullYear();
    const candidate = new Date(currentYear, month - 1, day, hour, minute, second, 0);
    if (
      candidate.getMonth() !== month - 1 ||
      candidate.getDate() !== day ||
      candidate.getHours() !== hour ||
      candidate.getMinutes() !== minute
    ) {
      return null;
    }

    if (candidate.getTime() + 24 * 60 * 60 * 1000 < now.getTime()) {
      candidate.setFullYear(currentYear + 1);
    }
    return candidate.getTime();
  };

  const direct = parseDateValue(trimmed);
  if (direct !== null) return direct;
  const monthDayDirect = parseMonthDayTime(trimmed);
  if (monthDayDirect !== null) return monthDayDirect;

  const strippedLabel = trimmed
    .replace(/^(重置时间|Reset time)[:：]?\s*/i, '')
    .replace(/^(重置日期|Reset date)[:：]?\s*/i, '')
    .trim();
  const stripped = parseDateValue(strippedLabel);
  if (stripped !== null) return stripped;
  const monthDayStripped = parseMonthDayTime(strippedLabel);
  if (monthDayStripped !== null) return monthDayStripped;

  const segments = strippedLabel.split(/[|/]/);
  const tail = (segments.length > 0 ? segments[segments.length - 1] : '').trim();
  const tailParsed = parseDateValue(tail);
  if (tailParsed !== null) return tailParsed;
  return parseMonthDayTime(tail);
};

const getEarliestQuotaResetTime = (
  result: { quotaLines?: string[]; quotaRemainingPercent?: number } | undefined
): number | null => {
  if (!result || !Array.isArray(result.quotaLines) || result.quotaLines.length === 0) {
    return null;
  }

  const times = result.quotaLines
    .map(parseQuotaDetail)
    .map((detail) => parseQuotaResetSortTime(detail.reset))
    .filter((value): value is number => value !== null);

  if (times.length === 0) return null;
  return Math.min(...times);
};

const getQuotaSummary = (
  value: unknown,
  t: ReturnType<typeof useTranslation>['t']
): { lines: string[]; remainingPercent?: number } => {
  const source = Array.isArray(value)
    ? value
    : isRecord(value) && Array.isArray(value.windows)
      ? value.windows
      : isRecord(value) && Array.isArray(value.buckets)
        ? value.buckets
        : isRecord(value) && Array.isArray(value.groups)
          ? value.groups
          : isRecord(value) && Array.isArray(value.rows)
            ? value.rows
            : [];

  const remainingPercents: number[] = [];
  const lines = source.reduce<string[]>((result, item) => {
    if (!isRecord(item)) return result;
    const label = resolveQuotaLabel(item, t);
    const usedPercent = getNumberValue(item.usedPercent ?? item.used_percent);
    const remainingFraction = getNumberValue(item.remainingFraction ?? item.remaining_fraction);
    const remainingAmount = getNumberValue(item.remainingAmount ?? item.remaining_amount);
    const used = getNumberValue(item.used);
    const limit = getNumberValue(item.limit);
    const reset = getStringValue(item.resetLabel) ?? getStringValue(item.resetTime) ?? getStringValue(item.resetHint);

    let remaining = '--';
    let percent: number | undefined;
    if (usedPercent !== null) {
      const remainingPercent = Math.max(0, Math.min(100, 100 - usedPercent));
      remainingPercents.push(remainingPercent);
      percent = remainingPercent;
      remaining = formatPercent(remainingPercent);
    } else if (remainingFraction !== null) {
      const remainingPercent = Math.max(0, Math.min(100, remainingFraction * 100));
      remainingPercents.push(remainingPercent);
      percent = remainingPercent;
      remaining = formatPercent(remainingPercent);
    } else if (remainingAmount !== null) {
      remaining = `${remainingAmount}`;
    } else if (used !== null && limit !== null && limit > 0) {
      const remainingPercent = ((limit - used) / limit) * 100;
      percent = remainingPercent;
      remaining = formatPercent(remainingPercent);
    }

    result.push(buildQuotaDetail(label, remaining, reset, percent));
    return result;
  }, []);

  const visibleLines = lines.length <= 3 ? lines : [...lines.slice(0, 3), `+${lines.length - 3} more`];
  return {
    lines: visibleLines,
    remainingPercent: remainingPercents.length > 0 ? Math.min(...remainingPercents) : undefined,
  };
};

const matchesCheckStatusFilter = (status: string | undefined, filter: string): boolean => {
  if (filter === DEFAULT_ACCOUNT_POOL_CHECK_STATUS_FILTER) return true;
  if (filter === 'unchecked') return !status;
  return status === filter;
};

const matchesQuotaFilter = (
  result: { quotaLines?: string[]; quotaRemainingPercent?: number } | undefined,
  filter: string
): boolean => {
  if (filter === DEFAULT_ACCOUNT_POOL_QUOTA_FILTER) return true;
  const remainingPercent =
    typeof result?.quotaRemainingPercent === 'number' ? result.quotaRemainingPercent : null;
  const hasUsableQuota = remainingPercent !== null && remainingPercent > 0;
  if (filter === 'with_quota') return hasUsableQuota;
  if (filter === 'without_quota') return !hasUsableQuota;
  if (filter === 'high_quota') return hasUsableQuota && remainingPercent > LOW_ACCOUNT_POOL_QUOTA_PERCENT;
  if (filter === 'low_quota') {
    return hasUsableQuota && remainingPercent <= LOW_ACCOUNT_POOL_QUOTA_PERCENT;
  }
  return true;
};

const buildDownloadFileName = () => {
  const stamp = new Date().toISOString().replace(/[:.]/g, '-');
  return `account-pool-${stamp}.zip`;
};

const usageMetricNumberFormatter = new Intl.NumberFormat(undefined, {
  maximumFractionDigits: 0,
});

const formatUsageMetric = (value: number | undefined): string => {
  if (typeof value !== 'number' || !Number.isFinite(value) || value <= 0) return '0';
  return usageMetricNumberFormatter.format(Math.round(value));
};

const parseJsonObject = (rawText: string | undefined): Record<string, unknown> | null => {
  if (!rawText) return null;
  try {
    const parsed = JSON.parse(rawText) as unknown;
    return isRecord(parsed) ? parsed : null;
  } catch {
    return null;
  }
};

const firstNonEmptyString = (...values: unknown[]): string => {
  for (const value of values) {
    if (typeof value === 'string' && value.trim()) {
      return value.trim();
    }
  }
  return '';
};

const getAccountPoolEmail = (
  file: AuthFileItem,
  fileContentCache: Record<string, string>
): string => {
  const metadata = isRecord(file.metadata) ? file.metadata : null;
  const attributes = isRecord(file.attributes) ? file.attributes : null;
  const parsedContent = parseJsonObject(fileContentCache[file.name]);
  const account = getNestedRecord(parsedContent, 'account');
  const user = getNestedRecord(parsedContent, 'user');
  const profile = getNestedRecord(parsedContent, 'profile');
  const nestedMetadata = getNestedRecord(parsedContent, 'metadata');

  return firstNonEmptyString(
    file.email,
    file['service_email'],
    metadata?.email,
    attributes?.email,
    parsedContent?.email,
    parsedContent?.service_email,
    account?.email,
    user?.email,
    profile?.email,
    nestedMetadata?.email
  ).toLowerCase();
};

const getAccountPoolAuthIdentifier = (file: AuthFileItem): string =>
  firstNonEmptyString(file.auth_id, file.authId, file.id, file.name);

const getAccountUsageSummary = (
  file: AuthFileItem,
  fileContentCache: Record<string, string>,
  summaryByEmail: Map<string, AccountPoolUsageSummary>,
  summaryByAuthID: Map<string, AccountPoolUsageSummary>
): AccountPoolUsageSummary | null => {
  const email = getAccountPoolEmail(file, fileContentCache);
  if (email) {
    const byEmail = summaryByEmail.get(email);
    if (byEmail) return byEmail;
  }

  const authIdentifier = getAccountPoolAuthIdentifier(file);
  if (authIdentifier) {
    const byAuthID = summaryByAuthID.get(authIdentifier);
    if (byAuthID) return byAuthID;
  }

  return null;
};

const getUsageMetricForSort = (
  file: AuthFileItem,
  fileContentCache: Record<string, string>,
  summaryByEmail: Map<string, AccountPoolUsageSummary>,
  summaryByAuthID: Map<string, AccountPoolUsageSummary>,
  key: 'requests' | 'successes' | 'total_tokens' | 'failures'
): number | null => {
  const summary = getAccountUsageSummary(file, fileContentCache, summaryByEmail, summaryByAuthID);
  if (!summary) return null;
  const value = summary[key];
  return typeof value === 'number' && Number.isFinite(value) ? value : null;
};

const clampAccountPoolPageSize = (value: number): number =>
  Math.min(MAX_ACCOUNT_POOL_PAGE_SIZE, Math.max(MIN_ACCOUNT_POOL_PAGE_SIZE, Math.round(value)));

const clampAccountPoolCheckConcurrency = (value: number): number =>
  Math.max(MIN_ACCOUNT_POOL_CHECK_CONCURRENCY, Math.round(value));

const readStoredCheckConcurrency = (): number => {
  if (typeof window === 'undefined') return DEFAULT_ACCOUNT_POOL_CHECK_CONCURRENCY;
  const raw = window.localStorage.getItem(ACCOUNT_POOL_CHECK_CONCURRENCY_STORAGE_KEY);
  if (!raw) return DEFAULT_ACCOUNT_POOL_CHECK_CONCURRENCY;
  const parsed = Number(raw);
  if (!Number.isFinite(parsed)) return DEFAULT_ACCOUNT_POOL_CHECK_CONCURRENCY;
  return clampAccountPoolCheckConcurrency(parsed);
};

const resolveQuotaConfig = (file: AuthFileItem): QuotaConfig<unknown, unknown> | null =>
  QUOTA_CONFIGS.find((config) => config.filterFn(file)) ?? null;

const getCheckSortRank = (status?: string): number => {
  if (status === 'success') return 0;
  if (status === 'loading') return 1;
  if (status === 'error') return 2;
  if (status === 'unsupported') return 3;
  return 4;
};

const getStatusCodeDescription = (code: number): string => {
  if (code >= 200 && code < 300) return '请求成功，凭证可用，额度接口返回正常。';
  if (code === 400) return '请求参数或账号数据格式异常，可能是认证文件内容不完整。';
  if (code === 401) return '认证失败，通常是 token 失效、账号退出登录或凭证无效。';
  if (code === 403) return '权限不足或账号被限制，可能没有访问该额度接口的权限。';
  if (code === 404) return '接口不存在或当前账号类型不支持该额度接口。';
  if (code === 408) return '请求超时，上游接口没有及时响应。';
  if (code === 409) return '请求冲突，可能是账号状态或上游会话状态不一致。';
  if (code === 429) return '请求过多或额度受限，上游触发限流。';
  if (code >= 400 && code < 500) return '客户端或凭证侧错误，请检查账号状态、权限和认证文件。';
  if (code >= 500 && code < 600) return '上游服务异常或临时不可用，可以稍后重试。';
  return '接口返回的其他状态码，请结合错误详情判断。';
};

const getStatusCodePillClassName = (code: number, styles: Record<string, string>): string => {
  if (code >= 200 && code < 300) return `${styles.statPill} ${styles.statPillSuccess}`;
  if (code === 401 || code === 403 || code === 429 || code >= 500) {
    return `${styles.statPill} ${styles.statPillError}`;
  }
  if (code >= 400) return `${styles.statPill} ${styles.statPillWarning}`;
  return styles.statPill;
};

const sleep = (ms: number): Promise<void> => new Promise((resolve) => window.setTimeout(resolve, ms));

const getAccountPoolCheckErrorStatus = (err: unknown): number | undefined => {
  const status = getStatusFromError(err);
  if (status) return status;
  if (err && typeof err === 'object' && 'code' in err) {
    const code = String((err as { code?: unknown }).code || '').toUpperCase();
    if (code === 'ECONNABORTED' || code === 'ETIMEDOUT') return 408;
  }
  const message = err instanceof Error ? err.message.toLowerCase() : '';
  if (message.includes('timeout')) return 408;
  return undefined;
};

const isRetryableAccountPoolCheckError = (err: unknown): boolean => {
  const status = getAccountPoolCheckErrorStatus(err);
  if (status === 408 || status === 409 || status === 425 || status === 429) return true;
  if (typeof status === 'number' && status >= 500) return true;
  const message = err instanceof Error ? err.message.toLowerCase() : '';
  return (
    message.includes('timeout') ||
    message.includes('network') ||
    message.includes('request failed') ||
    message.includes('token refresh failed')
  );
};

const compareOptionalTime = (
  left: number | null,
  right: number | null,
  direction: 'asc' | 'desc'
): number => {
  if (left === null && right === null) return 0;
  if (left === null) return 1;
  if (right === null) return -1;
  return direction === 'asc' ? left - right : right - left;
};

const applyAccountPoolRecords = (
  records: AccountPoolRecord[],
  setFiles: (files: AuthFileItem[]) => void,
  setFileContentCache: (cache: Record<string, string>) => void,
  setSavedAtByName: (savedAtByName: Map<string, number>) => void
) => {
  setFiles(records.map((record) => record.file));
  setFileContentCache(buildAccountPoolFileContentCache(records));
  setSavedAtByName(new Map(records.map((record) => [record.file.name, record.savedAt])));
};

export function AccountPoolPage() {
  const { t } = useTranslation();
  const showNotification = useNotificationStore((state) => state.showNotification);
  const showConfirmation = useNotificationStore((state) => state.showConfirmation);
  const checking = useAccountPoolCheckStore((state) => state.checking);
  const checkResults = useAccountPoolCheckStore((state) => state.results);
  const checkSummary = useAccountPoolCheckStore((state) => state.summary);
  const beginCheck = useAccountPoolCheckStore((state) => state.beginCheck);
  const setCheckResult = useAccountPoolCheckStore((state) => state.setResult);
  const finishCheck = useAccountPoolCheckStore((state) => state.finishCheck);
  const pruneCheckResults = useAccountPoolCheckStore((state) => state.pruneResults);
  const [files, setFiles] = useState<AuthFileItem[]>([]);
  const [fileContentCache, setFileContentCache] = useState<Record<string, string>>({});
  const [savedAtByName, setSavedAtByName] = useState<Map<string, number>>(() => new Map());
  const [loading, setLoading] = useState(true);
  const [downloading, setDownloading] = useState(false);
  const [downloadingArchive, setDownloadingArchive] = useState(false);
  const [overwritingPassed, setOverwritingPassed] = useState(false);
  const [deletingPoolEntries, setDeletingPoolEntries] = useState(false);
  const [error, setError] = useState('');
  const [search, setSearch] = useState('');
  const [typeFilter, setTypeFilter] = useState('all');
  const [planFilter, setPlanFilter] = useState(DEFAULT_ACCOUNT_POOL_PLAN_FILTER);
  const [checkStatusFilter, setCheckStatusFilter] = useState(DEFAULT_ACCOUNT_POOL_CHECK_STATUS_FILTER);
  const [quotaFilter, setQuotaFilter] = useState(DEFAULT_ACCOUNT_POOL_QUOTA_FILTER);
  const [sortMode, setSortMode] = useState(DEFAULT_ACCOUNT_POOL_SORT_MODE);
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(DEFAULT_ACCOUNT_POOL_PAGE_SIZE);
  const [pageSizeInput, setPageSizeInput] = useState(String(DEFAULT_ACCOUNT_POOL_PAGE_SIZE));
  const [checkConcurrency, setCheckConcurrency] = useState(readStoredCheckConcurrency);
  const [checkConcurrencyInput, setCheckConcurrencyInput] = useState(String(checkConcurrency));
  const [selectedNames, setSelectedNames] = useState<string[]>([]);
  const [usageSummaries, setUsageSummaries] = useState<AccountPoolUsageSummary[]>([]);
  const [checkViewSnapshot, setCheckViewSnapshot] = useState<string[] | null>(null);
  const [statusStatsSnapshot, setStatusStatsSnapshot] = useState<AccountPoolStatusCodeStats | null>(
    null
  );

  const applyRecords = useCallback((records: AccountPoolRecord[]) => {
    const nextRecords = uniqueAccountPoolRecords(records);
    applyAccountPoolRecords(nextRecords, setFiles, setFileContentCache, setSavedAtByName);
    setSelectedNames((current) =>
      current.filter((name) => nextRecords.some((record) => record.file.name === name))
    );
    pruneCheckResults(nextRecords);
    return nextRecords;
  }, [pruneCheckResults]);

  const hydrateStoredPool = useCallback(() => {
    const storedRecords = applyRecords(readAccountPoolRecords());
    setLoading(false);
    return storedRecords;
  }, [applyRecords]);

  const syncFiles = useCallback(async (showLoading = true) => {
    if (showLoading) {
      setLoading(true);
    }
    setError('');
    hydrateStoredPool();
    try {
      const mergedRecords = await syncAccountPoolFromAuthFiles(checkConcurrency);
      applyRecords(mergedRecords);
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : t('notification.refresh_failed');
      setError(message);
    } finally {
      if (showLoading) {
        setLoading(false);
      }
    }
  }, [applyRecords, checkConcurrency, hydrateStoredPool, t]);

  useEffect(() => {
    hydrateStoredPool();
    void syncFiles(false);

    const handleAccountPoolUpdated = (event: Event) => {
      const records = (event as CustomEvent<AccountPoolRecord[]>).detail;
      if (Array.isArray(records)) {
        applyRecords(records);
      }
    };

    window.addEventListener(ACCOUNT_POOL_UPDATED_EVENT, handleAccountPoolUpdated);
    return () => window.removeEventListener(ACCOUNT_POOL_UPDATED_EVENT, handleAccountPoolUpdated);
  }, [applyRecords, hydrateStoredPool, syncFiles]);

  const loadUsageSummaries = useCallback(async () => {
    try {
      const response = await authFilesApi.getAccountPoolUsageRecords(80);
      setUsageSummaries(response.summaries);
    } catch {
      setUsageSummaries([]);
    }
  }, []);

  useEffect(() => {
    void loadUsageSummaries();
    const timer = window.setInterval(() => {
      void loadUsageSummaries();
    }, 8000);
    return () => window.clearInterval(timer);
  }, [loadUsageSummaries]);


  const typeOptions = useMemo(() => {
    const types = Array.from(new Set(files.map(getFileType))).sort((a, b) => a.localeCompare(b));
    return [
      { value: 'all', label: t('account_pool.type_all') },
      ...types.map((type) => ({ value: type, label: type })),
    ];
  }, [files, t]);

  const sortOptions = useMemo(
    () => [
      { value: 'check', label: t('account_pool.sort_check') },
      { value: 'quota_desc', label: t('account_pool.sort_quota_desc') },
      { value: 'quota_asc', label: t('account_pool.sort_quota_asc') },
      { value: 'requests_desc', label: t('account_pool.sort_requests_desc', { defaultValue: '请求最多' }) },
      { value: 'success_desc', label: t('account_pool.sort_success_desc', { defaultValue: '成功最多' }) },
      { value: 'token_desc', label: t('account_pool.sort_token_desc', { defaultValue: 'Token 最多' }) },
      { value: 'failure_desc', label: t('account_pool.sort_failure_desc', { defaultValue: '失败最多' }) },
      { value: 'registered_desc', label: t('account_pool.sort_registered_desc') },
      { value: 'registered_asc', label: t('account_pool.sort_registered_asc') },
      { value: 'modified_desc', label: t('account_pool.sort_modified_desc') },
      { value: 'modified_asc', label: t('account_pool.sort_modified_asc') },
    ],
    [t]
  );

  const planOptions = useMemo(
    () => [
      { value: 'all', label: t('account_pool.plan_all') },
      { value: 'free', label: t('account_pool.plan_free') },
      { value: 'plus', label: t('account_pool.plan_plus') },
      { value: 'pro', label: t('account_pool.plan_pro') },
    ],
    [t]
  );

  const checkStatusOptions = useMemo(
    () => [
      { value: 'all', label: t('account_pool.check_status_all', { defaultValue: '全部状态' }) },
      { value: 'success', label: t('account_pool.check_status_success', { defaultValue: '通过' }) },
      { value: 'error', label: t('account_pool.check_status_error', { defaultValue: '失败' }) },
      { value: 'unsupported', label: t('account_pool.check_status_unsupported', { defaultValue: '不支持' }) },
      { value: 'unchecked', label: t('account_pool.check_status_unchecked', { defaultValue: '未检测' }) },
    ],
    [t]
  );

  const quotaOptions = useMemo(
    () => [
      { value: 'all', label: t('account_pool.quota_all', { defaultValue: '全部额度' }) },
      { value: 'with_quota', label: t('account_pool.quota_with', { defaultValue: '有额度' }) },
      { value: 'high_quota', label: t('account_pool.quota_high', { defaultValue: '高额度' }) },
      { value: 'low_quota', label: t('account_pool.quota_low', { defaultValue: '低额度' }) },
      { value: 'without_quota', label: t('account_pool.quota_without', { defaultValue: '无额度' }) },
    ],
    [t]
  );

  const usageSummaryByEmail = useMemo(() => {
    const map = new Map<string, AccountPoolUsageSummary>();
    usageSummaries.forEach((summary) => {
      const email = String(summary.service_email ?? '').trim().toLowerCase();
      if (email && !map.has(email)) {
        map.set(email, summary);
      }
    });
    return map;
  }, [usageSummaries]);

  const usageSummaryByAuthID = useMemo(() => {
    const map = new Map<string, AccountPoolUsageSummary>();
    usageSummaries.forEach((summary) => {
      const authID = String(summary.auth_id ?? '').trim();
      if (authID && !map.has(authID)) {
        map.set(authID, summary);
      }
    });
    return map;
  }, [usageSummaries]);

  const compareUsageMetric = useCallback(
    (
      left: AuthFileItem,
      right: AuthFileItem,
      key: 'requests' | 'successes' | 'total_tokens' | 'failures'
    ): number => {
      const leftValue = getUsageMetricForSort(
        left,
        fileContentCache,
        usageSummaryByEmail,
        usageSummaryByAuthID,
        key
      );
      const rightValue = getUsageMetricForSort(
        right,
        fileContentCache,
        usageSummaryByEmail,
        usageSummaryByAuthID,
        key
      );
      return compareOptionalTime(leftValue, rightValue, 'desc');
    },
    [fileContentCache, usageSummaryByAuthID, usageSummaryByEmail]
  );

  const filteredFiles = useMemo(() => {
    const term = search.trim().toLowerCase();
    return files
      .filter((file) => {
        const checkResult = checkResults[file.name];
        if (typeFilter !== 'all' && getFileType(file) !== typeFilter) return false;
        if (!matchesPlanFilter(file, fileContentCache, checkResult?.plan, planFilter)) return false;
        if (!matchesCheckStatusFilter(checkResult?.status, checkStatusFilter)) return false;
        if (!matchesQuotaFilter(checkResult, quotaFilter)) return false;
        if (!term) return true;
        return [file.name, getFileType(file), file.statusMessage, file.status]
          .some((value) => String(value ?? '').toLowerCase().includes(term));
      })
      .sort((left, right) => {
        if (sortMode === 'registered_desc' || sortMode === 'registered_asc') {
          const timeDiff = compareOptionalTime(
            getRegistrationTime(left, fileContentCache, savedAtByName),
            getRegistrationTime(right, fileContentCache, savedAtByName),
            sortMode === 'registered_asc' ? 'asc' : 'desc'
          );
          if (timeDiff !== 0) return timeDiff;
        } else if (sortMode === 'modified_desc' || sortMode === 'modified_asc') {
          const timeDiff = compareOptionalTime(
            getModifiedTime(left),
            getModifiedTime(right),
            sortMode === 'modified_asc' ? 'asc' : 'desc'
          );
          if (timeDiff !== 0) return timeDiff;
        } else if (sortMode === 'quota_desc' || sortMode === 'quota_asc') {
          const leftResult = checkResults[left.name];
          const rightResult = checkResults[right.name];
          const leftQuota = leftResult?.quotaRemainingPercent;
          const rightQuota = rightResult?.quotaRemainingPercent;
          const quotaDiff = compareOptionalTime(
            typeof leftQuota === 'number' ? leftQuota : null,
            typeof rightQuota === 'number' ? rightQuota : null,
            sortMode === 'quota_asc' ? 'asc' : 'desc'
          );
          if (quotaDiff !== 0) return quotaDiff;

          const leftIsZero = typeof leftQuota === 'number' && leftQuota <= 0;
          const rightIsZero = typeof rightQuota === 'number' && rightQuota <= 0;
          if (leftIsZero && rightIsZero) {
            const resetDiff = compareOptionalTime(
              getEarliestQuotaResetTime(leftResult),
              getEarliestQuotaResetTime(rightResult),
              'asc'
            );
            if (resetDiff !== 0) return resetDiff;
          }
        } else if (sortMode === 'requests_desc') {
          const diff = compareUsageMetric(left, right, 'requests');
          if (diff !== 0) return diff;
        } else if (sortMode === 'success_desc') {
          const diff = compareUsageMetric(left, right, 'successes');
          if (diff !== 0) return diff;
        } else if (sortMode === 'token_desc') {
          const diff = compareUsageMetric(left, right, 'total_tokens');
          if (diff !== 0) return diff;
        } else if (sortMode === 'failure_desc') {
          const diff = compareUsageMetric(left, right, 'failures');
          if (diff !== 0) return diff;
        }

        const rankDiff =
          getCheckSortRank(checkResults[left.name]?.status) -
          getCheckSortRank(checkResults[right.name]?.status);
        if (rankDiff !== 0) return rankDiff;
        return left.name.localeCompare(right.name);
      });
  }, [
    checkResults,
    checkStatusFilter,
    fileContentCache,
    files,
    planFilter,
    quotaFilter,
    savedAtByName,
    search,
    sortMode,
    typeFilter,
    compareUsageMetric,
  ]);

  const displayedFiles = useMemo(() => {
    if (!checking || !checkViewSnapshot || checkViewSnapshot.length === 0) {
      return filteredFiles;
    }
    const order = new Map(checkViewSnapshot.map((name, index) => [name, index]));
    return filteredFiles
      .filter((file) => order.has(file.name))
      .slice()
      .sort((left, right) => (order.get(left.name) ?? 0) - (order.get(right.name) ?? 0));
  }, [checkViewSnapshot, checking, filteredFiles]);

  const selectedSet = useMemo(() => new Set(selectedNames), [selectedNames]);
  const totalPages = Math.max(1, Math.ceil(displayedFiles.length / pageSize));
  const currentPage = Math.min(page, totalPages);
  const pageStart = (currentPage - 1) * pageSize;
  const pageItems = displayedFiles.slice(pageStart, pageStart + pageSize);
  const visibleSelectedCount = pageItems.filter((file) => selectedSet.has(file.name)).length;
  const allVisibleSelected = pageItems.length > 0 && visibleSelectedCount === pageItems.length;
  const filteredSelectedCount = displayedFiles.filter((file) => selectedSet.has(file.name)).length;
  const allFilteredSelected =
    displayedFiles.length > 0 && filteredSelectedCount === displayedFiles.length;

  const selectedFiles = useMemo(
    () => files.filter((file) => selectedSet.has(file.name)),
    [files, selectedSet]
  );
  const passedFiles = useMemo(
    () => files.filter((file) => checkResults[file.name]?.status === 'success'),
    [checkResults, files]
  );
  const statusCodeStats = useMemo(() => {
    const byCode = new Map<number, number>();
    let unchecked = 0;
    let unsupported = 0;
    let unknownError = 0;

    for (const file of files) {
      const result = checkResults[file.name];
      if (!result || result.status === 'loading') {
        unchecked += 1;
        continue;
      }
      if (result.status === 'unsupported') {
        unsupported += 1;
        continue;
      }
      if (typeof result.statusCode === 'number') {
        byCode.set(result.statusCode, (byCode.get(result.statusCode) ?? 0) + 1);
        continue;
      }
      if (result.status === 'error') {
        unknownError += 1;
      } else {
        unchecked += 1;
      }
    }

    return {
      codes: Array.from(byCode.entries()).sort(([left], [right]) => left - right),
      unchecked,
      unsupported,
      unknownError,
    };
  }, [checkResults, files]);

  const displayedStatusCodeStats =
    checking && statusStatsSnapshot ? statusStatsSnapshot : statusCodeStats;

  useEffect(() => {
    if (page > totalPages) {
      setPage(totalPages);
    }
  }, [page, totalPages]);

  useEffect(() => {
    setPage(1);
  }, [checkStatusFilter, planFilter, quotaFilter, search, sortMode, typeFilter]);

  useEffect(() => {
    if (!checking) {
      setCheckViewSnapshot(null);
      setStatusStatsSnapshot(null);
    }
  }, [checking]);

  const commitPageSizeInput = (rawValue: string) => {
    const trimmed = rawValue.trim();
    if (!trimmed) {
      setPageSizeInput(String(pageSize));
      return;
    }

    const value = Number(trimmed);
    if (!Number.isFinite(value)) {
      setPageSizeInput(String(pageSize));
      return;
    }

    const next = clampAccountPoolPageSize(value);
    setPageSize(next);
    setPageSizeInput(String(next));
    setPage(1);
  };

  const handlePageSizeChange = (event: ChangeEvent<HTMLInputElement>) => {
    const rawValue = event.currentTarget.value;
    setPageSizeInput(rawValue);

    const trimmed = rawValue.trim();
    if (!trimmed) return;

    const parsed = Number(trimmed);
    if (!Number.isFinite(parsed)) return;

    setPageSize(clampAccountPoolPageSize(parsed));
    setPage(1);
  };

  const commitCheckConcurrencyInput = (rawValue: string) => {
    const trimmed = rawValue.trim();
    if (!trimmed) {
      setCheckConcurrencyInput(String(checkConcurrency));
      return;
    }

    const value = Number(trimmed);
    if (!Number.isFinite(value)) {
      setCheckConcurrencyInput(String(checkConcurrency));
      return;
    }

    const next = clampAccountPoolCheckConcurrency(value);
    setCheckConcurrency(next);
    setCheckConcurrencyInput(String(next));
    if (typeof window !== 'undefined') {
      window.localStorage.setItem(ACCOUNT_POOL_CHECK_CONCURRENCY_STORAGE_KEY, String(next));
    }
  };

  const handleCheckConcurrencyChange = (event: ChangeEvent<HTMLInputElement>) => {
    const rawValue = event.currentTarget.value;
    setCheckConcurrencyInput(rawValue);

    const trimmed = rawValue.trim();
    if (!trimmed) return;

    const parsed = Number(trimmed);
    if (!Number.isFinite(parsed)) return;

    const next = clampAccountPoolCheckConcurrency(parsed);
    setCheckConcurrency(next);
    if (typeof window !== 'undefined') {
      window.localStorage.setItem(ACCOUNT_POOL_CHECK_CONCURRENCY_STORAGE_KEY, String(next));
    }
  };

  const toggleOne = (name: string, checked: boolean) => {
    setSelectedNames((current) => {
      const next = new Set(current);
      if (checked) {
        next.add(name);
      } else {
        next.delete(name);
      }
      return Array.from(next);
    });
  };

  const toggleVisible = (checked: boolean) => {
    setSelectedNames((current) => {
      const next = new Set(current);
      pageItems.forEach((file) => {
        if (checked) {
          next.add(file.name);
        } else {
          next.delete(file.name);
        }
      });
      return Array.from(next);
    });
  };

  const toggleFiltered = (checked: boolean) => {
    setSelectedNames((current) => {
      const next = new Set(current);
      displayedFiles.forEach((file) => {
        if (checked) {
          next.add(file.name);
        } else {
          next.delete(file.name);
        }
      });
      return Array.from(next);
    });
  };

  const readAccountPoolFileContent = async (name: string): Promise<string> => {
    const cachedContent = fileContentCache[name];
    if (cachedContent) return cachedContent;

    try {
      return await authFilesApi.downloadText(name);
    } catch (authFileErr) {
      try {
        return await authFilesApi.downloadAccountPoolText(name);
      } catch {
        throw authFileErr;
      }
    }
  };

  const handleDownloadSelected = async () => {
    if (selectedFiles.length === 0) return;
    setDownloading(true);
    try {
      const zipFiles = await Promise.all(
        selectedFiles.map(async (file) => ({
          name: file.name,
          text: await readAccountPoolFileContent(file.name),
        }))
      );
      const zipBlob = createZipBlob(zipFiles);
      downloadBlob({ filename: buildDownloadFileName(), blob: zipBlob });
      showNotification(
        t('account_pool.download_success', { count: selectedFiles.length }),
        'success'
      );
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : t('common.unknown_error');
      showNotification(t('account_pool.download_failed', { message }), 'error');
    } finally {
      setDownloading(false);
    }
  };

  const handleDownloadServerArchive = async () => {
    setDownloadingArchive(true);
    try {
      const blob = await authFilesApi.downloadAccountPoolArchive();
      downloadBlob({ filename: buildDownloadFileName(), blob });
      showNotification(t('account_pool.download_archive_success'), 'success');
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : t('common.unknown_error');
      showNotification(t('account_pool.download_failed', { message }), 'error');
    } finally {
      setDownloadingArchive(false);
    }
  };

  const deletePoolEntries = async (targets: AuthFileItem[]) => {
    if (targets.length === 0 || deletingPoolEntries) return;
    const names = targets.map((file) => file.name);
    setDeletingPoolEntries(true);
    let backendDeleteFailed = '';
    try {
      try {
        await authFilesApi.deleteAccountPoolEntries(names);
      } catch (err: unknown) {
        backendDeleteFailed = err instanceof Error ? err.message : t('common.unknown_error');
      }

      const nextRecords = deleteAccountPoolRecordsByName(names);
      applyRecords(nextRecords);
      showNotification(
        backendDeleteFailed
          ? t('account_pool.delete_local_success_backend_failed', {
              count: names.length,
              message: backendDeleteFailed,
              defaultValue: `已从账号池删除 ${names.length} 个，后台 ZIP 删除失败：${backendDeleteFailed}`,
            })
          : t('account_pool.delete_success', {
              count: names.length,
              defaultValue: `已从账号池删除 ${names.length} 个`,
            }),
        backendDeleteFailed ? 'warning' : 'success'
      );
    } finally {
      setDeletingPoolEntries(false);
    }
  };

  const confirmDeletePoolEntries = (targets: AuthFileItem[]) => {
    if (targets.length === 0) return;
    showConfirmation({
      title: t('account_pool.delete_title', { defaultValue: '删除账号池账号' }),
      message: t('account_pool.delete_confirm', {
        count: targets.length,
        defaultValue: `确认从账号池删除 ${targets.length} 个账号？认证文件不会被删除。`,
      }),
      confirmText: t('common.delete'),
      variant: 'danger',
      onConfirm: () => void deletePoolEntries(targets),
    });
  };

  const overwriteAccountFiles = async (targets: AuthFileItem[], mode: 'passed' | 'filtered') => {
    if (targets.length === 0 || overwritingPassed) return;
    setOverwritingPassed(true);
    try {
      const uploadFiles = await Promise.all(
        targets.map(async (file) => {
          const content = await readAccountPoolFileContent(file.name);
          return new File([content], file.name, { type: 'application/json' });
        })
      );
      await authFilesApi.deleteAll();
      const result = await authFilesApi.uploadFiles(uploadFiles);
      if (result.failed.length > 0) {
        showNotification(
          t(`account_pool.overwrite_${mode}_partial`, {
            success: result.uploaded,
            failed: result.failed.length,
          }),
          'warning'
        );
        return;
      }
      showNotification(
        t(`account_pool.overwrite_${mode}_success`, { count: result.uploaded }),
        'success'
      );
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : t('common.unknown_error');
      showNotification(t(`account_pool.overwrite_${mode}_failed`, { message }), 'error');
    } finally {
      setOverwritingPassed(false);
    }
  };

  const handleOverwritePassed = () => {
    if (passedFiles.length === 0) return;
    showConfirmation({
      title: t('account_pool.overwrite_passed_title'),
      message: t('account_pool.overwrite_passed_confirm', { count: passedFiles.length }),
      confirmText: t('common.confirm'),
      variant: 'danger',
      onConfirm: () => void overwriteAccountFiles(passedFiles, 'passed'),
    });
  };

  const handleOverwriteFiltered = () => {
    if (displayedFiles.length === 0) return;
    showConfirmation({
      title: t('account_pool.overwrite_filtered_title', {
        defaultValue: '覆盖筛选结果',
      }),
      message: t('account_pool.overwrite_filtered_confirm', {
        count: displayedFiles.length,
        defaultValue: `确认先删除当前所有认证文件，再写入 ${displayedFiles.length} 个筛选结果中的账号 JSON？账号池缓存不会被删除。`,
      }),
      confirmText: t('common.confirm'),
      variant: 'danger',
      onConfirm: () => void overwriteAccountFiles(displayedFiles, 'filtered'),
    });
  };

  const detectAccounts = async (targets: AuthFileItem[]) => {
    if (targets.length === 0 || checking) return;
    setCheckViewSnapshot(targets.map((file) => file.name));
    setStatusStatsSnapshot(statusCodeStats);
    const runId = beginCheck(targets.map((file) => file.name));
    if (!runId) {
      setCheckViewSnapshot(null);
      setStatusStatsSnapshot(null);
      return;
    }

    const checkOne = async (file: AuthFileItem): Promise<AccountCheckResult> => {
      const config = resolveQuotaConfig(file);
      if (!config) {
        return {
          status: 'unsupported',
          message: t('account_pool.check_unsupported'),
          checkedAt: Date.now(),
        };
      }

      let lastError: unknown;
      for (let attempt = 0; attempt <= ACCOUNT_POOL_CHECK_RETRY_ATTEMPTS; attempt += 1) {
        try {
          const quota = await config.fetchQuota(file, t);
          const quotaSummary = getQuotaSummary(quota, t);
          return {
            status: 'success',
            message: t('account_pool.check_success'),
            plan: getDetectedPlan(quota),
            quotaLines: quotaSummary.lines,
            quotaRemainingPercent: quotaSummary.remainingPercent,
            statusCode: 200,
            checkedAt: Date.now(),
          };
        } catch (err: unknown) {
          lastError = err;
          if (
            attempt >= ACCOUNT_POOL_CHECK_RETRY_ATTEMPTS ||
            !isRetryableAccountPoolCheckError(err)
          ) {
            break;
          }
          await sleep(ACCOUNT_POOL_CHECK_RETRY_DELAY_MS * (attempt + 1));
        }
      }

      const message = lastError instanceof Error ? lastError.message : t('common.unknown_error');
      const status = getAccountPoolCheckErrorStatus(lastError);
      return {
        status: 'error',
        message: status ? `${status}: ${message}` : message,
        statusCode: status,
        checkedAt: Date.now(),
      };
    };

    let cursor = 0;
    const worker = async () => {
      for (;;) {
        const index = cursor;
        cursor += 1;
        const file = targets[index];
        if (!file) return;
        setCheckResult(runId, file.name, await checkOne(file));
      }
    };

    try {
      await Promise.all(
        Array.from({ length: Math.min(checkConcurrency, targets.length) }, () => worker())
      );
      const summary = finishCheck(runId);
      if (!summary) return;
      showNotification(
        t('account_pool.check_done', {
          success: summary.success,
          failed: summary.failed,
          unsupported: summary.unsupported,
        }),
        summary.failed > 0 ? 'warning' : 'success'
      );
    } catch {
      const summary = finishCheck(runId);
      if (summary) {
        showNotification(
          t('account_pool.check_done', {
            success: summary.success,
            failed: summary.failed,
            unsupported: summary.unsupported,
          }),
          summary.failed > 0 ? 'warning' : 'success'
        );
      }
    }
  };

  return (
    <div className={styles.container}>
      <div className={styles.pageHeader}>
        <div className={styles.headerIntro}>
          <h1 className={styles.pageTitle}>{t('account_pool.title')}</h1>
          <p className={styles.description}>{t('account_pool.description')}</p>
        </div>
        <div
          className={styles.headerStats}
          aria-label={t('account_pool.status_stats', { defaultValue: '状态码统计' })}
        >
          {displayedStatusCodeStats.codes.map(([code, count]) => (
            <span
              className={getStatusCodePillClassName(code, styles)}
              key={code}
              title={`${code}：${getStatusCodeDescription(code)}`}
            >
              {code}
              <strong>{count}</strong>
            </span>
          ))}
          {displayedStatusCodeStats.unknownError > 0 && (
            <span
              className={`${styles.statPill} ${styles.statPillError}`}
              title="未知错误：检测过程抛出了错误，但没有拿到明确的 HTTP 状态码。"
            >
              {t('account_pool.stat_unknown_error', { defaultValue: '未知错误' })}
              <strong>{displayedStatusCodeStats.unknownError}</strong>
            </span>
          )}
          {displayedStatusCodeStats.unsupported > 0 && (
            <span className={styles.statPill} title="不支持：该认证文件类型暂未接入额度检测逻辑。">
              {t('account_pool.stat_unsupported', { defaultValue: '不支持' })}
              <strong>{displayedStatusCodeStats.unsupported}</strong>
            </span>
          )}
          {displayedStatusCodeStats.unchecked > 0 && (
            <span className={styles.statPill} title="未检测：该账号还没有执行过检测，或当前正在等待检测结果。">
              {t('account_pool.stat_unchecked', { defaultValue: '未检测' })}
              <strong>{displayedStatusCodeStats.unchecked}</strong>
            </span>
          )}
        </div>
        <div className={styles.headerActions}>
          <Button variant="secondary" size="sm" onClick={() => void syncFiles()} loading={loading}>
            {t('account_pool.sync')}
          </Button>
          <Button
            variant="secondary"
            size="sm"
            onClick={() => void detectAccounts(selectedFiles)}
            loading={checking && selectedFiles.length > 0}
            disabled={checking || selectedFiles.length === 0}
          >
            {t('account_pool.check_selected', { count: selectedFiles.length })}
          </Button>
          <Button
            variant="secondary"
            size="sm"
            onClick={() => void detectAccounts(displayedFiles)}
            loading={checking && selectedFiles.length === 0 && displayedFiles.length < files.length}
            disabled={checking || displayedFiles.length === 0}
          >
            {t('account_pool.check_filtered', {
              count: displayedFiles.length,
              defaultValue: `检测筛选 (${displayedFiles.length})`,
            })}
          </Button>
          <Button
            variant="secondary"
            size="sm"
            onClick={() => void detectAccounts(files)}
            loading={checking && selectedFiles.length === 0 && displayedFiles.length === files.length}
            disabled={checking || files.length === 0}
          >
            {t('account_pool.check_all')}
          </Button>
          <Button
            size="sm"
            onClick={handleDownloadSelected}
            loading={downloading}
            disabled={selectedFiles.length === 0 || downloading}
          >
            {t('account_pool.download_selected', { count: selectedFiles.length })}
          </Button>
          <Button
            variant="danger"
            size="sm"
            onClick={() => confirmDeletePoolEntries(selectedFiles)}
            loading={deletingPoolEntries}
            disabled={deletingPoolEntries || selectedFiles.length === 0}
          >
            {t('account_pool.delete_selected', {
              count: selectedFiles.length,
              defaultValue: `删除选中 (${selectedFiles.length})`,
            })}
          </Button>
          <Button
            variant="secondary"
            size="sm"
            onClick={() => void handleDownloadServerArchive()}
            loading={downloadingArchive}
            disabled={downloadingArchive}
          >
            {t('account_pool.download_archive')}
          </Button>
          <Button
            variant="danger"
            size="sm"
            onClick={handleOverwritePassed}
            loading={overwritingPassed}
            disabled={overwritingPassed || passedFiles.length === 0}
          >
            {t('account_pool.overwrite_passed', { count: passedFiles.length })}
          </Button>
          <Button
            variant="danger"
            size="sm"
            onClick={handleOverwriteFiltered}
            loading={overwritingPassed}
            disabled={overwritingPassed || displayedFiles.length === 0}
          >
            {t('account_pool.overwrite_filtered', {
              count: displayedFiles.length,
              defaultValue: `覆盖筛选 (${displayedFiles.length})`,
            })}
          </Button>
        </div>
      </div>

      {error && <div className={styles.errorBox}>{error}</div>}


      <Card>
        <div className={styles.toolbar}>
          <div className={styles.filters}>
            <div className={styles.filterControls}>
              <Input
                className={styles.searchInput}
                value={search}
                onChange={(event) => setSearch(event.target.value)}
                placeholder={t('account_pool.search_placeholder')}
              />
              <Select
                className={styles.typeSelect}
                fullWidth={false}
                value={typeFilter}
                options={typeOptions}
                onChange={setTypeFilter}
                ariaLabel={t('account_pool.type_filter')}
              />
              <Select
                className={styles.planSelect}
                fullWidth={false}
                value={planFilter}
                options={planOptions}
                onChange={setPlanFilter}
                ariaLabel={t('account_pool.plan_filter')}
              />
              <Select
                className={styles.statusSelect}
                fullWidth={false}
                value={checkStatusFilter}
                options={checkStatusOptions}
                onChange={setCheckStatusFilter}
                ariaLabel={t('account_pool.check_status_filter', { defaultValue: '检测状态' })}
              />
              <Select
                className={styles.quotaSelect}
                fullWidth={false}
                value={quotaFilter}
                options={quotaOptions}
                onChange={setQuotaFilter}
                ariaLabel={t('account_pool.quota_filter', { defaultValue: '额度状态' })}
              />
              <Select
                className={styles.sortSelect}
                fullWidth={false}
                value={sortMode}
                options={sortOptions}
                onChange={setSortMode}
                ariaLabel={t('account_pool.sort_filter')}
              />
            </div>
            <div className={styles.toolbarMeta}>
              <span className={styles.stats}>
                {t('account_pool.stats', { visible: displayedFiles.length, total: files.length })}
              </span>
              <label className={styles.pageSizeControl}>
                <span>{t('auth_files.page_size_label')}</span>
                <input
                  className={styles.pageSizeInput}
                  type="number"
                  min={MIN_ACCOUNT_POOL_PAGE_SIZE}
                  max={MAX_ACCOUNT_POOL_PAGE_SIZE}
                  step={1}
                  value={pageSizeInput}
                  onChange={handlePageSizeChange}
                  onBlur={(event) => commitPageSizeInput(event.currentTarget.value)}
                  onKeyDown={(event) => {
                    if (event.key === 'Enter') {
                      event.currentTarget.blur();
                    }
                  }}
                />
              </label>
              <label className={styles.pageSizeControl}>
                <span>{t('account_pool.check_concurrency')}</span>
                <input
                  className={styles.checkConcurrencyInput}
                  type="number"
                  min={MIN_ACCOUNT_POOL_CHECK_CONCURRENCY}
                  step={1}
                  value={checkConcurrencyInput}
                  disabled={checking}
                  onChange={handleCheckConcurrencyChange}
                  onBlur={(event) => commitCheckConcurrencyInput(event.currentTarget.value)}
                  onKeyDown={(event) => {
                    if (event.key === 'Enter') {
                      event.currentTarget.blur();
                    }
                  }}
                />
              </label>
            </div>
          </div>
          <div className={styles.selectionActions}>
            <SelectionCheckbox
              checked={allVisibleSelected}
              onChange={toggleVisible}
              disabled={pageItems.length === 0}
              label={t('account_pool.select_visible')}
            />
            <SelectionCheckbox
              checked={allFilteredSelected}
              onChange={toggleFiltered}
                disabled={displayedFiles.length === 0}
                label={t('account_pool.select_filtered', {
                  defaultValue: '选择筛选结果',
                })}
              />
            <Button variant="ghost" size="sm" onClick={() => setSelectedNames([])}>
              {t('account_pool.clear_selection')}
            </Button>
            <Button
              variant="ghost"
              size="sm"
              onClick={() => confirmDeletePoolEntries(displayedFiles)}
              disabled={deletingPoolEntries || displayedFiles.length === 0}
            >
              {t('account_pool.delete_filtered', {
                count: displayedFiles.length,
                defaultValue: `删除筛选结果 (${displayedFiles.length})`,
              })}
            </Button>
          </div>
        </div>

        {checking && (
          <div className={styles.checkProgress}>
            {t('account_pool.check_progress', {
              done: checkSummary.done,
              total: checkSummary.total,
              success: checkSummary.success,
              failed: checkSummary.failed,
              unsupported: checkSummary.unsupported,
            })}
          </div>
        )}

        {loading ? (
          <div className={styles.hint}>{t('common.loading')}</div>
        ) : displayedFiles.length === 0 ? (
          <EmptyState
            title={t('account_pool.empty_title')}
            description={t('account_pool.empty_desc')}
          />
        ) : (
          <div className={styles.poolGrid}>
            {pageItems.map((file) => {
              const checked = selectedSet.has(file.name);
              const type = getFileType(file);
              const modifiedLabel = getFileModifiedLabel(file);
              const statusMessage = String(file.statusMessage || file['status_message'] || '');
              const checkResult = checkResults[file.name];
              const usageSummary = getAccountUsageSummary(
                file,
                fileContentCache,
                usageSummaryByEmail,
                usageSummaryByAuthID
              );
              const planLabel = getPlanLabel(checkResult?.plan);
              const checkedAtLabel = checkResult?.checkedAt
                ? formatUnixTimestamp(Math.round(checkResult.checkedAt / 1000))
                : '';
              const quotaDetails = (checkResult?.quotaLines ?? []).map(parseQuotaDetail);
              return (
                <div
                  key={file.name}
                  className={`${styles.poolCard} ${checked ? styles.poolCardSelected : ''}`}
                >
                  <div className={styles.cardTop}>
                    <SelectionCheckbox
                      checked={checked}
                      onChange={(value) => toggleOne(file.name, value)}
                      ariaLabel={file.name}
                    />
                    <div className={styles.cardMain}>
                      <div className={styles.fileName}>{file.name}</div>
                      <div className={styles.metaRow}>
                        <span className={styles.typeBadge}>{type}</span>
                        {planLabel && <span className={styles.planBadge}>{planLabel}</span>}
                        {modifiedLabel && <span className={styles.muted}>{modifiedLabel}</span>}
                      </div>
                      <div className={styles.usageMetricRow}>
                        <div className={styles.usageMetric}>
                          <span className={styles.usageMetricLabel}>
                            {t('account_pool.usage_requests', { defaultValue: '请求' })}
                          </span>
                          <strong className={styles.usageMetricValue}>
                            {formatUsageMetric(usageSummary?.requests)}
                          </strong>
                        </div>
                        <div className={styles.usageMetric}>
                          <span className={styles.usageMetricLabel}>
                            {t('account_pool.usage_successes', { defaultValue: '成功' })}
                          </span>
                          <strong className={styles.usageMetricValue}>
                            {formatUsageMetric(usageSummary?.successes)}
                          </strong>
                        </div>
                        <div className={styles.usageMetric}>
                          <span className={styles.usageMetricLabel}>
                            {t('account_pool.usage_total_tokens', { defaultValue: 'Token' })}
                          </span>
                          <strong className={styles.usageMetricValue}>
                            {formatUsageMetric(usageSummary?.total_tokens)}
                          </strong>
                        </div>
                        <div className={styles.usageMetric}>
                          <span className={styles.usageMetricLabel}>
                            {t('account_pool.usage_failures', { defaultValue: '失败' })}
                          </span>
                          <strong className={styles.usageMetricValue}>
                            {formatUsageMetric(usageSummary?.failures)}
                          </strong>
                        </div>
                      </div>
                    </div>
                  </div>
                  {checkResult && (
                    <div
                      className={`${styles.checkLine} ${
                        checkResult.status === 'success'
                          ? styles.checkSuccess
                          : checkResult.status === 'loading'
                            ? styles.checkLoading
                            : checkResult.status === 'unsupported'
                              ? styles.checkUnsupported
                              : styles.checkError
                      }`}
                    >
                      <div className={styles.checkHeader}>
                        <span className={styles.checkStatusPill}>
                          {checkResult.status === 'loading'
                            ? t('account_pool.checking')
                            : checkResult.message}
                        </span>
                        {planLabel && <span className={styles.checkPlanPill}>{planLabel}</span>}
                        {checkedAtLabel && <span className={styles.checkTime}>{checkedAtLabel}</span>}
                      </div>
                      {quotaDetails.length > 0 && (
                        <div className={styles.quotaPanel}>
                          {quotaDetails.map((quota) => {
                            const percent =
                              typeof quota.percent === 'number'
                                ? Math.max(0, Math.min(100, quota.percent))
                                : null;
                            const empty = percent !== null && percent <= 0;
                            const low =
                              percent !== null &&
                              percent > 0 &&
                              percent <= LOW_ACCOUNT_POOL_QUOTA_PERCENT;
                            return (
                              <div
                                className={empty ? styles.quotaItemEmpty : styles.quotaItem}
                                key={`${quota.label}-${quota.reset}`}
                              >
                                <div className={styles.quotaItemTop}>
                                  <span className={styles.quotaName}>{quota.label}</span>
                                  <span
                                    className={
                                      empty
                                        ? styles.quotaEmptyValue
                                        : low
                                          ? styles.quotaLowValue
                                          : styles.quotaValue
                                    }
                                  >
                                    {quota.remaining}
                                  </span>
                                </div>
                                {percent !== null && (
                                  <div className={styles.quotaTrack}>
                                    <span
                                      className={
                                        empty
                                          ? styles.quotaFillEmpty
                                          : low
                                            ? styles.quotaFillLow
                                            : styles.quotaFill
                                      }
                                      style={{ width: `${percent}%` }}
                                    />
                                  </div>
                                )}
                                {quota.reset && (
                                  <div className={styles.quotaReset}>
                                    {formatQuotaResetMeta(t, quota.reset)}
                                  </div>
                                )}
                              </div>
                            );
                          })}
                        </div>
                      )}
                    </div>
                  )}
                  {statusMessage && <div className={styles.statusLine}>{statusMessage}</div>}
                </div>
              );
            })}
          </div>
        )}

        {!loading && displayedFiles.length > pageSize && (
          <div className={styles.pagination}>
            <Button
              variant="secondary"
              size="sm"
              onClick={() => setPage(Math.max(1, currentPage - 1))}
              disabled={currentPage <= 1}
            >
              {t('auth_files.pagination_prev')}
            </Button>
            <div className={styles.pageInfo}>
              {t('auth_files.pagination_info', {
                current: currentPage,
                total: totalPages,
                count: displayedFiles.length,
              })}
            </div>
            <Button
              variant="secondary"
              size="sm"
              onClick={() => setPage(Math.min(totalPages, currentPage + 1))}
              disabled={currentPage >= totalPages}
            >
              {t('auth_files.pagination_next')}
            </Button>
          </div>
        )}
      </Card>
    </div>
  );
}
