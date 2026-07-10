import { apiClient } from './client';
import type { AntigravityQuotaGroup, AntigravityQuotaState } from '@/types';

const endpoint = '/v0/management/antigravity-quota-state';

export type AntigravityQuotaSnapshot = {
  files: Record<string, unknown>;
  saved_at?: string;
  quota_refreshed_at?: string;
};

type LegacyBucket = {
  bucketId?: unknown;
  window?: unknown;
  displayName?: unknown;
  remainingFraction?: unknown;
  remaining_fraction?: unknown;
  resetTime?: unknown;
  reset_time?: unknown;
};

type LegacyGroup = {
  id?: unknown;
  label?: unknown;
  displayName?: unknown;
  description?: unknown;
  models?: unknown;
  remainingFraction?: unknown;
  resetTime?: unknown;
  buckets?: unknown;
};

const asRecord = (value: unknown): Record<string, unknown> | null =>
  value && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : null;

const asString = (value: unknown): string | undefined =>
  typeof value === 'string' && value.trim() ? value : undefined;

const asFraction = (value: unknown): number | undefined =>
  typeof value === 'number' && Number.isFinite(value) ? Math.max(0, Math.min(1, value)) : undefined;

const normalizeGroup = (group: LegacyGroup, index: number): AntigravityQuotaGroup[] => {
  const groupLabel = asString(group.label) ?? asString(group.displayName) ?? `额度组 ${index + 1}`;
  const models = Array.isArray(group.models)
    ? group.models.filter((model): model is string => typeof model === 'string')
    : [asString(group.description) ?? groupLabel];
  const buckets = Array.isArray(group.buckets) ? group.buckets : [];
  if (buckets.length === 0) {
    const remainingFraction = asFraction(group.remainingFraction);
    return remainingFraction === undefined ? [] : [{
      id: asString(group.id) ?? `group-${index}`,
      label: groupLabel,
      models,
      remainingFraction,
      resetTime: asString(group.resetTime),
    }];
  }
  return buckets.flatMap((rawBucket, bucketIndex) => {
    const bucket = asRecord(rawBucket) as LegacyBucket | null;
    if (!bucket) return [];
    const remainingFraction = asFraction(bucket.remainingFraction) ?? asFraction(bucket.remaining_fraction);
    if (remainingFraction === undefined) return [];
    const bucketLabel = asString(bucket.displayName) ?? asString(bucket.window) ?? `窗口 ${bucketIndex + 1}`;
    return [{
      id: asString(bucket.bucketId) ?? `${asString(group.id) ?? `group-${index}`}-${bucketIndex}`,
      label: `${groupLabel} · ${bucketLabel}`,
      models,
      remainingFraction,
      resetTime: asString(bucket.resetTime) ?? asString(bucket.reset_time),
    }];
  });
};

export const normalizeAntigravityQuotaStates = (
  files: Record<string, unknown> | undefined
): Record<string, AntigravityQuotaState> => Object.fromEntries(
  Object.entries(files ?? {}).map(([name, rawState]) => {
    const state = asRecord(rawState);
    const status = state?.status;
    if (status === 'error') {
      return [name, { status: 'error', groups: [], error: asString(state?.error) }];
    }
    const groups = Array.isArray(state?.groups)
      ? state.groups.flatMap((rawGroup, index) => normalizeGroup((asRecord(rawGroup) ?? {}) as LegacyGroup, index))
      : [];
    return [name, { status: status === 'loading' ? 'loading' : status === 'idle' ? 'idle' : 'success', groups }];
  })
);

export const antigravityQuotaStateApi = {
  get: () => apiClient.get<AntigravityQuotaSnapshot>(endpoint),
  save: (files: Record<string, AntigravityQuotaState>) =>
    apiClient.put<AntigravityQuotaSnapshot>(endpoint, {
      files,
      replace: true,
      quota_refreshed_at: new Date().toISOString(),
    }),
};
