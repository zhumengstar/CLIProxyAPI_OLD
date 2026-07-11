import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Card } from '@/components/ui/Card';
import { Button } from '@/components/ui/Button';
import { EmptyState } from '@/components/ui/EmptyState';
import { Select } from '@/components/ui/Select';
import { IconRefreshCw } from '@/components/ui/icons';
import { useNotificationStore } from '@/stores';
import type { AuthFileItem } from '@/types';
import { getStatusFromError } from '@/utils/quota';
import { normalizeUsageTotal, sumRecentRequests } from '@/utils/recentRequests';
import { quotaApi } from '@/services/api/quota';
import { antigravityQuotaStateApi } from '@/services/api/antigravityQuotaState';
import {
  ANTIGRAVITY_FILTER_LABELS,
  ANTIGRAVITY_POOL_LABELS,
  aggregateWindow,
  authIndexForFile,
  classifyAntigravityState,
  emptyAntigravityQuotaState,
  groupBelongsToPool,
  groupsForWindow,
  isCompleteAntigravityQuotaGroups,
  isAntigravityFileLike,
  manualPriorityForPool,
  normalizeAntigravityQuotaGroups,
  stateBelongsToPool,
  type AntigravityEnhancedQuotaState,
  type AntigravityFilter,
  type AntigravityPool,
  type AntigravityWindowSummary,
} from './antigravityQuota';
import styles from '@/pages/QuotaPage.module.scss';

type SortMode = 'weekly_quota_desc' | 'weekly_quota_asc' | 'weekly_reset_asc' | 'weekly_reset_desc' | 'name_asc';

const FILTERS: AntigravityFilter[] = [
  'all',
  'priority24',
  'priority48',
  'mid72',
  'reserve',
  'noquota',
  'unfetched',
  'invalid',
];

const SORT_OPTIONS = [
  { value: 'weekly_quota_desc', label: '周额度最高' },
  { value: 'weekly_quota_asc', label: '周额度最低' },
  { value: 'weekly_reset_asc', label: '周重置最近' },
  { value: 'weekly_reset_desc', label: '周重置最远' },
  { value: 'name_asc', label: '文件名' },
];

const INITIAL_RENDER_COUNT = 15;
const RENDER_BATCH_SIZE = 15;
const QUOTA_HIGH_THRESHOLD = 70;
const QUOTA_MEDIUM_THRESHOLD = 30;

interface AntigravityQuotaSectionProps {
  files: AuthFileItem[];
  loading: boolean;
  disabled: boolean;
  onFilesChanged?: () => void | Promise<void>;
}

type ManualPriorityOverrides = Record<string, Partial<Record<AntigravityPool, boolean>>>;

const nowLabel = () => new Date().toISOString();

const displayDateTime = (value: string | undefined): string => {
  if (!value) return '-';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return new Intl.DateTimeFormat('zh-CN', {
    timeZone: 'Asia/Shanghai',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    hour12: false,
  })
    .format(date)
    .replace(/\//g, '/');
};

const displayFullDateTime = (value: string | undefined): string => {
  if (!value) return '-';
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return new Intl.DateTimeFormat('zh-CN', {
    timeZone: 'Asia/Shanghai',
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
  })
    .format(date)
    .replace(/\//g, '/');
};

const displayCountdown = (value: string | undefined, nowMs: number): string => {
  if (!value) return '-';
  const resetMs = new Date(value).getTime();
  if (!Number.isFinite(resetMs)) return '-';
  const diffMs = resetMs - nowMs;
  if (diffMs <= 0) return '已到重置时间';
  const totalMinutes = Math.ceil(diffMs / 60_000);
  const days = Math.floor(totalMinutes / 1440);
  const hours = Math.floor((totalMinutes % 1440) / 60);
  const minutes = totalMinutes % 60;
  if (days > 0) return `还有 ${days} 天 ${hours} 小时`;
  if (hours > 0) return `还有 ${hours} 小时 ${minutes} 分钟`;
  return `还有 ${minutes} 分钟`;
};

const percentForSummary = (summary: AntigravityWindowSummary): number | null => summary.percent;

const scoreForFile = (
  state: AntigravityEnhancedQuotaState | undefined,
  pool: AntigravityPool
): { percent: number; resetMs: number } => {
  const groups = groupsForWindow(state, pool, 'weekly');
  const percents = groups
    .map((group) => group.remainingFraction)
    .filter((value): value is number => value !== null)
    .map((value) => value * 100);
  const resetTimes = groups
    .map((group) => (group.resetTime ? new Date(group.resetTime).getTime() : Number.NaN))
    .filter((value) => Number.isFinite(value));
  return {
    percent: percents.length > 0 ? Math.min(...percents) : -1,
    resetMs: resetTimes.length > 0 ? Math.min(...resetTimes) : Number.POSITIVE_INFINITY,
  };
};

const errorMessageFromUnknown = (err: unknown): string =>
  err instanceof Error ? err.message : '未知错误';

const successCountForFile = (file: AuthFileItem): number => {
  const direct = normalizeUsageTotal(file.success);
  if (direct > 0) return direct;
  const recent = file.recent_requests ?? file.recentRequests ?? [];
  return sumRecentRequests(recent).success;
};

const isEnabledFile = (file: AuthFileItem): boolean => {
  if (file.disabled || file.unavailable) return false;
  if (String(file.status ?? '').trim().toLowerCase() === 'disabled') return false;
  return true;
};

const buildStateFromResponse = (
  data: Record<string, unknown>
): AntigravityEnhancedQuotaState => {
  const candidates = [data.groups, data.quota_summary, data.models];
  let groups = [] as ReturnType<typeof normalizeAntigravityQuotaGroups>;
  for (const candidate of candidates) {
    const normalized = normalizeAntigravityQuotaGroups(candidate);
    if (normalized.length > groups.length) groups = normalized;
    if (isCompleteAntigravityQuotaGroups(normalized)) {
      groups = normalized;
      break;
    }
  }
  if (!isCompleteAntigravityQuotaGroups(groups)) {
    throw new Error('额度响应不完整，未同时取得 Gemini 与 Claude/GPT 的 5 小时和周额度');
  }
  return {
    status: 'success',
    groups,
    refreshedAt: nowLabel(),
    accountLevel:
      String(
          data.account_tier_label ??
          data.account_tier_name ??
          data.account_tier_id ??
          ''
      ).trim() || undefined,
  };
};

const mergeErrorState = (
  message: string,
  status: number | undefined,
  previous: AntigravityEnhancedQuotaState | undefined
): AntigravityEnhancedQuotaState => ({
  status: 'error',
  groups: previous?.groups ?? [],
  refreshedAt: nowLabel(),
  accountLevel: previous?.accountLevel,
  error: message,
  errorStatus: status,
});

export function AntigravityQuotaSection({
  files,
  loading,
  disabled,
  onFilesChanged,
}: AntigravityQuotaSectionProps) {
  const { t } = useTranslation();
  const showNotification = useNotificationStore((state) => state.showNotification);
  const [quota, setQuota] = useState<Record<string, AntigravityEnhancedQuotaState>>({});
  const [pool, setPool] = useState<AntigravityPool>('gemini');
  const [filter, setFilter] = useState<AntigravityFilter>('all');
  const [sortMode, setSortMode] = useState<SortMode>('weekly_quota_desc');
  const [refreshConcurrency, setRefreshConcurrency] = useState('5');
  const [visibleCount, setVisibleCount] = useState(INITIAL_RENDER_COUNT);
  const [refreshingNames, setRefreshingNames] = useState<Set<string>>(() => new Set());
  const [priorityUpdatingNames, setPriorityUpdatingNames] = useState<Set<string>>(() => new Set());
  const [manualPriorityOverrides, setManualPriorityOverrides] =
    useState<ManualPriorityOverrides>({});
  const saveTimerRef = useRef<number | null>(null);
  const quotaRef = useRef(quota);

  useEffect(() => {
    quotaRef.current = quota;
  }, [quota]);

  const quotaFiles = useMemo(() => files.filter(isAntigravityFileLike), [files]);

  // A file-list reload is the authoritative reconciliation point. Optimistic
  // pin state only exists between a successful PATCH and that reload, so an
  // automatic server-side unpin (for example, depleted quota) still wins.
  useEffect(() => {
    setManualPriorityOverrides({});
  }, [files]);

  const isManualPriorityEnabled = useCallback(
    (file: AuthFileItem, targetPool: AntigravityPool): boolean =>
      manualPriorityOverrides[file.name]?.[targetPool] ?? manualPriorityForPool(file, targetPool),
    [manualPriorityOverrides]
  );

  useEffect(() => {
    if (loading) return;
    let cancelled = false;
    antigravityQuotaStateApi
      .get()
      .then((snapshot) => {
        if (cancelled) return;
        setQuota((prev) => ({ ...prev, ...snapshot }));
      })
      .catch(() => undefined);
    return () => {
      cancelled = true;
    };
  }, [loading]);

  const scheduleSave = useCallback((nextQuota: Record<string, AntigravityEnhancedQuotaState>) => {
    if (saveTimerRef.current !== null) window.clearTimeout(saveTimerRef.current);
    saveTimerRef.current = window.setTimeout(() => {
      saveTimerRef.current = null;
      void antigravityQuotaStateApi.save(nextQuota).catch(() => undefined);
    }, 350);
  }, []);

  useEffect(
    () => () => {
      if (saveTimerRef.current !== null) window.clearTimeout(saveTimerRef.current);
    },
    []
  );

  const nowMs = Date.now();

  const poolFiles = useMemo(
    () =>
      quotaFiles.filter((file) =>
        stateBelongsToPool(quota[file.name] ?? emptyAntigravityQuotaState(), pool)
      ),
    [pool, quota, quotaFiles]
  );

  // Pool tabs and filter chips must use the same membership rule. The page badge
  // remains the total number of Antigravity authentication files.
  const poolFileCounts = useMemo(
    () =>
      Object.fromEntries(
        (['gemini', 'claude-gpt'] as AntigravityPool[]).map((item) => [
          item,
          quotaFiles.filter((file) =>
            stateBelongsToPool(quota[file.name] ?? emptyAntigravityQuotaState(), item)
          ).length,
        ])
      ) as Record<AntigravityPool, number>,
    [quota, quotaFiles]
  );

  const counts = useMemo(() => {
    const next = Object.fromEntries(FILTERS.map((item) => [item, 0])) as Record<
      AntigravityFilter,
      number
    >;
    poolFiles.forEach((file) => {
      const state = quota[file.name] ?? emptyAntigravityQuotaState();
      next.all += 1;
      next[classifyAntigravityState(state, pool, nowMs)] += 1;
    });
    return next;
  }, [nowMs, pool, poolFiles, quota]);

  const filteredFiles = useMemo(() => {
    const base =
      filter === 'all'
        ? poolFiles
        : poolFiles.filter((file) => {
            const state = quota[file.name] ?? emptyAntigravityQuotaState();
            return classifyAntigravityState(state, pool, nowMs) === filter;
          });

    return [...base].sort((a, b) => {
      if (sortMode === 'name_asc') return a.name.localeCompare(b.name);
      const aScore = scoreForFile(quota[a.name], pool);
      const bScore = scoreForFile(quota[b.name], pool);
      if (sortMode === 'weekly_quota_asc') return aScore.percent - bScore.percent;
      if (sortMode === 'weekly_reset_asc') return aScore.resetMs - bScore.resetMs;
      if (sortMode === 'weekly_reset_desc') return bScore.resetMs - aScore.resetMs;
      return bScore.percent - aScore.percent;
    });
  }, [filter, nowMs, pool, poolFiles, quota, sortMode]);

  useEffect(() => {
    setVisibleCount(Math.min(INITIAL_RENDER_COUNT, filteredFiles.length));
  }, [filteredFiles.length, filter, pool, sortMode]);

  useEffect(() => {
    if (visibleCount >= filteredFiles.length) return;
    const frame = window.requestAnimationFrame(() => {
      setVisibleCount((current) => Math.min(filteredFiles.length, current + RENDER_BATCH_SIZE));
    });
    return () => window.cancelAnimationFrame(frame);
  }, [filteredFiles.length, visibleCount]);

  const visibleFiles = filteredFiles.slice(0, visibleCount);

  const updateQuotaForFile = useCallback(
    (name: string, updater: (previous: AntigravityEnhancedQuotaState | undefined) => AntigravityEnhancedQuotaState) => {
      setQuota((prev) => {
        const next = { ...prev, [name]: updater(prev[name]) };
        scheduleSave(next);
        return next;
      });
    },
    [scheduleSave]
  );

  const refreshFile = useCallback(
    async (file: AuthFileItem, silent = false) => {
      if (disabled || !isEnabledFile(file)) return;
      const authIndex = authIndexForFile(file);
      if (!authIndex) return;
      setRefreshingNames((prev) => new Set(prev).add(file.name));
      updateQuotaForFile(file.name, (previous) => ({
        ...(previous ?? emptyAntigravityQuotaState()),
        status: 'loading',
      }));
      try {
        const data = await quotaApi.reset(authIndex);
        // Parse and validate before scheduling a React state update so an
        // incomplete response reaches the error path instead of looking successful.
        const nextState = buildStateFromResponse(data as Record<string, unknown>);
        updateQuotaForFile(file.name, () => nextState);
        if (!silent) showNotification(`刷新 "${file.name}" 的额度成功`, 'success');
      } catch (err: unknown) {
        const message = errorMessageFromUnknown(err);
        updateQuotaForFile(file.name, (previous) =>
          mergeErrorState(message, getStatusFromError(err), previous)
        );
        if (!silent) showNotification(`刷新 "${file.name}" 的额度失败：${message}`, 'error');
      } finally {
        setRefreshingNames((prev) => {
          const next = new Set(prev);
          next.delete(file.name);
          return next;
        });
      }
    },
    [disabled, showNotification, updateQuotaForFile]
  );

  const refreshFilteredFiles = useCallback(async () => {
    const targets = filteredFiles.filter((file) => isEnabledFile(file));
    if (targets.length === 0) return;
    const concurrency = Math.max(1, Math.min(20, Number(refreshConcurrency) || 5));
    let cursor = 0;
    const workers = Array.from({ length: Math.min(concurrency, targets.length) }, async () => {
      while (cursor < targets.length) {
        const target = targets[cursor];
        cursor += 1;
        await refreshFile(target, true);
      }
    });
    await Promise.all(workers);
    showNotification(`已刷新当前筛选范围 ${targets.length} 个 Antigravity 凭证`, 'success');
    onFilesChanged?.();
  }, [filteredFiles, onFilesChanged, refreshConcurrency, refreshFile, showNotification]);

  const toggleManualPriority = useCallback(
    async (file: AuthFileItem) => {
      if (disabled || !isEnabledFile(file)) return;
      const current = isManualPriorityEnabled(file, pool);
      setPriorityUpdatingNames((prev) => new Set(prev).add(file.name));
      try {
        await antigravityQuotaStateApi.setManualPriority(file.name, pool, !current);
        setManualPriorityOverrides((prev) => ({
          ...prev,
          [file.name]: { ...prev[file.name], [pool]: !current },
        }));
        showNotification(!current ? `已置顶 ${file.name}` : `已取消置顶 ${file.name}`, 'success');
        await onFilesChanged?.();
      } catch (err: unknown) {
        showNotification(`置顶更新失败：${errorMessageFromUnknown(err)}`, 'error');
      } finally {
        setPriorityUpdatingNames((prev) => {
          const next = new Set(prev);
          next.delete(file.name);
          return next;
        });
      }
    },
    [disabled, isManualPriorityEnabled, onFilesChanged, pool, showNotification]
  );

  const titleNode = (
    <div className={styles.titleWrapper}>
      <span>Antigravity 额度</span>
      {quotaFiles.length > 0 && <span className={styles.countBadge}>{quotaFiles.length}</span>}
    </div>
  );

  const isRefreshing = refreshingNames.size > 0;

  return (
    <Card
      title={titleNode}
      extra={
        <div className={styles.headerActions}>
          <span className={styles.inlineControlLabel}>排序</span>
          <Select
            value={sortMode}
            options={SORT_OPTIONS}
            onChange={(value) => setSortMode(value as SortMode)}
            fullWidth={false}
            size="sm"
            ariaLabel="Antigravity 排序"
          />
          <span className={styles.inlineControlLabel}>刷新并发</span>
          <input
            className={styles.refreshConcurrencyInput}
            value={refreshConcurrency}
            onChange={(event) => setRefreshConcurrency(event.target.value)}
            inputMode="numeric"
            aria-label="刷新并发"
          />
          <Button
            variant="secondary"
            size="sm"
            className={styles.refreshAllButton}
            onClick={() => void refreshFilteredFiles()}
            disabled={disabled || isRefreshing || filteredFiles.length === 0}
            loading={isRefreshing}
            title="刷新当前筛选范围"
          >
            {!isRefreshing && <IconRefreshCw size={16} />}
            刷新全部凭证
          </Button>
        </div>
      }
    >
      {quotaFiles.length === 0 ? (
        <EmptyState
          title={t('antigravity_quota.empty_title')}
          description={t('antigravity_quota.empty_desc')}
        />
      ) : (
        <>
          <div className={styles.antigravityNativeControls}>
            <div className={styles.antigravityPoolTabs}>
              {(['gemini', 'claude-gpt'] as AntigravityPool[]).map((item) => (
                <Button
                  key={item}
                  variant="secondary"
                  size="sm"
                  className={`${styles.antigravityPoolTab} ${
                    pool === item ? styles.antigravityPoolTabActive : ''
                  }`}
                  onClick={() => {
                    setPool(item);
                    setFilter('all');
                  }}
                >
                  {ANTIGRAVITY_POOL_LABELS[item]} {poolFileCounts[item]}
                </Button>
              ))}
            </div>
            <div className={styles.antigravityFilterTabs}>
              {FILTERS.map((item) => (
                <Button
                  key={item}
                  variant="secondary"
                  size="sm"
                  className={`${styles.antigravityFilterTab} ${
                    filter === item ? styles.antigravityFilterTabActive : ''
                  }`}
                  onClick={() => setFilter(item)}
                >
                  {ANTIGRAVITY_FILTER_LABELS[item]} {counts[item]}
                </Button>
              ))}
            </div>
            <div className={styles.antigravityRenderStatus}>
              {ANTIGRAVITY_POOL_LABELS[pool]}：筛选后 {filteredFiles.length} 个，当前展示{' '}
              {visibleFiles.length} 个
            </div>
          </div>

          {filteredFiles.length === 0 ? (
            <EmptyState title="暂无匹配凭证" description="当前池子和标签下没有匹配的认证文件。" />
          ) : (
            <div className={styles.antigravityGrid}>
              {visibleFiles.map((file) => {
                const state = quota[file.name] ?? emptyAntigravityQuotaState();
                return (
                  <AntigravityQuotaCard
                    key={file.name}
                    file={file}
                    state={state}
                    pool={pool}
                    pinned={isManualPriorityEnabled(file, pool)}
                    refreshing={refreshingNames.has(file.name)}
                    priorityUpdating={priorityUpdatingNames.has(file.name)}
                    disabled={disabled}
                    nowMs={nowMs}
                    onRefresh={() => void refreshFile(file)}
                    onTogglePriority={() => void toggleManualPriority(file)}
                  />
                );
              })}
            </div>
          )}
        </>
      )}
    </Card>
  );
}

interface AntigravityQuotaCardProps {
  file: AuthFileItem;
  state: AntigravityEnhancedQuotaState;
  pool: AntigravityPool;
  pinned: boolean;
  refreshing: boolean;
  priorityUpdating: boolean;
  disabled: boolean;
  nowMs: number;
  onRefresh: () => void;
  onTogglePriority: () => void;
}

function AntigravityQuotaCard({
  file,
  state,
  pool,
  pinned,
  refreshing,
  priorityUpdating,
  disabled,
  nowMs,
  onRefresh,
  onTogglePriority,
}: AntigravityQuotaCardProps) {
  const enabled = isEnabledFile(file);
  const rows = [
    aggregateWindow(state, 'gemini', 'five-hour', 'Gemini · 5 小时'),
    aggregateWindow(state, 'gemini', 'weekly', 'Gemini · 周'),
    aggregateWindow(state, 'claude-gpt', 'five-hour', 'Claude/GPT · 5 小时'),
    aggregateWindow(state, 'claude-gpt', 'weekly', 'Claude/GPT · 周'),
  ];
  const cardMatchesPool =
    state.groups.length === 0 || state.groups.some((group) => groupBelongsToPool(group, pool));
  const showRefreshHint = state.status === 'idle';

  return (
    <div className={`${styles.fileCard} ${styles.antigravityCard}`}>
      <div className={styles.cardHeader}>
        <span className={styles.typeBadge} style={{ backgroundColor: '#dff8fb', color: '#117780' }}>
          Antigravity
        </span>
        <span className={styles.fileName}>{file.name}</span>
      </div>

      <div className={styles.antigravityMetaRow}>
        <span>账号类型：{String(file.auth_type ?? file.authType ?? 'OAuth')}</span>
        <span>级别：{state.accountLevel || '-'}</span>
        <span className={enabled ? styles.accountEnabled : styles.accountDisabled}>
          {enabled ? '已启用' : '已停用'}
        </span>
        {!cardMatchesPool && <span className={styles.accountDisabled}>非当前池</span>}
      </div>

      <div className={styles.quotaSection}>
        {state.status === 'loading' ? (
          <div className={styles.quotaMessage}>正在刷新额度...</div>
        ) : showRefreshHint ? (
          <button
            type="button"
            className={`${styles.quotaMessage} ${styles.quotaMessageAction}`}
            onClick={onRefresh}
            disabled={disabled || !enabled}
          >
            点击此处刷新额度
          </button>
        ) : null}
        {state.status === 'error' && state.error && (
          <div className={styles.quotaError}>额度获取失败：{state.error}</div>
        )}
        {rows.map((row) => (
          <QuotaWindowRow key={row.label} row={row} nowMs={nowMs} />
        ))}
      </div>

      <div className={styles.quotaCardFooter}>
        <div className={styles.quotaSuccessCount}>请求成功 {successCountForFile(file)}</div>
        <div className={styles.quotaCardFooterActions}>
          <Button
            type="button"
            variant={pinned ? 'primary' : 'secondary'}
            size="sm"
            className={styles.quotaPriorityButton}
            onClick={onTogglePriority}
            disabled={disabled || !enabled || priorityUpdating}
            loading={priorityUpdating}
          >
            {pinned ? '已置顶' : '置顶'}
          </Button>
          <button
            type="button"
            className={styles.quotaCardRefreshButton}
            onClick={onRefresh}
            disabled={disabled || !enabled || refreshing}
            title="刷新额度"
            aria-label="刷新额度"
          >
            <IconRefreshCw
              size={15}
              className={refreshing ? styles.quotaCardRefreshIconLoading : undefined}
            />
          </button>
        </div>
      </div>
      <div className={styles.quotaRefreshedAt}>额度刷新：{displayFullDateTime(state.refreshedAt)}</div>
    </div>
  );
}

function QuotaWindowRow({
  row,
  nowMs,
}: {
  row: AntigravityWindowSummary;
  nowMs: number;
}) {
  const percent = percentForSummary(row);
  const normalized = percent === null ? null : Math.max(0, Math.min(100, percent));
  const fillClass =
    normalized === null
      ? styles.quotaBarFillMedium
      : normalized >= QUOTA_HIGH_THRESHOLD
        ? styles.quotaBarFillHigh
        : normalized >= QUOTA_MEDIUM_THRESHOLD
          ? styles.quotaBarFillMedium
          : styles.quotaBarFillLow;

  return (
    <div className={styles.quotaRow}>
      <div className={styles.quotaRowHeader}>
        <span className={styles.quotaModel}>{row.label}</span>
        <span className={styles.quotaMeta}>
          <span className={styles.quotaPercent}>{normalized === null ? '-' : `${normalized}%`}</span>
          <span className={styles.quotaReset}>重置时间：{displayDateTime(row.resetTime)}</span>
          <span className={styles.quotaReset}>{displayCountdown(row.resetTime, nowMs)}</span>
        </span>
      </div>
      <div className={styles.quotaBar}>
        <div
          className={`${styles.quotaBarFill} ${fillClass}`}
          style={{ width: `${normalized ?? 0}%` }}
        />
      </div>
    </div>
  );
}
