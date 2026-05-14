import { useEffect, useMemo, useRef, useState, type ChangeEvent } from 'react';
import { Button } from '@/components/ui/Button';
import { Card } from '@/components/ui/Card';
import { sub2apiApi, type Sub2APIImportJob } from '@/services/api';
import { ACCOUNT_POOL_UPDATED_EVENT } from '@/utils/accountPool';
import styles from './Sub2APIImportPage.module.scss';

const terminalStatuses = new Set(['done', 'failed']);

const statusLabel = (status: string) => {
  if (status === 'pending') return '等待中';
  if (status === 'running') return '导入中';
  if (status === 'done') return '已完成';
  if (status === 'failed') return '失败';
  return status || '未开始';
};

export function Sub2APIImportPage() {
  const [source, setSource] = useState('');
  const [job, setJob] = useState<Sub2APIImportJob | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState('');
  const pollTimerRef = useRef<number | null>(null);

  const progress = useMemo(() => {
    if (!job || job.total <= 0) return 0;
    return Math.max(0, Math.min(100, Math.round((job.done / job.total) * 100)));
  }, [job]);

  useEffect(() => {
    return () => {
      if (pollTimerRef.current !== null) {
        window.clearTimeout(pollTimerRef.current);
      }
    };
  }, []);

  useEffect(() => {
    if (!job || terminalStatuses.has(job.status)) return;

    const poll = async () => {
      try {
        const nextJob = await sub2apiApi.getImport(job.id);
        setJob(nextJob);
        if (nextJob.status === 'done') {
          window.dispatchEvent(new Event(ACCOUNT_POOL_UPDATED_EVENT));
        }
      } catch (err) {
        setError(err instanceof Error ? err.message : '查询导入任务失败');
      }
    };

    pollTimerRef.current = window.setTimeout(() => void poll(), 1200);
    return () => {
      if (pollTimerRef.current !== null) {
        window.clearTimeout(pollTimerRef.current);
        pollTimerRef.current = null;
      }
    };
  }, [job]);

  const handleFileChange = async (event: ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0];
    if (!file) return;
    setError('');
    try {
      setSource(await file.text());
    } catch (err) {
      setError(err instanceof Error ? err.message : '读取文件失败');
    }
  };

  const startImport = async () => {
    const trimmed = source.trim();
    if (!trimmed || submitting) return;
    setSubmitting(true);
    setError('');
    try {
      const nextJob = await sub2apiApi.startImport(trimmed);
      setJob(nextJob);
    } catch (err) {
      setError(err instanceof Error ? err.message : '启动导入失败');
    } finally {
      setSubmitting(false);
    }
  };

  const statusClassName =
    job?.status === 'done'
      ? `${styles.statusBadge} ${styles.statusDone}`
      : job?.status === 'failed'
        ? `${styles.statusBadge} ${styles.statusFailed}`
        : styles.statusBadge;

  return (
    <div className={styles.container}>
      <div className={styles.pageHeader}>
        <div>
          <h1 className={styles.pageTitle}>Sub2 转 CPA</h1>
          <p className={styles.description}>
            粘贴或选择 Sub2Api 导出的 JSON，后台会异步转换为 CPA 认证文件并自动写入认证文件目录，账号池会跟随现有同步流程读取这些账号。
          </p>
        </div>
      </div>

      <div className={styles.grid}>
        <Card>
          <h2 className={styles.sectionTitle}>Sub2Api JSON</h2>
          <textarea
            className={styles.textarea}
            spellCheck={false}
            value={source}
            onChange={(event) => setSource(event.target.value)}
            placeholder='粘贴 {"exported_at": "...", "accounts": [...]}'
          />
          <div className={styles.actions}>
            <label className={styles.fileControl}>
              <span>选择 JSON</span>
              <input
                className={styles.fileInput}
                type="file"
                accept=".json,application/json"
                onChange={(event) => void handleFileChange(event)}
              />
            </label>
            <Button onClick={() => void startImport()} loading={submitting} disabled={!source.trim()}>
              异步导入到认证文件
            </Button>
          </div>
        </Card>

        <Card>
          <h2 className={styles.sectionTitle}>任务状态</h2>
          <div className={styles.statusPanel}>
            <span className={statusClassName}>{statusLabel(job?.status ?? '')}</span>
            <div className={styles.progressTrack}>
              <span className={styles.progressFill} style={{ width: `${progress}%` }} />
            </div>
            <div className={styles.metrics}>
              <div className={styles.metric}>
                <span>总数</span>
                <strong>{job?.total ?? 0}</strong>
              </div>
              <div className={styles.metric}>
                <span>已处理</span>
                <strong>{job?.done ?? 0}</strong>
              </div>
              <div className={styles.metric}>
                <span>已导入</span>
                <strong>{job?.imported ?? 0}</strong>
              </div>
              <div className={styles.metric}>
                <span>失败</span>
                <strong>{job?.failed ?? 0}</strong>
              </div>
            </div>

            {error && <div className={`${styles.message} ${styles.error}`}>{error}</div>}
            {job?.error && <div className={`${styles.message} ${styles.error}`}>{job.error}</div>}
            {job?.status === 'done' && (
              <div className={styles.message}>导入完成。请到认证文件或账号池页面刷新查看。</div>
            )}

            {job?.files && job.files.length > 0 && (
              <div>
                <h2 className={styles.sectionTitle}>已写入文件</h2>
                <ul className={styles.list}>
                  {job.files.slice(0, 80).map((file) => (
                    <li key={file}>{file}</li>
                  ))}
                </ul>
              </div>
            )}

            {job?.warnings && job.warnings.length > 0 && (
              <div>
                <h2 className={styles.sectionTitle}>提醒</h2>
                <ul className={styles.list}>
                  {job.warnings.slice(0, 80).map((warning, index) => (
                    <li key={`${warning}-${index}`}>{warning}</li>
                  ))}
                </ul>
              </div>
            )}
          </div>
        </Card>
      </div>
    </div>
  );
}
