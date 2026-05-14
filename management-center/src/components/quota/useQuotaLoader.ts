/**
 * Generic hook for quota data fetching and management.
 */

import { useCallback, useRef } from 'react';
import { useTranslation } from 'react-i18next';
import type { AuthFileItem } from '@/types';
import { useQuotaStore } from '@/stores';
import { getStatusFromError } from '@/utils/quota';
import type { QuotaConfig } from './quotaConfigs';

type QuotaScope = 'page' | 'all';

type QuotaUpdater<T> = T | ((prev: T) => T);

type QuotaSetter<T> = (updater: QuotaUpdater<T>) => void;

interface LoadQuotaResult<TData> {
  name: string;
  status: 'success' | 'error';
  data?: TData;
  error?: string;
  errorStatus?: number;
}

const runWithConcurrency = async <T,>(
  items: T[],
  concurrency: number,
  worker: (item: T) => Promise<void>
) => {
  if (items.length === 0) return;
  let cursor = 0;
  const workerCount = Math.max(1, Math.min(concurrency, items.length));

  await Promise.all(
    Array.from({ length: workerCount }, async () => {
      for (;;) {
        const index = cursor;
        cursor += 1;
        const item = items[index];
        if (item === undefined) return;
        await worker(item);
      }
    })
  );
};

export function useQuotaLoader<TState, TData>(config: QuotaConfig<TState, TData>) {
  const { t } = useTranslation();
  const quota = useQuotaStore(config.storeSelector);
  const setQuota = useQuotaStore((state) => state[config.storeSetter]) as QuotaSetter<
    Record<string, TState>
  >;

  const loadingRef = useRef(false);
  const requestIdRef = useRef(0);

  const loadQuota = useCallback(
    async (
      targets: AuthFileItem[],
      scope: QuotaScope,
      setLoading: (loading: boolean, scope?: QuotaScope | null) => void,
      concurrency = 5
    ) => {
      if (loadingRef.current) return;
      loadingRef.current = true;
      const requestId = ++requestIdRef.current;
      setLoading(true, scope);

      try {
        if (targets.length === 0) return;

        const results: LoadQuotaResult<TData>[] = [];
        await runWithConcurrency(targets, concurrency, async (file) => {
          let result: LoadQuotaResult<TData>;
          try {
            const data = await config.fetchQuota(file, t);
            result = { name: file.name, status: 'success', data };
          } catch (err: unknown) {
            const message = err instanceof Error ? err.message : t('common.unknown_error');
            const errorStatus = getStatusFromError(err);
            result = { name: file.name, status: 'error', error: message, errorStatus };
          }
          results.push(result);
        });

        if (requestId !== requestIdRef.current) return;

        setQuota((prev) => {
          const nextState = { ...prev };
          results.forEach((result) => {
            if (result.status === 'success') {
              nextState[result.name] = config.buildSuccessState(result.data as TData);
            } else {
              nextState[result.name] = config.buildErrorState(
                result.error || t('common.unknown_error'),
                result.errorStatus
              );
            }
          });
          return nextState;
        });
      } finally {
        if (requestId === requestIdRef.current) {
          setLoading(false);
          loadingRef.current = false;
        }
      }
    },
    [config, setQuota, t]
  );

  return { quota, loadQuota };
}
