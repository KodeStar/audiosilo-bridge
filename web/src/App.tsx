import { useMemo, useRef, useState } from 'react';
import { ApiClient, LAN_ONLY_MESSAGE } from '@/api/client';
import { clearToken, getToken, setToken } from '@/lib/token';
import { Login } from '@/components/Login';
import { Shell } from '@/components/Shell';
import { Wordmark } from '@/components/Wordmark';

export function App() {
  const [token, setTokenState] = useState<string | null>(() => getToken());
  const [blocked, setBlocked] = useState(false);

  // The client reads the current token through a ref, so signing in or out
  // doesn't rebuild it.
  const tokenRef = useRef(token);
  tokenRef.current = token;

  const client = useMemo(
    () =>
      new ApiClient({
        getToken: () => tokenRef.current,
        onSignedOut: () => {
          clearToken();
          setTokenState(null);
        },
        onLanBlocked: () => setBlocked(true),
      }),
    [],
  );

  function signedIn(t: string) {
    setToken(t);
    setTokenState(t);
  }

  function signedOut() {
    clearToken();
    setTokenState(null);
  }

  if (blocked) {
    return (
      <main className="flex min-h-screen items-center justify-center p-6">
        <div className="flex max-w-sm flex-col items-center gap-4 rounded-xl border border-edge bg-surface p-8 text-center">
          <Wordmark size="lg" />
          <p role="alert" className="text-sm text-body">
            {LAN_ONLY_MESSAGE}
          </p>
        </div>
      </main>
    );
  }

  if (!token) return <Login client={client} onSignedIn={signedIn} />;
  return <Shell client={client} onSignedOut={signedOut} />;
}
