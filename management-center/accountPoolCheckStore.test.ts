const assertEqual = (actual: unknown, expected: unknown, message: string) => {
  if (actual !== expected) {
    throw new Error(`${message}: got ${String(actual)}, want ${String(expected)}`);
  }
};

import { useAccountPoolCheckStore } from './src/stores/useAccountPoolCheckStore';
import type { AuthFileItem } from './src/types/authFile';

const store = useAccountPoolCheckStore;

const seedResult = (name: string, result: { status: 'idle' | 'loading' | 'success' | 'error' | 'unsupported'; checkedAt?: number; message?: string }, hash = 'hash-old') => {
  const runId = store.getState().beginCheck([name]);
  if (!runId) throw new Error('failed to begin check');
  store.getState().setResult(runId, name, result, hash);
  store.getState().finishCheck(runId);
};

store.getState().clearResults();
seedResult('account-a.json', { status: 'success', checkedAt: 200 }, 'hash-a');
store.getState().hydrateResultsFromFiles([
  { name: 'account-a.json', content_hash: 'hash-a' } as AuthFileItem,
]);
assertEqual(
  store.getState().results['account-a.json'],
  undefined,
  'refresh removes stale local result when server has no check_status'
);

store.getState().clearResults();
seedResult('account-b.json', { status: 'success', checkedAt: 999 }, 'hash-b');
store.getState().hydrateResultsFromFiles([
  {
    name: 'account-b.json',
    content_hash: 'hash-b',
    check_content_hash: 'hash-b',
    check_status: 'error',
    check_message: '额度请求 401: unauthorized',
    check_checked_at: 1,
  } as AuthFileItem,
]);
assertEqual(
  store.getState().results['account-b.json']?.status,
  'error',
  'refresh uses server authoritative check result even when local checkedAt is newer'
);

store.getState().clearResults();
seedResult('account-c.json', { status: 'success', checkedAt: 200 }, 'hash-c-old');
store.getState().hydrateResultsFromFiles([
  {
    name: 'account-c.json',
    content_hash: 'hash-c-new',
    check_content_hash: 'hash-c-old',
    check_status: 'success',
    check_checked_at: 100,
  } as AuthFileItem,
]);
assertEqual(
  store.getState().results['account-c.json'],
  undefined,
  'refresh removes stale local result when server check hash mismatches current content hash'
);

store.getState().clearResults();
store.getState().hydrateResultsFromFiles([
  {
    name: 'account-d.json',
    content_hash: 'hash-d',
    check_content_hash: 'hash-d',
    check_status: 'success',
    check_message: '检测成功',
    check_real_request_ok: false,
    check_real_request_error: '模型请求 401 未认证：Unauthorized',
    check_real_request_status_code: 401,
    check_checked_at: 300,
  } as AuthFileItem,
]);
assertEqual(
  store.getState().results['account-d.json']?.status,
  'error',
  'remote success with failed real model request is normalized to the same error status as single-account check'
);
assertEqual(
  store.getState().results['account-d.json']?.message,
  '模型检测请求失败: 模型请求 401 未认证：Unauthorized',
  'remote failed real model request keeps readable model-request error details'
);

store.getState().clearResults();
seedResult('account-e.json', { status: 'success', checkedAt: 500 }, 'hash-e');
store.getState().hydrateResultsFromFiles([
  {
    name: 'account-e.json',
    content_hash: 'hash-e',
    check_content_hash: 'hash-e',
    check_status: 'error',
    check_message: '数据库中的最新失败状态',
    check_checked_at: 100,
  } as AuthFileItem,
  { name: 'account-f.json', content_hash: 'hash-f' } as AuthFileItem,
]);
assertEqual(
  store.getState().results['account-e.json']?.message,
  '数据库中的最新失败状态',
  'refresh rebuilds check results only from server DB fields, not local cache'
);
assertEqual(
  store.getState().results['account-f.json'],
  undefined,
  'visible account without server check_status remains unchecked instead of inheriting any local state'
);
