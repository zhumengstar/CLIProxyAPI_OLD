/**
 * 认证文件与 OAuth 排除模型相关 API
 */

import { apiClient } from './client';
import { computeApiUrl, detectApiBaseFromLocation } from '@/utils/connection';
import { REQUEST_TIMEOUT_MS } from '@/utils/constants';
import type { AuthFilesResponse } from '@/types/authFile';
import type { OAuthModelAliasEntry } from '@/types';
import { parseTimestampMs } from '@/utils/timestamp';
import { readZipTextFiles } from '@/utils/zip';
import { normalizeAccountPoolJsonForHash } from '@/utils/accountPool';

type StatusError = { status?: number };
type AuthFileStatusResponse = { status: string; disabled: boolean };
type AuthFileEntry = AuthFilesResponse['files'][number];
export type AuthFileFieldsPatch = {
  prefix?: string;
  proxy_url?: string;
  headers?: Record<string, string>;
  priority?: number;
  note?: string;
  account_cost?: number;
  account_started_at?: string;
  account_stopped_at?: string;
  source_channel?: string;
};
type AuthFileBatchFailure = { name: string; error: string };
type AuthFileBatchUploadResponse = {
  status?: string;
  uploaded?: number;
  files?: unknown;
  failed?: unknown;
  job?: unknown;
};
type AuthFileBatchDeleteResponse = {
  status?: string;
  deleted?: number;
  files?: unknown;
  failed?: unknown;
};
export type AccountPoolRepairResult = {
  repaired: boolean;
  convertedSub2: number;
  inferredCodex: number;
  llmRepaired: number;
  llmFailed: number;
};
export type AccountPoolCheckResultPatch = {
  name: string;
  content_hash?: string;
  result: {
    status: 'idle' | 'success' | 'error' | 'unsupported';
    message?: string;
    plan?: string;
    quotaLines?: string[];
    quotaRemainingPercent?: number;
    quotaOk?: boolean;
    realRequestOk?: boolean;
    realRequestError?: string;
    realRequestStatusCode?: number;
    requestedModel?: string;
    statusCode?: number;
    checkedAt?: number;
  };
};
export type AccountPoolCheckResultsPatchResponse = {
  status: string;
  updated: number;
  skipped: number;
  missing?: string[];
  auto_appended?: number;
  auto_append_skipped?: number;
};
type AuthFileBatchUploadResult = {
  status: string;
  uploaded: number;
  files: string[];
  failed: AuthFileBatchFailure[];
  job?: AccountPoolImportJob;
};
type AccountPoolUploadOptions = {
  sourceChannel?: string;
  accountCost?: string;
  overwriteMetadata?: boolean;
};
type AuthFileBatchDeleteResult = {
  status: string;
  deleted: number;
  files: string[];
  failed: AuthFileBatchFailure[];
};
export type AccountPoolUsageRecord = {
  id: string;
  requested_at: string;
  request_id?: string;
  request_path?: string;
  session_id?: string;
  newapi_user_id?: string;
  username?: string;
  provider?: string;
  model?: string;
  alias?: string;
  service_email?: string;
  auth_id?: string;
  auth_index?: string;
  auth_type?: string;
  success: boolean;
  status_code?: number;
  latency_ms?: number;
  input_tokens?: number;
  output_tokens?: number;
  cached_tokens?: number;
  cache_read_tokens?: number;
  cache_creation_tokens?: number;
  total_tokens?: number;
  request_params?: string;
};
export type AccountPoolUsageSummary = {
  key: string;
  service_email?: string;
  auth_id?: string;
  auth_index?: string;
  auth_type?: string;
  provider?: string;
  model?: string;
  alias?: string;
  requests: number;
  successes?: number;
  failures?: number;
  input_tokens?: number;
  output_tokens?: number;
  cached_tokens?: number;
  cache_read_tokens?: number;
  cache_creation_tokens?: number;
  total_tokens?: number;
  total_usd?: number;
  last_used_at?: string;
};
export type AccountPoolUsageTotals = {
  requests: number;
  successes?: number;
  failures?: number;
  input_tokens?: number;
  output_tokens?: number;
  cached_tokens?: number;
  cache_read_tokens?: number;
  cache_creation_tokens?: number;
  total_tokens?: number;
};
export type AccountPoolUsageResponse = {
  records: AccountPoolUsageRecord[];
  summaries: AccountPoolUsageSummary[];
  totals?: AccountPoolUsageTotals;
  total?: number;
  limit?: number;
  offset?: number;
};
export type AccountPoolImportJobStatus = 'pending' | 'running' | 'done' | 'failed';
export type AccountPoolImportJob = {
  id: string;
  status: AccountPoolImportJobStatus;
  total: number;
  done: number;
  imported: number;
  failed: number;
  skipped: number;
  files: string[];
  failures: AuthFileBatchFailure[];
  error?: string;
  created_at: string;
  updated_at: string;
};

export const AUTH_FILE_INVALID_JSON_OBJECT_ERROR = 'AUTH_FILE_INVALID_JSON_OBJECT';

const getStatusCode = (err: unknown): number | undefined => {
  if (!err || typeof err !== 'object') return undefined;
  if ('status' in err) return (err as StatusError).status;
  return undefined;
};

const getLocalManagementUrl = (path: string): string | null => {
  if (typeof window === 'undefined') return null;
  const apiUrl = computeApiUrl(detectApiBaseFromLocation());
  return apiUrl ? `${apiUrl}${path}` : null;
};

const normalizeRequestedAuthFileNames = (names: string[]): string[] => {
  const seen = new Set<string>();
  const normalized: string[] = [];

  names.forEach((name) => {
    const trimmed = String(name ?? '').trim();
    if (!trimmed || seen.has(trimmed)) return;
    seen.add(trimmed);
    normalized.push(trimmed);
  });

  return normalized;
};

const normalizeBatchFileNames = (value: unknown): string[] => {
  if (!Array.isArray(value)) return [];
  return normalizeRequestedAuthFileNames(value.map((item) => String(item ?? '')));
};

const normalizeBatchFailures = (value: unknown): AuthFileBatchFailure[] => {
  if (!Array.isArray(value)) return [];

  return value.reduce<AuthFileBatchFailure[]>((result, item) => {
    if (!item || typeof item !== 'object') return result;
    const entry = item as Record<string, unknown>;
    const name = String(entry.name ?? '').trim();
    const error =
      typeof entry.error === 'string'
        ? entry.error.trim()
        : typeof entry.message === 'string'
          ? entry.message.trim()
          : '';

    if (!name && !error) return result;
    result.push({ name, error: error || 'Unknown error' });
    return result;
  }, []);
};

const deriveSuccessfulFileNames = (requestedNames: string[], failed: AuthFileBatchFailure[]): string[] => {
  const failedNames = new Set(
    failed
      .map((entry) => entry.name.trim())
      .filter(Boolean)
  );

  if (failedNames.size === 0) {
    return [...requestedNames];
  }

  return requestedNames.filter((name) => !failedNames.has(name));
};

const normalizeBatchUploadResponse = (
  payload: AuthFileBatchUploadResponse | undefined,
  requestedNames: string[]
): AuthFileBatchUploadResult => {
  const failed = normalizeBatchFailures(payload?.failed);
  const uploadedFilesFromPayload = normalizeBatchFileNames(payload?.files);
  const uploaded =
    typeof payload?.uploaded === 'number'
      ? payload.uploaded
      : uploadedFilesFromPayload.length > 0
        ? uploadedFilesFromPayload.length
        : requestedNames.length === 1 && failed.length === 0
          ? 1
          : 0;

  let uploadedFiles = uploadedFilesFromPayload;
  if (uploadedFiles.length === 0 && uploaded > 0) {
    if (failed.length === 0 && uploaded === requestedNames.length) {
      uploadedFiles = [...requestedNames];
    } else {
      const derivedNames = deriveSuccessfulFileNames(requestedNames, failed);
      if (derivedNames.length === uploaded) {
        uploadedFiles = derivedNames;
      }
    }
  }

  return {
    status: typeof payload?.status === 'string' ? payload.status : failed.length > 0 ? 'partial' : 'ok',
    uploaded,
    files: uploadedFiles,
    failed,
  };
};

const normalizeBatchDeleteResponse = (
  payload: AuthFileBatchDeleteResponse | undefined,
  requestedNames: string[]
): AuthFileBatchDeleteResult => {
  const failed = normalizeBatchFailures(payload?.failed);
  const deletedFilesFromPayload = normalizeBatchFileNames(payload?.files);
  const deleted =
    typeof payload?.deleted === 'number'
      ? payload.deleted
      : deletedFilesFromPayload.length > 0
        ? deletedFilesFromPayload.length
        : requestedNames.length === 1 && failed.length === 0
          ? 1
          : 0;

  let deletedFiles = deletedFilesFromPayload;
  if (deletedFiles.length === 0 && deleted > 0) {
    if (failed.length === 0 && deleted === requestedNames.length) {
      deletedFiles = [...requestedNames];
    } else {
      const derivedNames = deriveSuccessfulFileNames(requestedNames, failed);
      if (derivedNames.length === deleted) {
        deletedFiles = derivedNames;
      }
    }
  }

  return {
    status: typeof payload?.status === 'string' ? payload.status : failed.length > 0 ? 'partial' : 'ok',
    deleted,
    files: deletedFiles,
    failed,
  };
};

const readTextField = (entry: AuthFileEntry, key: string): string => {
  const value = entry[key];
  return typeof value === 'string' ? value.trim() : '';
};

const readDateField = (entry: AuthFileEntry): number => {
  const candidates = [entry['modtime'], entry.modified, entry['updated_at'], entry['last_refresh']];

  for (const value of candidates) {
    if (typeof value === 'number' && Number.isFinite(value)) {
      return value < 1e12 ? value * 1000 : value;
    }
    if (typeof value === 'string') {
      const trimmed = value.trim();
      if (!trimmed) continue;
      const asNumber = Number(trimmed);
      if (Number.isFinite(asNumber)) {
        return asNumber < 1e12 ? asNumber * 1000 : asNumber;
      }
      const parsed = parseTimestampMs(trimmed);
      if (!Number.isNaN(parsed)) {
        return parsed;
      }
    }
  }

  return 0;
};

const isRuntimeOnlyEntry = (entry: AuthFileEntry): boolean => {
  const value = entry['runtime_only'] ?? entry.runtimeOnly;
  if (typeof value === 'boolean') return value;
  if (typeof value === 'string') return value.trim().toLowerCase() === 'true';
  return false;
};

const hasMeaningfulValue = (value: unknown): boolean => {
  if (value == null) return false;
  if (typeof value === 'string') return value.trim().length > 0;
  if (Array.isArray(value)) return value.length > 0;
  return true;
};

const countMeaningfulFields = (entry: AuthFileEntry): number =>
  Object.values(entry).reduce<number>(
    (count, value) => count + (hasMeaningfulValue(value) ? 1 : 0),
    0
  );

const authFilePriorityScore = (entry: AuthFileEntry): number => {
  let score = 0;
  if (readTextField(entry, 'source').toLowerCase() === 'file') score += 32;
  if (readTextField(entry, 'path')) score += 16;
  if (!isRuntimeOnlyEntry(entry)) score += 8;
  if (entry.disabled !== true) score += 4;
  if (readDateField(entry) > 0) score += 2;
  return score;
};

const compareAuthFileEntries = (left: AuthFileEntry, right: AuthFileEntry): number => {
  const scoreDiff = authFilePriorityScore(right) - authFilePriorityScore(left);
  if (scoreDiff !== 0) return scoreDiff;

  const dateDiff = readDateField(right) - readDateField(left);
  if (dateDiff !== 0) return dateDiff;

  const fieldDiff = countMeaningfulFields(right) - countMeaningfulFields(left);
  if (fieldDiff !== 0) return fieldDiff;

  return 0;
};

const mergeAuthFileEntries = (entries: AuthFileEntry[]): AuthFileEntry => {
  const [primary, ...rest] = [...entries].sort(compareAuthFileEntries);
  const merged: AuthFileEntry = { ...primary };

  rest.forEach((entry) => {
    Object.entries(entry).forEach(([key, value]) => {
      if (!hasMeaningfulValue(merged[key]) && hasMeaningfulValue(value)) {
        merged[key] = value;
      }
    });
  });

  return merged;
};

const dedupeAuthFilesResponse = (payload: AuthFilesResponse): AuthFilesResponse => {
  const files = Array.isArray(payload?.files) ? payload.files : [];
  const grouped = new Map<string, AuthFileEntry[]>();

  files.forEach((entry) => {
    const name = readTextField(entry, 'name');
    const key = name || JSON.stringify(entry);
    const bucket = grouped.get(key);
    if (bucket) {
      bucket.push(entry);
      return;
    }
    grouped.set(key, [entry]);
  });

  const normalizedFiles = Array.from(grouped.values()).map(mergeAuthFileEntries);
  normalizedFiles.sort((left, right) =>
    readTextField(left, 'name').localeCompare(readTextField(right, 'name'), undefined, {
      sensitivity: 'accent',
    })
  );

  return {
    ...payload,
    files: normalizedFiles,
    total: normalizedFiles.length,
  };
};

const parseAuthFileJsonObject = (rawText: string): Record<string, unknown> => {
  const trimmed = rawText.trim();

  let parsed: unknown;
  try {
    parsed = JSON.parse(trimmed) as unknown;
  } catch {
    throw new Error(AUTH_FILE_INVALID_JSON_OBJECT_ERROR);
  }

  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
    throw new Error(AUTH_FILE_INVALID_JSON_OBJECT_ERROR);
  }

  return { ...(parsed as Record<string, unknown>) };
};

const saveAuthFileText = async (name: string, text: string) => {
  const file = new File([text], name, { type: 'application/json' });
  await authFilesApi.upload(file);
};

const normalizeAccountPoolImportJob = (value: unknown): AccountPoolImportJob => {
  const record = value && typeof value === 'object' ? (value as Record<string, unknown>) : {};
  return {
    id: String(record.id ?? ''),
    status: String(record.status ?? 'pending') as AccountPoolImportJobStatus,
    total: Number(record.total ?? 0),
    done: Number(record.done ?? 0),
    imported: Number(record.imported ?? 0),
    failed: Number(record.failed ?? 0),
    skipped: Number(record.skipped ?? 0),
    files: normalizeBatchFileNames(record.files),
    failures: normalizeBatchFailures(record.failures),
    error: typeof record.error === 'string' ? record.error : undefined,
    created_at: String(record.created_at ?? ''),
    updated_at: String(record.updated_at ?? ''),
  };
};

const hashTextForAccountPool = async (value: string): Promise<string> => {
  const bytes = new TextEncoder().encode(normalizeAccountPoolJsonForHash(value));
  const digest = await crypto.subtle.digest('SHA-256', bytes);
  return Array.from(new Uint8Array(digest))
    .map((byte) => byte.toString(16).padStart(2, '0'))
    .join('');
};

const buildAccountPoolResponseFromArchive = async (blob: Blob): Promise<AuthFilesResponse> => {
  const zipFiles = await readZipTextFiles(blob);
  const files = await Promise.all(
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
        content_hash: await hashTextForAccountPool(file.text),
      } as AuthFileEntry;
    })
  );
  return { files, total: files.length } as AuthFilesResponse;
};

const buildAuthFilesFormData = (files: File[], options?: AccountPoolUploadOptions): FormData => {
  const formData = new FormData();
  files.forEach((file) => {
    const relativePath =
      typeof (file as File & { webkitRelativePath?: string }).webkitRelativePath === 'string'
        ? (file as File & { webkitRelativePath?: string }).webkitRelativePath
        : '';
    formData.append('file', file, relativePath || file.name);
  });
  const sourceChannel = options?.sourceChannel?.trim();
  const accountCost = options?.accountCost?.trim();
  if (sourceChannel) formData.append('source_channel', sourceChannel);
  if (accountCost) formData.append('account_cost', accountCost);
  if (options?.overwriteMetadata) formData.append('overwrite_metadata', 'true');
  return formData;
};

const uploadAuthFilesForm = (url: string, files: File[]): Promise<AuthFileBatchUploadResponse> =>
  apiClient.postForm<AuthFileBatchUploadResponse>(url, buildAuthFilesFormData(files), {
    timeout: getDynamicAuthFilesTimeout(files.length, { perItemMs: 220 }),
  });

export const isAuthFileInvalidJsonObjectError = (err: unknown): boolean =>
  err instanceof Error && err.message === AUTH_FILE_INVALID_JSON_OBJECT_ERROR;

const normalizeOauthExcludedModels = (payload: unknown): Record<string, string[]> => {
  if (!payload || typeof payload !== 'object') return {};

  const record = payload as Record<string, unknown>;
  const source = record['oauth-excluded-models'] ?? record.items ?? payload;
  if (!source || typeof source !== 'object') return {};

  const result: Record<string, string[]> = {};

  Object.entries(source as Record<string, unknown>).forEach(([provider, models]) => {
    const key = String(provider ?? '')
      .trim()
      .toLowerCase();
    if (!key) return;

    const rawList = Array.isArray(models)
      ? models
      : typeof models === 'string'
        ? models.split(/[\n,]+/)
        : [];

    const seen = new Set<string>();
    const normalized: string[] = [];
    rawList.forEach((item) => {
      const trimmed = String(item ?? '').trim();
      if (!trimmed) return;
      const modelKey = trimmed.toLowerCase();
      if (seen.has(modelKey)) return;
      seen.add(modelKey);
      normalized.push(trimmed);
    });

    result[key] = normalized;
  });

  return result;
};

const normalizeOauthModelAlias = (payload: unknown): Record<string, OAuthModelAliasEntry[]> => {
  if (!payload || typeof payload !== 'object') return {};

  const record = payload as Record<string, unknown>;
  const source =
    record['oauth-model-alias'] ??
    record.items ??
    payload;
  if (!source || typeof source !== 'object') return {};

  const result: Record<string, OAuthModelAliasEntry[]> = {};

  Object.entries(source as Record<string, unknown>).forEach(([channel, mappings]) => {
    const key = String(channel ?? '')
      .trim()
      .toLowerCase();
    if (!key) return;
    if (!Array.isArray(mappings)) return;

	    const seen = new Set<string>();
	    const normalized = mappings
	      .map((item) => {
	        if (!item || typeof item !== 'object') return null;
	        const entry = item as Record<string, unknown>;
	        const name = String(entry.name ?? entry.id ?? entry.model ?? '').trim();
	        const alias = String(entry.alias ?? '').trim();
	        if (!name || !alias) return null;
	        const fork = entry.fork === true;
	        return fork ? { name, alias, fork } : { name, alias };
	      })
      .filter(Boolean)
      .filter((entry) => {
        const aliasEntry = entry as OAuthModelAliasEntry;
        const dedupeKey = `${aliasEntry.name.toLowerCase()}::${aliasEntry.alias.toLowerCase()}::${aliasEntry.fork ? '1' : '0'}`;
        if (seen.has(dedupeKey)) return false;
        seen.add(dedupeKey);
        return true;
      }) as OAuthModelAliasEntry[];

    if (normalized.length) {
      result[key] = normalized;
    }
  });

  return result;
};

const OAUTH_MODEL_ALIAS_ENDPOINT = '/oauth-model-alias';
const AUTH_FILES_LAST_COUNT_STORAGE_KEY = 'cli-proxy-auth-files-last-count';
const AUTH_FILES_LIST_UNKNOWN_COUNT_TIMEOUT_MS = 120 * 1000;
const AUTH_FILES_DYNAMIC_TIMEOUT_MAX_MS = 10 * 60 * 1000;

export const getDynamicAuthFilesTimeout = (
  count: number,
  options: { baseMs?: number; perItemMs?: number; maxMs?: number } = {}
): number => {
  const safeCount = Math.max(0, Math.ceil(Number.isFinite(count) ? count : 0));
  const baseMs = options.baseMs ?? REQUEST_TIMEOUT_MS;
  const perItemMs = options.perItemMs ?? 250;
  const maxMs = options.maxMs ?? AUTH_FILES_DYNAMIC_TIMEOUT_MAX_MS;
  return Math.min(maxMs, Math.max(baseMs, baseMs + safeCount * perItemMs));
};

const readLastAuthFilesCount = (): number => {
  if (typeof window === 'undefined') return 0;
  const value = Number(window.localStorage.getItem(AUTH_FILES_LAST_COUNT_STORAGE_KEY));
  return Number.isFinite(value) && value > 0 ? value : 0;
};

const writeLastAuthFilesCount = (count: number) => {
  if (typeof window === 'undefined') return;
  if (!Number.isFinite(count) || count < 0) return;
  window.localStorage.setItem(AUTH_FILES_LAST_COUNT_STORAGE_KEY, String(Math.ceil(count)));
};

export const authFilesApi = {
  list: async (options: { expectedCount?: number } = {}) => {
    const expectedCount = Math.max(options.expectedCount || 0, readLastAuthFilesCount());
    const timeout =
      expectedCount > 0
        ? getDynamicAuthFilesTimeout(expectedCount, { perItemMs: 120 })
        : AUTH_FILES_LIST_UNKNOWN_COUNT_TIMEOUT_MS;
    try {
      const response = dedupeAuthFilesResponse(
        await apiClient.get<AuthFilesResponse>('/auth-files', { timeout })
      );
      writeLastAuthFilesCount(response.files.length);
      return response;
    } catch (err) {
      const fallbackUrl = getStatusCode(err) === 404 ? getLocalManagementUrl('/auth-files') : null;
      if (!fallbackUrl) throw err;
      const response = dedupeAuthFilesResponse(
        await apiClient.get<AuthFilesResponse>(fallbackUrl, { timeout })
      );
      writeLastAuthFilesCount(response.files.length);
      return response;
    }
  },

  setStatus: (name: string, disabled: boolean) =>
    apiClient.patch<AuthFileStatusResponse>('/auth-files/status', { name, disabled }),

  patchFields: (name: string, fields: AuthFileFieldsPatch) =>
    apiClient.patch('/auth-files/fields', { name, ...fields }),

  uploadFiles: async (files: File[]): Promise<AuthFileBatchUploadResult> => {
    const requestedNames = files.map((file) => file.name);
    if (requestedNames.length === 0) {
      return { status: 'ok', uploaded: 0, files: [], failed: [] };
    }

    let payload: AuthFileBatchUploadResponse;
    try {
      payload = await uploadAuthFilesForm('/auth-files', files);
    } catch (err) {
      const fallbackUrl = getStatusCode(err) === 404 ? getLocalManagementUrl('/auth-files') : null;
      if (!fallbackUrl) throw err;
      payload = await uploadAuthFilesForm(fallbackUrl, files);
    }
    return normalizeBatchUploadResponse(payload, requestedNames);
  },

  upload: (file: File) => authFilesApi.uploadFiles([file]),

  deleteFiles: async (names: string[]): Promise<AuthFileBatchDeleteResult> => {
    const requestedNames = normalizeRequestedAuthFileNames(names);
    if (requestedNames.length === 0) {
      return { status: 'ok', deleted: 0, files: [], failed: [] };
    }

    try {
      const payload = await apiClient.delete<AuthFileBatchDeleteResponse>('/auth-files', {
        data: { names: requestedNames },
        timeout: getDynamicAuthFilesTimeout(requestedNames.length, { perItemMs: 180 }),
      });
      return normalizeBatchDeleteResponse(payload, requestedNames);
    } catch (err) {
      if (getStatusCode(err) !== 404) throw err;

      const deleted: string[] = [];
      const failed: Array<{ name: string; error: string }> = [];
      await Promise.all(
        requestedNames.map(async (name) => {
          try {
            await apiClient.delete(`/auth-files?name=${encodeURIComponent(name)}`, {
              timeout: getDynamicAuthFilesTimeout(1, { perItemMs: 180 }),
            });
            deleted.push(name);
          } catch (singleErr) {
            failed.push({
              name,
              error: singleErr instanceof Error ? singleErr.message : 'delete failed',
            });
          }
        })
      );
      return {
        status: failed.length > 0 ? 'partial' : 'ok',
        deleted: deleted.length,
        files: deleted,
        failed,
      };
    }
  },

  deleteFile: (name: string) => authFilesApi.deleteFiles([name]),

  deleteAll: async (expectedCount = readLastAuthFilesCount(), fallbackNames: string[] = []) => {
    try {
      return await apiClient.delete('/auth-files', {
        params: { all: true },
        timeout: getDynamicAuthFilesTimeout(expectedCount, { perItemMs: 160 }),
      });
    } catch (err) {
      if (getStatusCode(err) !== 404 || fallbackNames.length === 0) throw err;
      return authFilesApi.deleteFiles(fallbackNames);
    }
  },

  downloadText: async (name: string): Promise<string> => {
    const response = await apiClient.getRaw(`/auth-files/download?name=${encodeURIComponent(name)}`, {
      responseType: 'blob'
    });
    const blob = response.data as Blob;
    return blob.text();
  },

  downloadAccountPoolArchive: async (): Promise<Blob> => {
    const response = await apiClient.getRaw('/account-pool/download', {
      responseType: 'blob',
      timeout: getDynamicAuthFilesTimeout(readLastAuthFilesCount(), { perItemMs: 120 })
    });
    return response.data as Blob;
  },

  downloadAccountPoolArchiveForNames: async (names: string[]): Promise<Blob> => {
    const requestedNames = normalizeRequestedAuthFileNames(names);
    const response = await apiClient.postRaw('/account-pool/download', {
      names: requestedNames,
    }, {
      responseType: 'blob',
      timeout: getDynamicAuthFilesTimeout(requestedNames.length, { perItemMs: 120 }),
    });
    return response.data as Blob;
  },

  downloadAccountPoolText: async (name: string): Promise<string> => {
    const response = await apiClient.getRaw(`/account-pool/download-entry?name=${encodeURIComponent(name)}`, {
      responseType: 'blob'
    });
    const blob = response.data as Blob;
    return blob.text();
  },

  deleteAccountPoolEntries: async (names: string[]): Promise<AuthFileBatchDeleteResult> => {
    const requestedNames = normalizeRequestedAuthFileNames(names);
    if (requestedNames.length === 0) {
      return { status: 'ok', deleted: 0, files: [], failed: [] };
    }
    let payload: AuthFileBatchDeleteResponse;
    try {
      payload = await apiClient.delete<AuthFileBatchDeleteResponse>('/account-pool/delete', {
        data: { names: requestedNames },
        timeout: getDynamicAuthFilesTimeout(requestedNames.length, { perItemMs: 180 }),
      });
    } catch (err) {
      if (getStatusCode(err) !== 404) throw err;
      payload = await apiClient.delete<AuthFileBatchDeleteResponse>('/account-pool', {
        data: { names: requestedNames },
        timeout: getDynamicAuthFilesTimeout(requestedNames.length, { perItemMs: 180 }),
      });
    }
    return normalizeBatchDeleteResponse(payload, requestedNames);
  },

  getAccountPoolUsageRecords: async (
    limitOrOptions: number | { limit?: number; page?: number; offset?: number; summaryOnly?: boolean } = 80
  ): Promise<AccountPoolUsageResponse> => {
    const options =
      typeof limitOrOptions === 'number' ? { limit: limitOrOptions } : limitOrOptions;
    const params = new URLSearchParams();
    params.set('limit', String(options.limit ?? 80));
    if (typeof options.page === 'number') {
      params.set('page', String(options.page));
    }
    if (typeof options.offset === 'number') {
      params.set('offset', String(options.offset));
    }
    if (options.summaryOnly) {
      params.set('summary_only', '1');
    }
    const response = await apiClient.get<{
      records?: AccountPoolUsageRecord[];
      summaries?: AccountPoolUsageSummary[];
      totals?: AccountPoolUsageTotals;
      total?: number;
      limit?: number;
      offset?: number;
    }>(
      `/account-pool/usage-records?${params.toString()}`
    );
    return {
      records: Array.isArray(response.records) ? response.records : [],
      summaries: Array.isArray(response.summaries) ? response.summaries : [],
      totals: response.totals,
      total: typeof response.total === 'number' ? response.total : undefined,
      limit: typeof response.limit === 'number' ? response.limit : undefined,
      offset: typeof response.offset === 'number' ? response.offset : undefined,
    };
  },

  listAccountPoolEntries: async (options: { includeHash?: boolean } = {}): Promise<AuthFilesResponse> => {
    const config = {
      params: { include_hash: options.includeHash !== false },
      timeout: getDynamicAuthFilesTimeout(readLastAuthFilesCount() || 1000, {
        perItemMs: 120,
      }),
    };
    let payload: AuthFilesResponse;
    try {
      payload = await apiClient.get<AuthFilesResponse>('/account-pool/list', config);
    } catch (err) {
      if (getStatusCode(err) !== 404) throw err;
      try {
        payload = await apiClient.get<AuthFilesResponse>('/account-pool', config);
      } catch (fallbackErr) {
        if (getStatusCode(fallbackErr) !== 404) throw fallbackErr;
        payload = await buildAccountPoolResponseFromArchive(
          await authFilesApi.downloadAccountPoolArchive()
        );
      }
    }
    const response = dedupeAuthFilesResponse(payload);
    return response;
  },

  uploadAccountPoolFiles: async (
    files: File[],
    options?: AccountPoolUploadOptions
  ): Promise<AuthFileBatchUploadResult> => {
    const requestedNames = files.map((file) => file.name);
    if (requestedNames.length === 0) {
      return { status: 'ok', uploaded: 0, files: [], failed: [] };
    }
    const formData = buildAuthFilesFormData(files, options);
    const config = {
      params: { async: true },
      timeout: getDynamicAuthFilesTimeout(files.length, { perItemMs: 220 }),
    };
    let payload: AuthFileBatchUploadResponse;
    try {
      payload = await apiClient.postForm<AuthFileBatchUploadResponse>(
        '/account-pool/upload',
        formData,
        config
      );
    } catch (err) {
      if (getStatusCode(err) !== 404) throw err;
      payload = await apiClient.postForm<AuthFileBatchUploadResponse>(
        '/account-pool',
        buildAuthFilesFormData(files, options),
        config
      );
    }
    if (payload && typeof payload === 'object' && 'job' in payload) {
      const job = normalizeAccountPoolImportJob((payload as { job?: unknown }).job);
      return {
        status: job.status,
        uploaded: job.imported,
        files: job.files,
        failed: job.failures,
        job,
      };
    }
    return normalizeBatchUploadResponse(payload, requestedNames);
  },

  writeAccountPoolToAuthFiles: async (
    names: string[],
    overwrite: boolean
  ): Promise<AuthFileBatchUploadResult> => {
    const requestedNames = normalizeRequestedAuthFileNames(names);
    if (requestedNames.length === 0) {
      return { status: 'ok', uploaded: 0, files: [], failed: [] };
    }
    const payload = await apiClient.post<AuthFileBatchUploadResponse>(
      '/account-pool/write-auth-files',
      { names: requestedNames, overwrite },
      { timeout: getDynamicAuthFilesTimeout(requestedNames.length, { perItemMs: 120 }) }
    );
    return normalizeBatchUploadResponse(payload, requestedNames);
  },

  getAccountPoolImport: async (id: string): Promise<AccountPoolImportJob> => {
    const response = await apiClient.get<{ job?: unknown }>(
      `/account-pool/import/${encodeURIComponent(id)}`
    );
    return normalizeAccountPoolImportJob(response.job);
  },

  repairAccountPoolEntries: async (): Promise<AccountPoolRepairResult> => {
    const response = await apiClient.post<{
      repaired?: boolean;
      converted_sub2?: number;
      inferred_codex?: number;
      llm_repaired?: number;
      llm_failed?: number;
    }>('/account-pool/repair', {});
    return {
      repaired: Boolean(response.repaired),
      convertedSub2:
        typeof response.converted_sub2 === 'number' ? response.converted_sub2 : 0,
      inferredCodex:
        typeof response.inferred_codex === 'number' ? response.inferred_codex : 0,
      llmRepaired: typeof response.llm_repaired === 'number' ? response.llm_repaired : 0,
      llmFailed: typeof response.llm_failed === 'number' ? response.llm_failed : 0,
    };
  },

  updateAccountPoolCheckResults: async (
    results: AccountPoolCheckResultPatch[]
  ): Promise<AccountPoolCheckResultsPatchResponse> => {
    if (results.length === 0) {
      return { status: 'ok', updated: 0, skipped: 0, missing: [] };
    }
    return apiClient.patch<AccountPoolCheckResultsPatchResponse>(
      '/account-pool/check-results',
      { results },
      { timeout: REQUEST_TIMEOUT_MS }
    );
  },

  updateAccountPoolFolder: (payload: {
    folder: string;
    source_model?: string;
    source_info?: string;
  }) => apiClient.patch('/account-pool/folder', payload),

  clearAccountPoolUsageRecords: () => apiClient.delete('/account-pool/usage-records'),

  async downloadJsonObject(name: string): Promise<Record<string, unknown>> {
    const rawText = await authFilesApi.downloadText(name);
    return parseAuthFileJsonObject(rawText);
  },

  saveText: (name: string, text: string) => saveAuthFileText(name, text),

  saveJsonObject: (name: string, json: Record<string, unknown>) =>
    saveAuthFileText(name, JSON.stringify(json)),

  // OAuth 排除模型
  async getOauthExcludedModels(): Promise<Record<string, string[]>> {
    const data = await apiClient.get('/oauth-excluded-models');
    return normalizeOauthExcludedModels(data);
  },

  saveOauthExcludedModels: (provider: string, models: string[]) =>
    apiClient.patch('/oauth-excluded-models', { provider, models }),

  deleteOauthExcludedEntry: (provider: string) =>
    apiClient.delete(`/oauth-excluded-models?provider=${encodeURIComponent(provider)}`),

  replaceOauthExcludedModels: (map: Record<string, string[]>) =>
    apiClient.put('/oauth-excluded-models', normalizeOauthExcludedModels(map)),

  // OAuth 模型别名
  async getOauthModelAlias(): Promise<Record<string, OAuthModelAliasEntry[]>> {
    const data = await apiClient.get(OAUTH_MODEL_ALIAS_ENDPOINT);
    return normalizeOauthModelAlias(data);
  },

  saveOauthModelAlias: async (channel: string, aliases: OAuthModelAliasEntry[]) => {
    const normalizedChannel = String(channel ?? '')
      .trim()
      .toLowerCase();
    const normalizedAliases = normalizeOauthModelAlias({ [normalizedChannel]: aliases })[normalizedChannel] ?? [];
    await apiClient.patch(OAUTH_MODEL_ALIAS_ENDPOINT, { channel: normalizedChannel, aliases: normalizedAliases });
  },

  deleteOauthModelAlias: async (channel: string) => {
    const normalizedChannel = String(channel ?? '')
      .trim()
      .toLowerCase();

    try {
      await apiClient.patch(OAUTH_MODEL_ALIAS_ENDPOINT, { channel: normalizedChannel, aliases: [] });
    } catch (err: unknown) {
      const status = getStatusCode(err);
      if (status !== 405) throw err;
      await apiClient.delete(`${OAUTH_MODEL_ALIAS_ENDPOINT}?channel=${encodeURIComponent(normalizedChannel)}`);
    }
  },

  // 获取认证凭证支持的模型
  async getModelsForAuthFile(name: string): Promise<{ id: string; display_name?: string; type?: string; owned_by?: string }[]> {
    const data = await apiClient.get<Record<string, unknown>>(
      `/auth-files/models?name=${encodeURIComponent(name)}`
    );
    const models = data.models ?? data['models'];
    return Array.isArray(models)
      ? (models as { id: string; display_name?: string; type?: string; owned_by?: string }[])
      : [];
  },

  // 获取指定 channel 的模型定义
  async getModelDefinitions(channel: string): Promise<{ id: string; display_name?: string; type?: string; owned_by?: string }[]> {
    const normalizedChannel = String(channel ?? '').trim().toLowerCase();
    if (!normalizedChannel) return [];
    const data = await apiClient.get<Record<string, unknown>>(
      `/model-definitions/${encodeURIComponent(normalizedChannel)}`
    );
    const models = data.models ?? data['models'];
    return Array.isArray(models)
      ? (models as { id: string; display_name?: string; type?: string; owned_by?: string }[])
      : [];
  }
};
