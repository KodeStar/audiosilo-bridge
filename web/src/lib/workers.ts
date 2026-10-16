import type { EngineASR, Worker, WorkerFallback } from '@/api/types';

// The Bridge's address as a worker reaches it: the public address when one is
// set, else the address this page was opened at (a worker on another computer
// can't use localhost, so the page says so).
export function bridgeAddress(publicUrl: string, origin: string): string {
  const pub = publicUrl.trim().replace(/\/+$/, '');
  return pub || origin.replace(/\/+$/, '');
}

// isLoopback: an address only this computer can reach.
export function isLoopback(address: string): boolean {
  try {
    const host = new URL(address).hostname;
    return host === 'localhost' || host === '[::1]' || host.startsWith('127.');
  } catch {
    return false;
  }
}

// shellQuote quotes a word for sh (the token and address are plain, but an
// address can hold a ? or &).
export function shellQuote(word: string): string {
  return /^[A-Za-z0-9_./:=@%+-]+$/.test(word) ? word : `'${word.replace(/'/g, `'\\''`)}'`;
}

// workerCommands are the two ways to run a worker with its token: installed
// as a service (it always runs), or once in a terminal.
export function workerCommands(address: string, token: string): { install: string; run: string } {
  const args = `--bridge ${shellQuote(address)} --token ${shellQuote(token)}`;
  return {
    install: `audiosilo-bridge worker install ${args}`,
    run: `audiosilo-bridge worker ${args}`,
  };
}

// workerSpeed is how many times faster than real time it transcribes ("" with
// no segment timed yet).
export function workerSpeed(w: Pick<Worker, 'audio_seconds' | 'decode_seconds'>): string {
  if (w.decode_seconds <= 0 || w.audio_seconds <= 0) return '';
  const x = w.audio_seconds / w.decode_seconds;
  return `${x >= 10 ? Math.round(x) : x.toFixed(1)}x real time`;
}

// workerASR words what it runs: "mlx-whisper on metal, darwin/arm64".
export function workerASR(w: Pick<Worker, 'backend' | 'device' | 'platform'>): string {
  const asr = w.backend ? `${w.backend}${w.device ? ` on ${w.device}` : ''}` : '';
  return [asr, w.platform].filter(Boolean).join(', ');
}

// audioHours words a number of audio seconds as hours (to one decimal) or minutes.
export function audioHours(seconds: number): string {
  if (seconds < 3600) return `${Math.round(seconds / 60)} min`;
  return `${(seconds / 3600).toFixed(1)} h`;
}

// fallbackText words what happens with no worker connected.
export function fallbackText(f: WorkerFallback): string {
  return f === 'wait' ? 'books wait for one' : 'the Bridge transcribes them itself';
}

// asrText words where speech recognition runs now.
export function asrText(asr: EngineASR): string {
  if (asr.waiting) return `Waiting for an ASR worker${asr.detail ? `: ${asr.detail}` : ''}`;
  if (!asr.available) return `Not available${asr.detail ? `: ${asr.detail}` : ''}`;
  if (asr.backend === 'worker') return 'An ASR worker (connected)';
  return `${asr.backend}${asr.device ? ` (${asr.device})` : ''} on the Bridge`;
}
