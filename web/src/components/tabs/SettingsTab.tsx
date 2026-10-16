import { useState, type FormEvent } from 'react';
import { errorText, type ApiClient } from '@/api/client';
import type { Settings, WorkerFallback } from '@/api/types';
import { useLoad } from '@/lib/useLoad';
import {
  KEY_CHECK_MINUTES,
  KEY_PUBLIC_URL,
  KEY_WORKER_FALLBACK,
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
import { Loading } from '../ui/Loading';
import { Notice } from '../ui/Notice';
import { primaryButton } from '../ui/buttons';

export function SettingsTab({ client }: { client: ApiClient }) {
  return (
    <div className="flex flex-col gap-5">
      <SettingsForm client={client} />
      <PasswordForm client={client} />
    </div>
  );
}

function SettingsForm({ client }: { client: ApiClient }) {
  const [publicUrl, setPublicUrl] = useState('');
  const [minutes, setMinutes] = useState('');
  const [fallback, setFallback] = useState<WorkerFallback>('local');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);

  // fill puts the Bridge's settings into the fields.
  function fill(s: Settings) {
    setPublicUrl(s.public_url);
    setMinutes(String(s.mapping_check_minutes));
    setFallback(s.asr_worker_fallback);
    return s;
  }

  const {
    data: settings,
    setData: setSettings,
    error: loadError,
  } = useLoad(async () => fill(await client.settings()), null, {
    fallback: "Couldn't load the settings.",
  });

  if (!settings) {
    return (
      <Card title="Settings">
        <Loading error={loadError} />
      </Card>
    );
  }

  const urlLocked = isLocked(settings, KEY_PUBLIC_URL);
  const minutesLocked = isLocked(settings, KEY_CHECK_MINUTES);
  const fallbackLocked = isLocked(settings, KEY_WORKER_FALLBACK);

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
      setSettings(
        fill(
          await client.saveSettings({
            public_url: urlLocked ? settings.public_url : publicUrl.trim(),
            mapping_check_minutes: parsed.value,
            asr_worker_fallback: fallbackLocked ? settings.asr_worker_fallback : fallback,
          }),
        ),
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
        <Field
          label="When no ASR worker is connected"
          htmlFor="worker-fallback"
          note={
            fallbackLocked
              ? LOCKED_NOTE
              : 'Wait suits a Bridge whose own speech recognition is slow (a CPU): the book carries on when a worker connects. It applies to the next segment.'
          }
        >
          <select
            id="worker-fallback"
            value={fallback}
            onChange={(e) => setFallback(e.target.value as WorkerFallback)}
            disabled={fallbackLocked}
            className={inputClass}
          >
            <option value="local">Transcribe on the Bridge</option>
            <option value="wait">Wait for a worker</option>
          </select>
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
