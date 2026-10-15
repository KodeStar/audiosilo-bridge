import { useState, type FormEvent } from 'react';
import { errorText, type ApiClient } from '@/api/client';
import type { Library } from '@/api/types';
import { useLoad } from '@/lib/useLoad';
import { checkBadges, checkMountPath, mountSource } from '@/lib/libraries';
import { Ago } from '../ui/Ago';
import { Card } from '../ui/Card';
import { Badge } from '../ui/Badge';
import { EmptyState } from '../ui/EmptyState';
import { Field, inputClass } from '../ui/Field';
import { Loading } from '../ui/Loading';
import { Notice } from '../ui/Notice';
import { Row } from '../ui/Row';
import { primaryButton, secondaryButton } from '../ui/buttons';

export const LIBRARIES_POLL_MS = 10000;

export function LibrariesTab({ client }: { client: ApiClient }) {
  const {
    data: libraries,
    setData: setLibraries,
    error,
    setError,
  } = useLoad(async () => (await client.libraries()).libraries, LIBRARIES_POLL_MS);
  const [checking, setChecking] = useState(false);
  const [checkNote, setCheckNote] = useState<string | null>(null);

  // The check runs in the background (a 202), so reloading now would show the
  // old results: the poll picks the new ones up.
  async function checkNow() {
    setChecking(true);
    setCheckNote(null);
    try {
      await client.checkLibraries();
      setCheckNote('Checking. The results show here when they are in.');
    } catch (err) {
      setError(errorText(err, "Couldn't start the check."));
    }
    setChecking(false);
  }

  if (libraries === null) return <Loading error={error} />;

  return (
    <div className="flex flex-col gap-5">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <p className="max-w-prose text-sm text-dim">
          Where the Bridge sees each of the server&apos;s libraries. Each one is checked by
          comparing a book&apos;s files with the server&apos;s, and by writing a test file.
        </p>
        <button
          type="button"
          onClick={checkNow}
          disabled={checking || libraries.length === 0}
          className={secondaryButton}
        >
          {checking ? 'Starting...' : 'Check now'}
        </button>
      </div>
      {error && <Notice kind="error">{error}</Notice>}
      {checkNote && <Notice kind="info">{checkNote}</Notice>}
      {libraries.length === 0 ? (
        <EmptyState title="No libraries yet">
          The server lists its libraries in its answer to the Bridge&apos;s heartbeat. Pair the
          Bridge on the Status tab, then wait for the first heartbeat.
        </EmptyState>
      ) : (
        libraries.map((lib) => (
          <LibraryCard key={lib.library.id} client={client} lib={lib} onSaved={setLibraries} />
        ))
      )}
    </div>
  );
}

function LibraryCard({
  client,
  lib,
  onSaved,
}: {
  client: ApiClient;
  lib: Library;
  onSaved: (libs: Library[]) => void;
}) {
  const [editing, setEditing] = useState(false);
  const [path, setPath] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const id = lib.library.id;
  const st = lib.status;

  function startEdit() {
    setPath(lib.configured ? lib.mount_path : '');
    setError(null);
    setEditing(true);
  }

  async function save(e: FormEvent) {
    e.preventDefault();
    const problem = checkMountPath(path);
    if (problem) {
      setError(problem);
      return;
    }
    setBusy(true);
    setError(null);
    try {
      onSaved((await client.setMount(id, path.trim())).libraries);
      setEditing(false);
    } catch (err) {
      setError(errorText(err, "Couldn't save the folder."));
    }
    setBusy(false);
  }

  return (
    <Card
      title={lib.library.name || `Library ${id}`}
      actions={checkBadges(lib).map((b) => (
        <Badge key={b.label} tone={b.tone}>
          {b.label}
        </Badge>
      ))}
    >
      <dl className="divide-y divide-edge">
        <Row label="Server path">
          <code className="text-xs">{lib.library.root}</code>
        </Row>
        <Row label="Bridge folder">
          <code className="text-xs">{lib.mount_path}</code>
          <span className="ml-2 text-xs text-dim">{mountSource(lib)}</span>
        </Row>
        {st && (
          <>
            {st.checked_path && (
              <Row label="Checked book">
                <code className="text-xs">{st.checked_path}</code>
              </Row>
            )}
            {st.error && (
              <Row label="Problem">
                <span className="text-red-300">{st.error}</span>
              </Row>
            )}
          </>
        )}
        <Row label="Checked">{lib.checked_at ? <Ago at={lib.checked_at} /> : 'Not yet'}</Row>
      </dl>
      <div className="mt-4">
        {lib.locked ? (
          <p className="text-xs text-dim">
            AUDIOSILO_BRIDGE_LIBRARIES sets the folders, so they can&apos;t be changed here.
          </p>
        ) : editing ? (
          <form onSubmit={save} className="flex flex-col gap-3" noValidate>
            <Field
              label="Bridge folder"
              htmlFor={`mount-${id}`}
              note="An absolute path inside the Bridge's container. Leave it empty to use the server's path."
            >
              <input
                id={`mount-${id}`}
                type="text"
                value={path}
                onChange={(e) => setPath(e.target.value)}
                placeholder={lib.library.root}
                className={inputClass}
              />
            </Field>
            {error && <Notice kind="error">{error}</Notice>}
            <div className="flex gap-2">
              <button type="submit" disabled={busy} className={primaryButton}>
                {busy ? 'Saving...' : 'Save folder'}
              </button>
              <button
                type="button"
                onClick={() => setEditing(false)}
                disabled={busy}
                className={secondaryButton}
              >
                Cancel
              </button>
            </div>
          </form>
        ) : (
          <button type="button" onClick={startEdit} className={secondaryButton}>
            Change folder
          </button>
        )}
      </div>
    </Card>
  );
}
