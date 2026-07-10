export type AccountPoolStatusCodeSource = {
  statusCode?: number;
  message?: string;
  realRequestError?: string;
  realRequestStatusCode?: number;
};

const HTTP_STATUS_PATTERN = /(?:^|[^\d])([1-5]\d{2})(?:[^\d]|$)/;

const ERROR_LABEL_PATTERNS: Array<[RegExp, string]> = [
  [/\bunauthorized\b|未授权|认证失败|authentication/i, '认证失败'],
  [/invalidated|invalid token|token.*invalid|无效|失效/i, '凭证失效'],
  [/forbidden|permission|权限/i, '权限不足'],
  [/timeout|timed out|超时/i, '超时'],
  [/rate limit|too many requests|限流/i, '限流'],
  [/quota|额度/i, '额度错误'],
  [/network|fetch failed|connection|connect|econn|网络/i, '网络错误'],
];

export const getAccountPoolEffectiveStatusCode = (
  result: AccountPoolStatusCodeSource | undefined
): number | undefined => {
  if (typeof result?.statusCode === 'number' && Number.isFinite(result.statusCode)) {
    return result.statusCode;
  }

  const value = result?.message;
  if (typeof value === 'string') {
    const match = value.match(HTTP_STATUS_PATTERN);
    if (match) {
      const statusCode = Number(match[1]);
      if (Number.isFinite(statusCode)) {
        return statusCode;
      }
    }
  }

  return undefined;
};

export const getAccountPoolErrorSummaryLabel = (
  result: AccountPoolStatusCodeSource | undefined
): string => {
  const statusCode = getAccountPoolEffectiveStatusCode(result);
  if (typeof statusCode === 'number') {
    return String(statusCode);
  }

  if (result?.realRequestError || typeof result?.realRequestStatusCode === 'number') {
    return '模型请求';
  }

  const candidates = [result?.message]
    .map((value) => (typeof value === 'string' ? value.trim() : ''))
    .filter(Boolean);

  for (const value of candidates) {
    for (const [pattern, label] of ERROR_LABEL_PATTERNS) {
      if (pattern.test(value)) return label;
    }
  }

  const firstMessage = candidates[0];
  if (!firstMessage) return '错误';

  const compact = firstMessage
    .replace(/^\d{3}\s*[:：-]\s*/, '')
    .replace(/\s+/g, ' ')
    .trim();
  if (!compact) return '错误';

  return compact.length > 10 ? `${compact.slice(0, 10)}…` : compact;
};
