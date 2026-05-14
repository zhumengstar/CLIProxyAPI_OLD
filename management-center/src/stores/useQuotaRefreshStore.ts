import { create } from 'zustand';

export type QuotaRefreshType = 'claude' | 'antigravity' | 'codex' | 'gemini-cli' | 'kimi';

export type QuotaRefreshSummary = {
  total: number;
  done: number;
  success: number;
  failed: number;
};

type QuotaRefreshTaskState = {
  activeRunId: string | null;
  refreshing: boolean;
  concurrency: number;
  summary: QuotaRefreshSummary;
};

type QuotaRefreshState = {
  tasks: Record<QuotaRefreshType, QuotaRefreshTaskState>;
  begin: (type: QuotaRefreshType, total: number, concurrency: number) => string | null;
  advance: (type: QuotaRefreshType, runId: string, ok: boolean) => void;
  finish: (type: QuotaRefreshType, runId: string) => void;
  setConcurrency: (type: QuotaRefreshType, concurrency: number) => void;
};

const emptySummary = (): QuotaRefreshSummary => ({
  total: 0,
  done: 0,
  success: 0,
  failed: 0,
});

const createTask = (): QuotaRefreshTaskState => ({
  activeRunId: null,
  refreshing: false,
  concurrency: 5,
  summary: emptySummary(),
});

const createRunID = (type: QuotaRefreshType) =>
  `quota-refresh-${type}-${Date.now()}-${Math.random().toString(36).slice(2)}`;

export const useQuotaRefreshStore = create<QuotaRefreshState>((set, get) => ({
  tasks: {
    claude: createTask(),
    antigravity: createTask(),
    codex: createTask(),
    'gemini-cli': createTask(),
    kimi: createTask(),
  },

  begin: (type, total, concurrency) => {
    const current = get().tasks[type];
    if (!current || current.refreshing || total <= 0) return null;
    const runId = createRunID(type);
    set((state) => ({
      tasks: {
        ...state.tasks,
        [type]: {
          ...state.tasks[type],
          activeRunId: runId,
          refreshing: true,
          concurrency,
          summary: {
            ...emptySummary(),
            total,
          },
        },
      },
    }));
    return runId;
  },

  advance: (type, runId, ok) => {
    set((state) => {
      const current = state.tasks[type];
      if (!current || current.activeRunId !== runId) return state;
      return {
        tasks: {
          ...state.tasks,
          [type]: {
            ...current,
            summary: {
              ...current.summary,
              done: current.summary.done + 1,
              success: current.summary.success + (ok ? 1 : 0),
              failed: current.summary.failed + (ok ? 0 : 1),
            },
          },
        },
      };
    });
  },

  finish: (type, runId) => {
    set((state) => {
      const current = state.tasks[type];
      if (!current || current.activeRunId !== runId) return state;
      return {
        tasks: {
          ...state.tasks,
          [type]: {
            ...current,
            activeRunId: null,
            refreshing: false,
          },
        },
      };
    });
  },

  setConcurrency: (type, concurrency) => {
    set((state) => ({
      tasks: {
        ...state.tasks,
        [type]: {
          ...state.tasks[type],
          concurrency,
        },
      },
    }));
  },
}));
