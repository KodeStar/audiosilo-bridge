import { useState, type FormEvent } from 'react';
import { errorText, type ApiClient } from '@/api/client';
import type { Accounts, AgentKind } from '@/api/types';
import { useLoad } from '@/lib/useLoad';
import {
  KIND_LABELS,
  PENDING_POLL_MS,
  checkCredential,
  fromText,
  githubState,
  minutesLeft,
  savedText,
} from '@/lib/accounts';
import { Ago } from '../ui/Ago';
import { Badge } from '../ui/Badge';
import { Card } from '../ui/Card';
import { Field, inputClass } from '../ui/Field';
import { Loading } from '../ui/Loading';
import { Notice } from '../ui/Notice';
import { Row } from '../ui/Row';
import { dangerButton, primaryButton, secondaryButton } from '../ui/buttons';

// The Accounts tab: the GitHub sign-in the Bridge shares with the community
// through, and the agent's credential it hands to the agent container. A
// credential's value goes to the Bridge and never comes back.
export function AccountsTab({ client }: { client: ApiClient }) {
  const {
    data: accounts,
    setData,
    error: loadError,
  } = useLoad(
    () => client.accounts(),
    // Poll only while a sign-in waits for its code.
    (shown) => (shown?.github.pending ? PENDING_POLL_MS : null),
    { fallback: "Couldn't load the accounts." },
  );

  if (!accounts) return <Loading error={loadError} />;
  return (
    <div className="flex flex-col gap-5">
      {loadError && <Notice kind="error">{loadError}</Notice>}
      <GitHubCard client={client} accounts={accounts} onChange={setData} />
      <AgentCard client={client} accounts={accounts} onChange={setData} />
    </div>
  );
}

interface CardProps {
  client: ApiClient;
  accounts: Accounts;
  onChange: (a: Accounts) => void;
}

function GitHubCard({ client, accounts, onChange }: CardProps) {
  const g = accounts.github;
  const state = githubState(g);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [warning, setWarning] = useState<string | null>(null);

  async function signIn() {
    setBusy(true);
    setError(null);
    setWarning(null);
    try {
      // The code is shown at once; the poll that starts with it does the rest.
      const pending = await client.startGitHubSignIn();
      onChange({ ...accounts, github: { ...accounts.github, pending, flow_error: '' } });
    } catch (err) {
      setError(errorText(err, "Couldn't start the GitHub sign-in."));
    }
    setBusy(false);
  }

  async function signOut() {
    setBusy(true);
    setError(null);
    setWarning(null);
    try {
      const res = await client.signOutGitHub();
      onChange(res.accounts);
      if (res.revoke_error) setWarning(res.revoke_error);
    } catch (err) {
      setError(errorText(err, "Couldn't sign out of GitHub."));
    }
    setBusy(false);
  }

  const badge =
    state === 'signed_in' ? (
      <Badge tone="good">Signed in</Badge>
    ) : state === 'refused' ? (
      <Badge tone="bad">Sign in again</Badge>
    ) : state === 'pending' ? (
      <Badge tone="busy">Waiting for the code</Badge>
    ) : (
      <Badge tone="muted">Not signed in</Badge>
    );

  return (
    <Card title="GitHub" actions={badge}>
      <div className="flex flex-col gap-4">
        <p className="text-sm text-body">
          The Bridge shares characters, recaps and corrections with the community database as you,
          through the AudioSilo Contributions app on GitHub. It can open issues, and a gist when a
          recaps file is too long for an issue, and nothing else.
        </p>
        {g.env_token && (
          <Notice kind="info">
            AUDIOSILO_BRIDGE_GITHUB_TOKEN is set, and the Bridge uses it before this sign-in.
          </Notice>
        )}
        {state === 'unavailable' && (
          <Notice kind="warn">
            Sign-in isn&apos;t available: no GitHub app is configured (github.client_id).
          </Notice>
        )}
        {state === 'refused' && (
          <Notice kind="warn">
            {g.last_error ||
              'GitHub refused the sign-in (it was revoked or expired): sign in again.'}
          </Notice>
        )}
        {g.pending && (
          <div className="flex flex-col gap-2 rounded-md border border-edge bg-raised p-4">
            <p className="text-sm text-body">
              Open{' '}
              <a
                href={g.pending.verification_uri}
                target="_blank"
                rel="noreferrer"
                className="text-pink-500 underline"
              >
                {g.pending.verification_uri}
              </a>{' '}
              and enter this code:
            </p>
            <p className="font-mono text-2xl tracking-widest text-hi" aria-label="Sign-in code">
              {g.pending.user_code}
            </p>
            <p className="text-xs text-dim">
              It works for {minutesLeft(g.pending.expires_at)} more minutes. This page goes on by
              itself once GitHub has it.
            </p>
          </div>
        )}
        {g.flow_error && !g.pending && <Notice kind="error">{g.flow_error}</Notice>}
        {error && <Notice kind="error">{error}</Notice>}
        {warning && <Notice kind="warn">{warning}</Notice>}
        <dl className="divide-y divide-edge">
          {g.signed_in && (
            <Row label="Signed in as">
              {g.login || 'Unknown'}
              {g.signed_in_at && (
                <span className="block text-xs text-dim">
                  Since <Ago at={g.signed_in_at} />
                </span>
              )}
            </Row>
          )}
          <Row label="Sharing uses">{fromText(g.from)}</Row>
        </dl>
        <div className="flex flex-wrap gap-2">
          {(state === 'signed_out' || state === 'refused') && (
            <button type="button" onClick={signIn} disabled={busy} className={primaryButton}>
              {busy ? 'Starting...' : 'Sign in to GitHub'}
            </button>
          )}
          {(g.signed_in || g.pending) && (
            <button type="button" onClick={signOut} disabled={busy} className={dangerButton}>
              {g.signed_in ? 'Sign out' : 'Cancel'}
            </button>
          )}
          {g.signed_in && g.manage_url && (
            <a href={g.manage_url} target="_blank" rel="noreferrer" className={secondaryButton}>
              Review access on GitHub
            </a>
          )}
        </div>
      </div>
    </Card>
  );
}

function AgentCard({ client, accounts, onChange }: CardProps) {
  const a = accounts.agent;
  const own = a.container_credential !== '';
  return (
    <Card
      title="Agent"
      actions={
        <Badge tone={a.available ? 'good' : 'warn'}>{a.available ? 'Ready' : 'Not ready'}</Badge>
      }
    >
      <div className="flex flex-col gap-4">
        <p className="text-sm text-body">
          The agent container runs {a.backend || 'the agent'} with the credential set here. It stays
          sealed on the Bridge and goes to the container for each run only.
        </p>
        {own && (
          <Notice kind="info">
            The agent container has a credential of its own, which it uses before the one set here.
          </Notice>
        )}
        {!a.available && a.detail && <Notice kind="warn">{a.detail}</Notice>}
        {a.kinds.map((kind) => (
          <CredentialForm
            key={kind}
            client={client}
            kind={kind}
            set={!!a.set[kind]}
            setAt={a.set_at}
            onChange={onChange}
          />
        ))}
      </div>
    </Card>
  );
}

function CredentialForm({
  client,
  kind,
  set,
  setAt,
  onChange,
}: {
  client: ApiClient;
  kind: AgentKind;
  set: boolean;
  setAt: string;
  onChange: (a: Accounts) => void;
}) {
  const [value, setValue] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState<string | null>(null);
  const { label, note } = KIND_LABELS[kind];
  const id = `cred-${kind}`;

  async function save(e: FormEvent) {
    e.preventDefault();
    setSaved(null);
    const problem = checkCredential(value);
    if (problem) {
      setError(problem);
      return;
    }
    setBusy(true);
    setError(null);
    try {
      const next = await client.setAgentCredential(kind, value.trim());
      onChange(next);
      setValue('');
      setSaved(savedText(next.agent, kind));
    } catch (err) {
      setError(errorText(err, "Couldn't save the credential."));
    }
    setBusy(false);
  }

  async function remove() {
    setBusy(true);
    setError(null);
    setSaved(null);
    try {
      onChange(await client.deleteAgentCredential(kind));
      setSaved('Removed.');
    } catch (err) {
      setError(errorText(err, "Couldn't remove the credential."));
    }
    setBusy(false);
  }

  return (
    <form onSubmit={save} className="flex flex-col gap-2 border-t border-edge pt-4" noValidate>
      <Field
        label={label}
        htmlFor={id}
        note={
          <>
            {note}{' '}
            {set ? (
              <span className="text-green-300">
                Set
                {setAt && (
                  <>
                    {' '}
                    (<Ago at={setAt} />)
                  </>
                )}
                .
              </span>
            ) : (
              'Not set.'
            )}
          </>
        }
      >
        <input
          id={id}
          type="password"
          autoComplete="off"
          spellCheck={false}
          value={value}
          onChange={(e) => setValue(e.target.value)}
          placeholder={set ? 'Paste a new one to replace it' : 'Paste it here'}
          className={inputClass}
        />
      </Field>
      {error && <Notice kind="error">{error}</Notice>}
      {saved && <Notice kind="ok">{saved}</Notice>}
      <div className="flex flex-wrap gap-2">
        <button type="submit" disabled={busy || value === ''} className={primaryButton}>
          {busy ? 'Saving...' : set ? 'Replace' : 'Save'}
        </button>
        {set && (
          <button type="button" onClick={remove} disabled={busy} className={dangerButton}>
            Remove
          </button>
        )}
      </div>
    </form>
  );
}
