import { useRef, useState, type FormEvent } from 'react';
import { errorText, type ApiClient } from '@/api/client';
import type { AgentBackendName, AgentSettings, EndpointCheck } from '@/api/types';
import { useLoad } from '@/lib/useLoad';
import { LOCKED_NOTE, isLocked } from '@/lib/settings';
import {
  BACKEND_LABELS,
  KEY_AGENT_BACKEND,
  KEY_ENDPOINT_BASE_URL,
  KEY_ENDPOINT_MODEL,
  checkBaseURL,
  chosenModels,
  endpointUpdate,
  missingModel,
  modelNote,
  modelWarnings,
  responsesWarning,
  stageLabel,
} from '@/lib/agent';
import { CredentialForm } from './AccountsTab';
import { Card } from '../ui/Card';
import { Field, inputClass } from '../ui/Field';
import { Loading } from '../ui/Loading';
import { Notice } from '../ui/Notice';
import { primaryButton, secondaryButton } from '../ui/buttons';

// AgentSettingsCard is Settings > Agent (bridge#84): which backend the agent
// runs, and the OpenAI-compatible endpoint's address, key and models, with a
// check of the endpoint.
export function AgentSettingsCard({ client }: { client: ApiClient }) {
  const [backend, setBackend] = useState<AgentBackendName>('claude');
  const [baseUrl, setBaseUrl] = useState('');
  const [model, setModel] = useState('');
  const [models, setModels] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const [check, setCheck] = useState<EndpointCheck | null>(null);
  const [checking, setChecking] = useState(false);
  const [checkError, setCheckError] = useState<string | null>(null);
  // keyless: the last check went without the API key that is set (the address
  // tested isn't the saved one). keyCleared: a save moved the address, which
  // removed the old address's key.
  const [keyless, setKeyless] = useState(false);
  const [keyCleared, setKeyCleared] = useState(false);
  // checkSeq numbers the checks and the address edits: a check's answer is
  // shown only while nothing newer came (it is of the address it was sent).
  const checkSeq = useRef(0);

  // fill puts the saved agent settings into the fields.
  function fill(s: AgentSettings) {
    setBackend(s.backend);
    setBaseUrl(s.openai_compatible.base_url);
    setModel(s.openai_compatible.model);
    setModels({ ...s.openai_compatible.models });
    return s;
  }

  const {
    data: settings,
    setData: setSettings,
    error: loadError,
  } = useLoad(async () => fill(await client.agentSettings()), null, {
    fallback: "Couldn't load the agent settings.",
  });

  if (!settings) {
    return (
      <Card title="Agent">
        <Loading error={loadError} />
      </Card>
    );
  }

  const backendLocked = isLocked(settings, KEY_AGENT_BACKEND);
  const urlLocked = isLocked(settings, KEY_ENDPOINT_BASE_URL);
  const modelLocked = isLocked(settings, KEY_ENDPOINT_MODEL);
  const endpoint = backend === 'openai-compatible';
  const update = endpointUpdate(baseUrl, model, models);
  // The key is kept for the saved address alone, so it is entered once the
  // address in the field is the saved one.
  const addressSaved =
    settings.openai_compatible.base_url !== '' &&
    (urlLocked || update.base_url === settings.openai_compatible.base_url);
  const warnings = endpoint ? modelWarnings(check, chosenModels(update, settings.stages)) : [];
  const responses = endpoint ? responsesWarning(check) : null;

  async function save(e: FormEvent) {
    e.preventDefault();
    if (!settings) return;
    setSaved(false);
    // Another backend keeps an address for a later switch, but only a usable one.
    const problem =
      !urlLocked && (endpoint || update.base_url !== '') ? checkBaseURL(baseUrl) : null;
    if (problem) {
      setError(problem);
      return;
    }
    const stage = endpoint ? missingModel(update, settings.stages) : null;
    if (stage) {
      setError(
        `Enter the model every stage uses, or one for ${stageLabel(stage)}: an endpoint has no default model.`,
      );
      return;
    }
    setBusy(true);
    setError(null);
    try {
      // A locked setting is sent with the value it has, which the Bridge accepts.
      const next = fill(
        await client.saveAgentSettings({
          backend: backendLocked ? settings.backend : backend,
          openai_compatible: {
            ...update,
            base_url: urlLocked ? settings.openai_compatible.base_url : update.base_url,
            model: modelLocked ? settings.openai_compatible.model : update.model,
          },
        }),
      );
      // Another address: the Bridge removed the key entered for the old one,
      // so "saving this address removes the key" has happened.
      setKeyCleared(settings.key_set && !next.key_set);
      if (next.openai_compatible.base_url !== settings.openai_compatible.base_url) {
        setKeyless(false);
      }
      setSettings(next);
      setSaved(true);
    } catch (err) {
      setError(errorText(err, "Couldn't save the agent settings."));
    }
    setBusy(false);
  }

  async function test() {
    const problem = checkBaseURL(baseUrl);
    setCheck(null);
    setKeyless(false);
    if (problem) {
      setCheckError(problem);
      return;
    }
    setChecking(true);
    setCheckError(null);
    const seq = ++checkSeq.current;
    try {
      const res = await client.checkEndpoint(update.base_url);
      if (seq !== checkSeq.current) return; // the address was edited meanwhile
      setKeyless(!!settings?.key_set && !res.key_sent);
      if (res.error) setCheckError(res.error);
      else setCheck(res);
    } catch (err) {
      if (seq !== checkSeq.current) return;
      setCheckError(errorText(err, "Couldn't check the endpoint."));
    }
    setChecking(false);
  }

  const restart =
    settings.backend !== settings.running_backend
      ? `The agent runs ${BACKEND_LABELS[settings.running_backend] ?? settings.running_backend} until the Bridge restarts: restart it to use ${BACKEND_LABELS[settings.backend] ?? settings.backend}.`
      : null;

  return (
    <Card title="Agent">
      <form onSubmit={save} className="flex flex-col gap-4" noValidate>
        <Field
          label="The agent runs"
          htmlFor="agent-backend"
          note={
            backendLocked
              ? LOCKED_NOTE
              : 'A change of backend applies when the Bridge restarts. Its sign-in is on the Accounts tab.'
          }
        >
          <select
            id="agent-backend"
            value={backend}
            onChange={(e) => setBackend(e.target.value as AgentBackendName)}
            disabled={backendLocked}
            className={inputClass}
          >
            {settings.backends.map((b) => (
              <option key={b} value={b}>
                {BACKEND_LABELS[b] ?? b}
              </option>
            ))}
          </select>
        </Field>
        {restart && <Notice kind="warn">{restart}</Notice>}
        {endpoint && (
          <>
            <Field
              label="Endpoint address"
              htmlFor="endpoint-url"
              note={
                urlLocked
                  ? LOCKED_NOTE
                  : 'Up to /v1, like https://api.openai.com/v1, or http://192.168.1.20:11434/v1 for Ollama on your network. A server on your network must listen on it (not only on localhost), and offer the Responses API.'
              }
            >
              <input
                id="endpoint-url"
                type="url"
                value={baseUrl}
                onChange={(e) => {
                  setBaseUrl(e.target.value);
                  // A check was of the address it had: another one's models
                  // and warnings would mislead (one still out is dropped).
                  checkSeq.current++;
                  setChecking(false);
                  setCheck(null);
                  setCheckError(null);
                  setKeyless(false);
                }}
                readOnly={urlLocked}
                disabled={urlLocked}
                placeholder="http://192.168.1.20:11434/v1"
                className={inputClass}
              />
            </Field>
            <Field
              label="Model"
              htmlFor="endpoint-model"
              note={
                modelLocked
                  ? LOCKED_NOTE
                  : 'The model every stage uses, as the endpoint names it. It must call tools and take a long context (32k tokens or more).'
              }
            >
              <input
                id="endpoint-model"
                type="text"
                value={model}
                onChange={(e) => setModel(e.target.value)}
                readOnly={modelLocked}
                disabled={modelLocked}
                placeholder="qwen3:32b"
                spellCheck={false}
                className={inputClass}
              />
            </Field>
            <details className="rounded-md border border-edge px-3 py-2">
              <summary className="cursor-pointer text-sm text-hi">Models per stage</summary>
              <div className="mt-3 flex flex-col gap-3">
                <p className="text-xs text-dim">Leave one blank to use the model above.</p>
                {settings.stages.map((stage) => (
                  <Field key={stage} label={stageLabel(stage)} htmlFor={`stage-${stage}`}>
                    <input
                      id={`stage-${stage}`}
                      type="text"
                      value={models[stage] ?? ''}
                      onChange={(e) => setModels({ ...models, [stage]: e.target.value })}
                      placeholder={model || 'model'}
                      spellCheck={false}
                      className={inputClass}
                    />
                  </Field>
                ))}
              </div>
            </details>
          </>
        )}
        {error && <Notice kind="error">{error}</Notice>}
        {saved && <Notice kind="ok">Saved.</Notice>}
        <div className="flex flex-wrap gap-2">
          <button type="submit" disabled={busy} className={primaryButton}>
            {busy ? 'Saving...' : 'Save agent settings'}
          </button>
          {endpoint && (
            <button type="button" onClick={test} disabled={checking} className={secondaryButton}>
              {checking ? 'Testing...' : 'Test connection'}
            </button>
          )}
        </div>
      </form>
      {endpoint && (
        <div className="mt-4 flex flex-col gap-3">
          {checkError && <Notice kind="error">{checkError}</Notice>}
          {keyless && (
            <Notice kind="warn">
              Tested without the API key: the key is kept for the saved address and sent nowhere
              else. Saving this address removes the key, and you enter this address&apos;s key then.
            </Notice>
          )}
          {check && (
            <Notice kind="ok">
              The endpoint answered with {check.models.length}{' '}
              {check.models.length === 1 ? 'model' : 'models'} (checked from the Bridge).
            </Notice>
          )}
          {responses && <Notice kind="warn">{responses}</Notice>}
          {warnings.map((w) => (
            <Notice key={w} kind="warn">
              {w}
            </Notice>
          ))}
          {check && check.models.length > 0 && (
            <ul
              aria-label="The endpoint's models"
              className="flex max-h-48 flex-col gap-1 overflow-y-auto text-sm"
            >
              {check.models.map((m) => (
                <li key={m.id} className="flex justify-between gap-3">
                  <span className="font-mono text-body">{m.id}</span>
                  <span className="text-dim">{modelNote(m)}</span>
                </li>
              ))}
            </ul>
          )}
          {keyCleared && (
            <Notice kind="warn">
              The endpoint&apos;s API key was for the old address and is removed. Enter this
              address&apos;s key below, if it takes one.
            </Notice>
          )}
          {addressSaved ? (
            <CredentialForm
              // A new form for each saved address: nothing typed or said of
              // the old address's key carries over.
              key={settings.openai_compatible.base_url}
              client={client}
              kind="endpoint_api_key"
              set={settings.key_set}
              setAt={settings.key_set_at}
              endpoint={settings.openai_compatible.base_url}
              onChange={(a) => {
                setKeyCleared(false);
                // Only the key's state: the fields may hold changes not saved yet.
                setSettings({
                  ...settings,
                  key_set: !!a.agent.set.endpoint_api_key,
                  key_set_at: a.agent.set_at_by_kind.endpoint_api_key ?? '',
                });
              }}
            />
          ) : (
            <p className="border-t border-edge pt-4 text-sm text-dim">
              Save the endpoint&apos;s address before its API key: the key is kept for the saved
              address alone
              {settings.key_set ? ', and saving another address removes the key set now.' : '.'}
            </p>
          )}
        </div>
      )}
    </Card>
  );
}
