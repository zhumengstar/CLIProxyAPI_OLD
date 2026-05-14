import { useCallback, useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Button } from '@/components/ui/Button';
import { Card } from '@/components/ui/Card';
import { EmptyState } from '@/components/ui/EmptyState';
import { Input } from '@/components/ui/Input';
import { Modal } from '@/components/ui/Modal';
import { Select } from '@/components/ui/Select';
import {
  authFilesApi,
  type AccountPoolUsageRecord,
  type AccountPoolUsageSummary,
} from '@/services/api';
import { useNotificationStore } from '@/stores';
import styles from './UsageRecordsPage.module.scss';

const MIN_USAGE_PAGE_SIZE = 1;
const DEFAULT_USAGE_PAGE_SIZE = 30;

const formatUsageRecordTime = (value: string): string => {
  const time = Date.parse(value);
  if (!Number.isFinite(time)) return value || '-';
  return new Date(time).toLocaleString(undefined, {
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
  });
};

const formatMetric = (value: number | undefined): string => {
  if (typeof value !== 'number' || !Number.isFinite(value)) return '0';
  return new Intl.NumberFormat(undefined, { maximumFractionDigits: 0 }).format(value);
};

const normalizeText = (value: unknown): string => String(value ?? '').trim().toLowerCase();

const getRecordStatusCode = (record: AccountPoolUsageRecord): number =>
  record.status_code ?? (record.success ? 200 : 0);

const matchesStatusFilter = (record: AccountPoolUsageRecord, filter: string): boolean => {
  if (filter === 'all') return true;
  if (filter === 'success') return record.success;
  if (filter === 'failed') return !record.success;

  const statusCode = getRecordStatusCode(record);
  if (filter === '2xx') return statusCode >= 200 && statusCode < 300;
  if (filter === '4xx') return statusCode >= 400 && statusCode < 500;
  if (filter === '5xx') return statusCode >= 500 && statusCode < 600;
  return String(statusCode) === filter;
};

const getRecordUserFilterValue = (record: AccountPoolUsageRecord): string =>
  String(record.newapi_user_id || record.username || record.session_id || '').trim();

const getRecordUserFilterLabel = (record: AccountPoolUsageRecord): string => {
  const username = String(record.username || '').trim();
  const userId = String(record.newapi_user_id || '').trim();
  if (username && userId) return `${username} (ID ${userId})`;
  if (username) return username;
  if (userId) return `ID ${userId}`;
  return String(record.session_id || '').trim();
};

const clampPageSize = (value: number): number =>
  Math.max(MIN_USAGE_PAGE_SIZE, Math.round(value));

export function UsageRecordsPage() {
  const { t } = useTranslation();
  const showNotification = useNotificationStore((state) => state.showNotification);
  const [records, setRecords] = useState<AccountPoolUsageRecord[]>([]);
  const [summaries, setSummaries] = useState<AccountPoolUsageSummary[]>([]);
  const [totalRecords, setTotalRecords] = useState(0);
  const [loading, setLoading] = useState(false);
  const [search, setSearch] = useState('');
  const [statusFilter, setStatusFilter] = useState('all');
  const [userFilter, setUserFilter] = useState('all');
  const [modelFilter, setModelFilter] = useState('all');
  const [detailRecord, setDetailRecord] = useState<AccountPoolUsageRecord | null>(null);
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState(DEFAULT_USAGE_PAGE_SIZE);
  const [pageSizeInput, setPageSizeInput] = useState(String(DEFAULT_USAGE_PAGE_SIZE));

  const loadRecords = useCallback(async () => {
    setLoading(true);
    try {
      const response = await authFilesApi.getAccountPoolUsageRecords({ limit: pageSize, page });
      setRecords(response.records);
      setSummaries(response.summaries);
      setTotalRecords(response.total ?? response.records.length);
    } catch (err: unknown) {
      setRecords([]);
      setSummaries([]);
      setTotalRecords(0);
      const message = err instanceof Error ? err.message : t('common.unknown_error');
      showNotification(message, 'error');
    } finally {
      setLoading(false);
    }
  }, [page, pageSize, showNotification, t]);

  const clearRecords = useCallback(async () => {
    try {
      await authFilesApi.clearAccountPoolUsageRecords();
      setRecords([]);
      setSummaries([]);
      setTotalRecords(0);
      showNotification('使用记录已清空', 'success');
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : t('common.unknown_error');
      showNotification(`清空使用记录失败：${message}`, 'error');
    }
  }, [showNotification, t]);

  useEffect(() => {
    void loadRecords();
    const timer = window.setInterval(() => {
      void loadRecords();
    }, 8000);
    return () => window.clearInterval(timer);
  }, [loadRecords]);

  const userOptions = useMemo(() => {
    const usersByValue = new Map<string, string>();
    records.forEach((record) => {
      const value = getRecordUserFilterValue(record);
      if (!value) return;
      usersByValue.set(value, getRecordUserFilterLabel(record) || value);
    });
    const users = Array.from(usersByValue.entries()).sort(([, left], [, right]) =>
      left.localeCompare(right)
    );
    return [
      { value: 'all', label: '全部用户' },
      ...users.map(([value, label]) => ({ value, label })),
    ];
  }, [records]);

  const modelOptions = useMemo(() => {
    const models = Array.from(
      new Set(
        records
          .map((record) => record.alias || record.model || '')
          .map((value) => value.trim())
          .filter(Boolean)
      )
    ).sort((left, right) => left.localeCompare(right));
    return [
      { value: 'all', label: '全部模型' },
      ...models.map((model) => ({ value: model, label: model })),
    ];
  }, [records]);

  const statusOptions = useMemo(() => {
    const codes = Array.from(
      new Set(records.map(getRecordStatusCode).filter((code) => code > 0))
    ).sort((left, right) => left - right);
    return [
      { value: 'all', label: '全部状态' },
      { value: 'success', label: '成功' },
      { value: 'failed', label: '失败' },
      { value: '2xx', label: '2xx' },
      { value: '4xx', label: '4xx' },
      { value: '5xx', label: '5xx' },
      ...codes.map((code) => ({ value: String(code), label: String(code) })),
    ];
  }, [records]);

  const filteredRecords = useMemo(() => {
    const term = normalizeText(search);
    return records.filter((record) => {
      if (!matchesStatusFilter(record, statusFilter)) return false;
      if (userFilter !== 'all' && getRecordUserFilterValue(record) !== userFilter) {
        return false;
      }
      if (modelFilter !== 'all' && (record.alias || record.model || '') !== modelFilter) {
        return false;
      }
      if (!term) return true;
      return [
        record.username,
        record.newapi_user_id,
        record.session_id,
        record.service_email,
        record.auth_id,
        record.auth_index,
        record.provider,
        record.model,
        record.alias,
        record.status_code,
        record.request_path,
      ].some((value) => normalizeText(value).includes(term));
    });
  }, [modelFilter, records, search, statusFilter, userFilter]);

  const totals = useMemo(
    () =>
      filteredRecords.reduce(
        (acc, item) => {
          acc.requests += 1;
          acc.successes += item.success ? 1 : 0;
          acc.failures += item.success ? 0 : 1;
          acc.tokens += item.total_tokens ?? 0;
          return acc;
        },
        { requests: 0, successes: 0, failures: 0, tokens: 0 }
      ),
    [filteredRecords]
  );

  const totalPages = Math.max(1, Math.ceil(totalRecords / pageSize));
  const currentPage = Math.min(page, totalPages);
  const pageRecords = filteredRecords;

  useEffect(() => {
    setPage(1);
  }, [modelFilter, search, statusFilter, userFilter]);

  useEffect(() => {
    if (page > totalPages) {
      setPage(totalPages);
    }
  }, [page, totalPages]);

  const commitPageSize = (rawValue: string) => {
    const parsed = Number(rawValue);
    if (!Number.isFinite(parsed)) {
      setPageSizeInput(String(pageSize));
      return;
    }
    const next = clampPageSize(parsed);
    setPageSize(next);
    setPageSizeInput(String(next));
    setPage(1);
  };

  const clearFilters = () => {
    setSearch('');
    setStatusFilter('all');
    setUserFilter('all');
    setModelFilter('all');
    setPage(1);
  };

  const summaryCount = summaries.length;
  const requestParamsText = detailRecord?.request_params?.trim() || '{}';

  return (
    <div className={styles.container}>
      <h1 className={styles.pageTitle}>{t('nav.usage_records', { defaultValue: '使用记录' })}</h1>

      <Card>
        <div className={styles.header}>
          <div>
            <h2 className={styles.title}>使用记录</h2>
            <p className={styles.desc}>
              记录外部请求的时间、NewAPI 用户、命中的服务账号邮箱、模型、状态和 Token。
            </p>
          </div>
          <div className={styles.actions}>
            <Button variant="secondary" size="sm" onClick={() => void loadRecords()} loading={loading}>
              {t('common.refresh')}
            </Button>
            <Button variant="ghost" size="sm" onClick={() => void clearRecords()} disabled={records.length === 0}>
              {t('common.clear', { defaultValue: '清空' })}
            </Button>
          </div>
        </div>

        <div className={styles.stats}>
          <div className={styles.statCard}>
            <span className={styles.statLabel}>请求数</span>
            <strong className={styles.statValue}>{formatMetric(totals.requests)}</strong>
          </div>
          <div className={styles.statCard}>
            <span className={styles.statLabel}>成功数</span>
            <strong className={styles.statValue}>{formatMetric(totals.successes)}</strong>
          </div>
          <div className={styles.statCard}>
            <span className={styles.statLabel}>失败数</span>
            <strong className={styles.statValue}>{formatMetric(totals.failures)}</strong>
          </div>
          <div className={styles.statCard}>
            <span className={styles.statLabel}>总 Token</span>
            <strong className={styles.statValue}>{formatMetric(totals.tokens)}</strong>
          </div>
        </div>

        <div className={styles.filters}>
          <Input
            className={styles.searchInput}
            value={search}
            onChange={(event) => setSearch(event.target.value)}
            placeholder="搜索用户、邮箱、模型、状态码"
          />
          <Select
            className={styles.filterSelect}
            fullWidth={false}
            value={statusFilter}
            options={statusOptions}
            onChange={setStatusFilter}
            ariaLabel="状态筛选"
          />
          <Select
            className={styles.filterSelect}
            fullWidth={false}
            value={userFilter}
            options={userOptions}
            onChange={setUserFilter}
            ariaLabel="用户筛选"
          />
          <Select
            className={styles.filterSelect}
            fullWidth={false}
            value={modelFilter}
            options={modelOptions}
            onChange={setModelFilter}
            ariaLabel="模型筛选"
          />
          <Button variant="secondary" size="sm" onClick={clearFilters}>
            清空筛选
          </Button>
        </div>

        {records.length === 0 ? (
          <EmptyState
            title={loading ? '正在加载使用记录...' : '暂无使用记录'}
            description="发起一条外部请求后，这里会显示命中的账号邮箱与请求明细。"
          />
        ) : filteredRecords.length === 0 ? (
          <EmptyState
            title="没有匹配的使用记录"
            description="调整关键词、状态、账号或模型筛选后再查看。"
          />
        ) : (
          <>
            <div className={styles.tableMeta}>
              <span>
                当前页 {filteredRecords.length} 条，共 {totalRecords} 条记录，{summaryCount} 个账号有汇总
              </span>
              <label className={styles.pageSizeControl}>
                <span>每页</span>
                <input
                  className={styles.pageSizeInput}
                  type="number"
                  min={MIN_USAGE_PAGE_SIZE}
                  step={1}
                  value={pageSizeInput}
                  onChange={(event) => {
                    const value = event.currentTarget.value;
                    setPageSizeInput(value);
                    if (value.trim()) {
                      commitPageSize(value);
                    }
                  }}
                  onBlur={(event) => commitPageSize(event.currentTarget.value)}
                  onKeyDown={(event) => {
                    if (event.key === 'Enter') {
                      event.currentTarget.blur();
                    }
                  }}
                />
                <span>条</span>
              </label>
            </div>

            <div className={styles.tableWrap}>
              <table className={styles.table}>
                <thead>
                  <tr>
                    <th>时间</th>
                    <th>NewAPI 用户</th>
                    <th>服务账号邮箱</th>
                    <th>模型</th>
                    <th>状态</th>
                    <th>Token</th>
                  </tr>
                </thead>
                <tbody>
                  {pageRecords.map((record) => {
                    const userLabel = record.username || record.newapi_user_id || '-';
                    const statusCode = getRecordStatusCode(record);
                    return (
                      <tr key={record.id}>
                        <td>
                          <div>{formatUsageRecordTime(record.requested_at)}</div>
                          <button
                            type="button"
                            className={styles.detailButton}
                            onClick={() => setDetailRecord(record)}
                          >
                            查看参数
                          </button>
                        </td>
                        <td>
                          <div className={styles.strong}>{userLabel}</div>
                          {record.newapi_user_id ? <div className={styles.muted}>ID {record.newapi_user_id}</div> : null}
                          {record.session_id ? (
                            <div className={styles.muted} title={record.session_id}>
                              {record.session_id}
                            </div>
                          ) : null}
                        </td>
                        <td>
                          <div className={styles.strong}>{record.service_email || record.auth_id || '-'}</div>
                          {record.auth_index ? <div className={styles.muted}>#{record.auth_index}</div> : null}
                        </td>
                        <td>
                          <div className={styles.strong}>{record.alias || record.model || '-'}</div>
                          <div className={styles.muted}>{record.provider || '-'}</div>
                        </td>
                        <td>
                          <span className={record.success ? styles.statusOk : styles.statusError}>
                            {statusCode || (record.success ? 'OK' : 'ERR')}
                          </span>
                          {typeof record.latency_ms === 'number' && record.latency_ms > 0 ? (
                            <div className={styles.muted}>{record.latency_ms} ms</div>
                          ) : null}
                        </td>
                        <td>{record.total_tokens ?? 0}</td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>

            <div className={styles.pagination}>
              <Button
                variant="secondary"
                size="sm"
                onClick={() => setPage((value) => Math.max(1, value - 1))}
                disabled={currentPage <= 1}
              >
                上一页
              </Button>
              <span className={styles.pageInfo}>
                第 {currentPage} / {totalPages} 页
              </span>
              <Button
                variant="secondary"
                size="sm"
                onClick={() => setPage((value) => Math.min(totalPages, value + 1))}
                disabled={currentPage >= totalPages}
              >
                下一页
              </Button>
            </div>
          </>
        )}
      </Card>

      <Modal
        open={Boolean(detailRecord)}
        title="请求参数"
        onClose={() => setDetailRecord(null)}
        width={760}
      >
        <div className={styles.detailMeta}>
          <span>{detailRecord ? formatUsageRecordTime(detailRecord.requested_at) : '-'}</span>
          <span>{detailRecord?.alias || detailRecord?.model || '-'}</span>
          <span>{detailRecord?.request_path || '-'}</span>
        </div>
        <div className={styles.contextNotice}>
          用户上下文已置空，不记录和展示 messages / input / contents / prompt 等内容。
        </div>
        <pre className={styles.requestParams}>{requestParamsText}</pre>
      </Modal>
    </div>
  );
}
