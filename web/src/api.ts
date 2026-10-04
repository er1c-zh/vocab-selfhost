export type Card = {
  id: string;
  word: string;
  ipa: string;
  definition: string;
  contextText: string;
  contextMeaning: string;
  source: string;
  createdAt: string;
  updatedAt: string;
  dueAt: string;
  state: string;
  difficulty: number;
  stability: number;
  lastReview?: string;
  reps: number;
  lapses: number;
};

export type CardInput = Pick<Card, 'id' | 'word' | 'ipa' | 'definition' | 'contextText' | 'contextMeaning' | 'source'>;
export type DictionaryEntry = { word: string; ipa: string; definition: string; locale?: string };
export type ReviewEvent = { id: string; cardId: string; rating: number; reviewedAt: string };
export type Bundle = { version: number; exportedAt: string; cards: Card[]; reviews: ReviewEvent[]; dictionary: DictionaryEntry[] };
export type ReviewResult = { card: Card; duplicate: boolean };

export type BatchItemInput = {
  id: string;
  surfaceText: string;
  normalizedText: string;
  start: number;
  end: number;
};

export type BatchCollectRequest = {
  requestId: string;
  contextText: string;
  source: string;
  items: BatchItemInput[];
};

export type BatchItemResult = {
  id: string;
  normalizedText: string;
  status: 'created' | 'duplicate' | 'error';
  cardId?: string;
  word?: string;
  error?: string;
};

export type BatchCollectResponse = {
  requestId: string;
  results: BatchItemResult[];
  created: number;
  duplicate: number;
  failed: number;
};

// Text-AI gloss settings saved on the server. The API key never round-trips;
// the UI only sees whether one exists plus a masked hint.
export type AiTextSettings = {
  provider: '' | 'disabled' | 'openai-compatible';
  baseUrl: string;
  model: string;
  hasKey: boolean;
  keyHint: string;
};

export type PronunciationSettings = {
  provider: '' | 'local' | 'azure';
  azureRegion: string;
  hasKey: boolean;
  keyHint: string;
};

export type PronunciationModelStatus = {
  state: 'not_downloaded' | 'downloading' | 'downloaded' | 'ready' | 'failed';
  downloaded: boolean;
  loaded: boolean;
  error: string;
};

// Operations that were saved locally and still need to reach the server.
export type PendingOperation =
  | { kind: 'create'; card: CardInput }
  | { kind: 'update'; id: string; card: CardInput }
  | { kind: 'delete'; id: string }
  | { kind: 'review'; cardId: string; id: string; rating: number; reviewedAt: string }
  | { kind: 'import'; bundle: Bundle }
  | { kind: 'batch'; request: BatchCollectRequest };

const cacheKey = 'wordwell.bundle.v1';
const wordCacheKey = 'wordwell.dictionary.v1';

export function readCachedBundle(): Bundle | null {
  try { return JSON.parse(localStorage.getItem(cacheKey) || 'null') as Bundle | null; } catch { return null; }
}
export function saveCachedBundle(bundle: Bundle) { localStorage.setItem(cacheKey, JSON.stringify(bundle)); }

export async function api<T>(path: string, init: RequestInit = {}, timeoutMs = 30000): Promise<T> {
  const headers = new Headers(init.headers);
  if (init.body && !headers.has('Content-Type')) headers.set('Content-Type', 'application/json');
  const controller = new AbortController();
  const timer = window.setTimeout(() => controller.abort(), timeoutMs);
  if (init.signal?.addEventListener) init.signal.addEventListener('abort', () => controller.abort());
  let response: Response;
  try {
    response = await fetch(`/api${path}`, { ...init, headers, signal: controller.signal });
  } catch (error) {
    // A request that neither succeeded nor got a server answer is a network problem.
    const networkError = new Error('Network request failed or timed out');
    networkError.name = controller.signal.aborted ? 'TIMEOUT_ERROR' : 'NETWORK_ERROR';
    throw networkError;
  } finally {
    window.clearTimeout(timer);
  }
  if (!response.ok) {
    const body = await response.json().catch(() => ({})) as { error?: string };
    const error = new Error(body.error || `Request failed (${response.status})`);
    error.name = response.status >= 500 ? 'SERVER_ERROR' : 'API_ERROR';
    throw error;
  }
  if (response.status === 204) return undefined as T;
  return await response.json() as T;
}

export function id() {
  // crypto.randomUUID only exists in secure contexts (HTTPS or localhost);
  // LAN HTTP deployments need the manual v4 fallback.
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') return crypto.randomUUID();
  const bytes = new Uint8Array(16);
  if (typeof crypto !== 'undefined' && typeof crypto.getRandomValues === 'function') crypto.getRandomValues(bytes);
  else for (let i = 0; i < 16; i += 1) bytes[i] = Math.floor(Math.random() * 256);
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

// Recent dictionary lookups cached in the browser, used to pre-fill the batch
// collection form when the user is offline or before the server responds.
export function saveWordCache(entries: DictionaryEntry[]) {
  try {
    const prior = getWordCache();
    for (const entry of entries) prior[entry.word.toLowerCase()] = entry;
    const items = Object.values(prior).slice(-250);
    localStorage.setItem(wordCacheKey, JSON.stringify(Object.fromEntries(items.map((entry) => [entry.word.toLowerCase(), entry]))));
  } catch { /* Dictionary cache is optional; card data is stored separately. */ }
}
export function getWordCache(): Record<string, DictionaryEntry> {
  try { return JSON.parse(localStorage.getItem(wordCacheKey) || '{}') as Record<string, DictionaryEntry>; } catch { return {}; }
}
export function emptyBundle(): Bundle { return { version: 1, exportedAt: new Date().toISOString(), cards: [], reviews: [], dictionary: [] }; }
