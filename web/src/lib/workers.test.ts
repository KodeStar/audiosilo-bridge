import { describe, expect, it } from 'vitest';
import {
  asrText,
  audioHours,
  bridgeAddress,
  fallbackText,
  isLoopback,
  shellQuote,
  workerASR,
  workerCommands,
  workerSpeed,
} from './workers';

describe('workers', () => {
  it('uses the public address, else the page origin', () => {
    expect(bridgeAddress('http://192.168.1.20:8091/', 'http://localhost:8091')).toBe(
      'http://192.168.1.20:8091',
    );
    expect(bridgeAddress(' ', 'http://nas.lan:8091/')).toBe('http://nas.lan:8091');
  });

  it('tells a loopback address', () => {
    expect(isLoopback('http://localhost:8091')).toBe(true);
    expect(isLoopback('http://127.0.0.1:8091')).toBe(true);
    expect(isLoopback('http://[::1]:8091')).toBe(true);
    expect(isLoopback('http://192.168.1.20:8091')).toBe(false);
    expect(isLoopback('not a url')).toBe(false);
  });

  it('quotes for the shell only when needed', () => {
    expect(shellQuote('asw_abc123')).toBe('asw_abc123');
    expect(shellQuote('http://nas.lan:8091')).toBe('http://nas.lan:8091');
    expect(shellQuote('http://x/?a=1&b=2')).toBe(`'http://x/?a=1&b=2'`);
    expect(shellQuote(`it's`)).toBe(`'it'\\''s'`);
  });

  it('builds the commands', () => {
    expect(workerCommands('http://nas.lan:8091', 'asw_t')).toEqual({
      install: 'audiosilo-bridge worker install --bridge http://nas.lan:8091 --token asw_t',
      run: 'audiosilo-bridge worker --bridge http://nas.lan:8091 --token asw_t',
    });
  });

  it('words a worker', () => {
    expect(workerSpeed({ audio_seconds: 0, decode_seconds: 0 })).toBe('');
    expect(workerSpeed({ audio_seconds: 3600, decode_seconds: 180 })).toBe('20x real time');
    expect(workerSpeed({ audio_seconds: 300, decode_seconds: 100 })).toBe('3.0x real time');
    expect(workerASR({ backend: 'mlx-whisper', device: 'metal', platform: 'darwin/arm64' })).toBe(
      'mlx-whisper on metal, darwin/arm64',
    );
    expect(workerASR({ backend: '', device: '', platform: '' })).toBe('');
    expect(audioHours(1800)).toBe('30 min');
    expect(audioHours(40100)).toBe('11.1 h');
  });

  it('words the fallback and where speech recognition runs', () => {
    expect(fallbackText('wait')).toBe('books wait for one');
    expect(fallbackText('local')).toBe('the Bridge transcribes them itself');
    const base = { available: true, detail: '', worker_fallback: 'local' as const, waiting: false };
    expect(asrText({ ...base, backend: 'worker', device: 'remote' })).toBe(
      'An ASR worker (connected)',
    );
    expect(
      asrText({
        ...base,
        backend: 'worker',
        device: 'remote',
        detail: 'no worker connected',
        waiting: true,
      }),
    ).toBe('Waiting for an ASR worker: no worker connected');
    expect(asrText({ ...base, backend: 'whisper-cpp', device: 'cpu' })).toBe(
      'whisper-cpp (cpu) on the Bridge',
    );
    expect(
      asrText({
        ...base,
        backend: 'whisper-cpp',
        device: '',
        available: false,
        detail: 'no binary',
      }),
    ).toBe('Not available: no binary');
  });
});
