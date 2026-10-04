const AZURE_SAMPLE_RATE = 16_000;

// Azure's short-audio REST endpoint accepts mono 16 kHz PCM WAV (or OGG Opus),
// while browsers commonly record WebM or MP4. Normalize the short recording in
// the browser so both Azure and the local ASR receive the same portable format.
export async function normalizeRecordingToWav(blob: Blob): Promise<Blob> {
  const legacyWindow = window as Window & { webkitAudioContext?: typeof AudioContext };
  const AudioContextConstructor = window.AudioContext || legacyWindow.webkitAudioContext;
  if (!AudioContextConstructor) throw new Error('当前浏览器无法转换录音格式');

  const context = new AudioContextConstructor();
  try {
    const decoded = await context.decodeAudioData(await blob.arrayBuffer());
    const mono = new Float32Array(decoded.length);
    for (let channel = 0; channel < decoded.numberOfChannels; channel += 1) {
      const samples = decoded.getChannelData(channel);
      for (let index = 0; index < samples.length; index += 1) mono[index] += samples[index] / decoded.numberOfChannels;
    }

    const outputLength = Math.max(1, Math.floor(mono.length * AZURE_SAMPLE_RATE / decoded.sampleRate));
    const resampled = new Float32Array(outputLength);
    for (let index = 0; index < outputLength; index += 1) {
      const sourcePosition = index * decoded.sampleRate / AZURE_SAMPLE_RATE;
      const left = Math.floor(sourcePosition);
      const right = Math.min(left + 1, mono.length - 1);
      const fraction = sourcePosition - left;
      resampled[index] = mono[left] * (1 - fraction) + mono[right] * fraction;
    }

    return new Blob([encodePCM16Wav(resampled, AZURE_SAMPLE_RATE)], { type: 'audio/wav' });
  } catch (error) {
    if (error instanceof Error && error.message) throw error;
    throw new Error('无法解码麦克风录音，请重试');
  } finally {
    if (context.state !== 'closed') await context.close().catch(() => undefined);
  }
}

export function encodePCM16Wav(samples: Float32Array, sampleRate: number): ArrayBuffer {
  const dataSize = samples.length * 2;
  const buffer = new ArrayBuffer(44 + dataSize);
  const view = new DataView(buffer);
  const writeText = (offset: number, value: string) => {
    for (let index = 0; index < value.length; index += 1) view.setUint8(offset + index, value.charCodeAt(index));
  };

  writeText(0, 'RIFF');
  view.setUint32(4, 36 + dataSize, true);
  writeText(8, 'WAVE');
  writeText(12, 'fmt ');
  view.setUint32(16, 16, true);
  view.setUint16(20, 1, true); // Linear PCM
  view.setUint16(22, 1, true); // Mono
  view.setUint32(24, sampleRate, true);
  view.setUint32(28, sampleRate * 2, true);
  view.setUint16(32, 2, true);
  view.setUint16(34, 16, true);
  writeText(36, 'data');
  view.setUint32(40, dataSize, true);
  for (let index = 0; index < samples.length; index += 1) {
    const sample = Math.max(-1, Math.min(1, samples[index]));
    view.setInt16(44 + index * 2, sample < 0 ? sample * 32_768 : sample * 32_767, true);
  }
  return buffer;
}
