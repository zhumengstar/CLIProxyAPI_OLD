import { useEffect, useState, type ReactElement } from 'react';
import { Navigate, useLocation } from 'react-router-dom';
import { useAuthStore } from '@/stores';
import { LoadingSpinner } from '@/components/ui/LoadingSpinner';

export function ProtectedRoute({ children }: { children: ReactElement }) {
  const location = useLocation();
  const isAuthenticated = useAuthStore((state) => state.isAuthenticated);
  const managementKey = useAuthStore((state) => state.managementKey);
  const apiBase = useAuthStore((state) => state.apiBase);
  const checkAuth = useAuthStore((state) => state.checkAuth);
  const restoreSession = useAuthStore((state) => state.restoreSession);
  const [checking, setChecking] = useState(true);

  useEffect(() => {
    const tryRestore = async () => {
      setChecking(true);
      try {
        if (!isAuthenticated) {
          const restored = await restoreSession();
          if (restored) return;
        }
        if (!isAuthenticated && managementKey && apiBase) {
          await checkAuth();
        }
      } finally {
        setChecking(false);
      }
    };
    tryRestore();
  }, [apiBase, isAuthenticated, managementKey, checkAuth, restoreSession]);

  if (checking) {
    return (
      <div className="main-content">
        <LoadingSpinner />
      </div>
    );
  }

  if (!isAuthenticated) {
    return <Navigate to="/login" replace state={{ from: location }} />;
  }

  return children;
}
