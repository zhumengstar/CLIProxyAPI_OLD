import { create } from 'zustand';

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
  pruneResults: (records: AccountCheckRecordRef[]) => void;
  clearResults: () => void;
}

const ACCOUNT_POOL_CHECK_RESULTS_STORAGE_KEY = 'cli-proxy-account-pool-check-results';

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

const writePersistedResults = (
  results: Record<string, AccountCheckResult>,
  resultHashes: Record<string, string>
) => {
  if (typeof window === 'undefined') return;
  const stableResults: Record<string, AccountCheckResult> = {};
  Object.entries(results).forEach(([name, result]) => {
    if (result.status === 'loading') return;
    stableResults[name] = result;
  });
  window.localStorage.setItem(
    ACCOUNT_POOL_CHECK_RESULTS_STORAGE_KEY,
    JSON.stringify({ results: stableResults, resultHashes })
  );
};

const initialPersisted = readPersistedResults();

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
      return nextState;
    });
  },

  finishCheck: (runId) => {
    const state = get();
    if (state.activeRunId !== runId) return null;
    const summary = state.summary;
    runAbortControllers.delete(runId);
    set({
      activeRunId: null,
      activeNames: [],
      activePreviousResults: {},
      checking: false,
      summary
    });
    return summary;
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
        if (
          hash &&
          (!state.resultHashes[name] ||
            state.resultHashes[name] === hash ||
            (state.checking && result.status === 'loading'))
        ) {
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
