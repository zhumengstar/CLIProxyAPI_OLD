/**
 * Generic quota section component.
 */

import { useCallback, type ChangeEvent, useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Card } from '@/components/ui/Card';
import { Button } from '@/components/ui/Button';
import { EmptyState } from '@/components/ui/EmptyState';
import { Select } from '@/components/ui/Select';
import { useNotificationStore, useQuotaRefreshStore, useQuotaStore, useThemeStore } from '@/stores';
import type { AuthFileItem, ResolvedTheme } from '@/types';
import { getStatusFromError } from '@/utils/quota';
import { QuotaCard } from './QuotaCard';
import type { QuotaStatusState } from './QuotaCard';
import type { QuotaConfig } from './quotaConfigs';
import { useGridColumns } from './useGridColumns';
import { IconRefreshCw } from '@/components/ui/icons';
import styles from '@/pages/QuotaPage.module.scss';

type QuotaUpdater<T> = T | ((prev: T) => T);

type QuotaSetter<T> = (updater: QuotaUpdater<T>) => void;

type ViewMode = 'paged' | 'all';

const MAX_AUTO_ITEMS_PER_PAGE = 25;
const MAX_SHOW_ALL_THRESHOLD = 30;
const MIN_QUOTA_PAGE_SIZE = 1;
const DEFAULT_QUOTA_REFRESH_CONCURRENCY = 5;
const MIN_QUOTA_REFRESH_CONCURRENCY = 1;
const MAX_QUOTA_REFRESH_CONCURRENCY = 20;

type QuotaSortMode = 'quota_desc' | 'quota_asc' | 'name_asc';

type PendingQuotaRefresh = {
  names: string[];
  completed: string[];
  concurrency: number;
  startedAt: number;
};

const quotaRefreshAbortControllers = new Map<string, AbortController>();

const quotaPendingKey = (type: string) => `cli-proxy-quota-refresh-pending:${type}`;

const readPendingQuotaRefresh = (type: string): PendingQuotaRefresh | null => {
  if (typeof window === 'undefined') return null;
  try {
    const raw = window.localStorage.getItem(quotaPendingKey(type));
    if (!raw) return null;
    const parsed = JSON.parse(raw) as Partial<PendingQuotaRefresh>;
    const names = Array.isArray(parsed.names)
      ? parsed.names.filter((name): name is string => typeof name === 'string' && Boolean(name.trim()))
      : [];
    if (names.length === 0) return null;
    const completed = Array.isArray(parsed.completed)
      ? parsed.completed.filter((name): name is string => typeof name === 'string' && Boolean(name.trim()))
      : [];
    return {
      names: Array.from(new Set(names)),
      completed: Array.from(new Set(completed)),
      concurrency: typeof parsed.concurrency === 'number' && Number.isFinite(parsed.concurrency)
        ? parsed.concurrency
        : DEFAULT_QUOTA_REFRESH_CONCURRENCY,
      startedAt: typeof parsed.startedAt === 'number' ? parsed.startedAt : Date.now(),
    };
  } catch {
    return null;
  }
};

const writePendingQuotaRefresh = (type: string, pending: PendingQuotaRefresh) => {
  if (typeof window === 'undefined') return;
  window.localStorage.setItem(quotaPendingKey(type), JSON.stringify(pending));
};

const clearPendingQuotaRefresh = (type: string) => {
  if (typeof window === 'undefined') return;
  window.localStorage.removeItem(quotaPendingKey(type));
};

const getRemainingPendingQuotaNames = (type: string): string[] => {
  const pending = readPendingQuotaRefresh(type);
  if (!pending) return [];
  const completed = new Set(pending.completed);
  return pending.names.filter((name) => !completed.has(name));
};

const markPendingQuotaCompleted = (type: string, name: string) => {
  const pending = readPendingQuotaRefresh(type);
  if (!pending) return;
  writePendingQuotaRefresh(type, {
    ...pending,
    completed: Array.from(new Set([...pending.completed, name])),
  });
};

interface QuotaPaginationState<T> {
  pageSize: number;
  totalPages: number;
  currentPage: number;
  pageItems: T[];
  setPageSize: (size: number) => void;
  goToPrev: () => void;
  goToNext: () => void;
}

const useQuotaPagination = <T,>(items: T[], defaultPageSize = 6): QuotaPaginationState<T> => {
  const [page, setPage] = useState(1);
  const [pageSize, setPageSizeState] = useState(defaultPageSize);

  const totalPages = useMemo(
    () => Math.max(1, Math.ceil(items.length / pageSize)),
    [items.length, pageSize]
  );

  const currentPage = useMemo(() => Math.min(page, totalPages), [page, totalPages]);

  const pageItems = useMemo(() => {
    const start = (currentPage - 1) * pageSize;
    return items.slice(start, start + pageSize);
  }, [items, currentPage, pageSize]);

  const setPageSize = useCallback((size: number) => {
    setPageSizeState(size);
    setPage(1);
  }, []);

  const goToPrev = useCallback(() => {
    setPage((prev) => Math.max(1, prev - 1));
  }, []);

  const goToNext = useCallback(() => {
    setPage((prev) => Math.min(totalPages, prev + 1));
  }, [totalPages]);

  return {
    pageSize,
    totalPages,
    currentPage,
    pageItems,
    setPageSize,
    goToPrev,
    goToNext,
  };
};

interface QuotaSectionProps<TState extends QuotaStatusState, TData> {
  config: QuotaConfig<TState, TData>;
  files: AuthFileItem[];
  disabled: boolean;
}

export function QuotaSection<TState extends QuotaStatusState, TData>({
  config,
  files,
  disabled
}: QuotaSectionProps<TState, TData>) {
  const { t } = useTranslation();
  const resolvedTheme: ResolvedTheme = useThemeStore((state) => state.resolvedTheme);
  const showNotification = useNotificationStore((state) => state.showNotification);
  const quota = useQuotaStore(config.storeSelector);
  const setQuota = useQuotaStore((state) => state[config.storeSetter]) as QuotaSetter<
    Record<string, TState>
  >;
  const refreshTask = useQuotaRefreshStore((state) => state.tasks[config.type]);
  const beginRefresh = useQuotaRefreshStore((state) => state.begin);
  const advanceRefresh = useQuotaRefreshStore((state) => state.advance);
  const finishRefresh = useQuotaRefreshStore((state) => state.finish);
  const setRefreshStoreConcurrency = useQuotaRefreshStore((state) => state.setConcurrency);

  /* Removed useRef */
  const [columns, gridRef] = useGridColumns(380); // Min card width 380px matches SCSS
  const [viewMode, setViewMode] = useState<ViewMode>('paged');
  const [customPageSize, setCustomPageSize] = useState<number | null>(null);
  const [pageSizeInput, setPageSizeInput] = useState('');
  const [sortMode, setSortMode] = useState<QuotaSortMode>('quota_desc');
  const [refreshConcurrency, setRefreshConcurrency] = useState(DEFAULT_QUOTA_REFRESH_CONCURRENCY);
  const [refreshConcurrencyInput, setRefreshConcurrencyInput] = useState(
    String(DEFAULT_QUOTA_REFRESH_CONCURRENCY)
  );
  const [showTooManyWarning, setShowTooManyWarning] = useState(false);
  const [resumedPendingRefresh, setResumedPendingRefresh] = useState(false);

  const filteredFiles = useMemo(() => files.filter((file) => config.filterFn(file)), [files, config]);
  const showAllAllowed = filteredFiles.length <= MAX_SHOW_ALL_THRESHOLD;
  const effectiveViewMode: ViewMode = viewMode === 'all' && !showAllAllowed ? 'paged' : viewMode;

  const automaticPageSize = useMemo(() => {
    if (effectiveViewMode === 'all') {
      return Math.max(MIN_QUOTA_PAGE_SIZE, filteredFiles.length);
    }
    return Math.min(columns * 3, MAX_AUTO_ITEMS_PER_PAGE);
  }, [columns, effectiveViewMode, filteredFiles.length]);
  const maxQuotaPageSize = Math.max(MIN_QUOTA_PAGE_SIZE, filteredFiles.length);

  const clampQuotaPageSize = useCallback(
    (value: number) =>
      Math.min(maxQuotaPageSize, Math.max(MIN_QUOTA_PAGE_SIZE, Math.round(value))),
    [maxQuotaPageSize]
  );

  useEffect(() => {
    if (showAllAllowed) return;
    if (viewMode !== 'all') return;

    let cancelled = false;
    queueMicrotask(() => {
      if (cancelled) return;
      setViewMode('paged');
      setShowTooManyWarning(true);
    });

    return () => {
      cancelled = true;
    };
  }, [showAllAllowed, viewMode]);

  const clampQuotaRefreshConcurrency = useCallback(
    (value: number) =>
      Math.min(
        MAX_QUOTA_REFRESH_CONCURRENCY,
        Math.max(MIN_QUOTA_REFRESH_CONCURRENCY, Math.round(value))
      ),
    []
  );

  useEffect(() => {
    if (typeof window === 'undefined') return;
    const storageKey = `quota-refresh-concurrency:${config.type}`;
    const raw = window.localStorage.getItem(storageKey);
    if (!raw) return;
    const parsed = Number(raw);
    if (!Number.isFinite(parsed)) return;
    const next = clampQuotaRefreshConcurrency(parsed);
    setRefreshConcurrency(next);
    setRefreshConcurrencyInput(String(next));
    setRefreshStoreConcurrency(config.type, next);
  }, [clampQuotaRefreshConcurrency, config.type, setRefreshStoreConcurrency]);

  const getQuotaRemainingPercent = useCallback(
    (file: AuthFileItem): number | null => {
      const state = quota[file.name] as Record<string, unknown> | undefined;
      if (!state || state.status !== 'success') return null;

      if (config.type === 'antigravity') {
        const groups = Array.isArray(state.groups) ? state.groups : [];
        const values = groups
          .map((group) => {
            if (!group || typeof group !== 'object') return null;
            const fraction = (group as Record<string, unknown>).remainingFraction;
            return typeof fraction === 'number' && Number.isFinite(fraction) ? fraction * 100 : null;
          })
          .filter((value): value is number => value !== null);
        return values.length > 0 ? Math.min(...values) : null;
      }

      if (config.type === 'codex' || config.type === 'claude') {
        const windows = Array.isArray(state.windows) ? state.windows : [];
        const values = windows
          .map((windowItem) => {
            if (!windowItem || typeof windowItem !== 'object') return null;
            const used = (windowItem as Record<string, unknown>).usedPercent;
            return typeof used === 'number' && Number.isFinite(used) ? Math.max(0, 100 - used) : null;
          })
          .filter((value): value is number => value !== null);
        return values.length > 0 ? Math.min(...values) : null;
      }

      if (config.type === 'gemini-cli') {
        const buckets = Array.isArray(state.buckets) ? state.buckets : [];
        const values = buckets
          .map((bucket) => {
            if (!bucket || typeof bucket !== 'object') return null;
            const fraction = (bucket as Record<string, unknown>).remainingFraction;
            return typeof fraction === 'number' && Number.isFinite(fraction) ? fraction * 100 : null;
          })
          .filter((value): value is number => value !== null);
        return values.length > 0 ? Math.min(...values) : null;
      }

      if (config.type === 'kimi') {
        const rows = Array.isArray(state.rows) ? state.rows : [];
        const values = rows
          .map((row) => {
            if (!row || typeof row !== 'object') return null;
            const used = (row as Record<string, unknown>).used;
            const limit = (row as Record<string, unknown>).limit;
            if (
              typeof used !== 'number' ||
              !Number.isFinite(used) ||
              typeof limit !== 'number' ||
              !Number.isFinite(limit) ||
              limit <= 0
            ) {
              return null;
            }
            return Math.max(0, ((limit - used) / limit) * 100);
          })
          .filter((value): value is number => value !== null);
        return values.length > 0 ? Math.min(...values) : null;
      }

      return null;
    },
    [config.type, quota]
  );

  const sortedFiles = useMemo(() => {
    const items = [...filteredFiles];
    items.sort((left, right) => {
      if (sortMode === 'name_asc') {
        return left.name.localeCompare(right.name);
      }
      const leftQuota = getQuotaRemainingPercent(left);
      const rightQuota = getQuotaRemainingPercent(right);
      if (leftQuota === null && rightQuota === null) {
        return left.name.localeCompare(right.name);
      }
      if (leftQuota === null) return 1;
      if (rightQuota === null) return -1;
      if (leftQuota !== rightQuota) {
        return sortMode === 'quota_asc' ? leftQuota - rightQuota : rightQuota - leftQuota;
      }
      return left.name.localeCompare(right.name);
    });
    return items;
  }, [filteredFiles, getQuotaRemainingPercent, sortMode]);

  const {
    pageSize,
    totalPages,
    currentPage,
    pageItems,
    setPageSize,
    goToPrev,
    goToNext,
  } = useQuotaPagination(sortedFiles);

  useEffect(() => {
    const nextPageSize =
      effectiveViewMode === 'all'
        ? automaticPageSize
        : clampQuotaPageSize(customPageSize ?? automaticPageSize);
    setPageSize(nextPageSize);
    setPageSizeInput(String(nextPageSize));
  }, [automaticPageSize, clampQuotaPageSize, customPageSize, effectiveViewMode, setPageSize]);

  const commitPageSizeInput = useCallback(
    (rawValue: string) => {
      const trimmed = rawValue.trim();
      if (!trimmed) {
        const next = clampQuotaPageSize(automaticPageSize);
        setCustomPageSize(null);
        setPageSize(next);
        setPageSizeInput(String(next));
        return;
      }

      const parsed = Number(trimmed);
      if (!Number.isFinite(parsed)) {
        setPageSizeInput(String(pageSize));
        return;
      }

      const next = clampQuotaPageSize(parsed);
      setCustomPageSize(next);
      setPageSize(next);
      setPageSizeInput(String(next));
    },
    [automaticPageSize, clampQuotaPageSize, pageSize, setPageSize]
  );

  const handlePageSizeChange = useCallback(
    (event: ChangeEvent<HTMLInputElement>) => {
      const rawValue = event.currentTarget.value;
      setPageSizeInput(rawValue);

      const trimmed = rawValue.trim();
      if (!trimmed) return;

      const parsed = Number(trimmed);
      if (!Number.isFinite(parsed)) return;

      const next = clampQuotaPageSize(parsed);
      setCustomPageSize(next);
      setPageSize(next);
    },
    [clampQuotaPageSize, setPageSize]
  );

  const commitRefreshConcurrencyInput = useCallback(
    (rawValue: string) => {
      const trimmed = rawValue.trim();
      if (!trimmed) {
        setRefreshConcurrencyInput(String(refreshConcurrency));
        return;
      }
      const parsed = Number(trimmed);
      if (!Number.isFinite(parsed)) {
        setRefreshConcurrencyInput(String(refreshConcurrency));
        return;
      }
      const next = clampQuotaRefreshConcurrency(parsed);
      setRefreshConcurrency(next);
      setRefreshConcurrencyInput(String(next));
      setRefreshStoreConcurrency(config.type, next);
      if (typeof window !== 'undefined') {
        window.localStorage.setItem(`quota-refresh-concurrency:${config.type}`, String(next));
      }
    },
    [clampQuotaRefreshConcurrency, config.type, refreshConcurrency, setRefreshStoreConcurrency]
  );

  const handleRefreshConcurrencyChange = useCallback(
    (event: ChangeEvent<HTMLInputElement>) => {
      const rawValue = event.currentTarget.value;
      setRefreshConcurrencyInput(rawValue);
      const trimmed = rawValue.trim();
      if (!trimmed) return;
      const parsed = Number(trimmed);
      if (!Number.isFinite(parsed)) return;
      const next = clampQuotaRefreshConcurrency(parsed);
      setRefreshConcurrency(next);
      setRefreshStoreConcurrency(config.type, next);
      if (typeof window !== 'undefined') {
        window.localStorage.setItem(`quota-refresh-concurrency:${config.type}`, String(next));
      }
    },
    [clampQuotaRefreshConcurrency, config.type, setRefreshStoreConcurrency]
  );

  const runRefreshTargets = useCallback(async (
    targets: AuthFileItem[],
    options: { resume?: boolean; concurrency?: number } = {}
  ) => {
    if (disabled || refreshTask.refreshing) return;
    if (targets.length === 0) return;

    const concurrency = Math.max(
      MIN_QUOTA_REFRESH_CONCURRENCY,
      Math.round(options.concurrency ?? refreshConcurrency)
    );
    const runId = beginRefresh(config.type, targets.length, concurrency);
    if (!runId) return;
    const controller = new AbortController();
    quotaRefreshAbortControllers.set(config.type, controller);
    if (!options.resume) {
      writePendingQuotaRefresh(config.type, {
        names: targets.map((file) => file.name),
        completed: [],
        concurrency,
        startedAt: Date.now(),
      });
    }

    setQuota((prev) => {
      const next = { ...prev };
      targets.forEach((file) => {
        next[file.name] = config.buildLoadingState();
      });
      return next;
    });

    let cursor = 0;
    const workerCount = Math.max(1, Math.min(concurrency, targets.length));

    const worker = async () => {
      for (;;) {
        if (controller.signal.aborted) return;
        const index = cursor;
        cursor += 1;
        const file = targets[index];
        if (!file) return;
        try {
          const data = await config.fetchQuota(file, t, controller.signal);
          if (controller.signal.aborted) return;
          setQuota((prev) => ({
            ...prev,
            [file.name]: config.buildSuccessState(data),
          }));
          advanceRefresh(config.type, runId, true);
          markPendingQuotaCompleted(config.type, file.name);
        } catch (err: unknown) {
          if (controller.signal.aborted) return;
          const message = err instanceof Error ? err.message : t('common.unknown_error');
          const status = getStatusFromError(err);
          setQuota((prev) => ({
            ...prev,
            [file.name]: config.buildErrorState(message, status),
          }));
          advanceRefresh(config.type, runId, false);
          markPendingQuotaCompleted(config.type, file.name);
        }
      }
    };

    try {
      await Promise.all(Array.from({ length: workerCount }, () => worker()));
      if (controller.signal.aborted) return;
      const latest = useQuotaRefreshStore.getState().tasks[config.type];
      const summary = latest.summary;
      showNotification(
        t('auth_files.quota_refresh_all_done', {
          success: summary.success,
          failed: summary.failed,
          defaultValue: `额度刷新完成：成功 ${summary.success}，失败 ${summary.failed}`,
        }),
        summary.failed > 0 ? 'warning' : 'success'
      );
      clearPendingQuotaRefresh(config.type);
    } finally {
      quotaRefreshAbortControllers.delete(config.type);
      finishRefresh(config.type, runId);
    }
  }, [
    advanceRefresh,
    beginRefresh,
    config,
    disabled,
    finishRefresh,
    refreshConcurrency,
    refreshTask.refreshing,
    setQuota,
    showNotification,
    t,
  ]);

  const handleRefresh = useCallback(async () => {
    await runRefreshTargets(sortedFiles);
  }, [runRefreshTargets, sortedFiles]);

  useEffect(() => {
    if (resumedPendingRefresh || disabled || refreshTask.refreshing || filteredFiles.length === 0) return;
    const pending = readPendingQuotaRefresh(config.type);
    const remainingNames = getRemainingPendingQuotaNames(config.type);
    if (!pending || remainingNames.length === 0) {
      setResumedPendingRefresh(true);
      if (pending) {
        clearPendingQuotaRefresh(config.type);
      }
      return;
    }
    const remainingSet = new Set(remainingNames);
    const targets = filteredFiles.filter((file) => remainingSet.has(file.name));
    if (targets.length === 0) return;
    setResumedPendingRefresh(true);
    showNotification(
      t('quota_management.refresh_resumed', {
        count: targets.length,
        defaultValue: `已自动恢复后台刷新：剩余 ${targets.length} 个`,
      }),
      'info'
    );
    void runRefreshTargets(targets, { resume: true, concurrency: pending.concurrency });
  }, [
    config.type,
    disabled,
    filteredFiles,
    refreshTask.refreshing,
    resumedPendingRefresh,
    runRefreshTargets,
    showNotification,
    t,
  ]);

  const refreshQuotaForFile = useCallback(
    async (file: AuthFileItem) => {
      if (disabled || file.disabled) return;
      if (quota[file.name]?.status === 'loading') return;

      setQuota((prev) => ({
        ...prev,
        [file.name]: config.buildLoadingState()
      }));

      try {
        const data = await config.fetchQuota(file, t);
        setQuota((prev) => ({
          ...prev,
          [file.name]: config.buildSuccessState(data)
        }));
        showNotification(t('auth_files.quota_refresh_success', { name: file.name }), 'success');
      } catch (err: unknown) {
        const message = err instanceof Error ? err.message : t('common.unknown_error');
        const status = getStatusFromError(err);
        setQuota((prev) => ({
          ...prev,
          [file.name]: config.buildErrorState(message, status)
        }));
        showNotification(
          t('auth_files.quota_refresh_failed', { name: file.name, message }),
          'error'
        );
      }
    },
    [config, disabled, quota, setQuota, showNotification, t]
  );

  const sortOptions = useMemo(
    () => [
      {
        value: 'quota_desc',
        label: t('quota_management.sort_quota_desc', { defaultValue: '额度最高' }),
      },
      {
        value: 'quota_asc',
        label: t('quota_management.sort_quota_asc', { defaultValue: '额度最低' }),
      },
      {
        value: 'name_asc',
        label: t('quota_management.sort_name_asc', { defaultValue: '名称 A-Z' }),
      },
    ],
    [t]
  );

  const titleNode = (
    <div className={styles.titleWrapper}>
      <span>{t(`${config.i18nPrefix}.title`)}</span>
      {filteredFiles.length > 0 && (
        <span className={styles.countBadge}>
          {filteredFiles.length}
        </span>
      )}
    </div>
  );

  const isRefreshing = refreshTask.refreshing;
  const interruptRefresh = useCallback(() => {
    const runId = refreshTask.activeRunId;
    if (!runId) return;
    quotaRefreshAbortControllers.get(config.type)?.abort();
    quotaRefreshAbortControllers.delete(config.type);
    clearPendingQuotaRefresh(config.type);
    finishRefresh(config.type, runId);
    showNotification(
      t('quota_management.refresh_cancelled', {
        done: refreshTask.summary.done,
        total: refreshTask.summary.total,
        defaultValue: `刷新已中断：已完成 ${refreshTask.summary.done} / ${refreshTask.summary.total}`,
      }),
      'warning'
    );
  }, [config.type, finishRefresh, refreshTask.activeRunId, refreshTask.summary, showNotification, t]);

  return (
    <Card
      title={titleNode}
      extra={
        <div className={styles.headerActions}>
          <div className={styles.viewModeToggle}>
            <Button
              variant="secondary"
              size="sm"
              className={`${styles.viewModeButton} ${
                effectiveViewMode === 'paged' ? styles.viewModeButtonActive : ''
              }`}
              onClick={() => setViewMode('paged')}
            >
              {t('auth_files.view_mode_paged')}
            </Button>
            <Button
              variant="secondary"
              size="sm"
              className={`${styles.viewModeButton} ${
                effectiveViewMode === 'all' ? styles.viewModeButtonActive : ''
              }`}
              onClick={() => {
                if (filteredFiles.length > MAX_SHOW_ALL_THRESHOLD) {
                  setShowTooManyWarning(true);
                } else {
                  setViewMode('all');
                }
              }}
            >
              {t('auth_files.view_mode_all')}
            </Button>
          </div>
          {effectiveViewMode === 'paged' && filteredFiles.length > 0 && (
            <label className={styles.quotaPageSizeControl}>
              <span>{t('quota_management.page_size_label')}</span>
              <input
                className={styles.pageSizeSelect}
                type="number"
                min={MIN_QUOTA_PAGE_SIZE}
                max={maxQuotaPageSize}
                step={1}
                value={pageSizeInput}
                onChange={handlePageSizeChange}
                onBlur={(event) => commitPageSizeInput(event.currentTarget.value)}
                onKeyDown={(event) => {
                  if (event.key === 'Enter') {
                    event.currentTarget.blur();
                  }
                }}
                aria-label={t('quota_management.page_size_label')}
              />
            </label>
          )}
          <label className={styles.quotaPageSizeControl}>
            <span>{t('quota_management.sort_label', { defaultValue: '排序' })}</span>
            <Select
              className={styles.quotaSortSelect}
              fullWidth={false}
              value={sortMode}
              options={sortOptions}
              onChange={(value) => setSortMode(value as QuotaSortMode)}
              ariaLabel={t('quota_management.sort_label', { defaultValue: '排序' })}
            />
          </label>
          <label className={styles.quotaPageSizeControl}>
            <span>{t('quota_management.check_concurrency', { defaultValue: '刷新并发' })}</span>
            <input
              className={styles.pageSizeSelect}
              type="number"
              min={MIN_QUOTA_REFRESH_CONCURRENCY}
              max={MAX_QUOTA_REFRESH_CONCURRENCY}
              step={1}
              value={refreshConcurrencyInput}
              onChange={handleRefreshConcurrencyChange}
              onBlur={(event) => commitRefreshConcurrencyInput(event.currentTarget.value)}
              onKeyDown={(event) => {
                if (event.key === 'Enter') {
                  event.currentTarget.blur();
                }
              }}
              aria-label={t('quota_management.check_concurrency', { defaultValue: '刷新并发' })}
            />
          </label>
          <Button
            variant="secondary"
            size="sm"
            className={styles.refreshAllButton}
            onClick={handleRefresh}
            disabled={disabled || isRefreshing}
            loading={isRefreshing}
            title={t('quota_management.refresh_all_credentials')}
            aria-label={t('quota_management.refresh_all_credentials')}
          >
            {!isRefreshing && <IconRefreshCw size={16} />}
            {t('quota_management.refresh_all_credentials')}
          </Button>
          {isRefreshing && (
            <Button
              variant="danger"
              size="sm"
              className={styles.refreshAllButton}
              onClick={interruptRefresh}
            >
              {t('quota_management.interrupt_refresh', { defaultValue: '中断刷新' })}
            </Button>
          )}
        </div>
      }
    >
      {filteredFiles.length === 0 ? (
        <EmptyState
          title={t(`${config.i18nPrefix}.empty_title`)}
          description={t(`${config.i18nPrefix}.empty_desc`)}
        />
      ) : (
        <>
          {refreshTask.refreshing && (
            <div className={styles.statsInfo}>
              {t('quota_management.refresh_progress', {
                done: refreshTask.summary.done,
                total: refreshTask.summary.total,
                success: refreshTask.summary.success,
                failed: refreshTask.summary.failed,
                defaultValue: `刷新进度 ${refreshTask.summary.done}/${refreshTask.summary.total}，成功 ${refreshTask.summary.success}，失败 ${refreshTask.summary.failed}`,
              })}
            </div>
          )}
          <div ref={gridRef} className={config.gridClassName}>
            {pageItems.map((item) => (
              <QuotaCard
                key={item.name}
                item={item}
                quota={quota[item.name]}
                resolvedTheme={resolvedTheme}
                i18nPrefix={config.i18nPrefix}
                cardIdleMessageKey={config.cardIdleMessageKey}
                cardClassName={config.cardClassName}
                defaultType={config.type}
                canRefresh={!disabled && !item.disabled}
                onRefresh={() => void refreshQuotaForFile(item)}
                renderQuotaItems={config.renderQuotaItems}
              />
            ))}
          </div>
          {filteredFiles.length > pageSize && effectiveViewMode === 'paged' && (
            <div className={styles.pagination}>
              <Button
                variant="secondary"
                size="sm"
                onClick={goToPrev}
                disabled={currentPage <= 1}
              >
                {t('auth_files.pagination_prev')}
              </Button>
              <div className={styles.pageInfo}>
                {t('auth_files.pagination_info', {
                  current: currentPage,
                  total: totalPages,
                  count: filteredFiles.length
                })}
              </div>
              <Button
                variant="secondary"
                size="sm"
                onClick={goToNext}
                disabled={currentPage >= totalPages}
              >
                {t('auth_files.pagination_next')}
              </Button>
            </div>
          )}
        </>
      )}
      {showTooManyWarning && (
        <div className={styles.warningOverlay} onClick={() => setShowTooManyWarning(false)}>
          <div className={styles.warningModal} onClick={(e) => e.stopPropagation()}>
            <p>{t('auth_files.too_many_files_warning')}</p>
            <Button variant="primary" size="sm" onClick={() => setShowTooManyWarning(false)}>
              {t('common.confirm')}
            </Button>
          </div>
        </div>
      )}
    </Card>
  );
}
