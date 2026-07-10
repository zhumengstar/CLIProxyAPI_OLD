import type { AxiosRequestConfig } from 'axios';
import { REQUEST_TIMEOUT_MS } from '@/utils/constants';
import { apiClient } from './client';

export interface ResetQuotaResponse {
  status?: string;
  auth_index?: string;
  groups?: unknown;
  models?: unknown;
  quota_summary?: unknown;
  quota_url?: string;
  project_id?: string;
  account_tier_id?: string;
  account_tier_name?: string;
  account_tier_label?: string;
  error?: string;
}

export const quotaApi = {
  reset: (authIndex: string, config?: AxiosRequestConfig) =>
    apiClient.post<ResetQuotaResponse>(
      '/reset-quota',
      { auth_index: authIndex },
      { timeout: REQUEST_TIMEOUT_MS, ...config }
    ),
};
