import { describe, expect, it } from 'vitest';
import { id } from './api';

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

describe('id', () => {
  it('returns a v4 UUID in secure contexts', () => {
    expect(id()).toMatch(UUID_RE);
  });

  it('falls back to a manual v4 UUID when crypto.randomUUID is unavailable (LAN HTTP)', () => {
    const original = globalThis.crypto.randomUUID;
    Object.defineProperty(globalThis.crypto, 'randomUUID', { value: undefined, configurable: true });
    try {
      const value = id();
      expect(value).toMatch(UUID_RE);
      expect(value).not.toEqual(id());
    } finally {
      Object.defineProperty(globalThis.crypto, 'randomUUID', { value: original, configurable: true });
    }
  });
});
