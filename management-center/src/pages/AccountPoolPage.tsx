import { useCallback, useEffect, useMemo, useRef, useState, type ChangeEvent } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { Card } from '@/components/ui/Card';
import { EmptyState } from '@/components/ui/EmptyState';
import { Input } from '@/components/ui/Input';
import { Modal } from '@/components/ui/Modal';
import { Select } from '@/components/ui/Select';
import { SelectionCheckbox } from '@/components/ui/SelectionCheckbox';
import {
  readPendingAccountPoolCheckNames,
  useAccountPoolCheckStore,
  useNotificationStore,
  type AccountCheckResult,
} from '@/stores';
import {
  apiCallApi,
  authFilesApi,
  getApiCallErrorMessage,
  type AccountPoolCheckResultPatch,
  type AccountPoolImportJob,
  type AccountPoolUsageSummary,
} from '@/services/api';
import {
  ANTIGRAVITY_CONFIG,
  CLAUDE_CONFIG,
  CODEX_CONFIG,
  GEMINI_CLI_CONFIG,
  KIMI_CONFIG,
  type QuotaConfig,
} from '@/components/quota';
import type { AuthFileItem, AuthFilesResponse } from '@/types/authFile';
import { downloadBlob } from '@/utils/download';
import { formatUnixTimestamp } from '@/utils/format';
import {
  CODEX_REQUEST_HEADERS,
  getStatusFromError,
  normalizeAuthIndex,
  normalizePlanType,
  resolveCodexChatgptAccountId,
} from '@/utils/quota';
import { createZipBlob } from '@/utils/zip';
import {
  ACCOUNT_POOL_UPDATED_EVENT,
  buildAccountPoolFileContentCache,
  isRuntimeOnlyAuthPoolFile,
  refreshAccountPoolFromServer,
  uniqueAccountPoolRecords,
  type AccountPoolSyncProgress,
  type AccountPoolRecord,
} from '@/utils/accountPool';
import {
  getAccountPoolEffectiveStatusCode,
  getAccountPoolErrorSummaryLabel,
} from '@/utils/accountPoolStatus';
import styles from './AccountPoolPage.module.scss';

const MIN_ACCOUNT_POOL_CHECK_CONCURRENCY = 1;
const MAX_ACCOUNT_POOL_CHECK_CONCURRENCY = 5;
const DEFAULT_ACCOUNT_POOL_CHECK_CONCURRENCY = 2;
const MIN_ACCOUNT_POOL_PAGE_SIZE = 1;
const MAX_ACCOUNT_POOL_PAGE_SIZE = 200;
const DEFAULT_ACCOUNT_POOL_PAGE_SIZE = 100;
const DEFAULT_ACCOUNT_POOL_SORT_MODE = 'folder_time_desc';
const DEFAULT_ACCOUNT_POOL_VIEW_MODE = 'folder';
const DEFAULT_ACCOUNT_POOL_FOLDER_FILTER = 'all';
const DEFAULT_ACCOUNT_POOL_PLAN_FILTER = 'all';
const DEFAULT_ACCOUNT_POOL_CHECK_STATUS_FILTER = 'all';
const DEFAULT_ACCOUNT_POOL_QUOTA_FILTER = 'all';
const LOW_ACCOUNT_POOL_QUOTA_PERCENT = 20;
const ACCOUNT_POOL_CHECK_RETRY_ATTEMPTS = 2;
const ACCOUNT_POOL_CHECK_RETRY_DELAY_MS = 700;
const ACCOUNT_POOL_REAL_REQUEST_MODEL = 'gpt-5.4-mini';
const ACCOUNT_POOL_CODEX_RESPONSES_URL = 'https://chatgpt.com/backend-api/codex/responses';
const ACCOUNT_POOL_IMPORT_ARCHIVE_EXTENSIONS = ['.zip', '.tar', '.tar.gz', '.tgz', '.gz'];
type AccountPoolImportKind = 'files' | 'folder';

type AccountPoolWriteAction =
  | 'overwrite-current'
  | 'append-current';
type AccountPoolViewMode = 'list' | 'folder';
type AccountPoolFolderInfo = NonNullable<AuthFilesResponse['folders']>[number];
const QUOTA_CONFIGS = [
  CLAUDE_CONFIG,
  ANTIGRAVITY_CONFIG,
  CODEX_CONFIG,
  GEMINI_CLI_CONFIG,
  KIMI_CONFIG,
] as Array<QuotaConfig<unknown, unknown>>;

const getFileType = (file: AuthFileItem): string => String(file.type || file.provider || 'unknown');

const getUploadFilePath = (file: File): string => {
  const relativePath =
    typeof (file as File & { webkitRelativePath?: string }).webkitRelativePath === 'string'
      ? (file as File & { webkitRelativePath?: string }).webkitRelativePath
      : '';
  return relativePath || file.name;
};

const getUploadTopFolder = (file: File): string => {
  const uploadPath = getUploadFilePath(file).replace(/\\/g, '/');
  const parts = uploadPath.split('/').filter(Boolean);
  return parts.length > 1 ? parts[0] : '';
};

const isSupportedAccountPoolImportName = (name: string): boolean => {
  const lowerName = name.trim().toLowerCase();
  return lowerName.endsWith('.json') || ACCOUNT_POOL_IMPORT_ARCHIVE_EXTENSIONS.some((ext) => lowerName.endsWith(ext));
};

const isArchiveAccountPoolImportName = (name: string): boolean => {
  const lowerName = name.trim().toLowerCase();
  return ACCOUNT_POOL_IMPORT_ARCHIVE_EXTENSIONS.some((ext) => lowerName.endsWith(ext));
};

const filterImportableAccountPoolFiles = async (files: File[]) => {
  const importable: File[] = [];
  let skipped = 0;

  await Promise.all(
    files.map(async (file) => {
      const uploadPath = getUploadFilePath(file);
      if (!isSupportedAccountPoolImportName(uploadPath)) {
        skipped += 1;
        return;
      }
      if (isArchiveAccountPoolImportName(uploadPath)) {
        importable.push(file);
        return;
      }
      try {
        JSON.parse(await file.text());
        importable.push(file);
      } catch {
        skipped += 1;
      }
    })
  );

  return { importable, skipped };
};

const buildAccountPoolFolderUploadFiles = async (files: File[]): Promise<File[]> => {
  const folderGroups = new Map<string, File[]>();
  const directFiles: File[] = [];

  files.forEach((file) => {
    const folder = getUploadTopFolder(file);
    if (!folder) {
      directFiles.push(file);
      return;
    }
    const group = folderGroups.get(folder) || [];
    group.push(file);
    folderGroups.set(folder, group);
  });

  if (folderGroups.size === 0) {
    return directFiles;
  }

  const packedFolders = await Promise.all(
    Array.from(folderGroups.entries()).map(async ([folder, group]) => {
      const zipFiles = await Promise.all(
        group.map(async (file) => ({
          name: getUploadFilePath(file).replace(/\\/g, '/'),
          text: await file.text(),
          modifiedAt: new Date(file.lastModified || Date.now()),
        }))
      );
      const blob = createZipBlob(zipFiles);
      return new File([blob], `${folder}.zip`, {
        type: 'application/zip',
        lastModified: Date.now(),
      });
    })
  );

  return [...directFiles, ...packedFolders];
};

const isAccountPoolImportDone = (job: AccountPoolImportJob): boolean =>
  job.status === 'done' || job.status === 'failed';

const normalizeFolderName = (value: unknown): string => {
  const folder = String(value || '').trim();
  return !folder || folder === '直接上传' ? '默认文件夹' : folder;
};

const getFileFolder = (file: AuthFileItem): string => {
  return normalizeFolderName(file.folder || file['source_folder']);
};

const getFileModifiedLabel = (file: AuthFileItem): string => {
  const value = file.modified ?? file['modtime'] ?? file['updated_at'];
  if (typeof value === 'number' && Number.isFinite(value)) {
    return formatUnixTimestamp(value < 1e12 ? value : Math.round(value / 1000));
  }
  if (typeof value === 'string' && value.trim()) return value;
  return '';
};

const formatFolderImportTime = (info?: AccountPoolFolderInfo): string => {
  const value = info?.created_at || info?.updated_at;
  const timestamp = parseDateValue(value);
  if (timestamp === null) return '';
  const date = new Date(timestamp);
  if (Number.isNaN(date.getTime())) return '';
  return date.toLocaleString(undefined, {
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
  });
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

const getFolderImportTimestamp = (info?: AccountPoolFolderInfo): number | null =>
  parseDateValue(info?.created_at || info?.updated_at);

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
  if (filter.startsWith('code:')) return true;
  if (filter === 'checking') return status === 'loading';
  if (filter === 'unchecked') return !status || status === 'idle';
  return status === filter;
};

const matchesStatusCodeFilter = (
  result: AccountCheckResult | undefined,
  filter: string
): boolean => {
  if (filter === DEFAULT_ACCOUNT_POOL_CHECK_STATUS_FILTER || !filter.startsWith('code:')) return true;
  const code = Number(filter.slice('code:'.length));
  return Number.isFinite(code) && getAccountPoolEffectiveStatusCode(result) === code;
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

const usdNumberFormatter = new Intl.NumberFormat(undefined, {
  minimumFractionDigits: 4,
  maximumFractionDigits: 4,
});

const formatUSDMetric = (value: number | undefined): string => {
  if (typeof value !== 'number' || !Number.isFinite(value) || value <= 0) return '$0.0000';
  return `$${usdNumberFormatter.format(value)}`;
};

const formatAccountCostMetric = (value: unknown): string => {
  const cost = readFiniteNumber(value);
  if (cost === null || cost <= 0) return '-';
  return `¥${cost.toFixed(cost >= 1 ? 2 : 3)}`;
};

const formatCostPerUSDMetric = (cost: unknown, totalUSD: unknown): string => {
  const costValue = readFiniteNumber(cost);
  const usdValue = readFiniteNumber(totalUSD);
  if (costValue === null || costValue <= 0 || usdValue === null || usdValue <= 0) return '-';
  const value = costValue / usdValue;
  return `¥${value.toFixed(value >= 1 ? 2 : 3)}/刀`;
};

const formatDurationSeconds = (seconds: number): string => {
  if (!Number.isFinite(seconds) || seconds <= 0) return '0分钟';
  const totalMinutes = Math.floor(seconds / 60);
  const days = Math.floor(totalMinutes / 1440);
  const hours = Math.floor((totalMinutes % 1440) / 60);
  const minutes = totalMinutes % 60;
  if (days > 0) return `${days}天${hours}小时`;
  if (hours > 0) return `${hours}小时${minutes}分钟`;
  return `${minutes}分钟`;
};

const formatAccountLifetime = (
  startedAt: number | null,
  stoppedAt?: number | null,
  lifetimeSeconds?: unknown,
  activeSince?: unknown
): string => {
  const serverSeconds = readFiniteNumber(lifetimeSeconds);
  if (serverSeconds !== null && serverSeconds >= 0) {
    const activeSinceAt = parseDateValue(activeSince);
    const shouldAccumulateActiveWindow =
      stoppedAt === null &&
      activeSinceAt !== null &&
      Number.isFinite(activeSinceAt) &&
      activeSinceAt > 0;
    const activeWindowSeconds = shouldAccumulateActiveWindow
      ? Math.max(0, Math.floor((Date.now() - activeSinceAt) / 1000))
      : 0;
    return formatDurationSeconds(serverSeconds + activeWindowSeconds);
  }
  if (startedAt === null || !Number.isFinite(startedAt) || startedAt <= 0) return '-';
  const endAt = stoppedAt !== null && stoppedAt !== undefined && Number.isFinite(stoppedAt) && stoppedAt > 0
    ? stoppedAt
    : Date.now();
  const diffMs = endAt - startedAt;
  return formatDurationSeconds(Math.floor(diffMs / 1000));
};

const getAccountCostValue = (file: AuthFileItem): unknown =>
  file.account_cost ?? file.accountCost ??
  (file.metadata && typeof file.metadata === 'object' && !Array.isArray(file.metadata)
    ? (file.metadata as Record<string, unknown>).account_cost
    : undefined) ??
  (file.attributes && typeof file.attributes === 'object' && !Array.isArray(file.attributes)
    ? (file.attributes as Record<string, unknown>).account_cost
    : undefined);

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


const getSourceChannelValue = (file: AuthFileItem): string => {
  const metadata = isRecord(file.metadata) ? file.metadata : null;
  const attributes = isRecord(file.attributes) ? file.attributes : null;
  const value = firstNonEmptyString(
    file.source_channel,
    file.sourceChannel,
    metadata?.source_channel,
    attributes?.source_channel
  );
  return value || '-';
};

const patchCachedAccountPoolContent = (
  rawText: string | undefined,
  fields: { account_cost: number; source_channel: string }
): string | undefined => {
  const parsed = parseJsonObject(rawText);
  if (!parsed) return rawText;
  if (fields.account_cost > 0) {
    parsed.account_cost = fields.account_cost;
  } else {
    delete parsed.account_cost;
  }
  if (fields.source_channel) {
    parsed.source_channel = fields.source_channel;
  } else {
    delete parsed.source_channel;
  }
  return JSON.stringify(parsed);
};

const isAccountInvalid = (file: AuthFileItem, checkResult?: AccountCheckResult): boolean => {
  const status = String(checkResult?.status ?? file.check_status ?? file.checkStatus ?? '').toLowerCase();
  if (checkResult?.realRequestOk === false) return true;
  if (status === 'error') return true;
  if (status === 'success') return checkResult?.realRequestOk !== true;
  if (status === 'loading') return false;
  if (file.disabled || file.unavailable) return true;
  const statusText = String(file.status ?? '').toLowerCase();
  if (['disabled', 'unavailable', 'invalid', 'error', 'failed'].includes(statusText)) return true;
  const message = String(
    checkResult?.message ?? file.check_message ?? file.checkMessage ?? file.statusMessage ?? file['status_message'] ?? ''
  ).toLowerCase();
  return /失效|无效|失败|错误|过期|登录|认证|invalid|expired|unauthorized|forbidden|failed|error/.test(message);
};

const getAccountLifetimeStoppedAt = (file: AuthFileItem, checkResult?: AccountCheckResult): number | null => {
  const persistedStoppedAt = parseDateValue(file.account_stopped_at ?? file.accountStoppedAt);
  if (persistedStoppedAt !== null) return persistedStoppedAt;
  if (!isAccountInvalid(file, checkResult)) return null;
  if (typeof checkResult?.checkedAt === 'number' && Number.isFinite(checkResult.checkedAt)) return checkResult.checkedAt;
  return parseDateValue(file.check_checked_at ?? file.checkCheckedAt);
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

const getAccountPoolIDVariants = (value: unknown): string[] => {
  const raw = String(value ?? '').trim();
  if (!raw) return [];
  const normalized = raw.replace(/\\/g, '/');
  const base = normalized.split('/').filter(Boolean).pop() || '';
  return Array.from(new Set([raw, normalized, base].filter(Boolean)));
};

const readFiniteNumber = (value: unknown): number | null => {
  if (typeof value === 'number' && Number.isFinite(value)) return value;
  if (typeof value === 'string' && value.trim()) {
    const parsed = Number(value);
    if (Number.isFinite(parsed)) return parsed;
  }
  return null;
};

const getAccountPoolUsageIdentifiers = (file: AuthFileItem): string[] =>
  Array.from(
    new Set(
      [
        ...getAccountPoolIDVariants(file.auth_id),
        ...getAccountPoolIDVariants(file.authId),
        ...getAccountPoolIDVariants(file.id),
        ...getAccountPoolIDVariants(file.authIndex),
        ...getAccountPoolIDVariants(file.auth_index),
        ...getAccountPoolIDVariants(file.name),
      ].filter(Boolean)
    )
  );

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

  const identifiers = getAccountPoolUsageIdentifiers(file);
  for (const authIdentifier of identifiers) {
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
  key: 'requests' | 'successes' | 'total_tokens' | 'failures' | 'total_usd'
): number | null => {
  const entryValue = readFiniteNumber(file[`usage_${key}`]);
  if (entryValue !== null) return entryValue;
  const summary = getAccountUsageSummary(file, fileContentCache, summaryByEmail, summaryByAuthID);
  if (!summary) return null;
  const value = summary[key];
  return typeof value === 'number' && Number.isFinite(value) ? value : null;
};

const clampAccountPoolPageSize = (value: number): number =>
  Math.min(MAX_ACCOUNT_POOL_PAGE_SIZE, Math.max(MIN_ACCOUNT_POOL_PAGE_SIZE, Math.round(value)));

const clampAccountPoolCheckConcurrency = (value: number): number =>
  Math.min(
    MAX_ACCOUNT_POOL_CHECK_CONCURRENCY,
    Math.max(MIN_ACCOUNT_POOL_CHECK_CONCURRENCY, Math.round(value))
  );

const readAccountPoolRemoteHash = (file: AuthFileItem): string => {
  const value =
    (file as Record<string, unknown>).content_hash ??
    (file as Record<string, unknown>).contentHash;
  return typeof value === 'string' ? value.trim() : '';
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

const isAbortError = (err: unknown): boolean => {
  if (!err || typeof err !== 'object') return false;
  const record = err as { name?: unknown; code?: unknown; message?: unknown };
  const name = typeof record.name === 'string' ? record.name.toLowerCase() : '';
  const code = typeof record.code === 'string' ? record.code.toLowerCase() : '';
  const message = typeof record.message === 'string' ? record.message.toLowerCase() : '';
  return (
    name === 'aborterror' ||
    code === 'err_canceled' ||
    code === 'abort_err' ||
    message.includes('canceled') ||
    message.includes('cancelled') ||
    message.includes('aborted')
  );
};

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
  if (status === 400 || status === 401 || status === 403 || status === 404) return false;
  if (status === 408 || status === 409 || status === 425 || status === 429) return true;
  if (typeof status === 'number' && status >= 500) return true;
  const message = err instanceof Error ? err.message.toLowerCase() : '';
  if (
    message.includes('auth token refresh failed') ||
    message.includes('could not parse your authentication token') ||
    message.includes('invalid token') ||
    message.includes('unauthorized')
  ) {
    return false;
  }
  return (
    message.includes('timeout') ||
    message.includes('network') ||
    message.includes('request failed')
  );
};

const fetchQuotaForAccountPool = async (
  config: QuotaConfig<unknown, unknown>,
  file: AuthFileItem,
  t: ReturnType<typeof useTranslation>['t'],
  parentSignal?: AbortSignal
): Promise<unknown> => {
  if (parentSignal?.aborted) {
    throw new DOMException('Account pool check aborted', 'AbortError');
  }

  const controller = new AbortController();
  const abortFromParent = () => controller.abort();
  parentSignal?.addEventListener('abort', abortFromParent, { once: true });

  try {
    return await config.fetchQuota(file, t, controller.signal);
  } finally {
    parentSignal?.removeEventListener('abort', abortFromParent);
  }
};

const makeAccountPoolModelRequestError = (message: string, statusCode?: number): Error & { statusCode?: number } => {
  const error = new Error(message) as Error & { statusCode?: number };
  if (typeof statusCode === 'number') {
    error.statusCode = statusCode;
  }
  return error;
};

const formatRealRequestErrorMessage = (err: unknown, t: ReturnType<typeof useTranslation>['t']): string => {
  const status = getAccountPoolCheckErrorStatus(err);
  const message = err instanceof Error ? err.message : t('common.unknown_error');
  if (status === 401) {
    return `模型请求 401 未认证：${message}`;
  }
  if (status) {
    return `模型请求 ${status}：${message}`;
  }
  return `模型请求失败：${message}`;
};

const requestCodexModelForAccountPool = async (
  file: AuthFileItem,
  t: ReturnType<typeof useTranslation>['t'],
  parentSignal?: AbortSignal
): Promise<void> => {
  const rawAuthIndex = file['auth_index'] ?? file.authIndex;
  const authIndex = normalizeAuthIndex(rawAuthIndex);
  if (!authIndex) {
    throw makeAccountPoolModelRequestError(t('codex_quota.missing_auth_index'));
  }

  const requestHeader: Record<string, string> = {
    ...CODEX_REQUEST_HEADERS,
  };
  const accountId = resolveCodexChatgptAccountId(file);
  if (accountId) {
    requestHeader['Chatgpt-Account-Id'] = accountId;
  }

  const result = await apiCallApi.request(
    {
      authIndex,
      authName: file.name,
      method: 'POST',
      url: ACCOUNT_POOL_CODEX_RESPONSES_URL,
      header: requestHeader,
      data: JSON.stringify({
        model: ACCOUNT_POOL_REAL_REQUEST_MODEL,
        input: [
          {
            role: 'user',
            content: [
              {
                type: 'input_text',
                text: 'ping',
              },
            ],
          },
        ],
        instructions: 'Reply with pong only.',
        stream: true,
        store: false,
      }),
    },
    { signal: parentSignal }
  );

  if (result.statusCode < 200 || result.statusCode >= 300) {
    throw makeAccountPoolModelRequestError(getApiCallErrorMessage(result), result.statusCode);
  }
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

const buildLazyAccountPoolRecords = (
  remoteFiles: AuthFileItem[],
  storedRecords: AccountPoolRecord[]
): AccountPoolRecord[] => {
  const storedByName = new Map(storedRecords.map((record) => [record.file.name, record]));
  const now = Date.now();
  return remoteFiles.map((file) => {
    const existing = storedByName.get(file.name);
    const remoteHash = readAccountPoolRemoteHash(file);
    const metadataHash = [
      'metadata',
      file.name,
      file.size ?? '',
      file.modified ?? file['modtime'] ?? file['updated_at'] ?? '',
      file.type ?? file.provider ?? '',
      getFileFolder(file),
    ].join(':');
    return {
      file: existing ? { ...existing.file, ...file } : file,
      content: existing?.content,
      hash: remoteHash || existing?.hash || metadataHash,
      savedAt: existing?.savedAt || now,
      sourceFingerprint: existing?.sourceFingerprint || metadataHash,
    };
  });
};

export function AccountPoolPage() {
  const { t } = useTranslation();
  const showNotification = useNotificationStore((state) => state.showNotification);
  const showConfirmation = useNotificationStore((state) => state.showConfirmation);
  const checking = useAccountPoolCheckStore((state) => state.checking);
  const checkResults = useAccountPoolCheckStore((state) => state.results);
  const checkSummary = useAccountPoolCheckStore((state) => state.summary);
  const beginCheck = useAccountPoolCheckStore((state) => state.beginCheck);
  const cancelCheck = useAccountPoolCheckStore((state) => state.cancelCheck);
  const getRunSignal = useAccountPoolCheckStore((state) => state.getRunSignal);
  const isRunCancelled = useAccountPoolCheckStore((state) => state.isRunCancelled);
  const setCheckResult = useAccountPoolCheckStore((state) => state.setResult);
  const finishCheck = useAccountPoolCheckStore((state) => state.finishCheck);
  const hydrateRemoteCheckResults = useAccountPoolCheckStore((state) => state.hydrateResultsFromFiles);
  const pruneCheckResults = useAccountPoolCheckStore((state) => state.pruneResults);
  const [files, setFiles] = useState<AuthFileItem[]>([]);
  const [fileContentCache, setFileContentCache] = useState<Record<string, string>>({});
  const [savedAtByName, setSavedAtByName] = useState<Map<string, number>>(() => new Map());
  const [hashByName, setHashByName] = useState<Map<string, string>>(() => new Map());
  const [loading, setLoading] = useState(true);
  const [downloading, setDownloading] = useState(false);
  const [importingPool, setImportingPool] = useState(false);
  const [importJob, setImportJob] = useState<AccountPoolImportJob | null>(null);
  const [importMetaOpen, setImportMetaOpen] = useState(false);
  const [importMetaKind, setImportMetaKind] = useState<AccountPoolImportKind>('files');
  const [importSourceChannel, setImportSourceChannel] = useState('');
  const [importAccountCost, setImportAccountCost] = useState('');
  const [importOverwriteMetadata, setImportOverwriteMetadata] = useState(false);
  const [activeWriteAction, setActiveWriteAction] = useState<AccountPoolWriteAction | null>(null);
  const [deletingPoolEntries, setDeletingPoolEntries] = useState(false);
  const [error, setError] = useState('');
  const [syncProgress, setSyncProgress] = useState<AccountPoolSyncProgress | null>(null);
  const [search, setSearch] = useState('');
  const [planFilter, setPlanFilter] = useState(DEFAULT_ACCOUNT_POOL_PLAN_FILTER);
  const [checkStatusFilter, setCheckStatusFilter] = useState(DEFAULT_ACCOUNT_POOL_CHECK_STATUS_FILTER);
  const [quickStatusFilter, setQuickStatusFilter] = useState(DEFAULT_ACCOUNT_POOL_CHECK_STATUS_FILTER);
  const [quotaFilter, setQuotaFilter] = useState(DEFAULT_ACCOUNT_POOL_QUOTA_FILTER);
  const [sortMode, setSortMode] = useState(DEFAULT_ACCOUNT_POOL_SORT_MODE);
  const [viewMode, setViewMode] = useState<AccountPoolViewMode>(DEFAULT_ACCOUNT_POOL_VIEW_MODE);
  const [folderFilter, setFolderFilter] = useState(DEFAULT_ACCOUNT_POOL_FOLDER_FILTER);
  const [sourceModelFilter, setSourceModelFilter] = useState(DEFAULT_ACCOUNT_POOL_FOLDER_FILTER);
  const [activeFolder, setActiveFolder] = useState<string | null>(null);
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(DEFAULT_ACCOUNT_POOL_PAGE_SIZE);
  const [pageSizeInput, setPageSizeInput] = useState(String(DEFAULT_ACCOUNT_POOL_PAGE_SIZE));
  const [checkConcurrency, setCheckConcurrency] = useState(DEFAULT_ACCOUNT_POOL_CHECK_CONCURRENCY);
  const [checkConcurrencyInput, setCheckConcurrencyInput] = useState(String(checkConcurrency));
  const [selectedNames, setSelectedNames] = useState<string[]>([]);
  const [usageSummaries, setUsageSummaries] = useState<AccountPoolUsageSummary[]>([]);
  const [folderInfos, setFolderInfos] = useState<AccountPoolFolderInfo[]>([]);
  const [sourceEditorOpen, setSourceEditorOpen] = useState(false);
  const [sourceEditorFolder, setSourceEditorFolder] = useState('');
  const [sourceEditorModel, setSourceEditorModel] = useState('');
  const [sourceEditorInfo, setSourceEditorInfo] = useState('');
  const [savingSourceInfo, setSavingSourceInfo] = useState(false);
  const [configViewerOpen, setConfigViewerOpen] = useState(false);
  const [configViewerName, setConfigViewerName] = useState('');
  const [configViewerContent, setConfigViewerContent] = useState('');
  const [configViewerLoading, setConfigViewerLoading] = useState(false);
  const [configViewerError, setConfigViewerError] = useState('');
  const [accountMetaEditorOpen, setAccountMetaEditorOpen] = useState(false);
  const [accountMetaEditorName, setAccountMetaEditorName] = useState('');
  const [accountMetaEditorCost, setAccountMetaEditorCost] = useState('');
  const [accountMetaEditorSourceChannel, setAccountMetaEditorSourceChannel] = useState('');
  const [savingAccountMeta, setSavingAccountMeta] = useState(false);
  const [resumedPendingCheck, setResumedPendingCheck] = useState(false);
  const importInputRef = useRef<HTMLInputElement | null>(null);
  const importFolderInputRef = useRef<HTMLInputElement | null>(null);
  const pendingCheckResultUpdatesRef = useRef<Map<string, AccountPoolCheckResultPatch>>(new Map());
  const checkResultFlushTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const flushingCheckResultsRef = useRef(false);
  const checkResultFlushPromiseRef = useRef<Promise<boolean> | null>(null);
  const initialRefreshStartedRef = useRef(false);

  const flushRemoteCheckResults = useCallback(async (): Promise<boolean> => {
    if (checkResultFlushTimerRef.current) {
      clearTimeout(checkResultFlushTimerRef.current);
      checkResultFlushTimerRef.current = null;
    }
    if (checkResultFlushPromiseRef.current) {
      return checkResultFlushPromiseRef.current;
    }
    const runFlush = async (): Promise<boolean> => {
      flushingCheckResultsRef.current = true;
      try {
        for (;;) {
          const updates = Array.from(pendingCheckResultUpdatesRef.current.values());
          if (updates.length === 0) return true;
          pendingCheckResultUpdatesRef.current.clear();
          try {
            const response = await authFilesApi.updateAccountPoolCheckResults(updates);
            if (response.auto_appended && response.auto_appended > 0) {
              showNotification(
                `已自动追加 ${response.auto_appended} 个检测通过且额度高的账号到认证文件`,
                'success'
              );
            }
          } catch (err) {
            updates.forEach((update) => {
              pendingCheckResultUpdatesRef.current.set(update.name, update);
            });
            if (!checkResultFlushTimerRef.current) {
              checkResultFlushTimerRef.current = setTimeout(() => {
                checkResultFlushTimerRef.current = null;
                void flushRemoteCheckResults();
              }, 2000);
            }
            console.warn('failed to persist account pool check results', err);
            return false;
          }
        }
      } finally {
        flushingCheckResultsRef.current = false;
      }
    };
    const promise = runFlush().finally(() => {
      if (checkResultFlushPromiseRef.current === promise) {
        checkResultFlushPromiseRef.current = null;
      }
    });
    checkResultFlushPromiseRef.current = promise;
    return promise;
  }, [showNotification]);

  const scheduleRemoteCheckResultFlush = useCallback(() => {
    if (checkResultFlushTimerRef.current) return;
    checkResultFlushTimerRef.current = setTimeout(() => {
      checkResultFlushTimerRef.current = null;
      void flushRemoteCheckResults();
    }, 300);
  }, [flushRemoteCheckResults]);

  const queueRemoteCheckResult = useCallback(
    (file: AuthFileItem, result: AccountCheckResult, contentHash?: string) => {
      if (!file.name || result.status === 'loading') return;
      const persistedStatus =
        result.status === 'success' && result.realRequestOk !== true ? 'error' : result.status;
      pendingCheckResultUpdatesRef.current.set(file.name, {
        name: file.name,
        content_hash: contentHash,
        result: {
          status: persistedStatus,
          message: result.message,
          plan: result.plan,
          quotaLines: result.quotaLines,
          quotaRemainingPercent: result.quotaRemainingPercent,
          quotaOk: result.quotaOk,
          realRequestOk: result.realRequestOk,
          realRequestError: result.realRequestError,
          realRequestStatusCode: result.realRequestStatusCode,
          requestedModel: result.requestedModel,
          statusCode: result.statusCode,
          checkedAt: result.checkedAt,
        },
      });
      scheduleRemoteCheckResultFlush();
    },
    [scheduleRemoteCheckResultFlush]
  );

  useEffect(
    () => () => {
      if (checkResultFlushTimerRef.current) {
        clearTimeout(checkResultFlushTimerRef.current);
        checkResultFlushTimerRef.current = null;
      }
      void flushRemoteCheckResults();
    },
    [flushRemoteCheckResults]
  );

  const applyRecords = useCallback((records: AccountPoolRecord[]) => {
    const nextRecords = uniqueAccountPoolRecords(records);
    applyAccountPoolRecords(nextRecords, setFiles, setFileContentCache, setSavedAtByName);
    setHashByName(new Map(nextRecords.map((record) => [record.file.name, record.hash])));
    setSelectedNames((current) =>
      current.filter((name) => nextRecords.some((record) => record.file.name === name))
    );
    hydrateRemoteCheckResults(nextRecords.map((record) => record.file));
    pruneCheckResults(nextRecords);
    return nextRecords;
  }, [hydrateRemoteCheckResults, pruneCheckResults]);

  const loadLazyRemotePool = useCallback(
    async () => {
      try {
        const response = await authFilesApi.listAccountPoolEntries({ includeHash: false });
        const remoteFiles = (response.files || []).filter(
          (file) => file && file.name && !isRuntimeOnlyAuthPoolFile(file)
        );
        if (remoteFiles.length === 0) return;
        const lazyRecords = buildLazyAccountPoolRecords(remoteFiles, []);
        applyRecords(lazyRecords);
        setFolderInfos(response.folders || []);
      } catch {
        // Full refresh below remains the source of truth when the lightweight list fails.
      }
    },
    [applyRecords]
  );

  const refreshAccountPoolDerivedState = useCallback(async () => {
    const [usageResponse, folderResponse] = await Promise.allSettled([
      authFilesApi.getAccountPoolUsageRecords({ summaryOnly: true }),
      authFilesApi.listAccountPoolEntries(),
    ]);
    if (usageResponse.status === 'fulfilled') {
      setUsageSummaries(usageResponse.value.summaries);
    }
    if (folderResponse.status === 'fulfilled') {
      setFolderInfos(folderResponse.value.folders || []);
    }
  }, []);

  const refreshPool = useCallback(
    async (showLoading = true) => {
      if (showLoading) {
        setLoading(true);
      }
      setError('');
      setSyncProgress(null);
      try {
        await loadLazyRemotePool();
        const mergedRecords = await refreshAccountPoolFromServer(checkConcurrency, setSyncProgress);
        applyRecords(mergedRecords);
        await refreshAccountPoolDerivedState();
        setSyncProgress(null);
      } catch (err: unknown) {
        const message = err instanceof Error ? err.message : t('notification.refresh_failed');
        setError(message);
      } finally {
        if (showLoading) {
          setLoading(false);
        }
      }
    },
    [applyRecords, checkConcurrency, loadLazyRemotePool, refreshAccountPoolDerivedState, t]
  );

  const loadFolderInfos = useCallback(async () => {
    try {
      const response = await authFilesApi.listAccountPoolEntries();
      setFolderInfos(response.folders || []);
    } catch {
      setFolderInfos([]);
    }
  }, []);

  const pollAccountPoolImportJob = useCallback(async (jobId: string) => {
    if (!jobId) return;
    setImportingPool(true);
    try {
      for (;;) {
        const nextJob = await authFilesApi.getAccountPoolImport(jobId);
        setImportJob(nextJob);
        if (isAccountPoolImportDone(nextJob)) {
          await refreshPool(false);
          await loadFolderInfos();
          showNotification(
            nextJob.status === 'done'
              ? `账号池后台导入完成：导入 ${nextJob.imported} 个，跳过 ${nextJob.skipped} 个，失败 ${nextJob.failed} 个`
              : `账号池后台导入失败：${nextJob.error || '未知错误'}`,
            nextJob.status === 'done' ? 'success' : 'error'
          );
          break;
        }
        await new Promise((resolve) => window.setTimeout(resolve, 1500));
      }
    } catch (err) {
      const message = err instanceof Error ? err.message : '查询导入任务失败';
      showNotification(message, 'error');
    } finally {
      setImportingPool(false);
    }
  }, [loadFolderInfos, refreshPool, showNotification]);

  useEffect(() => {
    if (!initialRefreshStartedRef.current) {
      initialRefreshStartedRef.current = true;
      void refreshPool(true);
    }

    const handleAccountPoolUpdated = (event: Event) => {
      const records = (event as CustomEvent<AccountPoolRecord[]>).detail;
      if (Array.isArray(records)) {
        applyRecords(records);
      }
    };

    window.addEventListener(ACCOUNT_POOL_UPDATED_EVENT, handleAccountPoolUpdated);
    return () => window.removeEventListener(ACCOUNT_POOL_UPDATED_EVENT, handleAccountPoolUpdated);
  }, [applyRecords, refreshPool]);

  const loadUsageSummaries = useCallback(async () => {
    try {
      const response = await authFilesApi.getAccountPoolUsageRecords({ summaryOnly: true });
      setUsageSummaries(response.summaries);
    } catch {
      // Keep the last known per-account usage stats unless the account is explicitly deleted.
    }
  }, []);

  useEffect(() => {
    void loadUsageSummaries();
  }, [loadUsageSummaries]);

  useEffect(() => {
    void loadFolderInfos();
  }, [loadFolderInfos]);


  const sortOptions = useMemo(
    () => [
      { value: 'check', label: t('account_pool.sort_check') },
      { value: 'quota_desc', label: t('account_pool.sort_quota_desc') },
      { value: 'quota_asc', label: t('account_pool.sort_quota_asc') },
      { value: 'requests_desc', label: t('account_pool.sort_requests_desc', { defaultValue: '请求最多' }) },
      { value: 'success_desc', label: t('account_pool.sort_success_desc', { defaultValue: '成功最多' }) },
      { value: 'token_desc', label: t('account_pool.sort_token_desc', { defaultValue: 'Token 最多' }) },
      { value: 'usd_desc', label: t('account_pool.sort_usd_desc', { defaultValue: '刀数最多' }) },
      { value: 'failure_desc', label: t('account_pool.sort_failure_desc', { defaultValue: '失败最多' }) },
      { value: 'folder_time_desc', label: t('account_pool.sort_folder_time_desc', { defaultValue: '文件夹时间最新' }) },
      { value: 'folder_time_asc', label: t('account_pool.sort_folder_time_asc', { defaultValue: '文件夹时间最早' }) },
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
      { value: 'checking', label: t('account_pool.check_status_checking', { defaultValue: '检测中' }) },
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

  const folderOptions = useMemo(() => {
    const folders = Array.from(new Set(files.map(getFileFolder))).sort((a, b) => a.localeCompare(b));
    return [
      { value: DEFAULT_ACCOUNT_POOL_FOLDER_FILTER, label: '全部文件夹' },
      ...folders.map((folder) => ({ value: folder, label: folder })),
    ];
  }, [files]);

  const sourceModelOptions = useMemo(() => {
    const modelByFolder = new Map(folderInfos.map((item) => [item.folder, item.source_model || '']));
    const models = Array.from(
      new Set(
        files
          .map((file) => modelByFolder.get(getFileFolder(file)) || String(file.type || file.provider || ''))
          .map((value) => value.trim())
          .filter(Boolean)
      )
    ).sort((a, b) => a.localeCompare(b));
    return [
      { value: DEFAULT_ACCOUNT_POOL_FOLDER_FILTER, label: '全部来源模型' },
      ...models.map((model) => ({ value: model, label: model })),
    ];
  }, [files, folderInfos]);

  const folderInfoByName = useMemo(
    () => new Map(folderInfos.map((item) => [normalizeFolderName(item.folder), item])),
    [folderInfos]
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
      [...getAccountPoolIDVariants(summary.auth_id), ...getAccountPoolIDVariants(summary.auth_index)].forEach((authID) => {
        if (authID && !map.has(authID)) {
          map.set(authID, summary);
        }
      });
    });
    return map;
  }, [usageSummaries]);

  const compareUsageMetric = useCallback(
    (
      left: AuthFileItem,
      right: AuthFileItem,
      key: 'requests' | 'successes' | 'total_tokens' | 'failures' | 'total_usd'
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

  const filterAndSortFiles = useCallback((sourceFiles: AuthFileItem[]) => {
    const term = search.trim().toLowerCase();
    return sourceFiles
      .filter((file) => {
        const checkResult = checkResults[file.name];
        const folder = getFileFolder(file);
        const folderInfo = folderInfoByName.get(folder);
        const sourceModel = folderInfo?.source_model || getFileType(file);
        if (folderFilter !== DEFAULT_ACCOUNT_POOL_FOLDER_FILTER && folder !== folderFilter) return false;
        if (sourceModelFilter !== DEFAULT_ACCOUNT_POOL_FOLDER_FILTER && sourceModel !== sourceModelFilter) return false;
        if (!matchesPlanFilter(file, fileContentCache, checkResult?.plan, planFilter)) return false;
        if (!matchesCheckStatusFilter(checkResult?.status, checkStatusFilter)) return false;
        if (!matchesCheckStatusFilter(checkResult?.status, quickStatusFilter)) return false;
        if (!matchesStatusCodeFilter(checkResult, quickStatusFilter)) return false;
        if (!matchesQuotaFilter(checkResult, quotaFilter)) return false;
        if (!term) return true;
        return [file.name, getFileType(file), folder, sourceModel, folderInfo?.source_info, file.statusMessage, file.status]
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
        } else if (sortMode === 'usd_desc') {
          const diff = compareUsageMetric(left, right, 'total_usd');
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
    folderFilter,
    folderInfoByName,
    planFilter,
    quotaFilter,
    quickStatusFilter,
    savedAtByName,
    search,
    sortMode,
    sourceModelFilter,
    compareUsageMetric,
  ]);

  const filteredFiles = useMemo(() => filterAndSortFiles(files), [files, filterAndSortFiles]);
  const displayedFiles = useMemo(
    () =>
      viewMode === 'folder' && activeFolder
        ? filteredFiles.filter((file) => getFileFolder(file) === activeFolder)
        : filteredFiles,
    [activeFolder, filteredFiles, viewMode]
  );

  const selectedSet = useMemo(() => new Set(selectedNames), [selectedNames]);
  const folderTotalTokensByName = useMemo(() => {
    const totals = new Map<string, number>();
    folderInfos.forEach((info) => {
      const folder = normalizeFolderName(info.folder);
      const value = readFiniteNumber(info.total_tokens);
      if (value !== null) {
        totals.set(folder, value);
      }
    });
    return totals;
  }, [folderInfos]);
  const folderTotalUSDByName = useMemo(() => {
    const totals = new Map<string, number>();
    folderInfos.forEach((info) => {
      const folder = normalizeFolderName(info.folder);
      const value = readFiniteNumber(info.total_usd);
      if (value !== null) {
        totals.set(folder, value);
      }
    });
    return totals;
  }, [folderInfos]);
  const folderGroups = useMemo(() => {
    const groups = new Map<string, AuthFileItem[]>();
    filteredFiles.forEach((file) => {
      const folder = getFileFolder(file);
      const items = groups.get(folder) || [];
      items.push(file);
      groups.set(folder, items);
    });
    return Array.from(groups.entries())
      .map(([folder, items]) => {
        const byCode = new Map<number, number>();
        const errorLabels = new Map<string, number>();
        let successCount = 0;
        let checkingCount = 0;
        let unchecked = 0;
        let unsupported = 0;
        let totalCost = 0;
        items.forEach((file) => {
          const accountCost = readFiniteNumber(getAccountCostValue(file));
          if (accountCost !== null && accountCost > 0) totalCost += accountCost;
          const result = checkResults[file.name];
          if (result?.status === 'loading') {
            checkingCount += 1;
            return;
          }
          if (!result || result.status === 'idle') {
            unchecked += 1;
            return;
          }
          if (result.status === 'unsupported') {
            unsupported += 1;
            return;
          }
          const effectiveStatusCode = getAccountPoolEffectiveStatusCode(result);
          if (typeof effectiveStatusCode === 'number') {
            byCode.set(effectiveStatusCode, (byCode.get(effectiveStatusCode) ?? 0) + 1);
            if (effectiveStatusCode >= 200 && effectiveStatusCode < 300) {
              successCount += 1;
            }
            return;
          }
          if (result.status === 'success') {
            successCount += 1;
            return;
          }
          if (result.status === 'error') {
            const label = getAccountPoolErrorSummaryLabel(result);
            errorLabels.set(label, (errorLabels.get(label) ?? 0) + 1);
          } else {
            unchecked += 1;
          }
        });
        return {
          folder,
          info: folderInfoByName.get(folder),
          items,
          stats: {
            codes: Array.from(byCode.entries()).sort(([left], [right]) => left - right),
            success: successCount,
            checking: checkingCount,
            unchecked,
            unsupported,
            errorLabels: Array.from(errorLabels.entries()).sort(([leftLabel, leftCount], [rightLabel, rightCount]) => {
              if (rightCount !== leftCount) return rightCount - leftCount;
              return leftLabel.localeCompare(rightLabel);
            }),
            totalTokens: folderTotalTokensByName.get(folder) ?? 0,
            totalUSD: folderTotalUSDByName.get(folder) ?? 0,
            totalCost,
          },
        };
      })
      .sort((left, right) => {
        if (sortMode === 'folder_time_desc' || sortMode === 'folder_time_asc') {
          const timeDiff = compareOptionalTime(
            getFolderImportTimestamp(left.info),
            getFolderImportTimestamp(right.info),
            sortMode === 'folder_time_asc' ? 'asc' : 'desc'
          );
          if (timeDiff !== 0) return timeDiff;
        }
        const successDiff = right.stats.success - left.stats.success;
        if (successDiff !== 0) return successDiff;
        const sizeDiff = right.items.length - left.items.length;
        if (sizeDiff !== 0) return sizeDiff;
        return left.folder.localeCompare(right.folder);
      });
  }, [
    checkResults,
    fileContentCache,
    filteredFiles,
    folderInfoByName,
    folderTotalTokensByName,
    folderTotalUSDByName,
    sortMode,
    usageSummaryByAuthID,
    usageSummaryByEmail,
  ]);

  const isFolderOverview = viewMode === 'folder' && !activeFolder;
  const paginatedItemCount = isFolderOverview ? folderGroups.length : displayedFiles.length;
  const totalPages = Math.max(1, Math.ceil(paginatedItemCount / pageSize));
  const currentPage = Math.min(page, totalPages);
  const pageStart = (currentPage - 1) * pageSize;
  const pageItems = displayedFiles.slice(pageStart, pageStart + pageSize);
  const folderPageGroups = isFolderOverview
    ? folderGroups.slice(pageStart, pageStart + pageSize)
    : folderGroups;
  const visibleSelectionFiles = isFolderOverview
    ? folderPageGroups.flatMap((group) => group.items)
    : pageItems;
  const visibleSelectedCount = visibleSelectionFiles.filter((file) =>
    selectedSet.has(file.name)
  ).length;
  const allVisibleSelected =
    visibleSelectionFiles.length > 0 && visibleSelectedCount === visibleSelectionFiles.length;
  const filteredSelectedCount = displayedFiles.filter((file) => selectedSet.has(file.name)).length;
  const allFilteredSelected =
    displayedFiles.length > 0 && filteredSelectedCount === displayedFiles.length;

  const selectedFiles = useMemo(
    () => files.filter((file) => selectedSet.has(file.name)),
    [files, selectedSet]
  );
  const currentActionFiles = useMemo(
    () => (selectedFiles.length > 0 ? selectedFiles : displayedFiles),
    [displayedFiles, selectedFiles]
  );
  const currentActionLabel = selectedFiles.length > 0 ? '选中' : '筛选';
  const currentActionCount = currentActionFiles.length;
  const currentActionDescription =
    selectedFiles.length > 0
      ? `当前手动选中的 ${currentActionCount} 个账号`
      : `当前筛选结果中的 ${currentActionCount} 个账号`;
  const statusStatsSourceFiles = useMemo(
    () =>
      viewMode === 'folder' && activeFolder
        ? files.filter((file) => getFileFolder(file) === activeFolder)
        : files,
    [activeFolder, files, viewMode]
  );

  const statusCodeStats = useMemo(() => {
    const byCode = new Map<number, number>();
    const errorLabels = new Map<string, number>();
    let checkingCount = 0;
    let unchecked = 0;
    let unsupported = 0;

    for (const file of statusStatsSourceFiles) {
      const result = checkResults[file.name];
      if (result?.status === 'loading') {
        checkingCount += 1;
        continue;
      }
      if (!result || result.status === 'idle') {
        unchecked += 1;
        continue;
      }
      if (result.status === 'unsupported') {
        unsupported += 1;
        continue;
      }
      const effectiveStatusCode = getAccountPoolEffectiveStatusCode(result);
      if (typeof effectiveStatusCode === 'number') {
        byCode.set(effectiveStatusCode, (byCode.get(effectiveStatusCode) ?? 0) + 1);
        continue;
      }
      if (result.status === 'error') {
        const label = getAccountPoolErrorSummaryLabel(result);
        errorLabels.set(label, (errorLabels.get(label) ?? 0) + 1);
      } else {
        unchecked += 1;
      }
    }

    return {
      codes: Array.from(byCode.entries()).sort(([left], [right]) => left - right),
      checking: checkingCount,
      unchecked,
      unsupported,
      errorLabels: Array.from(errorLabels.entries()).sort(([leftLabel, leftCount], [rightLabel, rightCount]) => {
        if (rightCount !== leftCount) return rightCount - leftCount;
        return leftLabel.localeCompare(rightLabel);
      }),
    };
  }, [checkResults, statusStatsSourceFiles]);

  const displayedStatusCodeStats = statusCodeStats;
  const applyStatusFilter = (filter: string) => {
    setCheckStatusFilter(DEFAULT_ACCOUNT_POOL_CHECK_STATUS_FILTER);
    setQuickStatusFilter((current) =>
      current === filter ? DEFAULT_ACCOUNT_POOL_CHECK_STATUS_FILTER : filter
    );
    setPage(1);
  };
  const syncProgressPercent = syncProgress?.total
    ? Math.round((syncProgress.processed / syncProgress.total) * 100)
    : 0;
  const syncPhaseLabel = syncProgress
    ? syncProgress.phase === 'listing'
      ? t('account_pool.refresh_phase_listing', { defaultValue: '读取账号池' })
      : syncProgress.phase === 'saving'
        ? t('account_pool.refresh_phase_saving', { defaultValue: '保存账号池' })
        : syncProgress.phase === 'done'
          ? t('account_pool.refresh_phase_done', { defaultValue: '刷新完成' })
          : t('account_pool.refresh_phase_syncing', { defaultValue: '刷新中' })
    : '';

  useEffect(() => {
    if (page > totalPages) {
      setPage(totalPages);
    }
  }, [page, totalPages]);

  useEffect(() => {
    setPage(1);
  }, [checkStatusFilter, folderFilter, planFilter, quickStatusFilter, quotaFilter, search, sortMode, sourceModelFilter]);

  useEffect(() => {
    if (viewMode !== 'folder') {
      setActiveFolder(null);
    }
  }, [viewMode]);

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
      visibleSelectionFiles.forEach((file) => {
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

  const toggleFolder = (items: AuthFileItem[], checked: boolean) => {
    setSelectedNames((current) => {
      const next = new Set(current);
      items.forEach((file) => {
        if (checked) {
          next.add(file.name);
        } else {
          next.delete(file.name);
        }
      });
      return Array.from(next);
    });
  };

  const closeConfigViewer = () => {
    setConfigViewerOpen(false);
    setConfigViewerName('');
    setConfigViewerContent('');
    setConfigViewerError('');
    setConfigViewerLoading(false);
  };

  const openAccountMetaEditor = (file: AuthFileItem) => {
    setAccountMetaEditorName(file.name);
    const cost = readFiniteNumber(getAccountCostValue(file));
    setAccountMetaEditorCost(cost !== null && cost > 0 ? String(cost) : '');
    const sourceChannel = getSourceChannelValue(file);
    setAccountMetaEditorSourceChannel(sourceChannel === '-' ? '' : sourceChannel);
    setAccountMetaEditorOpen(true);
  };

  const closeAccountMetaEditor = () => {
    if (savingAccountMeta) return;
    setAccountMetaEditorOpen(false);
    setAccountMetaEditorName('');
    setAccountMetaEditorCost('');
    setAccountMetaEditorSourceChannel('');
  };

  const saveAccountMetaEditor = async () => {
    const name = accountMetaEditorName.trim();
    if (!name) return;
    const costText = accountMetaEditorCost.trim();
    const cost = costText ? Number(costText) : 0;
    if (!Number.isFinite(cost) || cost < 0) {
      showNotification('账号成本必须是大于等于 0 的数字', 'error');
      return;
    }

    setSavingAccountMeta(true);
    try {
      const sourceChannel = accountMetaEditorSourceChannel.trim();
      const fields = {
        account_cost: cost,
        source_channel: sourceChannel,
      };
      await authFilesApi.patchFields(name, fields);
      setFiles((current) =>
        current.map((file) =>
          file.name === name
            ? {
                ...file,
                account_cost: cost > 0 ? cost : undefined,
                source_channel: sourceChannel || undefined,
              }
            : file
        )
      );
      setFileContentCache((current) => ({
        ...current,
        [name]: patchCachedAccountPoolContent(current[name], fields) ?? current[name],
      }));
      showNotification('账号成本和渠道来源已保存', 'success');
      setAccountMetaEditorOpen(false);
      setAccountMetaEditorName('');
      setAccountMetaEditorCost('');
      setAccountMetaEditorSourceChannel('');
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : t('common.unknown_error');
      showNotification(`保存账号信息失败：${message}`, 'error');
    } finally {
      setSavingAccountMeta(false);
    }
  };

  const downloadAccountPoolFiles = async (targets: AuthFileItem[], label: string) => {
    if (targets.length === 0) return;
    setDownloading(true);
    try {
      const zipBlob = await authFilesApi.downloadAccountPoolArchiveForNames(
        targets.map((file) => file.name)
      );
      downloadBlob({ filename: buildDownloadFileName(), blob: zipBlob });
      showNotification(
        t('account_pool.download_success', {
          count: targets.length,
          defaultValue: `已下载${label} ${targets.length} 个账号`,
        }),
        'success'
      );
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : t('common.unknown_error');
      showNotification(t('account_pool.download_failed', { message }), 'error');
    } finally {
      setDownloading(false);
    }
  };

  const handleDownloadCurrent = async () => {
    await downloadAccountPoolFiles(currentActionFiles, currentActionLabel);
  };

  const handleImportAccountPoolFiles = async (event: ChangeEvent<HTMLInputElement>) => {
    const uploadFiles = Array.from(event.currentTarget.files ?? []);
    event.currentTarget.value = '';
    if (uploadFiles.length === 0 || importingPool) return;
    setImportingPool(true);
    try {
      const { importable, skipped } = await filterImportableAccountPoolFiles(uploadFiles);
      if (importable.length === 0) {
        showNotification(`未发现可导入文件，已剔除 ${skipped} 个文件`, 'warning');
        return;
      }
      const uploadPayload = await buildAccountPoolFolderUploadFiles(importable);
      const result = await authFilesApi.uploadAccountPoolFiles(uploadPayload, {
        sourceChannel: importSourceChannel.trim(),
        accountCost: importAccountCost.trim(),
        overwriteMetadata: importOverwriteMetadata,
      });
      if (result.job?.id) {
        setImportJob(result.job);
        showNotification(`账号池后台导入已开始：${result.job.total} 个上传包`, 'success');
        void pollAccountPoolImportJob(result.job.id);
        if (skipped > 0) {
          showNotification(`已自动剔除 ${skipped} 个不可导入文件`, 'warning');
        }
        return;
      }
      await refreshPool(false);
      await loadFolderInfos();
      showNotification(
        result.failed.length > 0
          ? t('account_pool.import_partial', {
              success: result.uploaded,
              failed: result.failed.length,
              defaultValue: `已导入账号池 ${result.uploaded} 个，失败 ${result.failed.length} 个`,
            })
          : t('account_pool.import_success', {
              count: result.uploaded,
              defaultValue: `已导入账号池 ${result.uploaded} 个`,
            }),
        result.failed.length > 0 ? 'warning' : 'success'
      );
      if (skipped > 0) {
        showNotification(`已自动剔除 ${skipped} 个不可导入文件`, 'warning');
      }
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : t('common.unknown_error');
      showNotification(
        t('account_pool.import_failed', {
          message,
          defaultValue: `导入账号池失败：${message}`,
        }),
        'error'
      );
    } finally {
      setImportingPool(false);
    }
  };

  const openImportMeta = (kind: AccountPoolImportKind) => {
    if (importingPool) return;
    setImportMetaKind(kind);
    setImportMetaOpen(true);
  };

  const closeImportMeta = () => {
    if (importingPool) return;
    setImportMetaOpen(false);
  };

  const startImportWithMetadata = () => {
    if (importingPool) return;
    setImportMetaOpen(false);
    if (importMetaKind === 'folder') {
      importFolderInputRef.current?.click();
    } else {
      importInputRef.current?.click();
    }
  };

  const editFolderSourceInfo = (folder: string) => {
    const current = folderInfoByName.get(folder);
    setSourceEditorFolder(folder);
    setSourceEditorModel(current?.source_model || '');
    setSourceEditorInfo(current?.source_info || '');
    setSourceEditorOpen(true);
  };

  const closeSourceEditor = () => {
    if (savingSourceInfo) return;
    setSourceEditorOpen(false);
  };

  const saveFolderSourceInfo = async () => {
    if (!sourceEditorFolder || savingSourceInfo) return;
    setSavingSourceInfo(true);
    const folder = sourceEditorFolder;
    const sourceModel = sourceEditorModel.trim();
    const sourceInfo = sourceEditorInfo.trim();
    try {
      await authFilesApi.updateAccountPoolFolder({
        folder,
        source_model: sourceModel,
        source_info: sourceInfo,
      });
      setFolderInfos((current) => {
        const now = new Date().toISOString();
        const existing = current.find((item) => item.folder === folder);
        if (existing) {
          return current.map((item) =>
            item.folder === folder
              ? {
                  ...item,
                  source_model: sourceModel,
                  source_info: sourceInfo,
                  updated_at: now,
                }
              : item
          );
        }
        const count = files.filter((file) => getFileFolder(file) === folder).length;
        return [
          ...current,
          {
            folder,
            source_model: sourceModel,
            source_info: sourceInfo,
            count,
            created_at: now,
            updated_at: now,
          },
        ];
      });
      setSourceEditorOpen(false);
      showNotification('来源信息已保存', 'success');
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : t('common.unknown_error');
      showNotification(`保存来源信息失败：${message}`, 'error');
    } finally {
      setSavingSourceInfo(false);
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

      await refreshPool(false);
      const deletedNames = new Set(names);
      const deletedEmails = new Set(
        targets
          .map((file) => getAccountPoolEmail(file, fileContentCache))
          .filter(Boolean)
      );
      setUsageSummaries((current) =>
        current.filter((summary) => {
          const authID = String(summary.auth_id || '').trim();
          const authIndex = String(summary.auth_index || '').trim();
          const email = String(summary.service_email || '').trim().toLowerCase();
          return !(
            deletedNames.has(authID) ||
            deletedNames.has(authIndex) ||
            (email && deletedEmails.has(email))
          );
        })
      );
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

  const overwriteAccountFiles = async (targets: AuthFileItem[]) => {
    if (targets.length === 0 || activeWriteAction) return;
    setActiveWriteAction('overwrite-current');
    try {
      const result = await authFilesApi.writeAccountPoolToAuthFiles(
        targets.map((file) => file.name),
        true
      );
      if (result.failed.length > 0) {
        showNotification(
          t('account_pool.overwrite_current_partial', {
            success: result.uploaded,
            failed: result.failed.length,
            defaultValue: `覆盖部分完成：成功 ${result.uploaded}，失败 ${result.failed.length}`,
          }),
          'warning'
        );
        return;
      }
      showNotification(
        t('account_pool.overwrite_current_success', {
          count: result.uploaded,
          defaultValue: `已覆盖 ${result.uploaded} 个账号到认证文件`,
        }),
        'success'
      );
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : t('common.unknown_error');
      showNotification(
        t('account_pool.overwrite_current_failed', {
          message,
          defaultValue: `覆盖失败：${message}`,
        }),
        'error'
      );
    } finally {
      setActiveWriteAction(null);
    }
  };

  const appendAccountFiles = async (targets: AuthFileItem[]) => {
    if (targets.length === 0 || activeWriteAction) return;
    setActiveWriteAction('append-current');
    try {
      const result = await authFilesApi.writeAccountPoolToAuthFiles(
        targets.map((file) => file.name),
        false
      );
      if (result.failed.length > 0) {
        showNotification(
          t('account_pool.append_current_partial', {
            success: result.uploaded,
            failed: result.failed.length,
            defaultValue: `追加部分完成：成功 ${result.uploaded}，失败 ${result.failed.length}`,
          }),
          'warning'
        );
        return;
      }
      showNotification(
        t('account_pool.append_current_success', {
          count: result.uploaded,
          defaultValue: `已追加 ${result.uploaded} 个账号到认证文件`,
        }),
        'success'
      );
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : t('common.unknown_error');
      showNotification(
        t('account_pool.append_current_failed', {
          message,
          defaultValue: `追加失败：${message}`,
        }),
        'error'
      );
    } finally {
      setActiveWriteAction(null);
    }
  };

  const handleOverwriteCurrent = () => {
    if (currentActionFiles.length === 0) return;
    showConfirmation({
      title: '覆盖到认证文件',
      message: t('account_pool.overwrite_current_confirm', {
        count: currentActionFiles.length,
        defaultValue: `确认先删除当前所有认证文件，再写入${currentActionDescription}？账号池缓存不会被删除。`,
      }),
      confirmText: t('common.confirm'),
      variant: 'danger',
      onConfirm: () => void overwriteAccountFiles(currentActionFiles),
    });
  };

  const handleAppendCurrent = () => {
    if (currentActionFiles.length === 0) return;
    showConfirmation({
      title: '追加到认证文件',
      message: t('account_pool.append_current_confirm', {
        count: currentActionFiles.length,
        defaultValue: `确认将${currentActionDescription}追加到认证文件？现有认证文件不会删除。`,
      }),
      confirmText: t('common.confirm'),
      onConfirm: () => void appendAccountFiles(currentActionFiles),
    });
  };

  const detectAccounts = async (targets: AuthFileItem[]) => {
    if (targets.length === 0 || checking) return;
    let checkTargets = targets;
    const unsupportedTargets = targets.filter((file) => !resolveQuotaConfig(file));
    if (unsupportedTargets.length > 0) {
      try {
        const targetNames = new Set(targets.map((file) => file.name));
        const targetFolders = new Set(unsupportedTargets.map(getFileFolder));
        const repairResult = await authFilesApi.repairAccountPoolEntries();
        if (repairResult.repaired) {
          const repairedRecords = await refreshAccountPoolFromServer(checkConcurrency);
          applyRecords(repairedRecords);
          checkTargets = repairedRecords
            .map((record) => record.file)
            .filter((file) => {
              if (!resolveQuotaConfig(file)) return false;
              return targetNames.has(file.name) || targetFolders.has(getFileFolder(file));
            });
          showNotification(
            `已自动修复账号池：Sub2 转 CPA ${repairResult.convertedSub2} 个，补全 Codex ${repairResult.inferredCodex} 个，大模型修复 ${repairResult.llmRepaired} 个`,
            repairResult.llmFailed > 0 ? 'warning' : 'success'
          );
        }
      } catch (err) {
        showNotification(
          `自动修复不支持账号失败：${err instanceof Error ? err.message : t('common.unknown_error')}`,
          'warning'
        );
      }
    }
    if (checkTargets.length === 0) return;

    const runId = beginCheck(checkTargets.map((file) => file.name));
    if (!runId) {
      return;
    }
    const signal = getRunSignal(runId);

    const checkOne = async (file: AuthFileItem): Promise<AccountCheckResult> => {
      if (signal?.aborted || isRunCancelled(runId)) {
        throw new DOMException('Account pool check aborted', 'AbortError');
      }
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
        if (signal?.aborted || isRunCancelled(runId)) {
          throw new DOMException('Account pool check aborted', 'AbortError');
        }
        let quotaSummary: ReturnType<typeof getQuotaSummary> | null = null;
        let detectedPlan: string | undefined;
        try {
          const quota = await fetchQuotaForAccountPool(config, file, t, signal);
          quotaSummary = getQuotaSummary(quota, t);
          detectedPlan = getDetectedPlan(quota);
        } catch (err: unknown) {
          if (isAbortError(err) || signal?.aborted || isRunCancelled(runId)) {
            throw err;
          }
          lastError = err;
          if (
            attempt >= ACCOUNT_POOL_CHECK_RETRY_ATTEMPTS ||
            !isRetryableAccountPoolCheckError(err)
          ) {
            break;
          }
          await sleep(ACCOUNT_POOL_CHECK_RETRY_DELAY_MS * (attempt + 1));
          continue;
        }

        try {
          await requestCodexModelForAccountPool(file, t, signal);
          return {
            status: 'success',
            message: t('account_pool.check_success'),
            plan: detectedPlan,
            quotaLines: quotaSummary.lines,
            quotaRemainingPercent: quotaSummary.remainingPercent,
            quotaOk: true,
            realRequestOk: true,
            requestedModel: ACCOUNT_POOL_REAL_REQUEST_MODEL,
            statusCode: 200,
            checkedAt: Date.now(),
          };
        } catch (err: unknown) {
          if (isAbortError(err) || signal?.aborted || isRunCancelled(runId)) {
            throw err;
          }
          const status = getAccountPoolCheckErrorStatus(err);
          const message = formatRealRequestErrorMessage(err, t);
          return {
            status: 'error',
            message: '模型检测请求失败',
            plan: detectedPlan,
            quotaLines: quotaSummary.lines,
            quotaRemainingPercent: quotaSummary.remainingPercent,
            quotaOk: true,
            realRequestOk: false,
            realRequestError: message,
            realRequestStatusCode: status,
            requestedModel: ACCOUNT_POOL_REAL_REQUEST_MODEL,
            checkedAt: Date.now(),
          };
        }
      }

      const message = lastError instanceof Error ? lastError.message : t('common.unknown_error');
      const status = getAccountPoolCheckErrorStatus(lastError);

      try {
        await requestCodexModelForAccountPool(file, t, signal);
        return {
          status: 'success',
          message: '模型请求可用（额度接口不可访问）',
          quotaOk: false,
          realRequestOk: true,
          requestedModel: ACCOUNT_POOL_REAL_REQUEST_MODEL,
          statusCode: 200,
          realRequestError: status ? `额度请求 ${status}：${message}` : `额度请求失败：${message}`,
          checkedAt: Date.now(),
        };
      } catch (err: unknown) {
        if (isAbortError(err) || signal?.aborted || isRunCancelled(runId)) {
          throw err;
        }
        const realRequestStatus = getAccountPoolCheckErrorStatus(err);
        return {
          status: 'error',
          message: status ? `额度请求 ${status}：${message}` : `额度请求失败：${message}`,
          quotaOk: false,
          realRequestOk: false,
          realRequestError: formatRealRequestErrorMessage(err, t),
          realRequestStatusCode: realRequestStatus,
          requestedModel: ACCOUNT_POOL_REAL_REQUEST_MODEL,
          statusCode: status,
          checkedAt: Date.now(),
        };
      }
    };

    let cursor = 0;
    const worker = async () => {
      for (;;) {
        if (signal?.aborted || isRunCancelled(runId)) return;
        const index = cursor;
        cursor += 1;
        const file = checkTargets[index];
        if (!file) return;
        try {
          const result = await checkOne(file);
          if (signal?.aborted || isRunCancelled(runId)) return;
          const hash = hashByName.get(file.name);
          setCheckResult(runId, file.name, result, hash);
          queueRemoteCheckResult(file, result, hash);
        } catch (err: unknown) {
          if (isAbortError(err) || signal?.aborted || isRunCancelled(runId)) return;
          const result: AccountCheckResult = {
            status: 'error',
            message: err instanceof Error ? err.message : t('common.unknown_error'),
            statusCode: getAccountPoolCheckErrorStatus(err),
            checkedAt: Date.now(),
          };
          const hash = hashByName.get(file.name);
          setCheckResult(runId, file.name, result, hash);
          queueRemoteCheckResult(file, result, hash);
        }
      }
    };

    try {
      await Promise.all(
        Array.from({ length: Math.min(checkConcurrency, checkTargets.length) }, () => worker())
      );
      if (signal?.aborted || isRunCancelled(runId)) return;
      const persisted = await flushRemoteCheckResults();
      if (persisted) {
        await refreshPool(false);
      }
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
      if (signal?.aborted || isRunCancelled(runId)) return;
      const persisted = await flushRemoteCheckResults();
      if (persisted) {
        await refreshPool(false);
      }
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

  useEffect(() => {
    if (resumedPendingCheck || checking || loading || files.length === 0) return;
    const pendingNames = readPendingAccountPoolCheckNames();
    if (pendingNames.length === 0) {
      setResumedPendingCheck(true);
      return;
    }
    const pendingSet = new Set(pendingNames);
    const targets = files.filter((file) => pendingSet.has(file.name));
    if (targets.length === 0) return;
    setResumedPendingCheck(true);
    showNotification(
      t('account_pool.check_resumed', {
        count: targets.length,
        defaultValue: `已自动恢复检测任务：剩余 ${targets.length} 个`,
      }),
      'info'
    );
    const resumeTimer = window.setTimeout(() => {
      void detectAccounts(targets);
    }, 2500);
    return () => window.clearTimeout(resumeTimer);
  }, [checking, files, loading, resumedPendingCheck, showNotification, t]);

  const interruptCheck = () => {
    const summary = cancelCheck();
    if (!summary) return;
    showNotification(
      t('account_pool.check_cancelled', {
        done: summary.done,
        total: summary.total,
        defaultValue: `检测已中断：已完成 ${summary.done} / ${summary.total}`,
      }),
      'warning'
    );
  };

  const renderPoolCard = (file: AuthFileItem) => {
    const checked = selectedSet.has(file.name);
    const type = getFileType(file);
    const folder = getFileFolder(file);
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
    const quotaAccessLabel = checkResult?.quotaOk === true
      ? '额度接口：可访问'
      : checkResult?.quotaOk === false
        ? '额度接口：不可访问'
        : '';
    const realRequestLabel = checkResult?.realRequestOk === true
      ? `真实请求：${checkResult.requestedModel ?? ACCOUNT_POOL_REAL_REQUEST_MODEL} 可用`
      : checkResult?.realRequestOk === false
        ? (checkResult.realRequestStatusCode === 401
            ? `真实请求：${checkResult.requestedModel ?? ACCOUNT_POOL_REAL_REQUEST_MODEL} 401 未认证`
            : `真实请求：${checkResult.requestedModel ?? ACCOUNT_POOL_REAL_REQUEST_MODEL} 不可用`)
        : '';
    const accountStartedAt =
      parseDateValue(file.account_started_at ?? file.accountStartedAt) ??
      getRegistrationTime(file, fileContentCache, savedAtByName);
    const accountStoppedAt = getAccountLifetimeStoppedAt(file, checkResult);
    const accountInvalid = accountStoppedAt !== null;
    const showStatusMessage =
      Boolean(statusMessage) &&
      (!checkResult || (checkResult.status !== 'success' && checkResult.status !== 'loading'));
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
            <span className={styles.fileName} title={file.name}>{file.name}</span>
            <div className={styles.metaRow}>
              <span className={styles.typeBadge}>{type}</span>
              <span className={styles.folderBadge}>{folder}</span>
              {planLabel && <span className={styles.planBadge}>{planLabel}</span>}
              {modifiedLabel && <span className={styles.muted}>{modifiedLabel}</span>}
              <Button
                variant="secondary"
                size="sm"
                title="编辑账号成本和渠道来源"
                onClick={() => openAccountMetaEditor(file)}
              >
                编辑成本/来源
              </Button>
              <Button
                variant="secondary"
                size="sm"
                title={`请求 ${ACCOUNT_POOL_REAL_REQUEST_MODEL}，并按真实请求结果更新可用状态`}
                onClick={() => void detectAccounts([file])}
                loading={checking && checkResult?.status === 'loading'}
                disabled={checking}
              >
                请求大模型
              </Button>
            </div>
            <div className={styles.usageMetricRow}>
              <div className={styles.usageMetric}>
                <span className={styles.usageMetricLabel}>成本</span>
                <strong className={styles.usageMetricValue}>{formatAccountCostMetric(getAccountCostValue(file))}</strong>
              </div>
              <div className={styles.usageMetric}>
                <span className={styles.usageMetricLabel}>每刀成本</span>
                <strong className={styles.usageMetricValue}>{formatCostPerUSDMetric(getAccountCostValue(file), usageSummary?.total_usd)}</strong>
              </div>
              <div className={styles.usageMetric}>
                <span className={styles.usageMetricLabel}>渠道来源</span>
                <strong className={styles.usageMetricValue}>{getSourceChannelValue(file)}</strong>
              </div>
              <div className={styles.usageMetric}>
                <span className={styles.usageMetricLabel}>存活{accountInvalid ? '（已停止）' : ''}</span>
                <strong className={styles.usageMetricValue}>{formatAccountLifetime(accountStartedAt, accountStoppedAt, file.account_lifetime_seconds ?? file.accountLifetimeSeconds, file.account_lifetime_active_since ?? file.accountLifetimeActiveSince)}</strong>
              </div>
              <div className={styles.usageMetric}>
                <span className={styles.usageMetricLabel}>
                  {t('account_pool.usage_requests', { defaultValue: '请求' })}
                </span>
                <strong className={styles.usageMetricValue}>{formatUsageMetric(usageSummary?.requests)}</strong>
              </div>
              <div className={styles.usageMetric}>
                <span className={styles.usageMetricLabel}>
                  {t('account_pool.usage_successes', { defaultValue: '成功' })}
                </span>
                <strong className={styles.usageMetricValue}>{formatUsageMetric(usageSummary?.successes)}</strong>
              </div>
              <div className={styles.usageMetric}>
                <span className={styles.usageMetricLabel}>
                  {t('account_pool.usage_total_tokens', { defaultValue: 'Token' })}
                </span>
                <strong className={styles.usageMetricValue}>{formatUsageMetric(usageSummary?.total_tokens)}</strong>
              </div>
              <div className={styles.usageMetric}>
                <span className={styles.usageMetricLabel}>
                  {t('account_pool.usage_total_usd', { defaultValue: '刀数' })}
                </span>
                <strong className={styles.usageMetricValue}>{formatUSDMetric(usageSummary?.total_usd)}</strong>
              </div>
              <div className={styles.usageMetric}>
                <span className={styles.usageMetricLabel}>
                  {t('account_pool.usage_failures', { defaultValue: '失败' })}
                </span>
                <strong className={styles.usageMetricValue}>{formatUsageMetric(usageSummary?.failures)}</strong>
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
                {checkResult.status === 'loading' ? t('account_pool.checking') : checkResult.message}
              </span>
              {planLabel && <span className={styles.checkPlanPill}>{planLabel}</span>}
              {quotaAccessLabel && <span className={styles.checkPlanPill}>{quotaAccessLabel}</span>}
              {realRequestLabel && <span className={styles.checkPlanPill}>{realRequestLabel}</span>}
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
                    percent !== null && percent > 0 && percent <= LOW_ACCOUNT_POOL_QUOTA_PERCENT;
                  return (
                    <div
                      className={empty ? styles.quotaItemEmpty : styles.quotaItem}
                      key={`${quota.label}-${quota.reset}`}
                    >
                      <div className={styles.quotaItemTop}>
                        <span className={styles.quotaName}>{quota.label}</span>
                        <span className={empty ? styles.quotaEmptyValue : low ? styles.quotaLowValue : styles.quotaValue}>
                          {quota.remaining}
                        </span>
                      </div>
                      {percent !== null && (
                        <div className={styles.quotaTrack}>
                          <span
                            className={empty ? styles.quotaFillEmpty : low ? styles.quotaFillLow : styles.quotaFill}
                            style={{ width: `${percent}%` }}
                          />
                        </div>
                      )}
                      {quota.reset && (
                        <div className={styles.quotaReset}>{formatQuotaResetMeta(t, quota.reset)}</div>
                      )}
                    </div>
                  );
                })}
              </div>
            )}
          </div>
        )}
        {showStatusMessage && <div className={styles.statusLine}>{statusMessage}</div>}
      </div>
    );
  };

  return (
    <>
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
            <button
              type="button"
              className={`${getStatusCodePillClassName(code, styles)} ${
                quickStatusFilter === `code:${code}` ? styles.statPillActive : ''
              }`}
              key={code}
              title={`${code}：${getStatusCodeDescription(code)}`}
              onClick={() => applyStatusFilter(`code:${code}`)}
            >
              {code}
              <strong>{count}</strong>
            </button>
          ))}
          {displayedStatusCodeStats.errorLabels.map(([label, count]) => (
            <button
              type="button"
              className={`${styles.statPill} ${styles.statPillError} ${
                quickStatusFilter === 'error' ? styles.statPillActive : ''
              }`}
              key={`error-${label}`}
              title={`${label}：检测失败，未拿到明确 HTTP 状态码。`}
              onClick={() => applyStatusFilter('error')}
            >
              {label}
              <strong>{count}</strong>
            </button>
          ))}
          {displayedStatusCodeStats.unsupported > 0 && (
            <button
              type="button"
              className={`${styles.statPill} ${
                quickStatusFilter === 'unsupported' ? styles.statPillActive : ''
              }`}
              title="不支持：该认证文件类型暂未接入额度检测逻辑。"
              onClick={() => applyStatusFilter('unsupported')}
            >
              {t('account_pool.stat_unsupported', { defaultValue: '不支持' })}
              <strong>{displayedStatusCodeStats.unsupported}</strong>
            </button>
          )}
          {displayedStatusCodeStats.checking > 0 && (
            <button
              type="button"
              className={`${styles.statPill} ${
                quickStatusFilter === 'checking' ? styles.statPillActive : ''
              }`}
              title="检测中：该账号已经进入后台检测队列，正在等待或执行检测。"
              onClick={() => applyStatusFilter('checking')}
            >
              {t('account_pool.stat_checking', { defaultValue: '检测中' })}
              <strong>{displayedStatusCodeStats.checking}</strong>
            </button>
          )}
          {displayedStatusCodeStats.unchecked > 0 && (
            <button
              type="button"
              className={`${styles.statPill} ${
                quickStatusFilter === 'unchecked' ? styles.statPillActive : ''
              }`}
              title="未检测：该账号还没有执行过检测。"
              onClick={() => applyStatusFilter('unchecked')}
            >
              {t('account_pool.stat_unchecked', { defaultValue: '未检测' })}
              <strong>{displayedStatusCodeStats.unchecked}</strong>
            </button>
          )}
        </div>
        <div className={styles.headerActions}>
          <input
            ref={importInputRef}
            type="file"
            accept=".json,.zip,.tar,.gz,.tgz,.tar.gz,application/json,application/zip,application/gzip"
            multiple
            hidden
            onChange={handleImportAccountPoolFiles}
          />
          <input
            ref={importFolderInputRef}
            type="file"
            multiple
            {...({ webkitdirectory: '', directory: '' } as Record<string, string>)}
            hidden
            onChange={handleImportAccountPoolFiles}
          />
          <div className={styles.viewModeSwitch} aria-label="账号池显示模式">
            <button
              type="button"
              className={viewMode === 'list' ? styles.viewModeActive : ''}
              onClick={() => {
                setViewMode('list');
                setActiveFolder(null);
                setPage(1);
              }}
            >
              列表模式
            </button>
            <button
              type="button"
              className={viewMode === 'folder' ? styles.viewModeActive : ''}
              onClick={() => {
                setViewMode('folder');
                setActiveFolder(null);
                setPage(1);
              }}
            >
              文件夹模式
            </button>
          </div>
          <div className={styles.actionGroup}>
            <span className={styles.actionGroupLabel}>来源</span>
            <Button
              variant="secondary"
              size="sm"
              title="重新加载账号池"
              onClick={() => void refreshPool(true)}
              loading={loading}
            >
              刷新
            </Button>
            <Button
              variant="secondary"
              size="sm"
              title="导入 JSON、Sub2 文件、解压文件夹或压缩包，系统会自动识别类型"
              onClick={() => openImportMeta('files')}
              loading={importingPool}
              disabled={importingPool}
            >
              导入
            </Button>
            <Button
              variant="secondary"
              size="sm"
              title="导入已经解压出来的文件夹，系统会自动过滤并识别 Sub2/CPA JSON"
              onClick={() => openImportMeta('folder')}
              loading={importingPool}
              disabled={importingPool}
            >
              文件夹
            </Button>
          </div>
          <div className={styles.actionScope}>
            <span>当前范围</span>
            <strong>{currentActionCount}</strong>
            <em>{currentActionLabel}</em>
          </div>
          <div className={styles.actionGroup}>
            <span className={styles.actionGroupLabel}>处理</span>
            <Button
              variant="secondary"
              size="sm"
              onClick={() => void detectAccounts(currentActionFiles)}
              loading={checking && currentActionFiles.length > 0}
              disabled={checking || currentActionFiles.length === 0}
              title={`检测${currentActionDescription}`}
            >
              检测
            </Button>
            <Button
              variant="secondary"
              size="sm"
              onClick={() => void detectAccounts(files)}
              loading={checking && currentActionFiles.length === files.length}
              disabled={checking || files.length === 0}
              title="检测账号池中的全部账号"
            >
              检测全部
            </Button>
            {checking && (
              <Button variant="danger" size="sm" title="中断当前后台检测任务" onClick={interruptCheck}>
                中断
              </Button>
            )}
            <Button
              size="sm"
              onClick={() => void handleDownloadCurrent()}
              loading={downloading}
              disabled={currentActionFiles.length === 0 || downloading}
              title={`下载${currentActionDescription}`}
            >
              下载
            </Button>
          </div>
          <div className={styles.actionGroupDanger}>
            <span className={styles.actionGroupLabel}>写回</span>
            <Button
              variant="secondary"
              size="sm"
              onClick={handleAppendCurrent}
              loading={activeWriteAction === 'append-current'}
              disabled={Boolean(activeWriteAction) || currentActionFiles.length === 0}
              title={`追加${currentActionDescription}到认证文件`}
            >
              追加
            </Button>
            <Button
              variant="danger"
              size="sm"
              onClick={handleOverwriteCurrent}
              loading={activeWriteAction === 'overwrite-current'}
              disabled={Boolean(activeWriteAction) || currentActionFiles.length === 0}
              title={`用${currentActionDescription}覆盖认证文件`}
            >
              覆盖
            </Button>
            <Button
              variant="danger"
              size="sm"
              onClick={() => confirmDeletePoolEntries(selectedFiles)}
              loading={deletingPoolEntries}
              disabled={deletingPoolEntries || selectedFiles.length === 0}
              title="只删除手动选中的账号池账号，不影响认证文件"
            >
              删除 ({selectedFiles.length})
            </Button>
          </div>
        </div>
      </div>

      {error && <div className={styles.errorBox}>{error}</div>}

      {syncProgress && (
        <div className={styles.syncProgressPanel}>
          <div className={styles.syncProgressHeader}>
            <strong>{syncPhaseLabel}</strong>
            <span>
              {syncProgress.processed}/{syncProgress.total || 0}
              {syncProgress.total > 0 ? ` · ${syncProgressPercent}%` : ''}
            </span>
          </div>
          <div className={styles.syncProgressTrack}>
            <div
              className={styles.syncProgressBar}
              style={{ width: `${syncProgress.total > 0 ? syncProgressPercent : 8}%` }}
            />
          </div>
          <div className={styles.syncProgressStats}>
            <span>{t('account_pool.sync_added', { count: syncProgress.added, defaultValue: `新增 ${syncProgress.added}` })}</span>
            <span>{t('account_pool.sync_updated', { count: syncProgress.updated, defaultValue: `更新 ${syncProgress.updated}` })}</span>
            <span>{t('account_pool.sync_unchanged', { count: syncProgress.unchanged, defaultValue: `未变 ${syncProgress.unchanged}` })}</span>
            <span>{t('account_pool.refresh_retained', { count: syncProgress.skipped, defaultValue: `本地保留 ${syncProgress.skipped}` })}</span>
            <span>{t('account_pool.sync_failed', { count: syncProgress.failed, defaultValue: `失败 ${syncProgress.failed}` })}</span>
            <span>{t('account_pool.sync_deduped', { count: syncProgress.deduped, defaultValue: `去重 ${syncProgress.deduped}` })}</span>
          </div>
        </div>
      )}

      {importJob && !isAccountPoolImportDone(importJob) && (
        <div className={styles.syncProgressPanel}>
          <div className={styles.syncProgressHeader}>
            <strong>后台导入账号池</strong>
            <span>
              {importJob.done}/{importJob.total || 0}
              {importJob.total > 0 ? ` · ${Math.round((importJob.done / importJob.total) * 100)}%` : ''}
            </span>
          </div>
          <div className={styles.syncProgressTrack}>
            <div
              className={styles.syncProgressBar}
              style={{ width: `${importJob.total > 0 ? Math.round((importJob.done / importJob.total) * 100) : 8}%` }}
            />
          </div>
          <div className={styles.syncProgressStats}>
            <span>已导入 {importJob.imported}</span>
            <span>跳过 {importJob.skipped}</span>
            <span>失败 {importJob.failed}</span>
          </div>
        </div>
      )}

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
              {viewMode === 'list' && (
                <Select
                  className={styles.folderSelect}
                  fullWidth={false}
                  value={folderFilter}
                  options={folderOptions}
                  onChange={setFolderFilter}
                  ariaLabel="来源文件夹"
                />
              )}
              <Select
                className={styles.folderSelect}
                fullWidth={false}
                value={sourceModelFilter}
                options={sourceModelOptions}
                onChange={setSourceModelFilter}
                ariaLabel="来源模型"
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
                onChange={(value) => {
                  setCheckStatusFilter(value);
                  setQuickStatusFilter(DEFAULT_ACCOUNT_POOL_CHECK_STATUS_FILTER);
                }}
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
              <label className={styles.pageSizeControl}>
                <span>每页</span>
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
                <span>并发</span>
                <input
                  className={styles.checkConcurrencyInput}
                  type="number"
                  min={MIN_ACCOUNT_POOL_CHECK_CONCURRENCY}
                  max={MAX_ACCOUNT_POOL_CHECK_CONCURRENCY}
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
              disabled={visibleSelectionFiles.length === 0}
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
          </div>
        </div>

        {checking && (
          <div className={styles.checkProgress}>
            <span>
              {t('account_pool.check_progress', {
                done: checkSummary.done,
                total: checkSummary.total,
                success: checkSummary.success,
                failed: checkSummary.failed,
                unsupported: checkSummary.unsupported,
              })}
            </span>
            <Button variant="danger" size="sm" onClick={interruptCheck}>
              {t('account_pool.interrupt_check', { defaultValue: '中断检测' })}
            </Button>
          </div>
        )}

        {loading ? (
          <div className={styles.hint}>{t('common.loading')}</div>
        ) : displayedFiles.length === 0 && (viewMode !== 'folder' || activeFolder) ? (
          <EmptyState
            title={t('account_pool.empty_title')}
            description={t('account_pool.empty_desc')}
          />
        ) : viewMode === 'folder' && !activeFolder ? (
          <div className={styles.folderGroups}>
            {folderGroups.length === 0 ? (
              <EmptyState
                title={t('account_pool.empty_title')}
                description={t('account_pool.empty_desc')}
              />
            ) : folderPageGroups.map((group) => {
              const selectedCount = group.items.filter((file) => selectedSet.has(file.name)).length;
              const allSelected = group.items.length > 0 && selectedCount === group.items.length;
              const partiallySelected = selectedCount > 0 && !allSelected;
              const importTime = formatFolderImportTime(group.info);
              return (
                <section
                  className={`${styles.folderGroup} ${
                    selectedCount > 0 ? styles.folderGroupSelected : ''
                  }`}
                  key={group.folder}
                  role="button"
                  tabIndex={0}
                  onClick={() => {
                    setActiveFolder(group.folder);
                    setPage(1);
                  }}
                  onKeyDown={(event) => {
                    if (event.key === 'Enter' || event.key === ' ') {
                      event.preventDefault();
                      setActiveFolder(group.folder);
                      setPage(1);
                    }
                  }}
                >
                  <div className={styles.folderSelectControl} onClick={(event) => event.stopPropagation()}>
                    <SelectionCheckbox
                      checked={allSelected}
                      onChange={(checked) => toggleFolder(group.items, checked)}
                      ariaLabel={`选择 ${group.folder}`}
                      label={partiallySelected ? `已选 ${selectedCount}` : undefined}
                    />
                  </div>
                  <div className={styles.folderVisual}>
                    {importTime && (
                      <time
                        className={styles.folderImportTime}
                        dateTime={group.info?.created_at || group.info?.updated_at}
                        title={`导入时间：${formatUnixTimestamp(group.info?.created_at || group.info?.updated_at)}`}
                      >
                        {importTime}
                      </time>
                    )}
                    <div className={styles.folderIcon} aria-hidden="true">
                      <span className={styles.folderIconTab} />
                      <span className={styles.folderIconBody}>
                        <span className={styles.folderZipRail} />
                      </span>
                    </div>
                    <div className={styles.folderCountBadge}>{group.items.length} 个账号</div>
                  </div>
                  <div className={styles.folderHeader}>
                    <h3 title={group.folder}>{group.folder}</h3>
                    <p>
                      {group.info?.source_model || '未设置来源模型'}
                      {group.info?.source_info ? ` · ${group.info.source_info}` : ''}
                    </p>
                  </div>
                  <div
                    className={styles.folderStatusGrid}
                    onClick={(event) => event.stopPropagation()}
                  >
                    {group.stats.codes.slice(0, 4).map(([code, count]) => (
                      <span
                        className={`${styles.folderStatusPill} ${
                          code >= 200 && code < 300
                            ? styles.folderStatusSuccess
                            : code >= 400
                              ? styles.folderStatusError
                              : ''
                        }`}
                        key={code}
                        title={`${code}：${getStatusCodeDescription(code)}`}
                      >
                        {code}
                        <strong>{count}</strong>
                      </span>
                    ))}
                    <span className={`${styles.folderStatusPill} ${styles.folderTokenPill}`}>
                      总 Token
                      <strong>{formatUsageMetric(group.stats.totalTokens)}</strong>
                    </span>
                    <span className={`${styles.folderStatusPill} ${styles.folderTokenPill}`}>
                      总刀数
                      <strong>{formatUSDMetric(group.stats.totalUSD)}</strong>
                    </span>
                    <span className={`${styles.folderStatusPill} ${styles.folderTokenPill}`}>
                      每刀成本
                      <strong>{formatCostPerUSDMetric(group.stats.totalCost, group.stats.totalUSD)}</strong>
                    </span>
                    {group.stats.checking > 0 && (
                      <span className={`${styles.folderStatusPill} ${styles.folderStatusChecking}`}>
                        检测中
                        <strong>{group.stats.checking}</strong>
                      </span>
                    )}
                    {group.stats.unchecked > 0 && (
                      <span className={styles.folderStatusPill}>
                        未检测
                        <strong>{group.stats.unchecked}</strong>
                      </span>
                    )}
                    {group.stats.unsupported > 0 && (
                      <span className={styles.folderStatusPill}>
                        不支持
                        <strong>{group.stats.unsupported}</strong>
                      </span>
                    )}
                    {group.stats.errorLabels.slice(0, 2).map(([label, count]) => (
                      <span
                        className={`${styles.folderStatusPill} ${styles.folderStatusError}`}
                        key={`error-${label}`}
                        title={`${label}：检测失败，未拿到明确 HTTP 状态码。`}
                      >
                        {label}
                        <strong>{count}</strong>
                      </span>
                    ))}
                  </div>
                  <div className={styles.folderHeaderActions}>
                    <Button
                      variant="secondary"
                      size="sm"
                      onClick={(event) => {
                        event.stopPropagation();
                        editFolderSourceInfo(group.folder);
                      }}
                    >
                      设置来源
                    </Button>
                    <Button
                      variant="secondary"
                      size="sm"
                      onClick={(event) => {
                        event.stopPropagation();
                        setActiveFolder(group.folder);
                        setPage(1);
                      }}
                    >
                      进入
                    </Button>
                  </div>
                </section>
              );
            })}
          </div>
        ) : (
          <>
            {viewMode === 'folder' && activeFolder && (
              <div className={styles.folderBreadcrumb}>
                <Button variant="secondary" size="sm" onClick={() => setActiveFolder(null)}>
                  返回文件夹
                </Button>
                <span>{activeFolder}</span>
                <strong>{displayedFiles.length} 个账号</strong>
              </div>
            )}
            <div className={styles.poolGrid}>
              {pageItems.map(renderPoolCard)}
            </div>
          </>
        )}

        {!loading && paginatedItemCount > pageSize && (
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
                count: paginatedItemCount,
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
    <Modal
      open={importMetaOpen}
      onClose={closeImportMeta}
      title="导入账号池"
      width={520}
      closeDisabled={importingPool}
      footer={
        <>
          <Button variant="secondary" onClick={closeImportMeta} disabled={importingPool}>
            取消
          </Button>
          <Button onClick={startImportWithMetadata} loading={importingPool}>
            选择{importMetaKind === 'folder' ? '文件夹' : '文件'}
          </Button>
        </>
      }
    >
      <div className={styles.sourceEditor}>
        <div className={styles.sourceEditorFolder}>
          <span>批量写入</span>
          <strong>来源 / 成本</strong>
        </div>
        <label className={styles.sourceEditorField}>
          <span>渠道来源</span>
          <input
            value={importSourceChannel}
            onChange={(event) => setImportSourceChannel(event.target.value)}
            placeholder="例如 plus / 供应商A / 渠道1，留空则不写入"
            disabled={importingPool}
          />
        </label>
        <label className={styles.sourceEditorField}>
          <span>账号成本</span>
          <input
            type="number"
            min="0"
            step="0.001"
            inputMode="decimal"
            value={importAccountCost}
            onChange={(event) => setImportAccountCost(event.target.value)}
            placeholder="例如 2.5，留空则不写入"
            disabled={importingPool}
          />
        </label>
        <label className={styles.sourceEditorCheckbox}>
          <input
            type="checkbox"
            checked={importOverwriteMetadata}
            onChange={(event) => setImportOverwriteMetadata(event.target.checked)}
            disabled={importingPool}
          />
          <span>覆盖文件里已有的来源和成本</span>
        </label>
      </div>
    </Modal>
    <Modal
      open={sourceEditorOpen}
      onClose={closeSourceEditor}
      title="设置来源"
      width={560}
      closeDisabled={savingSourceInfo}
      footer={
        <>
          <Button variant="secondary" onClick={closeSourceEditor} disabled={savingSourceInfo}>
            取消
          </Button>
          <Button onClick={() => void saveFolderSourceInfo()} loading={savingSourceInfo}>
            保存
          </Button>
        </>
      }
    >
      <div className={styles.sourceEditor}>
        <div className={styles.sourceEditorFolder}>
          <span>文件夹</span>
          <strong title={sourceEditorFolder}>{sourceEditorFolder}</strong>
        </div>
        <label className={styles.sourceEditorField}>
          <span>来源模型</span>
          <input
            value={sourceEditorModel}
            onChange={(event) => setSourceEditorModel(event.target.value)}
            placeholder="例如 Claude、Codex、Gemini 或自定义模型来源"
            disabled={savingSourceInfo}
          />
        </label>
        <label className={styles.sourceEditorField}>
          <span>来源信息</span>
          <textarea
            value={sourceEditorInfo}
            onChange={(event) => setSourceEditorInfo(event.target.value)}
            placeholder="可填写批次、渠道、备注或其他来源说明"
            disabled={savingSourceInfo}
            rows={4}
          />
        </label>
      </div>
    </Modal>
    <Modal
      open={accountMetaEditorOpen}
      onClose={closeAccountMetaEditor}
      title="编辑账号信息"
      width={520}
      closeDisabled={savingAccountMeta}
      footer={
        <>
          <Button variant="secondary" onClick={closeAccountMetaEditor} disabled={savingAccountMeta}>
            取消
          </Button>
          <Button onClick={() => void saveAccountMetaEditor()} loading={savingAccountMeta}>
            保存
          </Button>
        </>
      }
    >
      <div className={styles.sourceEditor}>
        <div className={styles.sourceEditorFolder}>
          <span>账号</span>
          <strong title={accountMetaEditorName}>{accountMetaEditorName}</strong>
        </div>
        <label className={styles.sourceEditorField}>
          <span>账号成本</span>
          <input
            type="number"
            min="0"
            step="0.001"
            inputMode="decimal"
            value={accountMetaEditorCost}
            onChange={(event) => setAccountMetaEditorCost(event.target.value)}
            placeholder="例如 0.15"
            disabled={savingAccountMeta}
          />
        </label>
        <label className={styles.sourceEditorField}>
          <span>渠道来源</span>
          <input
            value={accountMetaEditorSourceChannel}
            onChange={(event) => setAccountMetaEditorSourceChannel(event.target.value)}
            placeholder="例如 plus / 供应商A / 渠道1"
            disabled={savingAccountMeta}
          />
        </label>
      </div>
    </Modal>
    <Modal
      open={configViewerOpen}
      onClose={closeConfigViewer}
      title="账号配置"
      width={820}
      footer={
        <Button variant="secondary" onClick={closeConfigViewer}>
          关闭
        </Button>
      }
    >
      <div className={styles.configViewer}>
        <div className={styles.configViewerName} title={configViewerName}>
          {configViewerName}
        </div>
        {configViewerLoading ? (
          <div className={styles.hint}>正在读取配置...</div>
        ) : configViewerError ? (
          <div className={styles.error}>{configViewerError}</div>
        ) : (
          <pre className={styles.configViewerContent}>{configViewerContent}</pre>
        )}
      </div>
    </Modal>
    </>
  );
}
