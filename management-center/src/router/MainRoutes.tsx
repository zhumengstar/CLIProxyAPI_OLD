import { lazy, type ComponentType } from 'react';
import { Navigate, useRoutes, type Location } from 'react-router-dom';

const lazyNamed = <TModule, TName extends keyof TModule>(
  loader: () => Promise<TModule>,
  name: TName
) =>
  lazy(async () => {
    const mod = await loader();
    return { default: mod[name] as ComponentType };
  });

const DashboardPage = lazyNamed(() => import('@/pages/DashboardPage'), 'DashboardPage');
const AiProvidersPage = lazyNamed(() => import('@/pages/AiProvidersPage'), 'AiProvidersPage');
const AiProvidersAmpcodeEditPage = lazyNamed(
  () => import('@/pages/AiProvidersAmpcodeEditPage'),
  'AiProvidersAmpcodeEditPage'
);
const AiProvidersClaudeEditLayout = lazyNamed(
  () => import('@/pages/AiProvidersClaudeEditLayout'),
  'AiProvidersClaudeEditLayout'
);
const AiProvidersClaudeEditPage = lazyNamed(
  () => import('@/pages/AiProvidersClaudeEditPage'),
  'AiProvidersClaudeEditPage'
);
const AiProvidersClaudeModelsPage = lazyNamed(
  () => import('@/pages/AiProvidersClaudeModelsPage'),
  'AiProvidersClaudeModelsPage'
);
const AiProvidersCodexEditPage = lazyNamed(
  () => import('@/pages/AiProvidersCodexEditPage'),
  'AiProvidersCodexEditPage'
);
const AiProvidersGeminiEditPage = lazyNamed(
  () => import('@/pages/AiProvidersGeminiEditPage'),
  'AiProvidersGeminiEditPage'
);
const AiProvidersOpenAIEditLayout = lazyNamed(
  () => import('@/pages/AiProvidersOpenAIEditLayout'),
  'AiProvidersOpenAIEditLayout'
);
const AiProvidersOpenAIEditPage = lazyNamed(
  () => import('@/pages/AiProvidersOpenAIEditPage'),
  'AiProvidersOpenAIEditPage'
);
const AiProvidersOpenAIModelsPage = lazyNamed(
  () => import('@/pages/AiProvidersOpenAIModelsPage'),
  'AiProvidersOpenAIModelsPage'
);
const AiProvidersVertexEditPage = lazyNamed(
  () => import('@/pages/AiProvidersVertexEditPage'),
  'AiProvidersVertexEditPage'
);
const AuthFilesPage = lazyNamed(() => import('@/pages/AuthFilesPage'), 'AuthFilesPage');
const AuthFilesOAuthExcludedEditPage = lazyNamed(
  () => import('@/pages/AuthFilesOAuthExcludedEditPage'),
  'AuthFilesOAuthExcludedEditPage'
);
const AuthFilesOAuthModelAliasEditPage = lazyNamed(
  () => import('@/pages/AuthFilesOAuthModelAliasEditPage'),
  'AuthFilesOAuthModelAliasEditPage'
);
const OAuthPage = lazyNamed(() => import('@/pages/OAuthPage'), 'OAuthPage');
const QuotaPage = lazyNamed(() => import('@/pages/QuotaPage'), 'QuotaPage');
const ConfigPage = lazyNamed(() => import('@/pages/ConfigPage'), 'ConfigPage');
const SystemPage = lazyNamed(() => import('@/pages/SystemPage'), 'SystemPage');
const AccountPoolPage = lazyNamed(() => import('@/pages/AccountPoolPage'), 'AccountPoolPage');
const UsageRecordsPage = lazyNamed(() => import('@/pages/UsageRecordsPage'), 'UsageRecordsPage');
const Sub2APIImportPage = lazyNamed(
  () => import('@/pages/Sub2APIImportPage'),
  'Sub2APIImportPage'
);

const mainRoutes = [
  { path: '/', element: <DashboardPage /> },
  { path: '/dashboard', element: <DashboardPage /> },
  { path: '/settings', element: <Navigate to="/config" replace /> },
  { path: '/api-keys', element: <Navigate to="/config" replace /> },
  { path: '/ai-providers/gemini/new', element: <AiProvidersGeminiEditPage /> },
  { path: '/ai-providers/gemini/:index', element: <AiProvidersGeminiEditPage /> },
  { path: '/ai-providers/codex/new', element: <AiProvidersCodexEditPage /> },
  { path: '/ai-providers/codex/:index', element: <AiProvidersCodexEditPage /> },
  {
    path: '/ai-providers/claude/new',
    element: <AiProvidersClaudeEditLayout />,
    children: [
      { index: true, element: <AiProvidersClaudeEditPage /> },
      { path: 'models', element: <AiProvidersClaudeModelsPage /> },
    ],
  },
  {
    path: '/ai-providers/claude/:index',
    element: <AiProvidersClaudeEditLayout />,
    children: [
      { index: true, element: <AiProvidersClaudeEditPage /> },
      { path: 'models', element: <AiProvidersClaudeModelsPage /> },
    ],
  },
  { path: '/ai-providers/vertex/new', element: <AiProvidersVertexEditPage /> },
  { path: '/ai-providers/vertex/:index', element: <AiProvidersVertexEditPage /> },
  {
    path: '/ai-providers/openai/new',
    element: <AiProvidersOpenAIEditLayout />,
    children: [
      { index: true, element: <AiProvidersOpenAIEditPage /> },
      { path: 'models', element: <AiProvidersOpenAIModelsPage /> },
    ],
  },
  {
    path: '/ai-providers/openai/:index',
    element: <AiProvidersOpenAIEditLayout />,
    children: [
      { index: true, element: <AiProvidersOpenAIEditPage /> },
      { path: 'models', element: <AiProvidersOpenAIModelsPage /> },
    ],
  },
  { path: '/ai-providers/ampcode', element: <AiProvidersAmpcodeEditPage /> },
  { path: '/ai-providers', element: <AiProvidersPage /> },
  { path: '/ai-providers/*', element: <AiProvidersPage /> },
  { path: '/auth-files', element: <AuthFilesPage /> },
  { path: '/auth-files/oauth-excluded', element: <AuthFilesOAuthExcludedEditPage /> },
  { path: '/auth-files/oauth-model-alias', element: <AuthFilesOAuthModelAliasEditPage /> },
  { path: '/oauth', element: <OAuthPage /> },
  { path: '/quota', element: <QuotaPage /> },
  { path: '/config', element: <ConfigPage /> },
  { path: '/logs', element: <Navigate to="/" replace /> },
  { path: '/external-request-logs', element: <Navigate to="/" replace /> },
  { path: '/system', element: <SystemPage /> },
  { path: '/usage-records', element: <UsageRecordsPage /> },
  { path: '/account-pool', element: <AccountPoolPage /> },
  { path: '/sub2api-import', element: <Sub2APIImportPage /> },
  { path: '*', element: <Navigate to="/" replace /> },
];

export function MainRoutes({ location }: { location?: Location }) {
  return useRoutes(mainRoutes, location);
}
