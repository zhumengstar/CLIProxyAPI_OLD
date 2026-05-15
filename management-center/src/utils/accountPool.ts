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
const ACCOUNT_POOL_SYNC_DEBOUNCE_MS = 400;
const ACCOUNT_POOL_SYNC_CONCURRENCY = 5;
const ACCOUNT_POOL_DYNAMIC_TIMEOUT_MAX_MS = 10 * 60 * 1000;

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
  'id_token',
] as const;

let syncTimer: ReturnType<typeof setTimeout> | null = null;
let syncInFlight: Promise<AccountPoolRecord[]> | null = null;
let latestSyncProgress: AccountPoolSyncProgress | null = null;
const syncProgressListeners = new Set<(progress: AccountPoolSyncProgress) => void>();

export const isRuntimeOnlyAuthPoolFile = (file: AuthFileItem): boolean => {
  const value = file.runtimeOnly ?? file['runtime_only'];
  if (typeof value === 'boolean') return value;
  if (typeof value === 'string') return value.trim().toLowerCase() === 'true';
  return false;
};

const normalizeJsonForDedupe = (rawText: string): string => {
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
    if (!Array.isArray(parsed)) return [];
    return parsed.reduce<AccountPoolRecord[]>((records, item) => {
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
    },
    hash: record.hash,
    savedAt: record.savedAt,
    sourceFingerprint: record.sourceFingerprint,
  }));
  const serialized = JSON.stringify(compactRecords);
  try {
    window.localStorage.setItem(ACCOUNT_POOL_STORAGE_KEY, serialized);
  } catch (err) {
    if (err instanceof DOMException && err.name === 'QuotaExceededError') {
      window.localStorage.removeItem(ACCOUNT_POOL_STORAGE_KEY);
      try {
        window.localStorage.setItem(ACCOUNT_POOL_STORAGE_KEY, serialized);
      } catch {
        try {
          window.localStorage.setItem(ACCOUNT_POOL_STORAGE_KEY, JSON.stringify(minimalRecords));
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
  const byHash = new Map<string, AccountPoolRecord>();
  records.forEach((record) => {
    const existing = byHash.get(record.hash);
    if (!existing || record.savedAt > existing.savedAt) {
      byHash.set(record.hash, record);
    }
  });
  return Array.from(byHash.values()).sort((left, right) =>
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
        content_hash: await hashText(normalizeJsonForDedupe(file.text)),
      } as AuthFileItem;
    })
  );
};

const readAuthFileField = (file: AuthFileItem, key: string): unknown =>
  (file as Record<string, unknown>)[key];

const buildAuthFileFingerprint = (file: AuthFileItem): string => {
  const parts = [
    file.name,
    readAuthFileField(file, 'content_hash'),
    readAuthFileField(file, 'contentHash'),
    readAuthFileField(file, 'size'),
    readAuthFileField(file, 'modified'),
    readAuthFileField(file, 'modtime'),
    readAuthFileField(file, 'updated_at'),
    readAuthFileField(file, 'last_refresh'),
    readAuthFileField(file, 'disabled'),
    readAuthFileField(file, 'status'),
  ];
  return parts.map((part) => String(part ?? '')).join('|');
};

const readAuthFileContentHash = (file: AuthFileItem): string => {
  const value = readAuthFileField(file, 'content_hash') ?? readAuthFileField(file, 'contentHash');
  return typeof value === 'string' ? value.trim() : '';
};

export const syncAccountPoolFromAuthFiles = async (
  concurrency = ACCOUNT_POOL_SYNC_CONCURRENCY,
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
    storedRecords.forEach((record) => {
      recordsByName.set(record.file.name, record);
    });

    await runWithConcurrency(
      importedFiles,
      syncConcurrency,
      async (file) => {
        const sourceFingerprint = buildAuthFileFingerprint(file);
        const existing = recordsByName.get(file.name);
        if (existing?.hash && existing.sourceFingerprint === sourceFingerprint) {
          recordsByName.set(file.name, {
            ...existing,
            file,
          });
          progress.unchanged += 1;
          progress.processed += 1;
          reportProgress();
          return;
        }

        const serverContentHash = readAuthFileContentHash(file);
        if (serverContentHash) {
          recordsByName.set(file.name, {
            file,
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
          const hash = await hashText(normalizeJsonForDedupe(rawText));
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
          // Keep the existing pool intact even when a source auth file can no longer be read.
          progress.failed += 1;
        }
        progress.processed += 1;
        reportProgress();
      }
    );

    progress.phase = 'saving';
    reportProgress(true);
    const mergedRecords = uniqueAccountPoolRecords(Array.from(recordsByName.values()));
    progress.deduped = Math.max(0, importedFiles.length - mergedRecords.length);
    writeAccountPoolRecords(mergedRecords);
    emitAccountPoolUpdated(mergedRecords);
    progress.phase = 'done';
    progress.processed = progress.total;
    reportProgress(true);
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

export const scheduleAccountPoolSync = () => {
  if (typeof window === 'undefined') return;
  if (syncTimer) {
    clearTimeout(syncTimer);
  }
  syncTimer = setTimeout(() => {
    syncTimer = null;
    void syncAccountPoolFromAuthFiles().catch(() => {
      // Background sync is best-effort.
    });
  }, ACCOUNT_POOL_SYNC_DEBOUNCE_MS);
};
