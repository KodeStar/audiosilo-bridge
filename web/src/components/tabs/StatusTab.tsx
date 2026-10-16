import { useState, type FormEvent } from 'react';
import { errorText, type ApiClient } from '@/api/client';
import type { Status } from '@/api/types';
import { useLoad } from '@/lib/useLoad';
import { laneText, onlineText, secretKeyText } from '@/lib/status';
import { agentText, budgetPercent, budgetText, credentialText, retentionText } from '@/lib/engine';
import { usd } from '@/lib/generate';
import { asrText, fallbackText } from '@/lib/workers';
import { Ago } from '../ui/Ago';
import { Card } from '../ui/Card';
import { Badge } from '../ui/Badge';
import { Field, inputClass } from '../ui/Field';
import { Loading } from '../ui/Loading';
import { ProgressBar } from '../ui/ProgressBar';
import { Notice } from '../ui/Notice';
import { Row } from '../ui/Row';
import { dangerButton, primaryButton, secondaryButton } from '../ui/buttons';

export const STATUS_POLL_MS = 5000;
export const ENGINE_POLL_MS = 10000;

export function StatusTab({ client }: { client: ApiClient }) {
  const {
    data: status,
    setData: setStatus,
    error: loadError,
  } = useLoad(() => client.status(), STATUS_POLL_MS);

  if (!status) return <Loading error={loadError} />;

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

      <EngineCard client={client} />
    </div>
  );
}

// EngineCard shows the sidecars engine: the agent container, speech recognition,
// the budgets and the transcript retention (all set in config.yaml).
function EngineCard({ client }: { client: ApiClient }) {
  const { data: engine, error } = useLoad(() => client.engine(), ENGINE_POLL_MS);
  if (!engine) {
    return (
      <Card title="Characters and recaps">
        <Loading error={error} />
      </Card>
    );
  }
  const { agent } = engine;
  const spent = budgetPercent(engine);
  return (
    <Card
      title="Characters and recaps"
      actions={
        <Badge tone={!engine.started ? 'muted' : agent.available ? 'good' : 'warn'}>
          {!engine.started ? 'Starting' : agent.available ? 'Ready' : 'Agent not ready'}
        </Badge>
      }
    >
      {error && <Notice kind="error">{error}</Notice>}
      <dl className="divide-y divide-edge">
        <Row label="Agent container">
          <span className={agent.available ? undefined : 'text-amber-300'}>{agentText(agent)}</span>
          {agent.heartbeat_at && (
            <span className="block text-xs text-dim">
              Last heard from <Ago at={agent.heartbeat_at} />
              {agent.slots > 0 &&
                ` - ${agent.slots === 1 ? '1 job' : `${agent.slots} jobs`} at a time`}
            </span>
          )}
          {Object.keys(agent.clis).length > 0 && (
            <ul className="mt-1 text-xs text-dim">
              {Object.entries(agent.clis).map(([name, cli]) => (
                <li key={name}>
                  {name} {cli.version || '(no version)'}: {credentialText(cli.credential)}
                </li>
              ))}
            </ul>
          )}
        </Row>
        <Row label="Speech recognition">
          {engine.started ? asrText(engine.asr) : 'Starting'}
          {engine.started && (
            <span className="block text-xs text-dim">
              With no worker connected, {fallbackText(engine.asr.worker_fallback)}.
            </span>
          )}
        </Row>
        <Row label="Monthly budget">
          {budgetText(engine)}
          {agent.subscription && (
            <span className="block text-xs text-dim">
              A subscription login: the costs are what the same use would cost on an API key.
            </span>
          )}
          {spent !== null && (
            <ProgressBar
              percent={spent}
              label="Monthly budget spent"
              tone={spent >= 100 ? 'red' : 'pink'}
              className="mt-1 max-w-xs"
            />
          )}
        </Row>
        <Row label="Per book">{`Up to ${usd(engine.book_budget_usd)} of agent use`}</Row>
        <Row label="Transcripts">{retentionText(engine.transcript_retention_days)}</Row>
      </dl>
      <p className="mt-3 text-xs text-dim">
        These are set in the Bridge&apos;s config.yaml (agent.monthly_budget_usd,
        agent.book_budget_usd, transcripts.retention_days) and apply at the next start.
      </p>
    </Card>
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
        <Row label="Paired">{pairing.paired_at ? <Ago at={pairing.paired_at} /> : 'Unknown'}</Row>
        <Row label="Last heartbeat">
          {status.last_heartbeat ? <Ago at={status.last_heartbeat} /> : 'None yet'}
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
