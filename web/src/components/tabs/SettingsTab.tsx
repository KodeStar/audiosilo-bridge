import { useCallback, useEffect, useState, type FormEvent } from 'react';
import { errorText, type ApiClient } from '@/api/client';
import type { Settings } from '@/api/types';
import {
  KEY_CHECK_MINUTES,
  KEY_PUBLIC_URL,
  LOCKED_NOTE,
  MAX_CHECK_MINUTES,
  MIN_CHECK_MINUTES,
  allLocked,
  checkPublicURL,
  isLocked,
  parseCheckMinutes,
} from '@/lib/settings';
import { Card } from '../ui/Card';
import { Field, inputClass } from '../ui/Field';
import { Notice } from '../ui/Notice';
import { primaryButton, secondaryButton } from '../ui/buttons';

interface SettingsTabProps {
  client: ApiClient;
  onSignOut: () => void;
  signingOut: boolean;
}

export function SettingsTab({ client, onSignOut, signingOut }: SettingsTabProps) {
  return (
    <div className="flex flex-col gap-5">
      <SettingsForm client={client} />
      <PasswordForm client={client} />
      <Card title="Session">
        <p className="mb-3 text-sm text-dim">Sign out of this page in this browser.</p>
        <button type="button" onClick={onSignOut} disabled={signingOut} className={secondaryButton}>
          {signingOut ? 'Signing out...' : 'Sign out'}
        </button>
      </Card>
    </div>
  );
}

function SettingsForm({ client }: { client: ApiClient }) {
  const [settings, setSettings] = useState<Settings | null>(null);
  const [publicUrl, setPublicUrl] = useState('');
  const [minutes, setMinutes] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);

  const apply = useCallback((s: Settings) => {
    setSettings(s);
    setPublicUrl(s.public_url);
    setMinutes(String(s.mapping_check_minutes));
  }, []);

  useEffect(() => {
    let cancelled = false;
    client
      .settings()
      .then((s) => {
        if (!cancelled) apply(s);
      })
      .catch((err: unknown) => {
        if (!cancelled) setError(errorText(err, "Couldn't load the settings."));
      });
    return () => {
      cancelled = true;
    };
  }, [client, apply]);

  if (!settings) {
    return (
      <Card title="Settings">
        {error ? (
          <Notice kind="error">{error}</Notice>
        ) : (
          <p className="text-sm text-dim">Loading...</p>
        )}
      </Card>
    );
  }

  const urlLocked = isLocked(settings, KEY_PUBLIC_URL);
  const minutesLocked = isLocked(settings, KEY_CHECK_MINUTES);

  async function save(e: FormEvent) {
    e.preventDefault();
    if (!settings) return;
    setSaved(false);
    const urlProblem = urlLocked ? null : checkPublicURL(publicUrl);
    if (urlProblem) {
      setError(urlProblem);
      return;
    }
    const parsed = minutesLocked
      ? { value: settings.mapping_check_minutes }
      : parseCheckMinutes(minutes);
    if ('error' in parsed) {
      setError(parsed.error);
      return;
    }
    setBusy(true);
    setError(null);
    try {
      // A locked field is sent with the value it has, which the Bridge accepts.
      apply(
        await client.saveSettings({
          public_url: urlLocked ? settings.public_url : publicUrl.trim(),
          mapping_check_minutes: parsed.value,
        }),
      );
      setSaved(true);
    } catch (err) {
      setError(errorText(err, "Couldn't save the settings."));
    }
    setBusy(false);
  }

  return (
    <Card title="Settings">
      <form onSubmit={save} className="flex flex-col gap-4" noValidate>
        <Field
          label="Public address"
          htmlFor="public-url"
          note={
            urlLocked
              ? LOCKED_NOTE
              : "The address this page has on your network. The server's console links to it."
          }
        >
          <input
            id="public-url"
            type="url"
            value={publicUrl}
            onChange={(e) => setPublicUrl(e.target.value)}
            readOnly={urlLocked}
            disabled={urlLocked}
            placeholder="http://192.168.1.20:8091"
            className={inputClass}
          />
        </Field>
        <Field
          label="Check the libraries every (minutes)"
          htmlFor="check-minutes"
          note={
            minutesLocked
              ? LOCKED_NOTE
              : `From ${MIN_CHECK_MINUTES} to ${MAX_CHECK_MINUTES} (a day).`
          }
        >
          <input
            id="check-minutes"
            type="number"
            inputMode="numeric"
            min={MIN_CHECK_MINUTES}
            max={MAX_CHECK_MINUTES}
            value={minutes}
            onChange={(e) => setMinutes(e.target.value)}
            readOnly={minutesLocked}
            disabled={minutesLocked}
            className={inputClass}
          />
        </Field>
        {error && <Notice kind="error">{error}</Notice>}
        {saved && <Notice kind="ok">Saved.</Notice>}
        <div>
          <button type="submit" disabled={busy || allLocked(settings)} className={primaryButton}>
            {busy ? 'Saving...' : 'Save settings'}
          </button>
        </div>
      </form>
    </Card>
  );
}

function PasswordForm({ client }: { client: ApiClient }) {
  const [current, setCurrent] = useState('');
  const [next, setNext] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [done, setDone] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    setDone(false);
    try {
      await client.changePassword(current, next);
      setCurrent('');
      setNext('');
      setDone(true);
    } catch (err) {
      setError(errorText(err, "Couldn't change the password."));
    }
    setBusy(false);
  }

  return (
    <Card title="Change password">
      <form onSubmit={submit} className="flex flex-col gap-4" noValidate>
        <Field label="Current password" htmlFor="pw-current">
          <input
            id="pw-current"
            type="password"
            autoComplete="current-password"
            value={current}
            onChange={(e) => setCurrent(e.target.value)}
            className={inputClass}
          />
        </Field>
        <Field label="New password" htmlFor="pw-new">
          <input
            id="pw-new"
            type="password"
            autoComplete="new-password"
            value={next}
            onChange={(e) => setNext(e.target.value)}
            className={inputClass}
          />
        </Field>
        {error && <Notice kind="error">{error}</Notice>}
        {done && <Notice kind="ok">The password is changed.</Notice>}
        <div>
          <button
            type="submit"
            disabled={busy || current === '' || next === ''}
            className={primaryButton}
          >
            {busy ? 'Changing...' : 'Change password'}
          </button>
        </div>
      </form>
    </Card>
  );
}
