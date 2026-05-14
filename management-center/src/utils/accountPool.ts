import { apiClient } from '@/services/api/client';
import type { AuthFileItem, AuthFilesResponse } from '@/types/authFile';

export type AccountPoolRecord = {
  file: AuthFileItem;
  content?: string;
  hash: string;
  savedAt: number;
  sourceFingerprint?: string;
};

export const ACCOUNT_POOL_STORAGE_KEY = 'cli-proxy-account-pool';
export const ACCOUNT_POOL_UPDATED_EVENT = 'cli-proxy-account-pool-updated';
const ACCOUNT_POOL_DELETED_HASHES_STORAGE_KEY = 'cli-proxy-account-pool-deleted-hashes';
const ACCOUNT_POOL_SYNC_DEBOUNCE_MS = 400;
const ACCOUNT_POOL_SYNC_CONCURRENCY = 5;

let syncTimer: ReturnType<typeof setTimeout> | null = null;
let syncInFlight: Promise<AccountPoolRecord[]> | null = null;

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
    file: record.file,
    hash: record.hash,
    savedAt: record.savedAt,
    sourceFingerprint: record.sourceFingerprint,
  }));
  window.localStorage.setItem(ACCOUNT_POOL_STORAGE_KEY, JSON.stringify(compactRecords));
};

const readDeletedAccountPoolHashes = (): Set<string> => {
  if (typeof window === 'undefined') return new Set();
  try {
    const raw = window.localStorage.getItem(ACCOUNT_POOL_DELETED_HASHES_STORAGE_KEY);
    if (!raw) return new Set();
    const parsed = JSON.parse(raw) as unknown;
    if (!Array.isArray(parsed)) return new Set();
    return new Set(parsed.filter((value): value is string => typeof value === 'string' && Boolean(value.trim())));
  } catch {
    return new Set();
  }
};

const writeDeletedAccountPoolHashes = (hashes: Set<string>) => {
  if (typeof window === 'undefined') return;
  window.localStorage.setItem(ACCOUNT_POOL_DELETED_HASHES_STORAGE_KEY, JSON.stringify(Array.from(hashes)));
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

  const deletedHashes = readDeletedAccountPoolHashes();
  const nextRecords = uniqueAccountPoolRecords(readAccountPoolRecords()).filter((record) => {
    if (!nameSet.has(record.file.name)) return true;
    if (record.hash) {
      deletedHashes.add(record.hash);
    }
    return false;
  });

  writeDeletedAccountPoolHashes(deletedHashes);
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

const readAuthFileField = (file: AuthFileItem, key: string): unknown =>
  (file as Record<string, unknown>)[key];

const buildAuthFileFingerprint = (file: AuthFileItem): string => {
  const parts = [
    file.name,
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

export const syncAccountPoolFromAuthFiles = async (): Promise<AccountPoolRecord[]> => {
  if (syncInFlight) return syncInFlight;

  syncInFlight = (async () => {
    const deletedHashes = readDeletedAccountPoolHashes();
    const storedRecords = uniqueAccountPoolRecords(readAccountPoolRecords()).filter(
      (record) => !deletedHashes.has(record.hash)
    );
    const response = await apiClient.get<unknown>('/auth-files');
    const importedFiles = normalizeAuthFilesPayload(response).filter(
      (file) => !isRuntimeOnlyAuthPoolFile(file)
    );
    const recordsByName = new Map<string, AccountPoolRecord>();
    storedRecords.forEach((record) => {
      recordsByName.set(record.file.name, record);
    });

    await runWithConcurrency(
      importedFiles,
      ACCOUNT_POOL_SYNC_CONCURRENCY,
      async (file) => {
        const sourceFingerprint = buildAuthFileFingerprint(file);
        const existing = recordsByName.get(file.name);
        if (existing?.hash && existing.sourceFingerprint === sourceFingerprint) {
          recordsByName.set(file.name, {
            ...existing,
            file,
          });
          return;
        }

        try {
          const responseText = await apiClient.getRaw(
            `/auth-files/download?name=${encodeURIComponent(file.name)}`,
            { responseType: 'blob' }
          );
          const rawText = await (responseText.data as Blob).text();
          const hash = await hashText(normalizeJsonForDedupe(rawText));
          if (deletedHashes.has(hash)) {
            recordsByName.delete(file.name);
            return;
          }
          recordsByName.set(file.name, {
            file,
            hash,
            savedAt: Date.now(),
            sourceFingerprint,
          });
        } catch {
          // Keep the existing pool intact even when a source auth file can no longer be read.
        }
      }
    );

    const mergedRecords = uniqueAccountPoolRecords(Array.from(recordsByName.values()));
    writeAccountPoolRecords(mergedRecords);
    emitAccountPoolUpdated(mergedRecords);
    return mergedRecords;
  })();

  try {
    return await syncInFlight;
  } finally {
    syncInFlight = null;
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
