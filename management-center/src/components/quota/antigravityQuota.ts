import type { AuthFileItem } from '@/types';

export type AntigravityPool = 'gemini' | 'claude-gpt';
export type AntigravityWindow = 'five-hour' | 'weekly';
export type AntigravityFilter =
  | 'all'
  | 'priority24'
  | 'priority48'
  | 'mid72'
  | 'reserve'
  | 'noquota'
  | 'unfetched'
  | 'invalid';

export interface AntigravityFlatQuotaGroup {
  id: string;
  label: string;
  models: string[];
  remainingFraction: number | null;
  resetTime?: string;
}

export interface AntigravityEnhancedQuotaState {
  status: 'idle' | 'loading' | 'success' | 'error';
  groups: AntigravityFlatQuotaGroup[];
  refreshedAt?: string;
  accountLevel?: string;
  error?: string;
  errorStatus?: number;
}

export interface AntigravityWindowSummary {
  label: string;
  percent: number | null;
  resetTime?: string;
}

export const ANTIGRAVITY_POOL_LABELS: Record<AntigravityPool, string> = {
  gemini: 'Gemini 池',
  'claude-gpt': 'Claude / GPT 池',
};

export const ANTIGRAVITY_FILTER_LABELS: Record<AntigravityFilter, string> = {
  all: '全部',
  priority24: '24h优先',
  priority48: '48h优先',
  mid72: '48-72h',
  reserve: '储备',
  noquota: '无额度',
  unfetched: '未拉取',
  invalid: '失效',
};

// The official quota API uses both human labels ("Five Hour Limit") and
// compact bucket identifiers such as "gemini-5h" / "3p-5h".
const SHORT_WINDOW_HINTS = ['5h', '5 h', '5 hour', '5-hour', 'five hour', 'five-hour', '5小时', '5 小时', 'hour'];
const WEEKLY_WINDOW_HINTS = ['week', 'weekly', '周', '7 day', '7-day'];

export const emptyAntigravityQuotaState = (): AntigravityEnhancedQuotaState => ({
  status: 'idle',
  groups: [],
});

export const normalizeAntigravityPool = (value: unknown): AntigravityPool | null => {
  const text = String(value ?? '').trim().toLowerCase().replace(/_/g, '-');
  if (!text) return null;
  if (text.includes('gemini')) return 'gemini';
  if (text.includes('claude') || text.includes('gpt') || text.includes('oss')) return 'claude-gpt';
  return null;
};

export const normalizeAntigravityWindow = (value: unknown): AntigravityWindow | null => {
  const text = String(value ?? '').trim().toLowerCase();
  if (!text) return null;
  if (SHORT_WINDOW_HINTS.some((hint) => text.includes(hint))) return 'five-hour';
  if (WEEKLY_WINDOW_HINTS.some((hint) => text.includes(hint))) return 'weekly';
  return null;
};

const toRecord = (value: unknown): Record<string, unknown> | null =>
  value && typeof value === 'object' && !Array.isArray(value) ? (value as Record<string, unknown>) : null;

const toFiniteNumber = (value: unknown): number | null => {
  const numberValue = typeof value === 'number' ? value : Number(value);
  return Number.isFinite(numberValue) ? numberValue : null;
};

const normalizeRemainingFraction = (value: unknown): number | null => {
  const numberValue = toFiniteNumber(value);
  if (numberValue === null) return null;
  if (numberValue > 1) return Math.max(0, Math.min(1, numberValue / 100));
  return Math.max(0, Math.min(1, numberValue));
};

const normalizeModels = (value: unknown): string[] => {
  if (Array.isArray(value)) {
    return value
      .map((item) => String(item ?? '').trim())
      .filter(Boolean);
  }
  if (typeof value === 'string') {
    return value
      .split(',')
      .map((item) => item.trim())
      .filter(Boolean);
  }
  return [];
};

const firstNonEmptyText = (...values: unknown[]): string => {
  for (const value of values) {
    const text = String(value ?? '').trim();
    if (text) return text;
  }
  return '';
};

export const normalizeAntigravityQuotaGroups = (
  input: unknown
): AntigravityFlatQuotaGroup[] => {
  const groups = Array.isArray(input) ? input : [];
  const flat: AntigravityFlatQuotaGroup[] = [];

  groups.forEach((rawGroup, groupIndex) => {
    const group = toRecord(rawGroup);
    if (!group) return;
    const groupID = firstNonEmptyText(group.id, group.group_id, group.groupId, group.family);
    const groupLabel = firstNonEmptyText(
      group.label,
      group.name,
      group.display_name,
      group.displayName,
      group.group_label,
      group.groupLabel,
      group.family,
      groupID,
      `quota-${groupIndex}`
    );
    const groupModels = normalizeModels(
      group.models ?? group.model_names ?? group.modelNames ?? group.model_id ?? group.model
    );
    const groupResetTime =
      firstNonEmptyText(group.reset_time, group.resetTime, group.reset_at, group.resetAt) ||
      undefined;
    const groupRemaining = normalizeRemainingFraction(
      group.remaining_fraction ??
        group.remainingFraction ??
        group.remaining_percent ??
        group.remainingPercent ??
        group.percent
    );

    const buckets = Array.isArray(group.buckets) ? group.buckets : [];
    if (buckets.length > 0) {
      buckets.forEach((rawBucket, bucketIndex) => {
        const bucket = toRecord(rawBucket);
        if (!bucket) return;
        const bucketID = firstNonEmptyText(
          bucket.id,
          bucket.bucket_id,
          bucket.bucketId,
          `${groupID || groupLabel}-${bucketIndex}`
        );
        const label = firstNonEmptyText(
          bucket.label,
          bucket.window,
          bucket.name,
          bucket.display_name,
          bucket.displayName,
          `${groupLabel}-${bucketIndex}`
        );
        flat.push({
          id: bucketID,
          label: `${groupLabel} ${label}`.trim(),
          models: normalizeModels(bucket.models ?? bucket.model_names ?? bucket.modelNames).concat(
            groupModels
          ),
          remainingFraction: normalizeRemainingFraction(
            bucket.remaining_fraction ??
              bucket.remainingFraction ??
              bucket.remaining_percent ??
              bucket.remainingPercent ??
              bucket.percent
          ),
          resetTime:
            firstNonEmptyText(
              bucket.reset_time,
              bucket.resetTime,
              bucket.reset_at,
              bucket.resetAt,
              groupResetTime
            ) || undefined,
        });
      });
      return;
    }

    flat.push({
      id: firstNonEmptyText(groupID, group.bucket_id, group.bucketId, groupLabel),
      label: `${groupLabel} ${firstNonEmptyText(group.scope, group.window)}`.trim(),
      models: groupModels,
      remainingFraction: groupRemaining,
      resetTime: groupResetTime,
    });
  });

  return flat;
};

export const isCompleteAntigravityQuotaGroups = (
  groups: AntigravityFlatQuotaGroup[]
): boolean => {
  const windows = new Map<string, { remaining: boolean; reset: boolean }>();
  groups.forEach((group) => {
    const pool = normalizeAntigravityPool(`${group.id} ${group.label} ${group.models.join(' ')}`);
    const window = normalizeAntigravityWindow(`${group.id} ${group.label}`);
    if (!pool || !window) return;
    const key = `${pool}-${window}`;
    const current = windows.get(key) ?? { remaining: false, reset: false };
    current.remaining ||= group.remainingFraction !== null;
    current.reset ||= Boolean(group.resetTime);
    windows.set(key, current);
  });

  return (['gemini', 'claude-gpt'] as AntigravityPool[]).every((pool) =>
    (['five-hour', 'weekly'] as AntigravityWindow[]).every((window) => {
      const state = windows.get(`${pool}-${window}`);
      return Boolean(state?.remaining && (window !== 'weekly' || state.reset));
    })
  );
};

export const groupBelongsToPool = (
  group: AntigravityFlatQuotaGroup,
  pool: AntigravityPool
): boolean => {
  const text = `${group.id} ${group.label} ${group.models.join(' ')}`.toLowerCase();
  if (pool === 'gemini') return text.includes('gemini');
  return text.includes('claude') || text.includes('gpt') || text.includes('oss');
};

export const groupWindow = (group: AntigravityFlatQuotaGroup): AntigravityWindow | null =>
  normalizeAntigravityWindow(`${group.id} ${group.label}`);

export const stateBelongsToPool = (
  state: AntigravityEnhancedQuotaState | undefined,
  pool: AntigravityPool
): boolean => {
  if (!state || state.groups.length === 0) return true;
  const hasRecognizedPool = state.groups.some(
    (group) => groupBelongsToPool(group, 'gemini') || groupBelongsToPool(group, 'claude-gpt')
  );
  // An incomplete response must remain visible in both pools so it can be
  // refreshed and classified as unfetched/invalid instead of disappearing.
  if (!hasRecognizedPool) return true;
  return state.groups.some((group) => groupBelongsToPool(group, pool));
};

export const groupsForPool = (
  state: AntigravityEnhancedQuotaState | undefined,
  pool: AntigravityPool
): AntigravityFlatQuotaGroup[] => {
  if (!state) return [];
  return state.groups.filter((group) => groupBelongsToPool(group, pool));
};

export const groupsForWindow = (
  state: AntigravityEnhancedQuotaState | undefined,
  pool: AntigravityPool,
  window: AntigravityWindow
): AntigravityFlatQuotaGroup[] =>
  groupsForPool(state, pool).filter((group) => groupWindow(group) === window);

export const aggregateWindow = (
  state: AntigravityEnhancedQuotaState | undefined,
  pool: AntigravityPool,
  window: AntigravityWindow,
  label: string
): AntigravityWindowSummary => {
  const groups = groupsForWindow(state, pool, window);
  const knownGroups = groups.filter((group) => group.remainingFraction !== null);
  if (knownGroups.length === 0) return { label, percent: null };
  const minRemaining = Math.min(...knownGroups.map((group) => group.remainingFraction ?? 0));
  const resetCandidates = knownGroups
    .map((group) => group.resetTime)
    .filter((value): value is string => Boolean(value))
    .sort((a, b) => new Date(a).getTime() - new Date(b).getTime());
  return {
    label,
    percent: Math.round(minRemaining * 100),
    resetTime: resetCandidates[0],
  };
};

export const weeklyGroupsForFilter = (
  state: AntigravityEnhancedQuotaState | undefined,
  pool: AntigravityPool
): AntigravityFlatQuotaGroup[] => groupsForWindow(state, pool, 'weekly');

export const classifyAntigravityState = (
  state: AntigravityEnhancedQuotaState | undefined,
  pool: AntigravityPool,
  nowMs: number
): AntigravityFilter => {
  if (!state || state.status === 'idle' || state.status === 'loading') return 'unfetched';
  if (state.status === 'error') return 'invalid';
  const weeklyGroups = weeklyGroupsForFilter(state, pool);
  if (weeklyGroups.length === 0) return 'unfetched';

  const known = weeklyGroups.filter((group) => group.remainingFraction !== null);
  if (known.length === 0) return 'unfetched';
  const minRemainingPercent = Math.min(...known.map((group) => (group.remainingFraction ?? 0) * 100));
  if (minRemainingPercent <= 2) return 'noquota';

  const resetTimes = known
    .map((group) => (group.resetTime ? new Date(group.resetTime).getTime() : Number.NaN))
    .filter((value) => Number.isFinite(value) && value > nowMs);
  if (resetTimes.length === 0) return 'reserve';

  const hoursUntilReset = (Math.min(...resetTimes) - nowMs) / 3_600_000;
  if (hoursUntilReset <= 24) return 'priority24';
  if (hoursUntilReset <= 48) return 'priority48';
  if (hoursUntilReset <= 72) return 'mid72';
  return 'reserve';
};

export const isAntigravityFileLike = (file: AuthFileItem): boolean => {
  const raw = String(file.provider ?? file.type ?? '').trim().toLowerCase().replace(/_/g, '-');
  return raw === 'antigravity';
};

export const authIndexForFile = (file: AuthFileItem): string =>
  String(file.authIndex ?? file.auth_index ?? file.index ?? file.name ?? '').trim();

export const manualPriorityForPool = (
  file: AuthFileItem,
  pool: AntigravityPool
): boolean => {
  const key = pool === 'gemini' ? 'manual_weekly_priority_gemini' : 'manual_weekly_priority_claude_gpt';
  const direct = file[key];
  if (typeof direct === 'boolean') return direct;
  if (typeof direct === 'string') return direct.toLowerCase() === 'true';
  const legacy = file.manual_weekly_priority ?? file.manualWeeklyPriority;
  if (typeof legacy === 'boolean') return legacy;
  if (typeof legacy === 'string') return legacy.toLowerCase() === 'true';
  return false;
};
