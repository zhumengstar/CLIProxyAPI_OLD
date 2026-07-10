import type { AntigravityQuotaGroup, AntigravityQuotaState } from '@/types';

export type AntigravityPool = 'gemini' | 'claude-gpt';
export type AntigravityWindow = 'five-hour' | 'weekly';

const groupDescriptor = (group: AntigravityQuotaGroup): string =>
  [group.id, group.label, ...group.models].join(' ').toLowerCase();

export const antigravityGroupBelongsToPool = (
  group: AntigravityQuotaGroup,
  pool: AntigravityPool
): boolean => {
  const descriptor = groupDescriptor(group);
  return pool === 'gemini' ? /gemini/.test(descriptor) : /claude|gpt/.test(descriptor);
};

export const antigravityWindowForGroup = (
  group: AntigravityQuotaGroup,
  now = Date.now()
): AntigravityWindow | null => {
  const descriptor = groupDescriptor(group).replace(/[_-]+/g, ' ');
  if (/weekly|\bweek\b|p?7\s*d\b|\b168\s*h\b|周/.test(descriptor)) return 'weekly';
  if (/p?t?5\s*h\b|\b5\s*hours?\b|five\s*hours?|五小时|short/.test(descriptor)) {
    return 'five-hour';
  }

  if (!group.resetTime) return null;
  const resetAt = Date.parse(group.resetTime);
  if (!Number.isFinite(resetAt)) return null;
  return resetAt - now >= 24 * 60 * 60 * 1000 ? 'weekly' : 'five-hour';
};

export const antigravityGroupsForPool = (
  state: AntigravityQuotaState,
  pool: AntigravityPool
): AntigravityQuotaGroup[] =>
  state.groups.filter((group) => antigravityGroupBelongsToPool(group, pool));

export const antigravityGroupsForWindow = (
  state: AntigravityQuotaState,
  pool: AntigravityPool,
  window: AntigravityWindow,
  now = Date.now()
): AntigravityQuotaGroup[] =>
  antigravityGroupsForPool(state, pool).filter(
    (group) => antigravityWindowForGroup(group, now) === window
  );

export const aggregateAntigravityWindow = (
  groups: AntigravityQuotaGroup[],
  window: AntigravityWindow,
  now = Date.now()
) => {
  const windowGroups = groups.filter((group) => antigravityWindowForGroup(group, now) === window);
  const fractions = windowGroups.map((group) => group.remainingFraction).filter(Number.isFinite);
  const resetTimes = windowGroups
    .map((group) => group.resetTime)
    .filter((value): value is string => Boolean(value))
    .sort((left, right) => Date.parse(left) - Date.parse(right));

  return {
    groups: windowGroups,
    remainingFraction: fractions.length > 0 ? Math.min(...fractions) : null,
    resetTime: resetTimes[0],
  };
};
