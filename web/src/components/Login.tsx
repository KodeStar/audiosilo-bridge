import { useState, type FormEvent } from 'react';
import { ApiError, type ApiClient } from '@/api/client';
import { Wordmark } from './Wordmark';
import { inputClass } from './ui/Field';
import { primaryButton } from './ui/buttons';

interface LoginProps {
  client: ApiClient;
  onSignedIn: (token: string) => void;
}

export function Login({ client, onSignedIn }: LoginProps) {
  const [password, setPassword] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setError(null);
    setBusy(true);
    try {
      const { token } = await client.login(password);
      onSignedIn(token);
    } catch (err) {
      if (err instanceof ApiError && err.status === 401) setError('Wrong password.');
      else if (err instanceof ApiError) setError(err.message);
      else setError("Can't reach the Bridge.");
      setBusy(false);
    }
  }

  return (
    <main className="flex min-h-screen items-center justify-center p-6">
      <div className="w-full max-w-sm rounded-xl border border-edge bg-surface p-8 shadow-lg">
        <div className="mb-6 flex justify-center">
          <Wordmark size="lg" />
        </div>
        <form onSubmit={submit} className="flex flex-col gap-4" noValidate>
          <div className="flex flex-col gap-2">
            <label htmlFor="password" className="text-sm font-medium text-hi">
              Password
            </label>
            <input
              id="password"
              type="password"
              autoComplete="current-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              autoFocus
              className={inputClass}
            />
          </div>
          {error && (
            <p role="alert" className="text-sm text-pink-500">
              {error}
            </p>
          )}
          <button type="submit" disabled={busy || password === ''} className={primaryButton}>
            {busy ? 'Signing in...' : 'Sign in'}
          </button>
          <p className="text-center text-xs text-dim">
            The password was printed in the Bridge&apos;s log the first time it started.
          </p>
        </form>
      </div>
    </main>
  );
}
