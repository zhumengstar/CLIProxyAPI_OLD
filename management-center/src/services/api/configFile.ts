/**
 * 配置文件相关 API（/config.yaml）
 */

import { apiClient } from './client';
import { computeApiUrl, detectApiBaseFromLocation } from '@/utils/connection';

type StatusError = { status?: number };

const getStatusCode = (err: unknown): number | undefined => {
  if (!err || typeof err !== 'object') return undefined;
  if ('status' in err) return (err as StatusError).status;
  return undefined;
};

const getLocalManagementUrl = (path: string): string | null => {
  if (typeof window === 'undefined') return null;
  const apiUrl = computeApiUrl(detectApiBaseFromLocation());
  return apiUrl ? `${apiUrl}${path}` : null;
};

const readYamlResponse = (data: unknown): string => {
  if (typeof data === 'string') return data;
  if (data === undefined || data === null) return '';
  return String(data);
};

const fetchConfigYamlFrom = async (url: string): Promise<string> => {
  const response = await apiClient.getRaw(url, {
    responseType: 'text',
    headers: { Accept: 'application/yaml, text/yaml, text/plain' }
  });
  return readYamlResponse(response.data);
};

export const configFileApi = {
  async fetchConfigYaml(): Promise<string> {
    try {
      return await fetchConfigYamlFrom('/config.yaml');
    } catch (err) {
      const fallbackUrl = getStatusCode(err) === 404 ? getLocalManagementUrl('/config.yaml') : null;
      if (!fallbackUrl) throw err;
      return fetchConfigYamlFrom(fallbackUrl);
    }
  },

  async saveConfigYaml(content: string): Promise<void> {
    const config = {
      headers: {
        'Content-Type': 'application/yaml',
        Accept: 'application/json, text/plain, */*'
      }
    };

    try {
      await apiClient.put('/config.yaml', content, config);
    } catch (err) {
      const fallbackUrl = getStatusCode(err) === 404 ? getLocalManagementUrl('/config.yaml') : null;
      if (!fallbackUrl) throw err;
      await apiClient.put(fallbackUrl, content, config);
    }
  }
};
