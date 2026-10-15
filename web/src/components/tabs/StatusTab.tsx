import { useCallback, useState, type FormEvent } from 'react';
import { errorText, type ApiClient } from '@/api/client';
import type { Status } from '@/api/types';
import { usePoll } from '@/lib/usePoll';
import { dateTime, relativeTime } from '@/lib/time';
import { laneText, onlineText, secretKeyText } from '@/lib/status';
import { Card } from '../ui/Card';
import { Badge } from '../ui/Badge';
import { Field, inputClass } from '../ui/Field';
import { Notice } from '../ui/Notice';
import { Row } from '../ui/Row';
import { dangerButton, primaryButton, secondaryButton } from '../ui/buttons';

export const STATUS_POLL_MS = 5000;

export function StatusTab({ client }: { client: ApiClient }) {
  const [status, setStatus] = useState<Status | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      setStatus(await client.status());
      setLoadError(null);
    } catch (err) {
      setLoadError(errorText(err, "Can't reach the Bridge."));
    }
  }, [client]);
  usePoll(load, STATUS_POLL_MS);

  if (!status) {
    return loadError ? (
      <Notice kind="error">{loadError}</Notice>
    ) : (
      <p className="text-sm text-dim">Loading...</p>
    );
  }

  const { pairing } = status;
  return (
    <div className="flex flex-col gap-5">
      {loadError && <Notice kind="error">{loadError}</Notice>}
      {pairing.paired ? (
        <PairedCard client={client} status={status} onChange={setStatus} />
      ) : (
        <PairForm client={client} status={status} onPaired={setStatus} />
      )}

      <Card title="This Bridge">
        <dl className="divide-y divide-edge">
          <Row label="Version">{status.version || 'Unknown'}</Row>
          <Row label="Job kinds">
            {status.kinds.length === 0 ? 'None' : status.kinds.join(', ')}
          </Row>
          <Row label="Lanes">
            {status.lanes.length === 0 ? (
              'None'
            ) : (
              <ul className="flex flex-col gap-1">
                {status.lanes.map((l) => (
                  <li key={l.name}>
                    <span className="text-hi">{l.name}</span>: {laneText(l)}
                  </li>
                ))}
              </ul>
            )}
          </Row>
          <Row label="Secret key">{secretKeyText(status.secret_key)}</Row>
          <Row label="Public address">
            {status.public_url ? (
              <a href={status.public_url} className="text-pink-500 underline">
                {status.public_url}
              </a>
            ) : (
              <span className="text-dim">
                Not set. Set one in Settings so the server&apos;s console links to this page.
              </span>
            )}
          </Row>
        </dl>
      </Card>
    </div>
  );
}

function PairedCard({
  client,
  status,
  onChange,
}: {
  client: ApiClient;
  status: Status;
  onChange: (s: Status) => void;
}) {
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const { pairing } = status;

  async function unpair() {
    setBusy(true);
    setError(null);
    try {
      onChange(await client.unpair());
      setConfirming(false);
    } catch (err) {
      setError(errorText(err, "Couldn't unpair."));
    }
    setBusy(false);
  }

  return (
    <Card
      title="Server"
      actions={<Badge tone={status.online ? 'good' : 'warn'}>{onlineText(status)}</Badge>}
    >
      <dl className="divide-y divide-edge">
        <Row label="Server">{pairing.server_name || 'Unnamed server'}</Row>
        <Row label="Address">{pairing.server_url}</Row>
        <Row label="Paired">
          {pairing.paired_at ? (
            <span title={dateTime(pairing.paired_at)}>{relativeTime(pairing.paired_at)}</span>
          ) : (
            'Unknown'
          )}
        </Row>
        <Row label="Last heartbeat">
          {status.last_heartbeat ? (
            <span title={dateTime(status.last_heartbeat)}>
              {relativeTime(status.last_heartbeat)}
            </span>
          ) : (
            'None yet'
          )}
        </Row>
        {status.last_error && (
          <Row label="Last error">
            <span className="text-red-300">{status.last_error}</span>
          </Row>
        )}
      </dl>
      <div className="mt-4 flex flex-col gap-3">
        {error && <Notice kind="error">{error}</Notice>}
        {confirming ? (
          <div className="flex flex-col gap-3 rounded-md border border-edge bg-raised p-3">
            <p className="text-sm text-body">
              Unpairing forgets the token here. Unpair it in the server&apos;s console too.
            </p>
            <div className="flex gap-2">
              <button type="button" onClick={unpair} disabled={busy} className={dangerButton}>
                {busy ? 'Unpairing...' : 'Unpair'}
              </button>
              <button
                type="button"
                onClick={() => setConfirming(false)}
                disabled={busy}
                className={secondaryButton}
              >
                Keep paired
              </button>
            </div>
          </div>
        ) : (
          <div>
            <button type="button" onClick={() => setConfirming(true)} className={dangerButton}>
              Unpair...
            </button>
          </div>
        )}
      </div>
    </Card>
  );
}

function PairForm({
  client,
  status,
  onPaired,
}: {
  client: ApiClient;
  status: Status;
  onPaired: (s: Status) => void;
}) {
  const [serverUrl, setServerUrl] = useState(
    () => status.pairing.server_url || status.server_url_default,
  );
  const [code, setCode] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      onPaired(await client.pair(serverUrl.trim(), code.trim()));
    } catch (err) {
      setError(errorText(err, "Couldn't pair."));
      setBusy(false);
    }
  }

  return (
    <Card title="Pair with a server" actions={<Badge tone="muted">Not paired</Badge>}>
      <form onSubmit={submit} className="flex flex-col gap-4" noValidate>
        {status.pairing.unpaired_reason && (
          <Notice kind="info">Unpaired: {status.pairing.unpaired_reason}</Notice>
        )}
        <Field
          label="Server address"
          htmlFor="pair-server"
          note="The address the Bridge uses to reach the AudioSilo server."
        >
          <input
            id="pair-server"
            type="url"
            value={serverUrl}
            onChange={(e) => setServerUrl(e.target.value)}
            placeholder="http://audiosilo:8080"
            className={inputClass}
          />
        </Field>
        <Field
          label="Pairing code"
          htmlFor="pair-code"
          note="Make one in the server's admin console: Settings > Bridge > Connect a bridge."
        >
          <input
            id="pair-code"
            type="text"
            autoComplete="off"
            value={code}
            onChange={(e) => setCode(e.target.value)}
            className={inputClass}
          />
        </Field>
        {error && <Notice kind="error">{error}</Notice>}
        <div>
          <button
            type="submit"
            disabled={busy || serverUrl.trim() === '' || code.trim() === ''}
            className={primaryButton}
          >
            {busy ? 'Pairing...' : 'Pair'}
          </button>
        </div>
      </form>
    </Card>
  );
}
