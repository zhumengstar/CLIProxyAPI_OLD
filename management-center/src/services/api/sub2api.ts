import { apiClient } from './client';

export type Sub2APIImportJobStatus = 'pending' | 'running' | 'done' | 'failed';

export type Sub2APIImportJob = {
  id: string;
  status: Sub2APIImportJobStatus;
  total: number;
  done: number;
  imported: number;
  failed: number;
  files: string[];
  warnings: string[];
  error?: string;
  created_at: string;
  updated_at: string;
};

const normalizeJob = (value: unknown): Sub2APIImportJob => {
  const record = value && typeof value === 'object' ? (value as Record<string, unknown>) : {};
  return {
    id: String(record.id ?? ''),
    status: String(record.status ?? 'pending') as Sub2APIImportJobStatus,
    total: Number(record.total ?? 0),
    done: Number(record.done ?? 0),
    imported: Number(record.imported ?? 0),
    failed: Number(record.failed ?? 0),
    files: Array.isArray(record.files)
      ? record.files.filter((item): item is string => typeof item === 'string')
      : [],
    warnings: Array.isArray(record.warnings)
      ? record.warnings.filter((item): item is string => typeof item === 'string')
      : [],
    error: typeof record.error === 'string' ? record.error : undefined,
    created_at: String(record.created_at ?? ''),
    updated_at: String(record.updated_at ?? ''),
  };
};

export const sub2apiApi = {
  startImport: async (source: string): Promise<Sub2APIImportJob> => {
    const response = await apiClient.post<{ job?: unknown }>('/sub2api/import', { source });
    return normalizeJob(response.job);
  },

  getImport: async (id: string): Promise<Sub2APIImportJob> => {
    const response = await apiClient.get<{ job?: unknown }>(
      `/sub2api/import/${encodeURIComponent(id)}`
    );
    return normalizeJob(response.job);
  },
};
