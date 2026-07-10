const assertEqual = (actual: unknown, expected: unknown, message: string) => {
  if (actual !== expected) {
    throw new Error(`${message}: got ${String(actual)}, want ${String(expected)}`);
  }
};

import {
  getAccountPoolEffectiveStatusCode,
  getAccountPoolErrorSummaryLabel,
} from './src/utils/accountPoolStatus';

assertEqual(
  getAccountPoolEffectiveStatusCode({
    statusCode: 429,
    message: '额度请求 400: bad request',
  }),
  429,
  'uses explicit statusCode before parsing message'
);

assertEqual(
  getAccountPoolEffectiveStatusCode({
    realRequestError: '模型请求失败: 401 {"detail":"Unauthorized"}',
    realRequestStatusCode: 401,
  }),
  undefined,
  'does not extract primary status from real request fields'
);

assertEqual(
  getAccountPoolEffectiveStatusCode({
    message: '额度请求 400: bad request',
    realRequestError: '模型请求失败: 401 {"detail":"Unauthorized"}',
  }),
  400,
  'extracts quota status from message fallback'
);

assertEqual(
  getAccountPoolErrorSummaryLabel({
    message: '模型检测请求失败',
    realRequestError: '模型请求失败: 401 {"detail":"Unauthorized"}',
    realRequestStatusCode: 401,
  }),
  '模型请求',
  'labels real request errors without using HTTP code as summary'
);

assertEqual(
  getAccountPoolEffectiveStatusCode({ realRequestError: 'network failed' }),
  undefined,
  'keeps truly status-less failures unknown'
);
