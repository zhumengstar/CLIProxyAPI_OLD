import { apiClient } from '@/services/api/client';
import type { AuthFileItem, AuthFilesResponse } from '@/types/authFile';
import { REQUEST_TIMEOUT_MS } from '@/utils/constants';
import { readZipTextFiles } from '@/utils/zip';

export type AccountPoolRecord = {
  file: AuthFileItem;
  content?: string;
  hash: string;
  savedAt: number;
  sourceFingerprint?: string;
};

export type AccountPoolSyncProgress = {
  phase: 'listing' | 'syncing' | 'saving' | 'done';
  total: number;
  processed: number;
  added: number;
  updated: number;
  unchanged: number;
  skipped: number;
  failed: number;
  deduped: number;
};

export const ACCOUNT_POOL_STORAGE_KEY = 'cli-proxy-account-pool';
export const ACCOUNT_POOL_UPDATED_EVENT = 'cli-proxy-account-pool-updated';
const ACCOUNT_POOL_REFRESH_DEBOUNCE_MS = 400;
const ACCOUNT_POOL_REFRESH_CONCURRENCY = 5;
const ACCOUNT_POOL_DYNAMIC_TIMEOUT_MAX_MS = 10 * 60 * 1000;
const ACCOUNT_POOL_LOCAL_CACHE_TTL_MS = 3 * 60 * 1000;

type AccountPoolStoragePayload = {
  cachedAt: number;
  records: AccountPoolRecord[];
};

const getAccountPoolDynamicTimeout = (count: number, perItemMs = 140): number => {
  const safeCount = Math.max(0, Math.ceil(Number.isFinite(count) ? count : 0));
  return Math.min(
    ACCOUNT_POOL_DYNAMIC_TIMEOUT_MAX_MS,
    Math.max(REQUEST_TIMEOUT_MS, REQUEST_TIMEOUT_MS + safeCount * perItemMs)
  );
};

const ACCOUNT_POOL_FILE_STORAGE_KEYS = [
  'id',
  'auth_id',
  'authId',
  'auth_index',
  'authIndex',
  'name',
  'type',
  'provider',
  'label',
  'status',
  'status_message',
  'statusMessage',
  'disabled',
  'unavailable',
  'runtime_only',
  'runtimeOnly',
  'source',
  'folder',
  'source_model',
  'sourceModel',
  'source_info',
  'sourceInfo',
  'size',
  'modified',
  'modtime',
  'updated_at',
  'updatedAt',
  'last_refresh',
  'lastRefresh',
  'created_at',
  'createdAt',
  'email',
  'service_email',
  'account',
  'account_type',
  'priority',
  'note',
  'content_hash',
  'contentHash',
  'check_status',
  'checkStatus',
  'check_message',
  'checkMessage',
  'check_plan',
  'checkPlan',
  'check_quota_lines',
  'checkQuotaLines',
  'check_quota_remaining_percent',
  'checkQuotaRemainingPercent',
  'check_status_code',
  'checkStatusCode',
  'check_checked_at',
  'checkCheckedAt',
  'check_content_hash',
  'checkContentHash',
  'usage_requests',
  'usage_successes',
  'usage_failures',
  'usage_input_tokens',
  'usage_output_tokens',
  'usage_cached_tokens',
  'usage_cache_read_tokens',
  'usage_cache_creation_tokens',
  'usage_total_tokens',
  'usage_last_used_at',
  'id_token',
] as const;

let refreshTimer: ReturnType<typeof setTimeout> | null = null;
let syncInFlight: Promise<AccountPoolRecord[]> | null = null;
let latestSyncProgress: AccountPoolSyncProgress | null = null;
const syncProgressListeners = new Set<(progress: AccountPoolSyncProgress) => void>();

export const isRuntimeOnlyAuthPoolFile = (file: AuthFileItem): boolean => {
  const value = file.runtimeOnly ?? file['runtime_only'];
  if (typeof value === 'boolean') return value;
  if (typeof value === 'string') return value.trim().toLowerCase() === 'true';
  return false;
};

export const normalizeAccountPoolJsonForHash = (rawText: string): string => {
  const normalize = (value: unknown): unknown => {
    if (Array.isArray(value)) return value.map(normalize);
    if (!value || typeof value !== 'object') return value;

    return Object.keys(value as Record<string, unknown>)
      .sort()
      .reduce<Record<string, unknown>>((result, key) => {
        result[key] = normalize((value as Record<string, unknown>)[key]);
        return result;
      }, {});
  };

  try {
    return JSON.stringify(normalize(JSON.parse(rawText)));
  } catch {
    return rawText.trim();
  }
};

const hashText = async (value: string): Promise<string> => {
  const bytes = new TextEncoder().encode(value);
  const digest = await crypto.subtle.digest('SHA-256', bytes);
  return Array.from(new Uint8Array(digest))
    .map((byte) => byte.toString(16).padStart(2, '0'))
    .join('');
};

const compactAuthFileForAccountPool = (file: AuthFileItem): AuthFileItem => {
  const compact: AuthFileItem = { name: file.name };
  ACCOUNT_POOL_FILE_STORAGE_KEYS.forEach((key) => {
    const value = (file as Record<string, unknown>)[key];
    if (value === undefined || value === null) return;
    if (typeof value === 'string' && !value.trim()) return;
    (compact as Record<string, unknown>)[key] = value;
  });
  return compact;
};

export const readAccountPoolRecords = (): AccountPoolRecord[] => {
  if (typeof window === 'undefined') return [];
  try {
    const raw = window.localStorage.getItem(ACCOUNT_POOL_STORAGE_KEY);
    if (!raw) return [];
    const parsed = JSON.parse(raw) as unknown;
    if (Array.isArray(parsed)) {
      window.localStorage.removeItem(ACCOUNT_POOL_STORAGE_KEY);
      return [];
    }
    if (!parsed || typeof parsed !== 'object') return [];
    const payload = parsed as Partial<AccountPoolStoragePayload>;
    if (
      typeof payload.cachedAt !== 'number' ||
      Date.now() - payload.cachedAt > ACCOUNT_POOL_LOCAL_CACHE_TTL_MS
    ) {
      window.localStorage.removeItem(ACCOUNT_POOL_STORAGE_KEY);
      return [];
    }
    if (!Array.isArray(payload.records)) return [];
    return payload.records.reduce<AccountPoolRecord[]>((records, item) => {
      if (!item || typeof item !== 'object') return records;
      const record = item as Partial<AccountPoolRecord>;
      if (!record.file || typeof record.file !== 'object') return records;
      if (typeof record.hash !== 'string' || !record.hash.trim()) return records;
      records.push({
        file: record.file,
        content: typeof record.content === 'string' ? record.content : undefined,
        hash: record.hash,
        savedAt: typeof record.savedAt === 'number' ? record.savedAt : 0,
        sourceFingerprint:
          typeof record.sourceFingerprint === 'string' ? record.sourceFingerprint : undefined,
      });
      return records;
    }, []);
  } catch {
    return [];
  }
};

export const writeAccountPoolRecords = (records: AccountPoolRecord[]) => {
  if (typeof window === 'undefined') return;
  const compactRecords = records.map((record) => ({
    file: compactAuthFileForAccountPool(record.file),
    hash: record.hash,
    savedAt: record.savedAt,
    sourceFingerprint: record.sourceFingerprint,
  }));
  const minimalRecords = records.map((record) => ({
    file: {
      name: record.file.name,
      type: record.file.type,
      provider: record.file.provider,
      auth_index: record.file['auth_index'] ?? record.file.authIndex ?? record.file.name,
      email: record.file.email,
      content_hash: record.file['content_hash'] ?? record.file.contentHash,
      check_status: record.file.check_status ?? record.file.checkStatus,
      check_message: record.file.check_message ?? record.file.checkMessage,
      check_plan: record.file.check_plan ?? record.file.checkPlan,
      check_quota_lines: record.file.check_quota_lines ?? record.file.checkQuotaLines,
      check_quota_remaining_percent:
        record.file.check_quota_remaining_percent ?? record.file.checkQuotaRemainingPercent,
      check_status_code: record.file.check_status_code ?? record.file.checkStatusCode,
      check_checked_at: record.file.check_checked_at ?? record.file.checkCheckedAt,
      check_content_hash: record.file.check_content_hash ?? record.file.checkContentHash,
      usage_requests: record.file.usage_requests,
      usage_successes: record.file.usage_successes,
      usage_failures: record.file.usage_failures,
      usage_input_tokens: record.file.usage_input_tokens,
      usage_output_tokens: record.file.usage_output_tokens,
      usage_cached_tokens: record.file.usage_cached_tokens,
      usage_cache_read_tokens: record.file.usage_cache_read_tokens,
      usage_cache_creation_tokens: record.file.usage_cache_creation_tokens,
      usage_total_tokens: record.file.usage_total_tokens,
      usage_last_used_at: record.file.usage_last_used_at,
    },
    hash: record.hash,
    savedAt: record.savedAt,
    sourceFingerprint: record.sourceFingerprint,
  }));
  const serialized = JSON.stringify({
    cachedAt: Date.now(),
    records: compactRecords,
  } satisfies AccountPoolStoragePayload);
  const minimalSerialized = JSON.stringify({
    cachedAt: Date.now(),
    records: minimalRecords,
  } satisfies AccountPoolStoragePayload);
  try {
    window.localStorage.setItem(ACCOUNT_POOL_STORAGE_KEY, serialized);
  } catch (err) {
    if (err instanceof DOMException && err.name === 'QuotaExceededError') {
      window.localStorage.removeItem(ACCOUNT_POOL_STORAGE_KEY);
      try {
        window.localStorage.setItem(ACCOUNT_POOL_STORAGE_KEY, serialized);
      } catch {
        try {
          window.localStorage.setItem(ACCOUNT_POOL_STORAGE_KEY, minimalSerialized);
        } catch {
          window.localStorage.removeItem(ACCOUNT_POOL_STORAGE_KEY);
        }
      }
      return;
    }
    throw err;
  }
};

export const uniqueAccountPoolRecords = (records: AccountPoolRecord[]): AccountPoolRecord[] => {
  const byName = new Map<string, AccountPoolRecord>();
  records.forEach((record) => {
    const key = record.file.name.trim();
    if (!key) return;
    const existing = byName.get(key);
    if (!existing || record.savedAt > existing.savedAt) {
      byName.set(key, record);
    }
  });
  return Array.from(byName.values()).sort((left, right) =>
    left.file.name.localeCompare(right.file.name)
  );
};

export const buildAccountPoolFileContentCache = (
  records: AccountPoolRecord[]
): Record<string, string> =>
  records.reduce<Record<string, string>>((cache, record) => {
    if (record.content) {
      cache[record.file.name] = record.content;
    }
    return cache;
  }, {});

const emitAccountPoolUpdated = (records: AccountPoolRecord[]) => {
  if (typeof window === 'undefined') return;
  window.dispatchEvent(
    new CustomEvent<AccountPoolRecord[]>(ACCOUNT_POOL_UPDATED_EVENT, {
      detail: records,
    })
  );
};

export const deleteAccountPoolRecordsByName = (names: string[]): AccountPoolRecord[] => {
  const nameSet = new Set(names.map((name) => name.trim()).filter(Boolean));
  if (nameSet.size === 0) return uniqueAccountPoolRecords(readAccountPoolRecords());

  const nextRecords = uniqueAccountPoolRecords(readAccountPoolRecords()).filter((record) => {
    if (!nameSet.has(record.file.name)) return true;
    return false;
  });

  writeAccountPoolRecords(nextRecords);
  emitAccountPoolUpdated(nextRecords);
  return nextRecords;
};

const runWithConcurrency = async <T,>(
  items: T[],
  concurrency: number,
  worker: (item: T) => Promise<void>
) => {
  let cursor = 0;
  const workers = Array.from({ length: Math.min(concurrency, items.length) }, async () => {
    for (;;) {
      const index = cursor;
      cursor += 1;
      const item = items[index];
      if (!item) return;
      await worker(item);
    }
  });
  await Promise.all(workers);
};

const normalizeAuthFilesPayload = (payload: unknown): AuthFileItem[] => {
  if (!payload || typeof payload !== 'object') return [];
  const files = (payload as Partial<AuthFilesResponse>).files;
  return Array.isArray(files) ? files : [];
};

const buildAccountPoolFilesFromArchive = async (): Promise<AuthFileItem[]> => {
  const response = await apiClient.getRaw('/account-pool/download', {
    responseType: 'blob',
    timeout: getAccountPoolDynamicTimeout(1000, 120),
  });
  const zipFiles = await readZipTextFiles(response.data as Blob);
  return Promise.all(
    zipFiles.map(async (file) => {
      let parsed: Record<string, unknown> = {};
      try {
        parsed = JSON.parse(file.text) as Record<string, unknown>;
      } catch {
        parsed = {};
      }
      const type = typeof parsed.type === 'string' ? parsed.type : '';
      const email = typeof parsed.email === 'string' ? parsed.email : '';
      return {
        name: file.name,
        id: file.name,
        auth_id: file.name,
        auth_index: file.name,
        type,
        provider: type,
        email,
        size: file.text.length,
        source: 'account_pool_archive',
        content_hash: await hashText(normalizeAccountPoolJsonForHash(file.text)),
      } as AuthFileItem;
    })
  );
};

const readAuthFileField = (file: AuthFileItem, key: string): unknown =>
  (file as Record<string, unknown>)[key];

const buildAuthFileFingerprint = (file: AuthFileItem): string => {
  const contentHash = readAuthFileContentHash(file);
  if (contentHash) return `${file.name}|${contentHash}`;

  const parts = [
    file.name,
    readAuthFileField(file, 'size'),
  ];
  return parts.map((part) => String(part ?? '')).join('|');
};

const readAuthFileContentHash = (file: AuthFileItem): string => {
  const value = readAuthFileField(file, 'content_hash') ?? readAuthFileField(file, 'contentHash');
  return typeof value === 'string' ? value.trim() : '';
};

const mergeAccountPoolFileMetadata = (
  existing: AuthFileItem | undefined,
  incoming: AuthFileItem
): AuthFileItem => {
  if (!existing) return incoming;

  const merged: AuthFileItem = {
    ...existing,
    ...incoming,
  };
  const preserveKeys = [
    'folder',
    'source_folder',
    'source_model',
    'sourceModel',
    'source_info',
    'sourceInfo',
  ];
  preserveKeys.forEach((key) => {
    const incomingValue = (incoming as Record<string, unknown>)[key];
    const existingValue = (existing as Record<string, unknown>)[key];
    if (
      (incomingValue === undefined ||
        incomingValue === null ||
        (typeof incomingValue === 'string' && !incomingValue.trim())) &&
      existingValue !== undefined &&
      existingValue !== null &&
      (typeof existingValue !== 'string' || existingValue.trim())
    ) {
      (merged as Record<string, unknown>)[key] = existingValue;
    }
  });
  return merged;
};

export const refreshAccountPoolFromServer = async (
  concurrency = ACCOUNT_POOL_REFRESH_CONCURRENCY,
  onProgress?: (progress: AccountPoolSyncProgress) => void
): Promise<AccountPoolRecord[]> => {
  if (onProgress) {
    syncProgressListeners.add(onProgress);
    if (latestSyncProgress) {
      onProgress({ ...latestSyncProgress });
    }
  }
  if (syncInFlight) {
    try {
      return await syncInFlight;
    } finally {
      if (onProgress) {
        syncProgressListeners.delete(onProgress);
      }
    }
  }

  syncInFlight = (async () => {
    const syncConcurrency = Math.max(1, Math.floor(concurrency));
    const progress: AccountPoolSyncProgress = {
      phase: 'listing',
      total: 0,
      processed: 0,
      added: 0,
      updated: 0,
      unchanged: 0,
      skipped: 0,
      failed: 0,
      deduped: 0,
    };
    let lastProgressAt = 0;
    const reportProgress = (force = false) => {
      if (syncProgressListeners.size === 0) return;
      const now = Date.now();
      if (!force && now - lastProgressAt < 120 && progress.processed < progress.total) return;
      lastProgressAt = now;
      latestSyncProgress = { ...progress };
      syncProgressListeners.forEach((listener) => listener({ ...progress }));
    };

    reportProgress(true);
    const storedRecords = uniqueAccountPoolRecords(readAccountPoolRecords());
    const listConfig = {
      params: { include_hash: true },
      timeout: getAccountPoolDynamicTimeout(storedRecords.length || 1000, 120),
    };
    let response: unknown;
    try {
      response = await apiClient.get<unknown>('/account-pool/list', listConfig);
    } catch (err) {
      if (!err || typeof err !== 'object' || (err as { status?: number }).status !== 404) {
        throw err;
      }
      try {
        response = await apiClient.get<unknown>('/account-pool', listConfig);
      } catch (fallbackErr) {
        if (
          !fallbackErr ||
          typeof fallbackErr !== 'object' ||
          (fallbackErr as { status?: number }).status !== 404
        ) {
          throw fallbackErr;
        }
        response = { files: await buildAccountPoolFilesFromArchive() };
      }
    }
    const importedFiles = normalizeAuthFilesPayload(response).filter(
      (file) => !isRuntimeOnlyAuthPoolFile(file)
    );
    progress.phase = 'syncing';
    progress.total = importedFiles.length;
    reportProgress(true);
    const recordsByName = new Map<string, AccountPoolRecord>();
    const refreshedNames = new Set<string>();
    const storedByName = new Map(storedRecords.map((record) => [record.file.name, record]));

    await runWithConcurrency(
      importedFiles,
      syncConcurrency,
      async (file) => {
        refreshedNames.add(file.name);
        const sourceFingerprint = buildAuthFileFingerprint(file);
        const existing = storedByName.get(file.name);
        const serverContentHash = readAuthFileContentHash(file);
        if (
          existing?.hash &&
          (existing.sourceFingerprint === sourceFingerprint ||
            (serverContentHash && existing.hash === serverContentHash))
        ) {
          recordsByName.set(file.name, {
            ...existing,
            file: mergeAccountPoolFileMetadata(existing.file, file),
            sourceFingerprint,
          });
          progress.unchanged += 1;
          progress.processed += 1;
          reportProgress();
          return;
        }

        if (serverContentHash) {
          recordsByName.set(file.name, {
            file: mergeAccountPoolFileMetadata(existing?.file, file),
            hash: serverContentHash,
            savedAt: existing?.savedAt || Date.now(),
            sourceFingerprint,
          });
          if (existing) {
            progress.updated += 1;
          } else {
            progress.added += 1;
          }
          progress.processed += 1;
          reportProgress();
          return;
        }

        try {
          const responseText = await apiClient.getRaw(
            `/account-pool/download-entry?name=${encodeURIComponent(file.name)}`,
            { responseType: 'blob', timeout: getAccountPoolDynamicTimeout(importedFiles.length, 80) }
          );
          const rawText = await (responseText.data as Blob).text();
          const hash = await hashText(normalizeAccountPoolJsonForHash(rawText));
          recordsByName.set(file.name, {
            file,
            hash,
            savedAt: Date.now(),
            sourceFingerprint,
          });
          if (existing) {
            progress.updated += 1;
          } else {
            progress.added += 1;
          }
        } catch {
          if (existing) {
            recordsByName.set(file.name, {
              ...existing,
              file: mergeAccountPoolFileMetadata(existing.file, file),
              sourceFingerprint,
            });
          }
          // Keep the existing pool intact even when a source auth file can no longer be read.
          progress.failed += 1;
        }
        progress.processed += 1;
        reportProgress();
      }
    );

    progress.phase = 'saving';
    reportProgress(true);
    const retainedStoredRecords = storedRecords.filter((record) => !refreshedNames.has(record.file.name));
    const mergeCandidates = [
      ...Array.from(recordsByName.values()),
      ...retainedStoredRecords,
    ];
    const mergedRecords = uniqueAccountPoolRecords(mergeCandidates);
    progress.skipped = retainedStoredRecords.length;
    progress.deduped = Math.max(0, mergeCandidates.length - mergedRecords.length);
    writeAccountPoolRecords(mergedRecords);
    emitAccountPoolUpdated(mergedRecords);
    progress.phase = 'done';
    progress.total = mergedRecords.length;
    progress.processed = mergedRecords.length;
    reportProgress(true);
    latestSyncProgress = null;
    return mergedRecords;
  })();

  try {
    return await syncInFlight;
  } finally {
    syncInFlight = null;
    if (onProgress) {
      syncProgressListeners.delete(onProgress);
    }
  }
};

export const scheduleAccountPoolRefresh = () => {
  if (typeof window === 'undefined') return;
  if (refreshTimer) {
    clearTimeout(refreshTimer);
  }
  refreshTimer = setTimeout(() => {
    refreshTimer = null;
    void refreshAccountPoolFromServer().catch(() => {
      // Background refresh is best-effort.
    });
  }, ACCOUNT_POOL_REFRESH_DEBOUNCE_MS);
};
