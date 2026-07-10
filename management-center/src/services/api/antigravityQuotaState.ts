import {
  normalizeAntigravityQuotaGroups,
  isCompleteAntigravityQuotaGroups,
  type AntigravityEnhancedQuotaState,
  type AntigravityPool,
} from '@/components/quota/antigravityQuota';
import { apiClient } from './client';

export interface AntigravityQuotaStateFilePayload {
  status?: string;
  groups?: unknown;
  refreshed_at?: string;
  refreshedAt?: string;
  account_level?: string;
  accountLevel?: string;
  account_tier_label?: string;
  account_tier_name?: string;
  account_tier_id?: string;
  error?: string;
  error_status?: number;
  errorStatus?: number;
}

export interface AntigravityQuotaStatePayload {
  files?: Record<string, AntigravityQuotaStateFilePayload>;
  quota_refreshed_at?: string;
}

const normalizeStatus = (value: unknown): AntigravityEnhancedQuotaState['status'] => {
  if (value === 'loading' || value === 'success' || value === 'error') return value;
  return 'idle';
};

const normalizeAccountLevel = (entry: AntigravityQuotaStateFilePayload): string | undefined => {
  const value =
    entry.account_level ??
    entry.accountLevel ??
    entry.account_tier_label ??
    entry.account_tier_name ??
    entry.account_tier_id;
  const text = String(value ?? '').trim();
  return text || undefined;
};

const normalizeErrorStatus = (entry: AntigravityQuotaStateFilePayload): number | undefined => {
  const value = entry.error_status ?? entry.errorStatus;
  const numeric = typeof value === 'number' ? value : Number(value);
  return Number.isFinite(numeric) ? numeric : undefined;
};

export const normalizeAntigravityQuotaStates = (
  files: Record<string, AntigravityQuotaStateFilePayload> | undefined,
  fallbackRefreshedAt?: string
): Record<string, AntigravityEnhancedQuotaState> => {
  const normalized: Record<string, AntigravityEnhancedQuotaState> = {};
  Object.entries(files ?? {}).forEach(([name, entry]) => {
    const groups = normalizeAntigravityQuotaGroups(entry.groups);
    const status = normalizeStatus(entry.status);
    const incomplete = status === 'success' && !isCompleteAntigravityQuotaGroups(groups);
    normalized[name] = {
      status: incomplete ? 'error' : status,
      groups,
      refreshedAt: entry.refreshed_at ?? entry.refreshedAt ?? fallbackRefreshedAt,
      accountLevel: normalizeAccountLevel(entry),
      error: incomplete ? entry.error ?? '缓存中的额度数据不完整，请重新刷新' : entry.error,
      errorStatus: normalizeErrorStatus(entry),
    };
  });
  return normalized;
};

const serializeQuotaState = (
  quota: Record<string, AntigravityEnhancedQuotaState>
): AntigravityQuotaStatePayload => ({
  files: Object.fromEntries(
    Object.entries(quota).map(([name, state]) => [
      name,
      {
        status: state.status,
        groups: state.groups,
        refreshed_at: state.refreshedAt,
        account_level: state.accountLevel,
        error: state.error,
        error_status: state.errorStatus,
      },
    ])
  ),
});

export const antigravityQuotaStateApi = {
  get: async () => {
    const payload = await apiClient.get<AntigravityQuotaStatePayload>('/antigravity-quota-state');
    return normalizeAntigravityQuotaStates(payload.files, payload.quota_refreshed_at);
  },
  save: (quota: Record<string, AntigravityEnhancedQuotaState>) =>
    apiClient.put<AntigravityQuotaStatePayload>('/antigravity-quota-state', serializeQuotaState(quota)),
  setManualPriority: (name: string, pool: AntigravityPool, enabled: boolean) =>
    apiClient.patch('/auth-files/manual-priority', { name, pool, enabled }),
};
