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
  type AccountPoolUsageTotals,
} from '@/services/api';
import { useNotificationStore } from '@/stores';
import styles from './UsageRecordsPage.module.scss';

const MIN_USAGE_PAGE_SIZE = 1;
const DEFAULT_USAGE_PAGE_SIZE = 30;
const USAGE_RECORDS_CACHE_KEY = 'usage-records-page-cache-v1';
const USAGE_RECORDS_CACHE_TTL_MS = 3 * 60 * 1000;

type UsageRecordsPageCache = {
  cachedAt: number;
  page: number;
  pageSize: number;
  records: AccountPoolUsageRecord[];
  summaries: AccountPoolUsageSummary[];
  totals: AccountPoolUsageTotals | null;
  totalRecords: number;
};

const readUsageRecordsCache = (): UsageRecordsPageCache | null => {
  if (typeof window === 'undefined') return null;
  try {
    const raw = window.localStorage.getItem(USAGE_RECORDS_CACHE_KEY);
    if (!raw) return null;
    const parsed = JSON.parse(raw) as Partial<UsageRecordsPageCache>;
    if (
      typeof parsed !== 'object' ||
      parsed == null ||
      typeof parsed.cachedAt !== 'number' ||
      Date.now() - parsed.cachedAt > USAGE_RECORDS_CACHE_TTL_MS ||
      typeof parsed.page !== 'number' ||
      typeof parsed.pageSize !== 'number' ||
      !Array.isArray(parsed.records) ||
      !Array.isArray(parsed.summaries)
    ) {
      return null;
    }
    return {
      cachedAt: parsed.cachedAt,
      page: parsed.page,
      pageSize: parsed.pageSize,
      records: parsed.records as AccountPoolUsageRecord[],
      summaries: parsed.summaries as AccountPoolUsageSummary[],
      totals: (parsed.totals as AccountPoolUsageTotals | null) ?? null,
      totalRecords: typeof parsed.totalRecords === 'number' ? parsed.totalRecords : 0,
    };
  } catch {
    return null;
  }
};

const writeUsageRecordsCache = (value: UsageRecordsPageCache): void => {
  if (typeof window === 'undefined') return;
  try {
    window.localStorage.setItem(USAGE_RECORDS_CACHE_KEY, JSON.stringify(value));
  } catch {
    // Ignore storage quota and serialization errors.
  }
};

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

const getCacheTokens = (
  item: Pick<AccountPoolUsageRecord | AccountPoolUsageSummary, 'cached_tokens' | 'cache_read_tokens' | 'cache_creation_tokens'>
): number => (item.cached_tokens ?? 0) + (item.cache_read_tokens ?? 0) + (item.cache_creation_tokens ?? 0);

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
  const { username, userId } = getUsageRecordUserParts(record);
  if (username && userId) return `${username} (ID ${userId})`;
  if (username) return username;
  if (userId) return `ID ${userId}`;
  return String(record.session_id || '').trim();
};

const clampPageSize = (value: number): number =>
  Math.max(MIN_USAGE_PAGE_SIZE, Math.round(value));

const compactSessionID = (value: string): string => {
  const trimmed = value.trim();
  if (trimmed.length <= 24) return trimmed;
  return `${trimmed.slice(0, 8)}...${trimmed.slice(-6)}`;
};

const parseNewAPISession = (sessionID: string): { userId: string; username: string } => {
  const prefix = 'newapi-user-';
  const trimmed = sessionID.trim();
  if (!trimmed.toLowerCase().startsWith(prefix)) {
    return { userId: '', username: '' };
  }
  const raw = trimmed.slice(prefix.length);
  const [userId = '', username = ''] = raw.split('+', 2);
  return { userId: userId.trim(), username: username.trim() };
};

const isUUIDLikeSessionID = (value: string): boolean =>
  /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(value.trim());

const isReadableSessionName = (value: string): boolean => {
  const trimmed = value.trim();
  return trimmed !== '' && !trimmed.includes(':') && !isUUIDLikeSessionID(trimmed) && /^[\w.@-]+$/.test(trimmed);
};

const getUsageRecordUserParts = (record: AccountPoolUsageRecord) => {
  const sessionID = String(record.session_id || '').trim();
  const parsedSession = parseNewAPISession(sessionID);
  const username =
    String(record.username || '').trim() || parsedSession.username || (isReadableSessionName(sessionID) ? sessionID : '');
  const userId = String(record.newapi_user_id || '').trim() || parsedSession.userId;
  return { username, userId, sessionID };
};

const getUsageRecordUserNameDisplay = (record: AccountPoolUsageRecord) => {
  const { username, userId } = getUsageRecordUserParts(record);
  if (username) return { text: username, title: username };
  if (userId) return { text: `ID ${userId}`, title: userId };
  return { text: '-', title: '' };
};

const getUsageRecordSessionDisplay = (record: AccountPoolUsageRecord) => {
  const { sessionID } = getUsageRecordUserParts(record);
  if (!sessionID) return { text: '-', title: '' };
  return { text: compactSessionID(sessionID), title: sessionID };
};

export function UsageRecordsPage() {
  const { t } = useTranslation();
  const showNotification = useNotificationStore((state) => state.showNotification);
  const [records, setRecords] = useState<AccountPoolUsageRecord[]>([]);
  const [summaries, setSummaries] = useState<AccountPoolUsageSummary[]>([]);
  const [usageTotals, setUsageTotals] = useState<AccountPoolUsageTotals | null>(null);
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
    const cache = readUsageRecordsCache();
    const useCache = cache != null && cache.page === page && cache.pageSize === pageSize;
    if (useCache) {
      setRecords(cache.records);
      setSummaries(cache.summaries);
      setUsageTotals(cache.totals);
      setTotalRecords(cache.totalRecords);
      setLoading(false);
    } else {
      setLoading(true);
    }
    try {
      const response = await authFilesApi.getAccountPoolUsageRecords({ limit: pageSize, page });
      setRecords(response.records);
      setSummaries(response.summaries);
      setUsageTotals(response.totals ?? null);
      setTotalRecords(response.total ?? response.records.length);
      writeUsageRecordsCache({
        cachedAt: Date.now(),
        page,
        pageSize,
        records: response.records,
        summaries: response.summaries,
        totals: response.totals ?? null,
        totalRecords: response.total ?? response.records.length,
      });
    } catch (err: unknown) {
      if (!useCache) {
        setRecords([]);
        setSummaries([]);
        setUsageTotals(null);
        setTotalRecords(0);
      }
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
      setUsageTotals(null);
      setTotalRecords(0);
      if (typeof window !== 'undefined') {
        window.localStorage.removeItem(USAGE_RECORDS_CACHE_KEY);
      }
      showNotification('使用记录已清空', 'success');
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : t('common.unknown_error');
      showNotification(`清空使用记录失败：${message}`, 'error');
    }
  }, [showNotification, t]);

  useEffect(() => {
    void loadRecords();
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
    () => ({
      requests: usageTotals?.requests ?? totalRecords,
      successes: usageTotals?.successes ?? 0,
      failures: usageTotals?.failures ?? 0,
      inputTokens: usageTotals?.input_tokens ?? 0,
      outputTokens: usageTotals?.output_tokens ?? 0,
      cacheTokens: usageTotals ? getCacheTokens(usageTotals) : 0,
      tokens: usageTotals?.total_tokens ?? 0,
    }),
    [totalRecords, usageTotals]
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
            <Button variant="ghost" size="sm" onClick={() => void clearRecords()} disabled={totalRecords === 0}>
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
            <span className={styles.statLabel}>输入 Token</span>
            <strong className={styles.statValue}>{formatMetric(totals.inputTokens)}</strong>
          </div>
          <div className={styles.statCard}>
            <span className={styles.statLabel}>输出 Token</span>
            <strong className={styles.statValue}>{formatMetric(totals.outputTokens)}</strong>
          </div>
          <div className={styles.statCard}>
            <span className={styles.statLabel}>缓存 Token</span>
            <strong className={styles.statValue}>{formatMetric(totals.cacheTokens)}</strong>
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
                当前页 {pageRecords.length} 条，筛选后 {filteredRecords.length} 条，共 {totalRecords} 条记录，{summaryCount} 个账号有汇总
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
                    <th>用户名</th>
                    <th>Session ID</th>
                    <th>服务账号邮箱</th>
                    <th>模型</th>
                    <th>状态</th>
                    <th>Token</th>
                  </tr>
                </thead>
                <tbody>
                  {pageRecords.map((record) => {
                    const userDisplay = getUsageRecordUserNameDisplay(record);
                    const sessionDisplay = getUsageRecordSessionDisplay(record);
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
                          <div className={styles.strong} title={userDisplay.title}>
                            {userDisplay.text}
                          </div>
                        </td>
                        <td>
                          <div className={styles.sessionValue} title={sessionDisplay.title}>
                            {sessionDisplay.text}
                          </div>
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
                        <td>
                          <div className={styles.tokenStack}>
                            <span>输入 {formatMetric(record.input_tokens ?? 0)}</span>
                            <span>输出 {formatMetric(record.output_tokens ?? 0)}</span>
                            <span>缓存 {formatMetric(getCacheTokens(record))}</span>
                            <strong>总计 {formatMetric(record.total_tokens ?? 0)}</strong>
                          </div>
                        </td>
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
