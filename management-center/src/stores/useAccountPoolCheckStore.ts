import { create } from 'zustand';
import type { AuthFileItem } from '@/types/authFile';

export type AccountCheckStatus = 'idle' | 'loading' | 'success' | 'error' | 'unsupported';

export type AccountCheckResult = {
  status: AccountCheckStatus;
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

type AccountCheckRecordRef = {
  file: {
    name: string;
  };
  hash: string;
};

type AccountCheckSummary = {
  total: number;
  done: number;
  success: number;
  failed: number;
  unsupported: number;
};

interface AccountPoolCheckState {
  activeRunId: string | null;
  activeNames: string[];
  activePreviousResults: Record<string, AccountCheckResult | undefined>;
  checking: boolean;
  results: Record<string, AccountCheckResult>;
  resultHashes: Record<string, string>;
  summary: AccountCheckSummary;
  beginCheck: (names: string[]) => string | null;
  cancelCheck: () => AccountCheckSummary | null;
  getRunSignal: (runId: string) => AbortSignal | undefined;
  isRunCancelled: (runId: string) => boolean;
  setResult: (runId: string, name: string, result: AccountCheckResult, hash?: string) => void;
  finishCheck: (runId: string) => AccountCheckSummary | null;
  hydrateResultsFromFiles: (files: AuthFileItem[]) => void;
  pruneResults: (records: AccountCheckRecordRef[]) => void;
  clearResults: () => void;
}

const LEGACY_ACCOUNT_POOL_CHECK_STORAGE_KEYS = [
  'cli-proxy-account-pool-check-results',
  'cli-proxy-account-pool-check-pending',
];

const emptySummary = (): AccountCheckSummary => ({
  total: 0,
  done: 0,
  success: 0,
  failed: 0,
  unsupported: 0
});

const createRunId = () => `account-pool-check-${Date.now()}-${Math.random().toString(36).slice(2)}`;
const runAbortControllers = new Map<string, AbortController>();

export const normalizeAccountCheckResult = (result: AccountCheckResult): AccountCheckResult => {
  if (result.status !== 'success' || result.realRequestOk !== false) return result;
  const message = result.message?.trim();
  return {
    ...result,
    status: 'error',
    message: result.realRequestError
      ? `模型检测请求失败: ${result.realRequestError}`
      : message && message !== 'Check passed' && message !== '检测成功'
        ? message
        : '模型检测请求失败',
  };
};

const clearLegacyPersistedResults = () => {
  if (typeof window === 'undefined') return;
  try {
    LEGACY_ACCOUNT_POOL_CHECK_STORAGE_KEYS.forEach((key) => window.localStorage.removeItem(key));
  } catch {
    // Ignore browser storage failures; database state is authoritative.
  }
};

const writePersistedResults = (
  results: Record<string, AccountCheckResult>,
  resultHashes: Record<string, string>
) => {
  void results;
  void resultHashes;
  clearLegacyPersistedResults();
};

export const readPendingAccountPoolCheckNames = (): string[] => {
  clearLegacyPersistedResults();
  return [];
};

clearLegacyPersistedResults();

const readStringField = (file: AuthFileItem, ...keys: string[]): string => {
  for (const key of keys) {
    const value = (file as Record<string, unknown>)[key];
    if (typeof value === 'string' && value.trim()) return value.trim();
  }
  return '';
};

const readNumberField = (file: AuthFileItem, ...keys: string[]): number | undefined => {
  for (const key of keys) {
    const value = (file as Record<string, unknown>)[key];
    if (typeof value === 'number' && Number.isFinite(value)) return value;
    if (typeof value === 'string' && value.trim()) {
      const parsed = Number(value);
      if (Number.isFinite(parsed)) return parsed;
    }
  }
  return undefined;
};

const readQuotaLinesField = (file: AuthFileItem): string[] | undefined => {
  const value =
    (file as Record<string, unknown>).check_quota_lines ??
    (file as Record<string, unknown>).checkQuotaLines;
  if (Array.isArray(value)) return value.filter((line): line is string => typeof line === 'string');
  if (typeof value === 'string' && value.trim()) {
    try {
      const parsed = JSON.parse(value) as unknown;
      if (Array.isArray(parsed)) {
        return parsed.filter((line): line is string => typeof line === 'string');
      }
    } catch {
      return value.split('\n').map((line) => line.trim()).filter(Boolean);
    }
  }
  return undefined;
};

const readRemoteCheckResult = (
  file: AuthFileItem
): { result: AccountCheckResult; hash?: string } | null => {
  const status = readStringField(file, 'check_status', 'checkStatus');
  if (status !== 'success' && status !== 'error' && status !== 'unsupported' && status !== 'idle') {
    return null;
  }
  const contentHash = readStringField(file, 'content_hash', 'contentHash');
  const checkHash = readStringField(file, 'check_content_hash', 'checkContentHash');
  if (contentHash && checkHash && contentHash !== checkHash) return null;

  const realRequestOk =
    typeof file.check_real_request_ok === 'boolean'
      ? file.check_real_request_ok
      : typeof file.checkRealRequestOk === 'boolean'
        ? file.checkRealRequestOk
        : undefined;
  const result: AccountCheckResult = normalizeAccountCheckResult({
    status,
    message: readStringField(file, 'check_message', 'checkMessage') || undefined,
    plan: readStringField(file, 'check_plan', 'checkPlan') || undefined,
    quotaLines: readQuotaLinesField(file),
    quotaRemainingPercent: readNumberField(
      file,
      'check_quota_remaining_percent',
      'checkQuotaRemainingPercent'
    ),
    realRequestOk,
    realRequestError: readStringField(file, 'check_real_request_error', 'checkRealRequestError') || undefined,
    realRequestStatusCode: readNumberField(
      file,
      'check_real_request_status_code',
      'checkRealRequestStatusCode'
    ),
    statusCode: readNumberField(file, 'check_status_code', 'checkStatusCode'),
    checkedAt: readNumberField(file, 'check_checked_at', 'checkCheckedAt'),
  });
  return { result, hash: checkHash || contentHash || undefined };
};

export const useAccountPoolCheckStore = create<AccountPoolCheckState>((set, get) => ({
  activeRunId: null,
  activeNames: [],
  activePreviousResults: {},
  checking: false,
  results: {},
  resultHashes: {},
  summary: emptySummary(),

  beginCheck: (names) => {
    const uniqueNames = Array.from(new Set(names.filter(Boolean)));
    if (uniqueNames.length === 0 || get().checking) return null;

    const runId = createRunId();
    runAbortControllers.set(runId, new AbortController());
    clearLegacyPersistedResults();
    set((state) => {
      const nextResults = { ...state.results };
      const activePreviousResults: Record<string, AccountCheckResult | undefined> = {};
      uniqueNames.forEach((name) => {
        activePreviousResults[name] = nextResults[name];
        nextResults[name] = {
          ...nextResults[name],
          status: 'loading',
        };
      });
      return {
        activeRunId: runId,
        activeNames: uniqueNames,
        activePreviousResults,
        checking: true,
        results: nextResults,
        summary: {
          ...emptySummary(),
          total: uniqueNames.length
        }
      };
    });
    return runId;
  },

  cancelCheck: () => {
    const state = get();
    const runId = state.activeRunId;
    if (!runId) return null;

    runAbortControllers.get(runId)?.abort();
    runAbortControllers.delete(runId);
    clearLegacyPersistedResults();
    const summary = state.summary;

    set((current) => {
      const nextResults = { ...current.results };
      current.activeNames.forEach((name) => {
        const previous = current.activePreviousResults[name];
        if (previous) {
          nextResults[name] = previous;
        } else {
          delete nextResults[name];
        }
      });
      writePersistedResults(nextResults, current.resultHashes);
      return {
        activeRunId: null,
        activeNames: [],
        activePreviousResults: {},
        checking: false,
        results: nextResults,
        summary
      };
    });
    return summary;
  },

  getRunSignal: (runId) => runAbortControllers.get(runId)?.signal,

  isRunCancelled: (runId) => runAbortControllers.get(runId)?.signal.aborted ?? true,

  setResult: (runId, name, result, hash) => {
    const state = get();
    if (state.activeRunId !== runId || state.isRunCancelled(runId)) return;

    set((current) => {
      const previous = current.results[name];
      const nextSummary = { ...current.summary };

      if (previous?.status === 'loading') {
        nextSummary.done += 1;
      }
      if (result.status === 'success') {
        nextSummary.success += 1;
      } else if (result.status === 'unsupported') {
        nextSummary.unsupported += 1;
      } else if (result.status === 'error') {
        nextSummary.failed += 1;
      }

      const nextState = {
        results: {
          ...current.results,
          [name]: result
        },
        resultHashes: {
          ...current.resultHashes,
          ...(hash ? { [name]: hash } : {}),
        },
        summary: nextSummary
      };
      writePersistedResults(nextState.results, nextState.resultHashes);
      clearLegacyPersistedResults();
      return nextState;
    });
  },

  finishCheck: (runId) => {
    const state = get();
    if (state.activeRunId !== runId) return null;
    const summary = state.summary;
    runAbortControllers.delete(runId);
    clearLegacyPersistedResults();
    set({
      activeRunId: null,
      activeNames: [],
      activePreviousResults: {},
      checking: false,
      summary
    });
    return summary;
  },

  hydrateResultsFromFiles: (files) => {
    set((state) => {
      const nextResults: Record<string, AccountCheckResult> = {};
      const nextHashes: Record<string, string> = {};
      const seenNames = new Set<string>();

      files.forEach((file) => {
        if (!file.name) return;
        seenNames.add(file.name);
        const remote = readRemoteCheckResult(file);
        if (remote) {
          nextResults[file.name] = remote.result;
          if (remote.hash) {
            nextHashes[file.name] = remote.hash;
          }
        }
      });

      // While a check is running, keep only in-flight loading markers for visible accounts.
      // Finished/error/success results are re-hydrated exclusively from server check_* fields.
      Object.entries(state.results).forEach(([name, result]) => {
        if (result.status === 'loading' && seenNames.has(name)) {
          nextResults[name] = result;
          if (state.resultHashes[name]) nextHashes[name] = state.resultHashes[name];
        }
      });

      const changed =
        Object.keys(nextResults).length !== Object.keys(state.results).length ||
        Object.keys(nextHashes).length !== Object.keys(state.resultHashes).length ||
        Object.entries(nextResults).some(([name, result]) => state.results[name] !== result) ||
        Object.entries(nextHashes).some(([name, hash]) => state.resultHashes[name] !== hash);

      if (!changed) return state;
      writePersistedResults(nextResults, nextHashes);
      return { results: nextResults, resultHashes: nextHashes };
    });
  },

  pruneResults: (records) => {
    const allowedHashes = new Map<string, string>();
    records.forEach((record) => {
      if (record.file.name && record.hash) {
        allowedHashes.set(record.file.name, record.hash);
      }
    });
    set((state) => {
      const next: Record<string, AccountCheckResult> = {};
      const nextHashes: Record<string, string> = {};
      Object.entries(state.results).forEach(([name, result]) => {
        const hash = allowedHashes.get(name);
        if (hash && result.status === 'loading') {
          next[name] = result;
          nextHashes[name] = hash;
        }
      });
      writePersistedResults(next, nextHashes);
      return { results: next, resultHashes: nextHashes };
    });
  },

  clearResults: () => {
    writePersistedResults({}, {});
    clearLegacyPersistedResults();
    set({
      activeRunId: null,
      activeNames: [],
      activePreviousResults: {},
      checking: false,
      results: {},
      resultHashes: {},
      summary: emptySummary()
    });
  }
}));
