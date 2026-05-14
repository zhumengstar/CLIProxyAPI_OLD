import { create } from 'zustand';
import { persist, createJSONStorage } from 'zustand/middleware';
import type { AuthState, LoginCredentials, ConnectionStatus } from '@/types';
import { STORAGE_KEY_AUTH } from '@/utils/constants';
import { obfuscatedStorage } from '@/services/storage/secureStorage';
import { apiClient } from '@/services/api/client';
import { useConfigStore } from './useConfigStore';
import { useModelsStore } from './useModelsStore';
import { detectApiBaseFromLocation, normalizeApiBase } from '@/utils/connection';
import { deobfuscateData, obfuscateData } from '@/utils/encryption';

interface AuthStoreState extends AuthState {
  connectionStatus: ConnectionStatus;
  connectionError: string | null;

  login: (credentials: LoginCredentials) => Promise<void>;
  logout: () => void;
  checkAuth: () => Promise<boolean>;
  restoreSession: () => Promise<boolean>;
  updateServerVersion: (version: string | null, buildDate?: string | null) => void;
  updateConnectionStatus: (status: ConnectionStatus, error?: string | null) => void;
}

type AuthSessionSnapshot = {
  apiBase?: string;
  managementKey?: string;
  rememberPassword?: boolean;
};

let restoreSessionPromise: Promise<boolean> | null = null;
const AUTH_SESSION_STORAGE_KEY = `${STORAGE_KEY_AUTH}:session`;

const readSessionSnapshot = (): AuthSessionSnapshot | null => {
  if (typeof window === 'undefined') return null;

  const raw = window.sessionStorage.getItem(AUTH_SESSION_STORAGE_KEY);
  if (!raw) return null;

  try {
    return JSON.parse(deobfuscateData(raw)) as AuthSessionSnapshot;
  } catch {
    window.sessionStorage.removeItem(AUTH_SESSION_STORAGE_KEY);
    return null;
  }
};

const writeSessionSnapshot = (snapshot: AuthSessionSnapshot): void => {
  if (typeof window === 'undefined') return;
  window.sessionStorage.setItem(AUTH_SESSION_STORAGE_KEY, obfuscateData(JSON.stringify(snapshot)));
  window.sessionStorage.setItem('isLoggedIn', 'true');
};

const clearSessionSnapshot = (): void => {
  if (typeof window === 'undefined') return;
  window.sessionStorage.removeItem(AUTH_SESSION_STORAGE_KEY);
  window.sessionStorage.removeItem('isLoggedIn');
};

export const useAuthStore = create<AuthStoreState>()(
  persist(
    (set, get) => ({
      isAuthenticated: false,
      apiBase: '',
      managementKey: '',
      rememberPassword: false,
      serverVersion: null,
      serverBuildDate: null,
      connectionStatus: 'disconnected',
      connectionError: null,

      restoreSession: () => {
        if (restoreSessionPromise) return restoreSessionPromise;

        restoreSessionPromise = (async () => {
          obfuscatedStorage.migratePlaintextKeys(['apiBase', 'apiUrl', 'managementKey']);

          const sessionSnapshot = readSessionSnapshot();
          const wasLoggedIn =
            localStorage.getItem('isLoggedIn') === 'true' ||
            sessionStorage.getItem('isLoggedIn') === 'true' ||
            Boolean(sessionSnapshot?.managementKey);
          const legacyBase =
            obfuscatedStorage.getItem<string>('apiBase') ||
            obfuscatedStorage.getItem<string>('apiUrl', { encrypt: true });
          const legacyKey = obfuscatedStorage.getItem<string>('managementKey');

          const { apiBase, managementKey, rememberPassword } = get();
          const resolvedBase = normalizeApiBase(
            apiBase || sessionSnapshot?.apiBase || legacyBase || detectApiBaseFromLocation()
          );
          const resolvedKey = managementKey || sessionSnapshot?.managementKey || legacyKey || '';
          const resolvedRememberPassword =
            rememberPassword ||
            Boolean(managementKey) ||
            Boolean(legacyKey) ||
            Boolean(sessionSnapshot?.rememberPassword);

          set({
            apiBase: resolvedBase,
            managementKey: resolvedKey,
            rememberPassword: resolvedRememberPassword
          });
          apiClient.setConfig({ apiBase: resolvedBase, managementKey: resolvedKey });

          if (wasLoggedIn && resolvedBase && resolvedKey) {
            try {
              await get().login({
                apiBase: resolvedBase,
                managementKey: resolvedKey,
                rememberPassword: resolvedRememberPassword
              });
              return true;
            } catch (error) {
              console.warn('Auto login failed:', error);
              clearSessionSnapshot();
              return false;
            }
          }

          return false;
        })();

        return restoreSessionPromise;
      },

      login: async (credentials: LoginCredentials) => {
        const apiBase = normalizeApiBase(credentials.apiBase);
        const managementKey = credentials.managementKey.trim();
        const rememberPassword = credentials.rememberPassword ?? get().rememberPassword ?? false;

        try {
          set({ connectionStatus: 'connecting' });
          useModelsStore.getState().clearCache();

          apiClient.setConfig({
            apiBase,
            managementKey
          });

          await useConfigStore.getState().fetchConfig(undefined, true);

          set({
            isAuthenticated: true,
            apiBase,
            managementKey,
            rememberPassword,
            connectionStatus: 'connected',
            connectionError: null
          });
          writeSessionSnapshot({ apiBase, managementKey, rememberPassword });
          if (rememberPassword) {
            localStorage.setItem('isLoggedIn', 'true');
          } else {
            localStorage.removeItem('isLoggedIn');
          }
        } catch (error: unknown) {
          const message =
            error instanceof Error
              ? error.message
              : typeof error === 'string'
                ? error
                : 'Connection failed';
          set({
            connectionStatus: 'error',
            connectionError: message || 'Connection failed'
          });
          throw error;
        }
      },

      logout: () => {
        restoreSessionPromise = null;
        useConfigStore.getState().clearCache();
        useModelsStore.getState().clearCache();
        set({
          isAuthenticated: false,
          apiBase: '',
          managementKey: '',
          serverVersion: null,
          serverBuildDate: null,
          connectionStatus: 'disconnected',
          connectionError: null
        });
        localStorage.removeItem('isLoggedIn');
        clearSessionSnapshot();
      },

      checkAuth: async () => {
        const { managementKey, apiBase } = get();

        if (!managementKey || !apiBase) {
          return false;
        }

        try {
          apiClient.setConfig({ apiBase, managementKey });

          await useConfigStore.getState().fetchConfig();

          set({
            isAuthenticated: true,
            connectionStatus: 'connected'
          });

          return true;
        } catch {
          clearSessionSnapshot();
          set({
            isAuthenticated: false,
            connectionStatus: 'error'
          });
          return false;
        }
      },

      updateServerVersion: (version, buildDate) => {
        set({ serverVersion: version || null, serverBuildDate: buildDate || null });
      },

      updateConnectionStatus: (status, error = null) => {
        set({
          connectionStatus: status,
          connectionError: error
        });
      }
    }),
    {
      name: STORAGE_KEY_AUTH,
      storage: createJSONStorage(() => ({
        getItem: (name) => {
          const data = obfuscatedStorage.getItem<AuthStoreState>(name);
          return data ? JSON.stringify(data) : null;
        },
        setItem: (name, value) => {
          obfuscatedStorage.setItem(name, JSON.parse(value));
        },
        removeItem: (name) => {
          obfuscatedStorage.removeItem(name);
        }
      })),
      partialize: (state) => ({
        apiBase: state.apiBase,
        ...(state.rememberPassword ? { managementKey: state.managementKey } : {}),
        rememberPassword: state.rememberPassword,
        serverVersion: state.serverVersion,
        serverBuildDate: state.serverBuildDate
      })
    }
  )
);

if (typeof window !== 'undefined') {
  window.addEventListener('unauthorized', () => {
    useAuthStore.getState().logout();
  });

  window.addEventListener(
    'server-version-update',
    ((e: CustomEvent) => {
      const detail = e.detail || {};
      useAuthStore.getState().updateServerVersion(detail.version || null, detail.buildDate || null);
    }) as EventListener
  );
}
