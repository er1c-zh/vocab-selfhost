import { describe, expect, it } from 'vitest';
import { encodePCM16Wav } from './audio';

describe('encodePCM16Wav', () => {
  it('writes mono 16-bit PCM WAV data with the requested sample rate', () => {
    const buffer = encodePCM16Wav(new Float32Array([0, -1, 1, 0.5]), 16_000);
    const view = new DataView(buffer);
    const text = (offset: number, length: number) => String.fromCharCode(...new Uint8Array(buffer, offset, length));

    expect(text(0, 4)).toBe('RIFF');
    expect(view.getUint32(4, true)).toBe(buffer.byteLength - 8);
    expect(text(8, 4)).toBe('WAVE');
    expect(view.getUint16(20, true)).toBe(1);
    expect(view.getUint16(22, true)).toBe(1);
    expect(view.getUint32(24, true)).toBe(16_000);
    expect(view.getUint16(34, true)).toBe(16);
    expect(text(36, 4)).toBe('data');
    expect(view.getUint32(40, true)).toBe(8);
    expect(view.getInt16(44, true)).toBe(0);
    expect(view.getInt16(46, true)).toBe(-32_768);
    expect(view.getInt16(48, true)).toBe(32_767);
    expect(view.getInt16(50, true)).toBe(16_383);
  });
});
