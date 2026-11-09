import { useId, useState, type FormEvent } from 'react';
import { errorText, type ApiClient } from '@/api/client';
import type {
  AudibleAccount,
  AudibleAccounts,
  AudibleMapping,
  AudiblePolicy,
  AudibleSignIn,
} from '@/api/types';
import { useLoad } from '@/lib/useLoad';
import { minutesLeft } from '@/lib/accounts';
import {
  POLICY_LABELS,
  accountState,
  defaultMapping,
  mappingOf,
  mappingProblem,
  marketplaceName,
  needsSignIn,
  OLD_PROFILE_TEXT,
  pollSummary,
  redirectProblem,
} from '@/lib/audible';
import { Ago } from '../ui/Ago';
import { Badge } from '../ui/Badge';
import { Card } from '../ui/Card';
import { Field, inputClass } from '../ui/Field';
import { Loading } from '../ui/Loading';
import { Notice } from '../ui/Notice';
import { Row } from '../ui/Row';
import { dangerButton, primaryButton, secondaryButton } from '../ui/buttons';

// The Accounts tab's Audible section: sign in to an Audible account (open
// Amazon's sign-in, paste back the page it lands on), map it to a person and a
// books library on the server, and remove it. The sign-in stays sealed on the
// Bridge; the page sees the account's label and how its checks went.
export function AudibleCard({ client }: { client: ApiClient }) {
  const {
    data,
    setData,
    error: loadError,
  } = useLoad(() => client.audible(), null, { fallback: "Couldn't load the Audible accounts." });
  // adding: the sign-in form is open (again: for that account's marketplace).
  // key names whose it is ("new", or the account's id), so the form starts
  // afresh for another account rather than keep the last one's store.
  const [adding, setAdding] = useState<{
    key: string;
    marketplace: string;
    mapping?: AudibleMapping;
  } | null>(null);
  const [notice, setNotice] = useState<{ kind: 'ok' | 'warn'; text: string } | null>(null);

  if (!data)
    return (
      <Card title="Audible">
        <Loading error={loadError} />
      </Card>
    );
  const refused = data.accounts.some(needsSignIn);
  const badge =
    data.accounts.length === 0 ? (
      <Badge tone="muted">Not signed in</Badge>
    ) : refused ? (
      <Badge tone="bad">Sign in again</Badge>
    ) : (
      <Badge tone="good">
        {data.accounts.length === 1 ? '1 account' : `${data.accounts.length} accounts`}
      </Badge>
    );

  return (
    <Card title="Audible" actions={badge}>
      <div className="flex flex-col gap-4">
        <p className="text-sm text-body">
          The Bridge signs in to Audible as a device called AudioSilo, lists the books the account
          owns and finds the ones already in the library. Each account belongs to one person on the
          server. The sign-in stays sealed on the Bridge.
        </p>
        <Notice kind="info">
          The server checks the accounts only while the Audible module is on: turn it on in the
          console, under Settings, Modules. Signing in here works either way.
        </Notice>
        {loadError && <Notice kind="error">{loadError}</Notice>}
        {notice && <Notice kind={notice.kind}>{notice.text}</Notice>}
        {!data.paired && <Notice kind="warn">Pair the Bridge with a server first.</Notice>}
        {data.accounts.map((a) => (
          <AccountRow
            key={a.id}
            client={client}
            account={a}
            data={data}
            onChange={setData}
            onNotice={setNotice}
            onSignInAgain={() =>
              setAdding({ key: a.id, marketplace: a.marketplace, mapping: mappingOf(a) })
            }
          />
        ))}
        {adding ? (
          <SignInForm
            key={adding.key}
            client={client}
            data={data}
            initial={adding}
            onDone={(next) => {
              setData(next);
              setAdding(null);
              setNotice({ kind: 'ok', text: 'Signed in to Audible.' });
            }}
            onCancel={() => setAdding(null)}
          />
        ) : (
          data.paired && (
            <div>
              <button
                type="button"
                className={data.accounts.length === 0 ? primaryButton : secondaryButton}
                onClick={() => {
                  setNotice(null);
                  setAdding({
                    key: 'new',
                    marketplace: data.marketplaces.includes('us')
                      ? 'us'
                      : (data.marketplaces[0] ?? ''),
                  });
                }}
              >
                {data.accounts.length === 0 ? 'Sign in to Audible' : 'Add another Audible account'}
              </button>
            </div>
          )
        )}
      </div>
    </Card>
  );
}

function AccountRow({
  client,
  account: a,
  data,
  onChange,
  onNotice,
  onSignInAgain,
}: {
  client: ApiClient;
  account: AudibleAccount;
  data: AudibleAccounts;
  onChange: (d: AudibleAccounts) => void;
  onNotice: (n: { kind: 'ok' | 'warn'; text: string } | null) => void;
  onSignInAgain: () => void;
}) {
  const [editing, setEditing] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const state = accountState(a);
  const summary = pollSummary(a);

  async function remove() {
    setBusy(true);
    setError(null);
    onNotice(null);
    try {
      const res = await client.removeAudibleAccount(a.id);
      onChange(res.audible);
      onNotice(
        res.warning
          ? { kind: 'warn', text: `Removed from the Bridge, but ${res.warning}.` }
          : {
              kind: 'ok',
              text: `Removed ${a.label}, and its AudioSilo device from the Amazon account.`,
            },
      );
    } catch (err) {
      setError(errorText(err, "Couldn't remove the account."));
      setBusy(false);
    }
  }

  return (
    <section
      aria-label={a.label}
      className="flex flex-col gap-3 rounded-md border border-edge bg-raised p-4"
    >
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h3 className="text-sm font-medium text-hi">{a.label}</h3>
        {state === 'refused' ? (
          <Badge tone="bad">Sign in again</Badge>
        ) : state === 'old_profile' ? (
          <Badge tone="warn">Sign in again</Badge>
        ) : state === 'other_server' ? (
          <Badge tone="warn">Mapped on another server</Badge>
        ) : state === 'checked' ? (
          <Badge tone="good">Signed in</Badge>
        ) : (
          <Badge tone="muted">Not checked yet</Badge>
        )}
      </div>
      {state === 'refused' && (
        <Notice kind="warn">{a.last_error || 'Audible refused the sign-in: sign in again.'}</Notice>
      )}
      {state === 'old_profile' && <Notice kind="warn">{OLD_PROFILE_TEXT}</Notice>}
      {state === 'other_server' && (
        <Notice kind="warn">
          This account was mapped on another server. Change it to map it on this one.
        </Notice>
      )}
      {state !== 'refused' && a.last_error && <Notice kind="error">{a.last_error}</Notice>}
      <dl className="divide-y divide-edge">
        <Row label="Audible store">{marketplaceName(a.marketplace)}</Row>
        <Row label="Belongs to">{a.user_name || `Person ${a.user_id}`}</Row>
        <Row label="Library">{a.library_name || `Library ${a.library_id}`}</Row>
        <Row label="Back up">{POLICY_IN_CONSOLE}</Row>
        <Row label="Last checked">
          {a.last_poll_at ? (
            <>
              <Ago at={a.last_poll_at} />
              {summary && <span className="block text-xs text-dim">{summary}</span>}
            </>
          ) : (
            'Not yet'
          )}
        </Row>
      </dl>
      {editing && (
        <MappingForm
          data={data}
          initial={mappingOf(a)}
          policyHere={false}
          submitLabel="Save"
          busyLabel="Saving..."
          onSubmit={async (m) => {
            onChange(await client.mapAudibleAccount(a.id, m));
            setEditing(false);
          }}
          onCancel={() => setEditing(false)}
        />
      )}
      {error && <Notice kind="error">{error}</Notice>}
      {confirming ? (
        <div className="flex flex-col gap-2">
          <p className="text-sm text-body">
            Remove {a.label}? The Bridge forgets the sign-in and removes its AudioSilo device from
            the Amazon account. The books already in the library stay.
          </p>
          <div className="flex flex-wrap gap-2">
            <button type="button" onClick={remove} disabled={busy} className={dangerButton}>
              {busy ? 'Removing...' : 'Remove the account'}
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
        !editing && (
          <div className="flex flex-wrap gap-2">
            {needsSignIn(a) && data.paired && (
              <button type="button" onClick={onSignInAgain} className={primaryButton}>
                Sign in again
              </button>
            )}
            {data.paired && (
              <button type="button" onClick={() => setEditing(true)} className={secondaryButton}>
                Change
              </button>
            )}
            <button type="button" onClick={() => setConfirming(true)} className={dangerButton}>
              Remove
            </button>
          </div>
        )
      )}
    </section>
  );
}

function SignInForm({
  client,
  data,
  initial,
  onDone,
  onCancel,
}: {
  client: ApiClient;
  data: AudibleAccounts;
  initial: { marketplace: string; mapping?: AudibleMapping };
  onDone: (d: AudibleAccounts) => void;
  onCancel: () => void;
}) {
  // Each open form its own ids: an account's sign-in again and a new sign-in
  // can be open together.
  const id = useId();
  const [marketplace, setMarketplace] = useState(initial.marketplace);
  const [started, setStarted] = useState<AudibleSignIn | null>(null);
  // tried: a finish was sent, which spends the sign-in (whatever it answered).
  const [tried, setTried] = useState(false);
  const [redirect, setRedirect] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function open() {
    // The same sign-in while it can still be finished: a new one would make the
    // page an Amazon tab opened earlier lands on useless.
    if (started && !tried && minutesLeft(started.expires_at) > 0) {
      window.open(started.url, '_blank', 'noopener,noreferrer');
      return;
    }
    setBusy(true);
    setError(null);
    try {
      const s = await client.startAudibleSignIn(marketplace);
      setStarted(s);
      setTried(false);
      window.open(s.url, '_blank', 'noopener,noreferrer');
    } catch (err) {
      setError(errorText(err, "Couldn't start the Audible sign-in."));
    }
    setBusy(false);
  }

  return (
    <div className="flex flex-col gap-4 rounded-md border border-edge bg-raised p-4">
      <Field
        label="Audible store"
        htmlFor={`${id}-marketplace`}
        note="The store the Audible account buys from."
      >
        <select
          id={`${id}-marketplace`}
          value={marketplace}
          onChange={(e) => {
            setMarketplace(e.target.value);
            setStarted(null);
          }}
          className={inputClass}
        >
          {data.marketplaces.map((m) => (
            <option key={m} value={m}>
              {marketplaceName(m)}
            </option>
          ))}
        </select>
      </Field>
      <div className="flex flex-wrap gap-2">
        <button
          type="button"
          onClick={open}
          disabled={busy || marketplace === ''}
          className={started ? secondaryButton : primaryButton}
        >
          {busy ? 'Opening...' : started ? 'Open the Amazon sign-in again' : 'Open Amazon sign-in'}
        </button>
        <button type="button" onClick={onCancel} className={secondaryButton}>
          Cancel
        </button>
      </div>
      {error && <Notice kind="error">{error}</Notice>}
      {started && (
        <>
          <p className="text-sm text-body">
            Sign in on the Amazon page that opened (or{' '}
            <a
              href={started.url}
              target="_blank"
              rel="noreferrer"
              className="text-pink-500 underline"
            >
              open it here
            </a>
            ). Amazon then shows a page that says it can&apos;t be found: that is expected. Copy
            that page&apos;s address from the browser&apos;s address bar and paste it below within{' '}
            {minutesLeft(started.expires_at)} minutes.
          </p>
          <Field label="The address Amazon showed" htmlFor={`${id}-redirect`}>
            <input
              id={`${id}-redirect`}
              type="text"
              autoComplete="off"
              spellCheck={false}
              value={redirect}
              onChange={(e) => setRedirect(e.target.value)}
              placeholder="https://www.amazon.../ap/maplanding?..."
              className={inputClass}
            />
          </Field>
          <MappingForm
            data={data}
            initial={initial.mapping ?? defaultMapping(data)}
            policyHere={!initial.mapping}
            submitLabel="Finish the sign-in"
            busyLabel="Signing in..."
            check={() => redirectProblem(redirect)}
            onSubmit={async (m) => {
              setTried(true);
              onDone(await client.finishAudibleSignIn(started.login, redirect.trim(), m));
              setRedirect('');
            }}
          />
        </>
      )}
    </div>
  );
}

// POLICY_IN_CONSOLE: an account's backup policy is the server's once the
// account exists (the console's Audible account card edits it); the page picks
// it only for a new account, and the server keeps its own after that.
const POLICY_IN_CONSOLE = "Chosen in the server's console (the Audible account card)";

function MappingForm({
  data,
  initial,
  policyHere,
  submitLabel,
  busyLabel,
  check,
  onSubmit,
  onCancel,
}: {
  data: AudibleAccounts;
  initial: AudibleMapping;
  // policyHere: the form picks the backup policy (a new account only).
  policyHere: boolean;
  submitLabel: string;
  busyLabel: string;
  check?: () => string | null;
  onSubmit: (m: AudibleMapping) => Promise<void>;
  onCancel?: () => void;
}) {
  const id = useId();
  const [m, setM] = useState<AudibleMapping>(initial);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function submit(e: FormEvent) {
    e.preventDefault();
    const problem = check?.() ?? mappingProblem(m);
    if (problem) {
      setError(problem);
      return;
    }
    setBusy(true);
    setError(null);
    try {
      await onSubmit(m);
    } catch (err) {
      setError(errorText(err, "Couldn't save the account."));
    }
    setBusy(false);
  }

  return (
    <form onSubmit={submit} className="flex flex-col gap-3" noValidate>
      {data.users_error && <Notice kind="warn">{data.users_error}</Notice>}
      <Field
        label="Belongs to"
        htmlFor={`${id}-user`}
        note="The person on the server whose books these are."
      >
        <select
          id={`${id}-user`}
          value={m.user_id || ''}
          onChange={(e) => setM({ ...m, user_id: Number(e.target.value) || 0 })}
          className={inputClass}
        >
          <option value="">Pick a person</option>
          {data.users.map((u) => (
            <option key={u.id} value={u.id}>
              {u.name}
            </option>
          ))}
        </select>
      </Field>
      <Field label="Library" htmlFor={`${id}-library`} note="The books library its titles go to.">
        <select
          id={`${id}-library`}
          value={m.library_id || ''}
          onChange={(e) => setM({ ...m, library_id: Number(e.target.value) || 0 })}
          className={inputClass}
        >
          <option value="">Pick a library</option>
          {data.libraries.map((l) => (
            <option key={l.id} value={l.id}>
              {l.name}
            </option>
          ))}
        </select>
      </Field>
      {policyHere ? (
        <Field
          label="Back up"
          htmlFor={`${id}-policy`}
          note="Which new purchases the Bridge backs up on its own after each check (titles first seen after you choose it); any title can still be backed up from the console, which changes this later."
        >
          <select
            id={`${id}-policy`}
            value={m.policy}
            onChange={(e) => setM({ ...m, policy: e.target.value as AudiblePolicy })}
            className={inputClass}
          >
            {data.policies.map((p) => (
              <option key={p} value={p}>
                {POLICY_LABELS[p] ?? p}
              </option>
            ))}
          </select>
        </Field>
      ) : (
        <p className="text-sm text-dim">
          Which new purchases are backed up on their own is changed in the server&apos;s console, on
          the Audible account card.
        </p>
      )}
      <p className="text-sm text-dim">
        Keeping the listening place in step with Audible is turned on in the server&apos;s console,
        on the Audible account card.
      </p>
      <Field
        label="Name (optional)"
        htmlFor={`${id}-label`}
        note="How the console names the account."
      >
        <input
          id={`${id}-label`}
          type="text"
          value={m.label}
          maxLength={100}
          onChange={(e) => setM({ ...m, label: e.target.value })}
          placeholder="The person and the store, if left empty"
          className={inputClass}
        />
      </Field>
      {error && <Notice kind="error">{error}</Notice>}
      <div className="flex flex-wrap gap-2">
        <button type="submit" disabled={busy} className={primaryButton}>
          {busy ? busyLabel : submitLabel}
        </button>
        {onCancel && (
          <button type="button" onClick={onCancel} disabled={busy} className={secondaryButton}>
            Cancel
          </button>
        )}
      </div>
    </form>
  );
}
