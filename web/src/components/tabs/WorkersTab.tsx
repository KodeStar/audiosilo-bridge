import { useState, type FormEvent } from 'react';
import { errorText, type ApiClient } from '@/api/client';
import type { NewWorker, Worker } from '@/api/types';
import { useLoad } from '@/lib/useLoad';
import {
  audioHours,
  bridgeAddress,
  isLoopback,
  workerASR,
  workerCommands,
  workerSpeed,
} from '@/lib/workers';
import { Ago } from '../ui/Ago';
import { Badge } from '../ui/Badge';
import { Card } from '../ui/Card';
import { Field, inputClass } from '../ui/Field';
import { Loading } from '../ui/Loading';
import { Notice } from '../ui/Notice';
import { dangerButton, primaryButton, secondaryButton } from '../ui/buttons';

// How often the list looks again (a worker's connection and segment change).
const POLL_MS = 10_000;

export function WorkersTab({ client }: { client: ApiClient }) {
  const {
    data: workers,
    error,
    reload,
  } = useLoad(async () => (await client.workers()).workers, POLL_MS);
  const { data: status } = useLoad(() => client.status(), null);
  const [added, setAdded] = useState<NewWorker | null>(null);
  const address = bridgeAddress(status?.public_url ?? '', window.location.origin);

  return (
    <div className="flex flex-col gap-5">
      <Card title="ASR workers">
        <p className="mb-4 max-w-prose text-sm text-dim">
          A worker transcribes the audio on another computer, where speech recognition is fast: a
          Mac with Apple Silicon (Metal) or a computer with an NVIDIA GPU. It connects to this
          Bridge over the local network; it needs no library and no mapping. While a worker is
          connected, every book&apos;s transcription goes to it.
        </p>
        {workers === null ? (
          <Loading error={error} />
        ) : workers.length === 0 ? (
          <p className="text-sm text-dim">No workers yet. Add one below.</p>
        ) : (
          <ul className="flex flex-col gap-3" aria-label="Workers">
            {workers.map((w) => (
              <WorkerItem key={w.id} client={client} worker={w} onRemoved={reload} />
            ))}
          </ul>
        )}
        {error && workers !== null && (
          <div className="mt-3">
            <Notice kind="error">{error}</Notice>
          </div>
        )}
      </Card>
      {added ? (
        <AddedCard added={added} address={address} onDone={() => setAdded(null)} />
      ) : (
        <AddForm
          client={client}
          onAdded={(nw) => {
            setAdded(nw);
            void reload();
          }}
        />
      )}
    </div>
  );
}

function WorkerItem({
  client,
  worker: w,
  onRemoved,
}: {
  client: ApiClient;
  worker: Worker;
  onRemoved: () => void;
}) {
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const speed = workerSpeed(w);
  const asr = workerASR(w);

  async function remove() {
    setBusy(true);
    setError(null);
    try {
      await client.revokeWorker(w.id);
      onRemoved();
    } catch (err) {
      setError(errorText(err, "Couldn't remove the worker."));
      setBusy(false);
    }
  }

  return (
    <li className="rounded-lg border border-edge bg-raised p-4 text-sm">
      <div className="flex flex-wrap items-center gap-2">
        <span className="font-medium text-hi [overflow-wrap:anywhere]">{w.name}</span>
        {w.connected ? (
          <Badge tone={w.task ? 'busy' : 'good'}>{w.task ? 'Transcribing' : 'Connected'}</Badge>
        ) : (
          <Badge tone="muted">{w.last_seen_at ? 'Not connected' : 'Never connected'}</Badge>
        )}
      </div>
      <p className="mt-1 text-dim">
        {[asr, w.version && `version ${w.version}`].filter(Boolean).join(' - ') ||
          'It says what it runs when it connects.'}
      </p>
      {(w.segments > 0 || w.last_seen_at) && (
        <p className="mt-1 text-dim">
          {w.segments > 0 &&
            `${w.segments} ${w.segments === 1 ? 'segment' : 'segments'}, ${audioHours(w.audio_seconds)} of audio${speed ? `, ${speed}` : ''}. `}
          {w.last_seen_at && (
            <>
              Last heard from <Ago at={w.last_seen_at} />.
            </>
          )}
        </p>
      )}
      {error && (
        <div className="mt-2">
          <Notice kind="error">{error}</Notice>
        </div>
      )}
      <div className="mt-3">
        {confirming ? (
          <div className="flex flex-col gap-3 rounded-md border border-edge bg-surface p-3">
            <p className="text-body">
              Removing the worker revokes its token: it stops at its next call, and the segment it
              is on goes to another worker or the Bridge. Uninstall it on its computer too
              (audiosilo-bridge worker uninstall).
            </p>
            <div className="flex gap-2">
              <button type="button" onClick={remove} disabled={busy} className={dangerButton}>
                {busy ? 'Removing...' : 'Remove'}
              </button>
              <button
                type="button"
                onClick={() => setConfirming(false)}
                disabled={busy}
                className={secondaryButton}
              >
                Keep it
              </button>
            </div>
          </div>
        ) : (
          <button type="button" onClick={() => setConfirming(true)} className={dangerButton}>
            Remove...
          </button>
        )}
      </div>
    </li>
  );
}

function AddForm({ client, onAdded }: { client: ApiClient; onAdded: (nw: NewWorker) => void }) {
  const [name, setName] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function add(e: FormEvent) {
    e.preventDefault();
    if (!name.trim()) {
      setError('Give the worker a name, like the computer it runs on.');
      return;
    }
    setBusy(true);
    setError(null);
    try {
      onAdded(await client.createWorker(name.trim()));
      setName('');
    } catch (err) {
      setError(errorText(err, "Couldn't add the worker."));
    }
    setBusy(false);
  }

  return (
    <Card title="Add a worker">
      <form onSubmit={add} className="flex flex-col gap-4" noValidate>
        <Field label="Name" htmlFor="worker-name" note="The computer it runs on, say.">
          <input
            id="worker-name"
            value={name}
            onChange={(e) => setName(e.target.value)}
            maxLength={64}
            placeholder="Mac Studio"
            className={inputClass}
          />
        </Field>
        {error && <Notice kind="error">{error}</Notice>}
        <div>
          <button type="submit" disabled={busy} className={primaryButton}>
            {busy ? 'Adding...' : 'Add worker'}
          </button>
        </div>
      </form>
    </Card>
  );
}

function AddedCard({
  added,
  address,
  onDone,
}: {
  added: NewWorker;
  address: string;
  onDone: () => void;
}) {
  const cmd = workerCommands(address, added.token);
  return (
    <Card title={`Start "${added.worker.name}"`}>
      <div className="flex flex-col gap-4 text-sm">
        <Notice kind="warn">
          The token is shown only now. Copy a command before you close this.
        </Notice>
        {isLoopback(address) && (
          <Notice kind="info">
            This page is open at a local-only address. A worker on another computer needs the
            Bridge&apos;s network address: set the public address in Settings, or swap it into the
            command.
          </Notice>
        )}
        <div>
          <p className="mb-1 font-medium text-hi">
            On the worker&apos;s computer, install it as a service (it starts at login and keeps
            running):
          </p>
          <Command text={cmd.install} />
        </div>
        <div>
          <p className="mb-1 font-medium text-hi">Or run it once, in a terminal:</p>
          <Command text={cmd.run} />
        </div>
        <p className="text-dim">
          It needs the audiosilo-bridge program for its computer (macOS or Linux). On a Mac it uses
          mlx-whisper on Metal, which needs Python 3 (and ffmpeg) from Homebrew; the first start
          builds it and downloads the model, a few minutes.
        </p>
        <div>
          <button type="button" onClick={onDone} className={secondaryButton}>
            Done
          </button>
        </div>
      </div>
    </Card>
  );
}

function Command({ text }: { text: string }) {
  const [copied, setCopied] = useState(false);
  async function copy() {
    try {
      await navigator.clipboard.writeText(text);
      setCopied(true);
    } catch {
      setCopied(false);
    }
  }
  return (
    <div className="flex flex-col gap-2 sm:flex-row sm:items-start">
      <code className="block flex-1 rounded-md border border-edge bg-raised p-3 font-mono text-xs break-all text-body">
        {text}
      </code>
      <button type="button" onClick={copy} className={secondaryButton}>
        {copied ? 'Copied' : 'Copy'}
      </button>
    </div>
  );
}
