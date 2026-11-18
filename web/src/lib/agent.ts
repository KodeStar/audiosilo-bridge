import type { AgentBackendName, AgentEndpoint, AgentSettingKey, EndpointCheck } from '@/api/types';

// Settings > Agent (bridge#84): the backend, and the OpenAI-compatible endpoint
// the openai-compatible backend runs codex against.

export const KEY_AGENT_BACKEND: AgentSettingKey = 'agent.backend';
export const KEY_ENDPOINT_BASE_URL: AgentSettingKey = 'agent.openai_compatible.base_url';
export const KEY_ENDPOINT_MODEL: AgentSettingKey = 'agent.openai_compatible.model';

export const BACKEND_LABELS: Record<AgentBackendName, string> = {
  claude: 'Claude (the claude CLI)',
  codex: 'ChatGPT or OpenAI (the codex CLI)',
  'openai-compatible': 'An OpenAI-compatible endpoint (OpenAI API, a router, Ollama, LM Studio)',
};

// What each stage a model can be set for does, in a few words.
export const STAGE_LABELS: Record<string, string> = {
  markers_normalizing: 'Chapter titles',
  qa_adjudicating: 'Transcript checks',
  spelling_research: 'Name spellings',
  fact_pass: 'Fact notes',
  synthesizing: 'Characters and recaps',
  auditing: 'Audit',
  fixing: 'Fixes',
  chapter_mapping: 'Chapter mapping',
  describing: 'Description',
};

export function stageLabel(stage: string): string {
  return STAGE_LABELS[stage] ?? stage;
}

// MIN_CONTEXT_TOKENS is the context below which the page warns: a chapter's
// transcript, the stage's notes and codex's own instructions take about this.
export const MIN_CONTEXT_TOKENS = 32000;

// checkBaseURL is the page's first look at the endpoint's address: an error
// sentence, or null (the Bridge checks it again, by the same rules).
export function checkBaseURL(text: string): string | null {
  const t = text.trim();
  if (t === '') return "Enter the endpoint's address, like http://192.168.1.20:11434/v1.";
  if (/[^\x21-\x7e]|["\\]/.test(t)) {
    return 'The address holds a space, a quote or a character an address never has.';
  }
  let u: URL;
  try {
    u = new URL(t);
  } catch {
    return 'Enter a full address, like http://192.168.1.20:11434/v1.';
  }
  if (u.protocol !== 'http:' && u.protocol !== 'https:') {
    return 'The address starts with http:// or https://.';
  }
  if (u.username !== '' || u.password !== '') {
    return 'Leave the user name and password out of the address: set the API key instead.';
  }
  if (t.includes('?') || t.includes('#')) {
    return 'Leave the query (?...) and the fragment (#...) out of the address.';
  }
  return null;
}

// endpointUpdate is the endpoint as the form holds it, trimmed, with the
// stages left blank dropped (they use the endpoint's model).
export function endpointUpdate(
  baseUrl: string,
  model: string,
  models: Record<string, string>,
): AgentEndpoint {
  const out: Record<string, string> = {};
  for (const [stage, m] of Object.entries(models)) {
    if (m.trim() !== '') out[stage] = m.trim();
  }
  return { base_url: baseUrl.trim().replace(/\/+$/, ''), model: model.trim(), models: out };
}

// missingModel names the first stage with no model at all (no stage model and
// no endpoint model), or null: an endpoint has no default model.
export function missingModel(e: AgentEndpoint, stages: string[]): string | null {
  if (e.model !== '') return null;
  return stages.find((s) => !e.models[s]) ?? null;
}

// chosenModels are the models the stages use, each once, in stage order.
export function chosenModels(e: AgentEndpoint, stages: string[]): string[] {
  const out: string[] = [];
  for (const s of stages) {
    const m = e.models[s] || e.model;
    if (m && !out.includes(m)) out.push(m);
  }
  return out;
}

// modelWarnings are the page's warnings about the chosen models, from what the
// endpoint said in a check (never a block: the stages' own checks still refuse
// bad output): a model the endpoint doesn't list, one it says can't call tools,
// one with a small context. Nothing before a check, nor for an endpoint that
// listed no models.
export function modelWarnings(check: EndpointCheck | null, chosen: string[]): string[] {
  if (!check || check.error !== '' || check.models.length === 0) return [];
  const out: string[] = [];
  for (const id of chosen) {
    const m = check.models.find((x) => x.id === id);
    if (!m) {
      out.push(`${id} isn't among the endpoint's models.`);
      continue;
    }
    if (m.tools === false) {
      out.push(
        `${id} doesn't call tools, which the agent needs to read its notes and write its results: expect its runs to fail.`,
      );
    }
    if (m.context_tokens && m.context_tokens < MIN_CONTEXT_TOKENS) {
      out.push(
        `${id} takes ${m.context_tokens.toLocaleString('en-US')} tokens of context; the agent's stages need about ${MIN_CONTEXT_TOKENS.toLocaleString('en-US')} or more, so long chapters may not fit.`,
      );
    }
  }
  return out;
}

// responsesWarning is the warning when the endpoint doesn't offer (or may not
// offer) the Responses API the agent needs, else null.
export function responsesWarning(check: EndpointCheck | null): string | null {
  if (!check || check.error !== '') return null;
  if (check.responses === 'no')
    return (
      check.responses_detail ||
      "The endpoint doesn't offer the Responses API, which the agent needs."
    );
  if (check.responses === 'unknown') {
    return `The check couldn't tell whether the endpoint offers the Responses API, which the agent needs${check.responses_detail ? ` (${check.responses_detail})` : ''}.`;
  }
  return null;
}

// modelNote is what the endpoint says of a model, for the check's list.
export function modelNote(m: { context_tokens?: number; tools?: boolean }): string {
  const parts: string[] = [];
  if (m.context_tokens) parts.push(`${m.context_tokens.toLocaleString('en-US')} tokens`);
  if (m.tools === true) parts.push('tools');
  if (m.tools === false) parts.push('no tools');
  return parts.join(', ');
}
