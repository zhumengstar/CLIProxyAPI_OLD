import { create } from 'zustand';
import type { AuthFileItem } from '@/types/authFile';

export type AccountCheckStatus = 'idle' | 'loading' | 'success' | 'error' | 'unsupported';

export type AccountCheckResult = {
  status: AccountCheckStatus;
  message?: string;
  plan?: string;
  quotaLines?: string[];
  quotaRemainingPercent?: number;
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

const ACCOUNT_POOL_CHECK_RESULTS_STORAGE_KEY = 'cli-proxy-account-pool-check-results';
const ACCOUNT_POOL_CHECK_PENDING_STORAGE_KEY = 'cli-proxy-account-pool-check-pending';
const MAX_STORED_CHECK_MESSAGE_LENGTH = 180;
const MAX_STORED_CHECK_QUOTA_LINES = 4;
const MAX_STORED_CHECK_QUOTA_LINE_LENGTH = 120;
const CHECK_RESULTS_STORAGE_LIMITS = [3000, 1500, 800, 400, 160];

const emptySummary = (): AccountCheckSummary => ({
  total: 0,
  done: 0,
  success: 0,
  failed: 0,
  unsupported: 0
});

const createRunId = () => `account-pool-check-${Date.now()}-${Math.random().toString(36).slice(2)}`;
const runAbortControllers = new Map<string, AbortController>();

const isRecord = (value: unknown): value is Record<string, unknown> =>
  value !== null && typeof value === 'object';

const resolveStoredStatusCode = (value: Record<string, unknown>): number | undefined => {
  if (typeof value.statusCode === 'number') return value.statusCode;
  if (value.status === 'success') return 200;
  if (typeof value.message !== 'string') return undefined;
  const match = value.message.match(/^(\d{3})\s*:/);
  if (!match) return undefined;
  const code = Number(match[1]);
  return Number.isFinite(code) ? code : undefined;
};

const readPersistedResults = (): Pick<AccountPoolCheckState, 'results' | 'resultHashes'> => {
  if (typeof window === 'undefined') return { results: {}, resultHashes: {} };
  try {
    const raw = window.localStorage.getItem(ACCOUNT_POOL_CHECK_RESULTS_STORAGE_KEY);
    if (!raw) return { results: {}, resultHashes: {} };
    const parsed = JSON.parse(raw) as unknown;
    if (!isRecord(parsed)) return { results: {}, resultHashes: {} };

    const results: Record<string, AccountCheckResult> = {};
    if (isRecord(parsed.results)) {
      Object.entries(parsed.results).forEach(([name, value]) => {
        if (!isRecord(value)) return;
        const status = value.status;
        if (
          status !== 'success' &&
          status !== 'error' &&
          status !== 'unsupported' &&
          status !== 'idle'
        ) {
          return;
        }
        results[name] = {
          status,
          message: typeof value.message === 'string' ? value.message : undefined,
          plan: typeof value.plan === 'string' ? value.plan : undefined,
          quotaLines: Array.isArray(value.quotaLines)
            ? value.quotaLines.filter((line): line is string => typeof line === 'string')
            : undefined,
          quotaRemainingPercent:
            typeof value.quotaRemainingPercent === 'number' ? value.quotaRemainingPercent : undefined,
          statusCode: resolveStoredStatusCode(value),
          checkedAt: typeof value.checkedAt === 'number' ? value.checkedAt : undefined,
        };
      });
    }

    const resultHashes: Record<string, string> = {};
    if (isRecord(parsed.resultHashes)) {
      Object.entries(parsed.resultHashes).forEach(([name, value]) => {
        if (typeof value === 'string' && value.trim()) {
          resultHashes[name] = value;
        }
      });
    }

    return { results, resultHashes };
  } catch {
    return { results: {}, resultHashes: {} };
  }
};

const isStorageQuotaExceeded = (err: unknown): boolean => {
  if (!(err instanceof DOMException)) return false;
  return (
    err.name === 'QuotaExceededError' ||
    err.name === 'NS_ERROR_DOM_QUOTA_REACHED' ||
    err.code === 22 ||
    err.code === 1014
  );
};

const compactCheckResultForStorage = (result: AccountCheckResult): AccountCheckResult => {
  const compact: AccountCheckResult = { status: result.status };
  if (result.message) {
    compact.message = result.message.slice(0, MAX_STORED_CHECK_MESSAGE_LENGTH);
  }
  if (result.plan) compact.plan = result.plan;
  if (typeof result.quotaRemainingPercent === 'number') {
    compact.quotaRemainingPercent = result.quotaRemainingPercent;
  }
  if (typeof result.statusCode === 'number') compact.statusCode = result.statusCode;
  if (typeof result.checkedAt === 'number') compact.checkedAt = result.checkedAt;
  if (Array.isArray(result.quotaLines) && result.quotaLines.length > 0) {
    compact.quotaLines = result.quotaLines
      .slice(0, MAX_STORED_CHECK_QUOTA_LINES)
      .map((line) => line.slice(0, MAX_STORED_CHECK_QUOTA_LINE_LENGTH));
  }
  return compact;
};

const buildCheckResultsStoragePayload = (
  results: Record<string, AccountCheckResult>,
  resultHashes: Record<string, string>,
  maxResults?: number
) => {
  const entries = Object.entries(results)
    .filter(([, result]) => result.status !== 'loading')
    .sort((left, right) => (right[1].checkedAt ?? 0) - (left[1].checkedAt ?? 0));
  const limitedEntries = typeof maxResults === 'number' ? entries.slice(0, maxResults) : entries;
  const stableResults: Record<string, AccountCheckResult> = {};
  const stableHashes: Record<string, string> = {};
  limitedEntries.forEach(([name, result]) => {
    stableResults[name] = compactCheckResultForStorage(result);
    if (resultHashes[name]) stableHashes[name] = resultHashes[name];
  });
  return { results: stableResults, resultHashes: stableHashes };
};

const tryWriteCheckResultsPayload = (payload: {
  results: Record<string, AccountCheckResult>;
  resultHashes: Record<string, string>;
}): boolean => {
  try {
    window.localStorage.setItem(ACCOUNT_POOL_CHECK_RESULTS_STORAGE_KEY, JSON.stringify(payload));
    return true;
  } catch (err) {
    if (!isStorageQuotaExceeded(err)) return false;
    try {
      window.localStorage.removeItem(ACCOUNT_POOL_CHECK_RESULTS_STORAGE_KEY);
    } catch {
      // Ignore cleanup failures; the next smaller payload may still fit.
    }
    return false;
  }
};

const writePersistedResults = (
  results: Record<string, AccountCheckResult>,
  resultHashes: Record<string, string>
) => {
  if (typeof window === 'undefined') return;
  const fullPayload = buildCheckResultsStoragePayload(results, resultHashes);
  if (tryWriteCheckResultsPayload(fullPayload)) return;

  for (const limit of CHECK_RESULTS_STORAGE_LIMITS) {
    if (tryWriteCheckResultsPayload(buildCheckResultsStoragePayload(results, resultHashes, limit))) {
      return;
    }
  }

  try {
    window.localStorage.removeItem(ACCOUNT_POOL_CHECK_RESULTS_STORAGE_KEY);
  } catch {
    // The page should never crash because browser storage is full.
  }
};

type PendingAccountPoolCheck = {
  names: string[];
  completed: string[];
  startedAt: number;
};

const readPendingAccountPoolCheck = (): PendingAccountPoolCheck | null => {
  if (typeof window === 'undefined') return null;
  try {
    const raw = window.localStorage.getItem(ACCOUNT_POOL_CHECK_PENDING_STORAGE_KEY);
    if (!raw) return null;
    const parsed = JSON.parse(raw) as unknown;
    if (!isRecord(parsed) || !Array.isArray(parsed.names)) return null;
    const names = parsed.names.filter((name): name is string => typeof name === 'string' && Boolean(name.trim()));
    if (names.length === 0) return null;
    const completed = Array.isArray(parsed.completed)
      ? parsed.completed.filter((name): name is string => typeof name === 'string' && Boolean(name.trim()))
      : [];
    return {
      names: Array.from(new Set(names)),
      completed: Array.from(new Set(completed)),
      startedAt: typeof parsed.startedAt === 'number' ? parsed.startedAt : Date.now(),
    };
  } catch {
    return null;
  }
};

const writePendingAccountPoolCheck = (pending: PendingAccountPoolCheck) => {
  if (typeof window === 'undefined') return;
  try {
    window.localStorage.setItem(ACCOUNT_POOL_CHECK_PENDING_STORAGE_KEY, JSON.stringify(pending));
  } catch {
    // Pending state is best-effort; quota pressure must not interrupt checks.
  }
};

const clearPendingAccountPoolCheck = () => {
  if (typeof window === 'undefined') return;
  window.localStorage.removeItem(ACCOUNT_POOL_CHECK_PENDING_STORAGE_KEY);
};

export const readPendingAccountPoolCheckNames = (): string[] => {
  const pending = readPendingAccountPoolCheck();
  if (!pending) return [];
  const completed = new Set(pending.completed);
  return pending.names.filter((name) => !completed.has(name));
};

const initialPersisted = readPersistedResults();

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

  const result: AccountCheckResult = {
    status,
    message: readStringField(file, 'check_message', 'checkMessage') || undefined,
    plan: readStringField(file, 'check_plan', 'checkPlan') || undefined,
    quotaLines: readQuotaLinesField(file),
    quotaRemainingPercent: readNumberField(
      file,
      'check_quota_remaining_percent',
      'checkQuotaRemainingPercent'
    ),
    statusCode: readNumberField(file, 'check_status_code', 'checkStatusCode'),
    checkedAt: readNumberField(file, 'check_checked_at', 'checkCheckedAt'),
  };
  return { result, hash: checkHash || contentHash || undefined };
};

export const useAccountPoolCheckStore = create<AccountPoolCheckState>((set, get) => ({
  activeRunId: null,
  activeNames: [],
  activePreviousResults: {},
  checking: false,
  results: initialPersisted.results,
  resultHashes: initialPersisted.resultHashes,
  summary: emptySummary(),

  beginCheck: (names) => {
    const uniqueNames = Array.from(new Set(names.filter(Boolean)));
    if (uniqueNames.length === 0 || get().checking) return null;

    const runId = createRunId();
    runAbortControllers.set(runId, new AbortController());
    writePendingAccountPoolCheck({ names: uniqueNames, completed: [], startedAt: Date.now() });
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
    clearPendingAccountPoolCheck();
    const summary = state.summary;

    set((current) => {
      const nextResults = { ...current.results };
      current.activeNames.forEach((name) => {
        if (nextResults[name]?.status !== 'loading') return;
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
      const pending = readPendingAccountPoolCheck();
      if (pending) {
        writePendingAccountPoolCheck({
          ...pending,
          completed: Array.from(new Set([...pending.completed, name])),
        });
      }
      return nextState;
    });
  },

  finishCheck: (runId) => {
    const state = get();
    if (state.activeRunId !== runId) return null;
    const summary = state.summary;
    runAbortControllers.delete(runId);
    clearPendingAccountPoolCheck();
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
    if (files.length === 0) return;
    set((state) => {
      const nextResults = { ...state.results };
      const nextHashes = { ...state.resultHashes };
      let changed = false;
      files.forEach((file) => {
        if (!file.name) return;
        if (nextResults[file.name]?.status === 'loading') return;
        const remote = readRemoteCheckResult(file);
        if (!remote) return;
        const existing = nextResults[file.name];
        const existingTime = existing?.checkedAt ?? 0;
        const remoteTime = remote.result.checkedAt ?? 0;
        if (existing && existingTime > remoteTime) return;
        nextResults[file.name] = remote.result;
        if (remote.hash) nextHashes[file.name] = remote.hash;
        changed = true;
      });
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
        if (hash) {
          next[name] = result;
        }
      });
      allowedHashes.forEach((hash, name) => {
        nextHashes[name] = hash;
      });
      writePersistedResults(next, nextHashes);
      return { results: next, resultHashes: nextHashes };
    });
  },

  clearResults: () => {
    writePersistedResults({}, {});
    clearPendingAccountPoolCheck();
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
