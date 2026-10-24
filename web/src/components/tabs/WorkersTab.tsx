import { useRef, useState, type FormEvent } from 'react';
import { errorText, type ApiClient } from '@/api/client';
import type { NewWorker, Worker } from '@/api/types';
import { copyText, selectText } from '@/lib/clipboard';
import { useLoad } from '@/lib/useLoad';
import {
  audioHours,
  bridgeAddress,
  isLoopback,
  WORKER_BINARY,
  WORKER_DOCS_URL,
  WORKER_RELEASES_URL,
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
        <AddedCard
          added={added}
          address={address}
          downloads={status?.worker_downloads ?? true}
          onDone={() => setAdded(null)}
        />
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
              is on goes to another worker or the Bridge. Uninstall it on its computer too: run
              audiosilo-bridge worker uninstall there (the program is in
              ~/.audiosilo-bridge-worker/bin if you used the install command).
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
  downloads,
  onDone,
}: {
  added: NewWorker;
  address: string;
  // The Bridge serves install.sh and its worker builds (its Docker image).
  downloads: boolean;
  onDone: () => void;
}) {
  const cmd = workerCommands(address, added.token);
  const releases = (
    <a href={WORKER_RELEASES_URL} target="_blank" rel="noreferrer" className={linkClass}>
      the GitHub releases
    </a>
  );
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
        {downloads ? (
          <div>
            <p className="mb-1 font-medium text-hi">
              On the worker&apos;s computer, paste this into a terminal. It downloads the worker
              from this Bridge and installs it as a service (it starts at login and keeps running):
            </p>
            <Command text={cmd.oneLine} />
            <p className="mt-1 text-dim">
              Add --run at the end to run it once in that terminal instead.
            </p>
          </div>
        ) : (
          <Notice kind="info">
            This Bridge has no worker downloads (it isn&apos;t running from the Docker image).
            Download audiosilo-bridge for the worker&apos;s computer from {releases}, then use the
            commands below.
          </Notice>
        )}
        <details open={!downloads} className="rounded-md border border-edge p-3">
          <summary className="cursor-pointer font-medium text-hi">Install it by hand</summary>
          <div className="mt-3 flex flex-col gap-4">
            <p className="text-dim">
              Download audiosilo-bridge for its computer (macOS or Linux, arm64 or amd64) from{' '}
              {releases}, unpack it and move the audiosilo-bridge program to {WORKER_BINARY}. On a
              Mac, a browser download is quarantined: clear it with xattr -d com.apple.quarantine on
              the program. Then install it as a service:
            </p>
            <Command text={cmd.install} />
            <div>
              <p className="mb-1 text-dim">Or run it once, in a terminal:</p>
              <Command text={cmd.run} />
            </div>
          </div>
        </details>
        <p className="text-dim">
          On a Mac it uses mlx-whisper on Metal, which needs Python 3 (and ffmpeg) from Homebrew;
          the first start builds it and downloads the model, a few minutes. The worker&apos;s
          computer must reach this Bridge on the local network. More in{' '}
          <a href={WORKER_DOCS_URL} target="_blank" rel="noreferrer" className={linkClass}>
            Running an ASR worker
          </a>
          .
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

const linkClass = 'text-pink-500 underline';

// Command is a command with a Copy button. On plain http the Clipboard API is
// off, so copyText falls back; when that fails too, the command is selected
// for a copy by hand, and the page says so (never silently).
function Command({ text }: { text: string }) {
  const [state, setState] = useState<'idle' | 'copied' | 'failed'>('idle');
  const code = useRef<HTMLElement>(null);
  async function copy() {
    if (await copyText(text)) {
      setState('copied');
      return;
    }
    if (code.current) selectText(code.current);
    setState('failed');
  }
  return (
    <div className="flex flex-col gap-2">
      <div className="flex flex-col gap-2 sm:flex-row sm:items-start">
        <code
          ref={code}
          className="block flex-1 rounded-md border border-edge bg-raised p-3 font-mono text-xs break-all text-body"
        >
          {text}
        </code>
        <button type="button" onClick={copy} className={secondaryButton}>
          {state === 'copied' ? 'Copied' : 'Copy'}
        </button>
      </div>
      {state === 'failed' && (
        <p role="status" className="text-xs text-dim">
          The browser wouldn&apos;t copy it. The command is selected: select and copy it (Ctrl-C, or
          Cmd-C on a Mac).
        </p>
      )}
    </div>
  );
}
