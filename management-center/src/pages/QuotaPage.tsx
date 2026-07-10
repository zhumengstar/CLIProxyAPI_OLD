/**
 * Quota management page - coordinates the three quota sections.
 */

import { useCallback, useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { useHeaderRefresh } from '@/hooks/useHeaderRefresh';
import { useAuthStore, useQuotaStore } from '@/stores';
import {
  antigravityQuotaStateApi,
  authFilesApi,
  configFileApi,
  normalizeAntigravityQuotaStates,
} from '@/services/api';
import {
  QuotaSection,
  ANTIGRAVITY_CONFIG,
  CLAUDE_CONFIG,
  CODEX_CONFIG,
  GEMINI_CLI_CONFIG,
  KIMI_CONFIG,
} from '@/components/quota';
import type { AuthFileItem } from '@/types';
import styles from './QuotaPage.module.scss';

export function QuotaPage() {
  const { t } = useTranslation();
  const connectionStatus = useAuthStore((state) => state.connectionStatus);
  const setAntigravityQuota = useQuotaStore((state) => state.setAntigravityQuota);

  const [files, setFiles] = useState<AuthFileItem[]>([]);
  const [error, setError] = useState('');

  const disableControls = connectionStatus !== 'connected';

  const loadConfig = useCallback(async () => {
    try {
      await configFileApi.fetchConfigYaml();
    } catch (err: unknown) {
      const errorMessage = err instanceof Error ? err.message : t('notification.refresh_failed');
      setError((prev) => prev || errorMessage);
    }
  }, [t]);

  const loadFiles = useCallback(async () => {
    setError('');
    try {
      const data = await authFilesApi.list();
      setFiles(data?.files || []);
    } catch (err: unknown) {
      const errorMessage = err instanceof Error ? err.message : t('notification.refresh_failed');
      setError(errorMessage);
    }
  }, [t]);

  const loadAntigravityQuota = useCallback(async () => {
    try {
      const snapshot = await antigravityQuotaStateApi.get();
      setAntigravityQuota(
        normalizeAntigravityQuotaStates(
          snapshot.files,
          snapshot.quota_refreshed_at ?? snapshot.saved_at
        )
      );
    } catch {
      // Older servers may not have the state endpoint. The page remains fully usable.
    }
  }, [setAntigravityQuota]);

  const handleHeaderRefresh = useCallback(async () => {
    await Promise.all([loadConfig(), loadFiles()]);
  }, [loadConfig, loadFiles]);

  useHeaderRefresh(handleHeaderRefresh);

  useEffect(() => {
    queueMicrotask(() => {
      void Promise.all([loadFiles(), loadConfig(), loadAntigravityQuota()]);
    });
  }, [loadAntigravityQuota, loadFiles, loadConfig]);

  return (
    <div className={styles.container}>
      <div className={styles.pageHeader}>
        <h1 className={styles.pageTitle}>{t('quota_management.title')}</h1>
        <p className={styles.description}>{t('quota_management.description')}</p>
      </div>

      {error && <div className={styles.errorBox}>{error}</div>}

      <QuotaSection config={CLAUDE_CONFIG} files={files} disabled={disableControls} />
      <QuotaSection config={ANTIGRAVITY_CONFIG} files={files} disabled={disableControls} />
      <QuotaSection config={CODEX_CONFIG} files={files} disabled={disableControls} />
      <QuotaSection config={GEMINI_CLI_CONFIG} files={files} disabled={disableControls} />
      <QuotaSection config={KIMI_CONFIG} files={files} disabled={disableControls} />
    </div>
  );
}
