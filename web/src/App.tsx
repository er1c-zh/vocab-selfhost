import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { ChangeEvent, FormEvent, ReactNode } from 'react';
import { api, emptyBundle, getWordCache, id, readCachedBundle, saveCachedBundle, saveWordCache } from './api';
import type { AiTextSettings, BatchCollectRequest, BatchCollectResponse, Bundle, Card, CardInput, DictionaryEntry, OpsLogs, OpsMetrics, PendingOperation, PronunciationModelStatus, PronunciationSettings, ReviewResult, SpeechStorageStatus } from './api';
import { normalizeRecordingToWav } from './audio';
import { createSyncController } from './sync';
import type { SyncController, SyncStatus } from './sync';
import BatchCollectView from './BatchCollect';

type View = 'review' | 'library' | 'browse' | 'add' | 'collect' | 'monitor' | 'settings';
type PronunciationResult = {
  expected: string;
  transcript: string;
  similarity: number;
  confidence: number;
  status: string;
  method: string;
  notice: string;
  scores?: { overall?: number; accuracy: number; fluency?: number; prosody?: number; completeness?: number };
  words?: { word: string; accuracy: number; phonemes?: { phoneme: string; accuracy: number }[] }[];
};

type RecordingSession = {
  cardId: string;
  expected: string;
  pressed: boolean;
  cancelled: boolean;
  stream: MediaStream | null;
  recorder: MediaRecorder | null;
  chunks: BlobPart[];
};

let reusableMicStream: MediaStream | null = null;

function setMicStreamEnabled(stream: MediaStream | null, enabled: boolean) {
  stream?.getAudioTracks().forEach((track) => { track.enabled = enabled; });
}

function releaseMicStream(stream: MediaStream | null) {
  if (!stream) return;
  stream.getTracks().forEach((track) => track.stop());
  if (reusableMicStream === stream) reusableMicStream = null;
}

function cancelRecordingSession(session: RecordingSession) {
  session.cancelled = true;
  session.pressed = false;
  if (session.recorder?.state === 'recording') session.recorder.stop();
  releaseMicStream(session.stream);
}

const queueKey = 'wordwell.queue.v1';
const navItems: { id: View; label: string }[] = [
  { id: 'review', label: '复习' },
  { id: 'library', label: '词库' },
  { id: 'browse', label: '浏览' },
  { id: 'add', label: '新增' },
  { id: 'collect', label: '文本收词' },
  { id: 'monitor', label: '监控' },
  { id: 'settings', label: '设置' },
];

function readQueue(): PendingOperation[] {
  try { return JSON.parse(localStorage.getItem(queueKey) || '[]') as PendingOperation[]; } catch { return []; }
}
function writeQueue(queue: PendingOperation[]) { localStorage.setItem(queueKey, JSON.stringify(queue)); }
function isNetworkError(error: unknown) {
  return !navigator.onLine || error instanceof TypeError || (error instanceof Error && (error.name === 'NETWORK_ERROR' || error.name === 'TIMEOUT_ERROR'));
}
function syncLabel(status: SyncStatus) {
  if (status.state === 'syncing') return '正在同步…';
  if (status.state === 'offline') return status.pending ? `离线 · ${status.pending} 项待同步` : '离线 · 等待同步';
  if (status.state === 'error') return '同步失败 · 点击重试';
  if (status.pending) return `↻ 待同步 ${status.pending} 项`;
  return '已同步';
}
function localCard(input: CardInput, old?: Card): Card {
  const now = new Date().toISOString();
  return { ...input, createdAt: old?.createdAt || now, updatedAt: now, dueAt: old?.dueAt || now, state: old?.state || 'new', difficulty: old?.difficulty || 0, stability: old?.stability || 0, lastReview: old?.lastReview, reps: old?.reps || 0, lapses: old?.lapses || 0 };
}

// Requests Kokoro speech for `text` and returns a playable blob URL.
async function fetchSpeechAudio(text: string, voice = 'af_heart'): Promise<string> {
  // The first Kokoro request may download and load model weights; leave a
  // minute beyond the Go API's six-minute upstream timeout before aborting.
  const generated = await api<{ audioUrl: string }>('/tts', { method: 'POST', body: JSON.stringify({ text, voice }) }, 7 * 60 * 1000);
  const controller = new AbortController();
  const timer = window.setTimeout(() => controller.abort(), 30000);
  try {
    const response = await fetch(generated.audioUrl, { signal: controller.signal });
    if (!response.ok) throw new Error((await response.json().catch(() => ({})) as { error?: string }).error || '音频读取失败');
    return URL.createObjectURL(await response.blob());
  } finally {
    window.clearTimeout(timer);
  }
}

// Microphone access (recording) is a secure-context feature: browsers hide it
// on plain-HTTP pages served from a LAN IP. TTS playback is not restricted.
function microphoneState(): { supported: boolean; reason: string } {
  if (!window.isSecureContext) return { supported: false, reason: '当前页面不是安全上下文（HTTP + IP 地址）；录音功能被浏览器禁用，需 HTTPS 或 localhost' };
  if (!navigator.mediaDevices?.getUserMedia || !('MediaRecorder' in window)) return { supported: false, reason: '此浏览器未提供麦克风录制接口' };
  return { supported: true, reason: '可用（首次使用会请求麦克风权限）' };
}
function formatDue(value: string) {
  const delta = Date.parse(value) - Date.now();
  if (delta <= 0) return '现在';
  const days = Math.ceil(delta / 86_400_000);
  return days === 1 ? '明天' : `${days} 天后`;
}

export default function App() {
  const [bundle, setBundle] = useState<Bundle>(() => readCachedBundle() || emptyBundle());
  const [view, setView] = useState<View>('review');
  const [online, setOnline] = useState(navigator.onLine);
  const [syncStatus, setSyncStatus] = useState<SyncStatus>({ state: 'idle', pending: readQueue().length, error: '' });
  const [toast, setToast] = useState('');
  const [search, setSearch] = useState('');
  const [editor, setEditor] = useState<Card | null>(null);
  const [browseCardId, setBrowseCardId] = useState('');
  const toastTimer = useRef<number | undefined>(undefined);
  const bundleRef = useRef(bundle);
  bundleRef.current = bundle;

  const updateBundle = useCallback((next: Bundle) => {
    setBundle(next);
    try { saveCachedBundle(next); } catch { setToast('本机缓存空间不足，请导出备份后清理浏览器数据'); }
  }, []);

  const notify = useCallback((message: string) => {
    setToast(message);
    window.clearTimeout(toastTimer.current);
    toastTimer.current = window.setTimeout(() => setToast(''), 3600);
  }, []);

  // The sync controller is created once; it depends only on stable callbacks,
  // so component re-renders cannot re-trigger or duplicate a sync run.
  const controllerRef = useRef<SyncController | null>(null);
  if (!controllerRef.current) {
    controllerRef.current = createSyncController({
      readQueue,
      writeQueue,
      runOperation: async (op) => {
        if (op.kind === 'create') await api<Card>('/cards', { method: 'POST', body: JSON.stringify(op.card) });
        else if (op.kind === 'update') await api<Card>(`/cards/${encodeURIComponent(op.id)}`, { method: 'PUT', body: JSON.stringify(op.card) });
        else if (op.kind === 'delete') await api<void>(`/cards/${encodeURIComponent(op.id)}`, { method: 'DELETE' });
        else if (op.kind === 'review') await api<ReviewResult>(`/cards/${encodeURIComponent(op.cardId)}/review`, { method: 'POST', body: JSON.stringify({ id: op.id, rating: op.rating, reviewedAt: op.reviewedAt }) });
        else if (op.kind === 'batch') await api<BatchCollectResponse>('/cards/batch', { method: 'POST', body: JSON.stringify(op.request) });
        else await api('/import', { method: 'POST', body: JSON.stringify(op.bundle) });
      },
      pullServerState: async () => {
        const serverBundle = await api<Bundle>('/sync');
        const cached = readCachedBundle();
        updateBundle({ ...serverBundle, dictionary: cached?.dictionary || bundleRef.current.dictionary || [] });
      },
      onStatus: setSyncStatus,
      onDroppedOperation: (_op, error) => notify(`一条待同步操作被跳过：${error.message}`),
      isOnline: () => navigator.onLine,
    });
  }

  useEffect(() => {
    const controller = controllerRef.current;
    if (!controller) return;
    const onlineHandler = () => { setOnline(true); controller.notifyOnline(); };
    const offlineHandler = () => { setOnline(false); controller.notifyOffline(); };
    window.addEventListener('online', onlineHandler);
    window.addEventListener('offline', offlineHandler);
    void controller.sync();
    return () => {
      window.removeEventListener('online', onlineHandler);
      window.removeEventListener('offline', offlineHandler);
      window.clearTimeout(toastTimer.current);
      controller.dispose();
    };
  }, []);

  const syncNow = useCallback(() => controllerRef.current?.retry(), []);

  const enqueue = useCallback((op: PendingOperation) => {
    const next = [...readQueue(), op];
    writeQueue(next);
    setSyncStatus((status) => ({ ...status, pending: next.length }));
    // Upload as soon as possible; the controller ignores this while offline or retrying.
    controllerRef.current?.notifyLocalChange();
  }, []);

  const dueCards = useMemo(() => bundle.cards.filter((card) => Date.parse(card.dueAt) <= Date.now()).sort((a, b) => Date.parse(a.dueAt) - Date.parse(b.dueAt)), [bundle.cards]);
  const reviewedToday = useMemo(() => {
    const today = new Date().toDateString();
    return bundle.reviews.filter((event) => new Date(event.reviewedAt).toDateString() === today).length;
  }, [bundle.reviews]);
  const visibleCards = useMemo(() => {
    const q = search.trim().toLowerCase();
    return q ? bundle.cards.filter((card) => [card.word, card.definition, card.contextText].some((value) => value.toLowerCase().includes(q))) : bundle.cards;
  }, [bundle.cards, search]);

  const saveCard = async (input: CardInput) => {
    const existing = bundle.cards.find((card) => card.id === input.id);
    const nextCard = localCard(input, existing);
    try {
      const saved = existing
        ? await api<Card>(`/cards/${encodeURIComponent(input.id)}`, { method: 'PUT', body: JSON.stringify(input) })
        : await api<Card>('/cards', { method: 'POST', body: JSON.stringify(input) });
      updateBundle({ ...bundle, cards: [saved, ...bundle.cards.filter((card) => card.id !== saved.id)], exportedAt: new Date().toISOString() });
      notify(existing ? '卡片已更新' : '单词已保存');
    } catch (error) {
      if (!isNetworkError(error)) throw error;
      enqueue(existing ? { kind: 'update', id: input.id, card: input } : { kind: 'create', card: input });
      updateBundle({ ...bundle, cards: [nextCard, ...bundle.cards.filter((card) => card.id !== nextCard.id)] });
      notify('已保存在本机，联网后会自动同步');
    }
    setEditor(null);
    setView('library');
  };

  const removeCard = async (card: Card) => {
    if (!window.confirm(`删除 “${card.word}” 及其复习记录？`)) return;
    try {
      await api<void>(`/cards/${encodeURIComponent(card.id)}`, { method: 'DELETE' });
      updateBundle({ ...bundle, cards: bundle.cards.filter((item) => item.id !== card.id), reviews: bundle.reviews.filter((event) => event.cardId !== card.id) });
      notify('卡片已删除');
    } catch (error) {
      if (!isNetworkError(error)) { notify((error as Error).message); return; }
      enqueue({ kind: 'delete', id: card.id });
      updateBundle({ ...bundle, cards: bundle.cards.filter((item) => item.id !== card.id), reviews: bundle.reviews.filter((event) => event.cardId !== card.id) });
      notify('已在本机删除，联网后会同步');
    }
  };

  const rateCard = async (card: Card, rating: number) => {
    const eventId = id();
    const reviewedAt = new Date().toISOString();
    try {
      const result = await api<ReviewResult>(`/cards/${encodeURIComponent(card.id)}/review`, { method: 'POST', body: JSON.stringify({ id: eventId, rating, reviewedAt }) });
      updateBundle({ ...bundle, cards: bundle.cards.map((item) => item.id === card.id ? result.card : item), reviews: [...bundle.reviews, { id: eventId, cardId: card.id, rating, reviewedAt }] });
    } catch (error) {
      if (!isNetworkError(error)) { notify((error as Error).message); return; }
      enqueue({ kind: 'review', cardId: card.id, id: eventId, rating, reviewedAt });
      const optimistic = { ...card, dueAt: new Date(Date.now() + 86_400_000).toISOString(), state: 'review', reps: card.reps + 1, lapses: card.lapses + (rating === 1 ? 1 : 0), updatedAt: reviewedAt, lastReview: reviewedAt };
      updateBundle({ ...bundle, cards: bundle.cards.map((item) => item.id === card.id ? optimistic : item), reviews: [...bundle.reviews, { id: eventId, cardId: card.id, rating, reviewedAt }] });
      notify('复习已记录在本机，稍后会同步');
    }
  };

  const importBundle = async (file: File) => {
    const parsed = JSON.parse(await file.text()) as Bundle;
    if (parsed.version !== 1 || !Array.isArray(parsed.cards)) throw new Error('备份格式不受支持');
    try {
      await api('/import', { method: 'POST', body: JSON.stringify(parsed) });
      await controllerRef.current?.sync();
      notify(`已导入 ${parsed.cards.length} 张卡片`);
    } catch (error) {
      if (!isNetworkError(error)) throw error;
      enqueue({ kind: 'import', bundle: parsed });
      updateBundle({ ...bundle, cards: [...parsed.cards, ...bundle.cards.filter((card) => !parsed.cards.some((incoming) => incoming.id === card.id))] });
      notify('备份已排入本机同步队列');
    }
  };

  const submitBatch = useCallback(async (request: BatchCollectRequest): Promise<BatchCollectResponse | null> => {
    try {
      // AI gloss for phrases can make this slow; allow a generous timeout.
      const response = await api<BatchCollectResponse>('/cards/batch', { method: 'POST', body: JSON.stringify(request) }, 120000);
      void controllerRef.current?.sync(); // refresh the local bundle with the new cards
      return response;
    } catch (error) {
      if (!isNetworkError(error)) throw error;
      enqueue({ kind: 'batch', request });
      notify('当前离线：整批词语已保存，联网后自动同步');
      return null;
    }
  }, [enqueue, notify]);

  return <div className="app-shell">
    <aside className="sidebar"><Brand /><div className="sidebar-caption">YOUR STUDY SPACE</div><nav>{navItems.map((item) => <button key={item.id} className={`nav-item ${view === item.id ? 'active' : ''}`} onClick={() => { setView(item.id); setEditor(null); }}><span className="nav-icon"><NavIcon view={item.id} /></span>{item.label}{item.id === 'review' && dueCards.length > 0 && <span className="nav-count">{dueCards.length}</span>}</button>)}</nav><div className="sidebar-bottom"><div className="privacy-card"><span className="privacy-dot" /><div><b>私有词库</b><small>数据保存在你的服务器</small></div></div><button className={`subtle-button sync-${syncStatus.state}`} onClick={() => syncNow()} disabled={syncStatus.state === 'syncing'} title={syncStatus.error || undefined}>{syncLabel(syncStatus)}</button></div></aside>
    <main className="main-area">
      <header className="topbar"><div className="mobile-brand"><Brand /></div><div className="topbar-left"><span className={`connection ${online ? 'is-online' : 'is-offline'}`}><i />{online ? '已连接' : '离线'}</span>{syncStatus.pending > 0 && <span className="pending-pill">{syncStatus.pending} 项待同步</span>}</div><div className="topbar-right"><button className="sync-icon" title="同步" onClick={() => syncNow()} disabled={syncStatus.state === 'syncing'}>{syncStatus.state === 'syncing' ? '…' : '↻'}</button></div></header>
      <div className="content-wrap">
        {view === 'review' && <ReviewView cards={dueCards} allCount={bundle.cards.length} totalToday={dueCards.length + reviewedToday} onRate={rateCard} onAdd={() => { setEditor(null); setView('add'); }} notify={notify} />}
        {view === 'library' && <LibraryView cards={visibleCards} total={bundle.cards.length} search={search} setSearch={setSearch} onBrowse={(card) => { setBrowseCardId(card.id); setView('browse'); }} onEdit={(card) => { setEditor(card); setView('add'); }} onDelete={removeCard} onAdd={() => { setEditor(null); setView('add'); }} />}
        {view === 'browse' && <BrowseView cards={bundle.cards} initialCardId={browseCardId} onAdd={() => { setEditor(null); setView('add'); }} notify={notify} />}
        {view === 'add' && <EditorView key={editor?.id || 'new'} card={editor} onSave={saveCard} onCancel={() => { setEditor(null); setView('library'); }} notify={notify} />}
        {view === 'collect' && <BatchCollectView cards={bundle.cards} onSubmitBatch={submitBatch} notify={notify} />}
        {view === 'settings' && <SettingsView bundle={bundle} pending={syncStatus.pending} syncing={syncStatus.state === 'syncing'} onSync={() => syncNow()} onImport={importBundle} notify={notify} />}
        {view === 'monitor' && <MonitorView notify={notify} />}
      </div>
      <footer className="footer-note"><span>WORDWELL</span><span>慢慢积累，反复遇见</span></footer>
    </main>
    <nav className="mobile-nav">{navItems.map((item) => <button key={item.id} className={view === item.id ? 'active' : ''} onClick={() => { setView(item.id); setEditor(null); }}><span className="nav-icon"><NavIcon view={item.id} /></span><small>{item.label}</small>{item.id === 'review' && dueCards.length > 0 && <i>{dueCards.length}</i>}</button>)}</nav>
    {toast && <div className="toast">{toast}</div>}
  </div>;
}

function Brand() { return <div className="brand"><span className="brand-mark">W</span><span>wordwell</span></div>; }
function SpeakerGlyph() { return <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 10v4h3l5 4V6l-5 4H4Z" /><path d="M16 9a4 4 0 0 1 0 6M18.5 6.5a8 8 0 0 1 0 11" /></svg>; }
function NavIcon({ view }: { view: View }) {
  const paths: Record<View, ReactNode> = {
    review: <><circle cx="12" cy="12" r="8.5" /><path d="M12 7v5l3.2 2" /></>,
    library: <><path d="M4 5.5A2.5 2.5 0 0 1 6.5 3H20v17H6.5A2.5 2.5 0 0 1 4 17.5z" /><path d="M4 17.5A2.5 2.5 0 0 1 6.5 15H20M8 7h8M8 10h8" /></>,
    browse: <><circle cx="10.8" cy="10.8" r="6.8" /><path d="m16 16 4.2 4.2" /></>,
    add: <><path d="M12 5v14M5 12h14" /></>,
    collect: <><path d="M6 3.5h8l4 4V20H6z" /><path d="M14 3.5V8h4M9 12h6M9 15.5h6" /></>,
    monitor: <><path d="M4 19.5h16" /><path d="M6 16V10m6 6V5m6 11v-8" /></>,
    settings: <><path d="M4 6h16M4 12h16M4 18h16" /><circle cx="9" cy="6" r="2" /><circle cx="15" cy="12" r="2" /><circle cx="11" cy="18" r="2" /></>,
  };
  return <svg viewBox="0 0 24 24" aria-hidden="true">{paths[view]}</svg>;
}

function ReviewView({ cards, allCount, totalToday, onRate, onAdd, notify }: { cards: Card[]; allCount: number; totalToday: number; onRate: (card: Card, rating: number) => Promise<void>; onAdd: () => void; notify: (message: string) => void }) {
  const [revealed, setRevealed] = useState(false);
  const [startingRecording, setStartingRecording] = useState(false);
  const [recording, setRecording] = useState(false);
  const [cancelingRecording, setCancelingRecording] = useState(false);
  const [checking, setChecking] = useState(false);
  const [ratingSaving, setRatingSaving] = useState(false);
  const [result, setResult] = useState<PronunciationResult | null>(null);
  const [played, setPlayed] = useState(false);
  const [speechState, setSpeechState] = useState<'idle' | 'loading' | 'playing'>('idle');
  const recordingSessionRef = useRef<RecordingSession | null>(null);
  const speechBusyRef = useRef(false);
  const speechRequestRef = useRef(0);
  const speechAudioRef = useRef<HTMLAudioElement | null>(null);
  const speechUrlRef = useRef<string | null>(null);
  const current = cards[0];
  useEffect(() => {
    speechRequestRef.current += 1;
    speechAudioRef.current?.pause();
    speechAudioRef.current = null;
    if (speechUrlRef.current) URL.revokeObjectURL(speechUrlRef.current);
    speechUrlRef.current = null;
    speechBusyRef.current = false;
    setSpeechState('idle');
    const session = recordingSessionRef.current;
    if (session && session.cardId !== current?.id) {
      cancelRecordingSession(session);
      recordingSessionRef.current = null;
      setStartingRecording(false);
      setRecording(false);
      setChecking(false);
    }
    setCancelingRecording(false);
    setRevealed(false);
    setResult(null);
    setPlayed(false);
  }, [current?.id]);
  useEffect(() => {
    return () => {
      const session = recordingSessionRef.current;
      if (session) {
        cancelRecordingSession(session);
        recordingSessionRef.current = null;
      }
      releaseMicStream(reusableMicStream);
      speechAudioRef.current?.pause();
      speechAudioRef.current = null;
      if (speechUrlRef.current) URL.revokeObjectURL(speechUrlRef.current);
      speechUrlRef.current = null;
      speechRequestRef.current += 1;
    };
  }, []);
  const submitRating = (rating: number) => {
    if (!current || ratingSaving) return;
    setRatingSaving(true);
    void onRate(current, rating).finally(() => setRatingSaving(false));
  };

  const playWord = async () => {
    if (!current || speechBusyRef.current) return;
    speechBusyRef.current = true;
    const requestId = ++speechRequestRef.current;
    setSpeechState('loading');
    let url: string | null = null;
    let audio: HTMLAudioElement | null = null;
    const finish = () => {
      if (audio) {
        audio.onended = null;
        audio.onerror = null;
      }
      if (speechAudioRef.current === audio) speechAudioRef.current = null;
      if (url && speechUrlRef.current === url) speechUrlRef.current = null;
      if (url) URL.revokeObjectURL(url);
      if (speechRequestRef.current === requestId) {
        speechBusyRef.current = false;
        setSpeechState('idle');
      }
    };
    try {
      url = await fetchSpeechAudio(current.word);
      if (speechRequestRef.current !== requestId) {
        URL.revokeObjectURL(url);
        return;
      }
      speechUrlRef.current = url;
      audio = new Audio(url);
      speechAudioRef.current = audio;
      audio.onended = finish;
      audio.onerror = () => {
        finish();
        if (speechRequestRef.current === requestId) notify('音频播放失败，请重试');
      };
      setSpeechState('playing');
      await audio.play();
      setPlayed(true);
    } catch (error) {
      finish();
      if (speechRequestRef.current === requestId) notify((error as Error).message || 'TTS 暂不可用；请检查 AI 服务与模型状态');
    }
  };

  const speechButton = <button className={`sound-button${revealed ? '' : ' review-sound-button'} ${played ? 'played' : ''}${speechState === 'loading' ? ' is-loading' : ''}`} onClick={playWord} disabled={speechState !== 'idle'} aria-label={speechState === 'loading' ? '正在生成美式发音' : speechState === 'playing' ? '正在播放美式发音' : '播放美式发音'}><SpeakerGlyph /></button>;

  const startRecording = async () => {
    if (recordingSessionRef.current || checking) return;
    if (!current || !navigator.mediaDevices?.getUserMedia || !('MediaRecorder' in window)) {
      notify('当前浏览器不支持录音；iPhone 请使用 HTTPS 或本机地址打开');
      return;
    }
    const session: RecordingSession = {
      cardId: current.id,
      expected: current.word,
      pressed: true,
      cancelled: false,
      stream: null,
      recorder: null,
      chunks: [],
    };
    recordingSessionRef.current = session;
    setStartingRecording(true);
    try {
      let stream = reusableMicStream;
      if (!stream || stream.getAudioTracks().length === 0 || stream.getAudioTracks().every((track) => track.readyState === 'ended')) {
        stream = await navigator.mediaDevices.getUserMedia({ audio: true });
        reusableMicStream = stream;
      }
      session.stream = stream;
      if (session.cancelled || !session.pressed || recordingSessionRef.current !== session) {
        releaseMicStream(stream);
        return;
      }
      setMicStreamEnabled(stream, true);
      const mime = ['audio/webm;codecs=opus', 'audio/mp4', 'audio/webm'].find((type) => MediaRecorder.isTypeSupported(type));
      const recorder = new MediaRecorder(stream, mime ? { mimeType: mime } : undefined);
      session.recorder = recorder;
      recorder.ondataavailable = (event) => { if (event.data.size > 0) session.chunks.push(event.data); };
      recorder.onerror = () => {
        session.cancelled = true;
        releaseMicStream(stream);
        if (recordingSessionRef.current === session) recordingSessionRef.current = null;
        setStartingRecording(false);
        setRecording(false);
        setChecking(false);
        setResult(null);
        notify('录音失败，请检查麦克风权限后重试');
      };
      recorder.onstop = async () => {
        releaseMicStream(stream);
        if (session.cancelled) {
          if (recordingSessionRef.current === session) recordingSessionRef.current = null;
          return;
        }
        setStartingRecording(false);
        setRecording(false);
        setChecking(true);
        try {
          const blob = new Blob(session.chunks, { type: recorder.mimeType || 'audio/webm' });
          const normalized = await normalizeRecordingToWav(blob);
          if (session.cancelled) return;
          const audioBase64 = await new Promise<string>((resolve, reject) => {
            const reader = new FileReader();
            reader.onload = () => resolve(String(reader.result).split(',')[1] || '');
            reader.onerror = () => reject(new Error('无法读取录音'));
            reader.readAsDataURL(normalized);
          });
          if (session.cancelled) return;
          const assessed = await api<PronunciationResult>('/pronunciation', { method: 'POST', body: JSON.stringify({ expected: session.expected, audioMime: normalized.type, audioBase64 }) });
          if (!session.cancelled) {
            setResult(assessed);
          }
        } catch (error) {
          if (!session.cancelled) {
            setResult(null);
            notify((error as Error).message || '读音评估失败，请检查 AI 模型和录音');
          }
        } finally {
          if (recordingSessionRef.current === session) recordingSessionRef.current = null;
          if (!session.cancelled) setChecking(false);
        }
      };
      recorder.start();
      setStartingRecording(false);
      setRecording(true);
    } catch {
      if (recordingSessionRef.current === session) {
        session.cancelled = true;
        releaseMicStream(session.stream);
        recordingSessionRef.current = null;
        setStartingRecording(false);
        setRecording(false);
        notify('无法访问麦克风，请允许浏览器使用麦克风后再试');
      }
    }
  };

  const cancelCurrentRecording = () => {
    const session = recordingSessionRef.current;
    if (session) {
      cancelRecordingSession(session);
      recordingSessionRef.current = null;
    }
    setStartingRecording(false);
    setRecording(false);
    setChecking(false);
  };

  const stopRecording = () => {
    const session = recordingSessionRef.current;
    if (!session || !session.pressed) return;
    session.pressed = false;
    if (!session.recorder) {
      session.cancelled = true;
      releaseMicStream(session.stream);
      recordingSessionRef.current = null;
      setStartingRecording(false);
      setRecording(false);
      return;
    }
    if (session.recorder.state === 'recording') session.recorder.stop();
  };

  if (!current) return <section className="review-page"><div className="page-heading"><div><span className="eyebrow">DAILY PRACTICE</span><h1>复习</h1></div><span className="date-stamp">{new Intl.DateTimeFormat('zh-CN', { month: 'long', day: 'numeric', weekday: 'long' }).format(new Date())}</span></div><div className="empty-review"><div className="empty-orbit"><span>✓</span></div><span className="eyebrow">ALL CAUGHT UP</span><h2>今天的复习完成了</h2><p>{allCount === 0 ? '先把阅读中遇到的词语收进来，之后每天再回来复习。' : '新的复习卡片会按间隔自动出现。'}<br />给自己一点空白，再继续积累。</p><button className="button button-primary" onClick={onAdd}>＋ 收集一个新词</button></div><div className="review-tip"><span>✦</span><p>复习时尽量先回想，再翻开释义。诚实的反馈会让间隔更合适。</p></div></section>;

  return <section className="review-page">
    <div className="page-heading"><div><span className="eyebrow">DAILY PRACTICE</span><h1>复习 <span className="heading-count">{cards.length}</span></h1></div><span className="date-stamp">{new Intl.DateTimeFormat('zh-CN', { month: 'long', day: 'numeric', weekday: 'long' }).format(new Date())}</span></div>
    {(startingRecording || recording || cancelingRecording) && <div className={`recording-overlay${cancelingRecording ? ' is-canceling' : ''}`} aria-live="assertive"><div className="recording-overlay-icon"><span /><svg viewBox="0 0 24 24" aria-hidden="true"><rect x="9" y="2.5" width="6" height="12" rx="3" /><path d="M5.5 11a6.5 6.5 0 0 0 13 0M12 17.5V22m-4 0h8" /></svg></div><b>{cancelingRecording ? '本次评测已取消' : startingRecording ? '正在连接麦克风' : '正在录音'}</b><span>{cancelingRecording ? '松开按钮结束，不会提交这次录音' : '松开提交评估 · 向上滑动取消'}</span></div>}
    <div className="review-progress"><div className="progress-label"><span>今日进度</span><b>{totalToday - cards.length} <i>/</i> {totalToday}</b></div><div className="progress-track"><i style={{ width: `${totalToday ? Math.max(4, ((totalToday - cards.length) / totalToday) * 100) : 100}%` }} /></div></div>
    <article className={`flash-card ${revealed ? 'is-revealed' : ''}`}>
      <div className="flash-top"><span className="pill">{current.state === 'new' ? '新词' : '间隔复习'}</span><span className="card-index">{totalToday - cards.length + 1} / {totalToday}</span></div>
      <div className="word-face"><div className="word-title-row"><h2>{current.word}</h2>{revealed && speechButton}</div>{current.ipa && <span className="ipa">/{current.ipa}/ <small>US</small></span>}<span className="word-hint">先在心里回想它的意思</span></div>
      {!revealed ? (
        <>
          <div className="reveal-area">
            <div className="review-action-row">
              <button className="button button-primary reveal-button" onClick={() => setRevealed(true)} disabled={startingRecording || recording || checking}>查看释义</button>
              <div className="review-action-slot">{speechButton}<span>朗读</span></div>
              {!result && <HoldToRecordButton className="mic-link" starting={startingRecording} recording={recording} checking={checking} canceling={cancelingRecording} onStart={startRecording} onStop={stopRecording} onCancel={cancelCurrentRecording} onCancelingChange={setCancelingRecording} idleLabel="按住录音" />}
            </div>
            <span className="review-actions-hint">松开提交评估 · 向上滑动取消</span>
          </div>
          {result && <PronunciationFeedback result={result} onRetry={startRecording} onStop={stopRecording} onCancel={cancelCurrentRecording} onCancelingChange={setCancelingRecording} starting={startingRecording} recording={recording} checking={checking} canceling={cancelingRecording} />}
        </>
      ) : (
        <div className="answer-face">
          <div className="answer-divider" />
          <h3>{current.definition || '还没有释义'}</h3>
          {current.contextMeaning && <p className="context-meaning">{current.contextMeaning}</p>}
          {current.contextText && <blockquote>{current.contextText}</blockquote>}
          {current.source && <span className="source-line">来源 · {current.source}</span>}
          {result
            ? <PronunciationFeedback result={result} onRetry={startRecording} onStop={stopRecording} onCancel={cancelCurrentRecording} onCancelingChange={setCancelingRecording} starting={startingRecording} recording={recording} checking={checking} canceling={cancelingRecording} />
            : <HoldToRecordButton className="pron-retry" starting={startingRecording} recording={recording} checking={checking} canceling={cancelingRecording} onStart={startRecording} onStop={stopRecording} onCancel={cancelCurrentRecording} onCancelingChange={setCancelingRecording} idleLabel="按住说出单词，松开提交评估" />}
          <div className="rating-prompt">你这次回想得怎么样？</div>
          <div className="rating-row">
            <button className="rating again" disabled={ratingSaving || startingRecording || recording || checking} onClick={() => submitRating(1)}><b>没想起</b><small>Again</small></button>
            <button className="rating hard" disabled={ratingSaving || startingRecording || recording || checking} onClick={() => submitRating(2)}><b>很费力</b><small>Hard</small></button>
            <button className="rating good" disabled={ratingSaving || startingRecording || recording || checking} onClick={() => submitRating(3)}><b>想起来了</b><small>Good</small></button>
            <button className="rating easy" disabled={ratingSaving || startingRecording || recording || checking} onClick={() => submitRating(4)}><b>很轻松</b><small>Easy</small></button>
          </div>
        </div>
      )}
    </article>
  </section>;
}

function HoldToRecordButton({ className, starting, recording, checking, canceling, onStart, onStop, onCancel, onCancelingChange, idleLabel }: { className: string; starting: boolean; recording: boolean; checking: boolean; canceling: boolean; onStart: () => void; onStop: () => void; onCancel: () => void; onCancelingChange: (canceling: boolean) => void; idleLabel: string }) {
  const active = useRef<{ kind: 'pointer'; pointerId: number; originY: number; canceling: boolean } | { kind: 'keyboard' } | null>(null);
  const begin = (source: 'pointer' | 'keyboard', pointerId?: number, originY = 0) => {
    if (checking || active.current) return;
    active.current = source === 'pointer' ? { kind: 'pointer', pointerId: pointerId ?? -1, originY, canceling: false } : { kind: 'keyboard' };
    onStart();
  };
  const end = (source?: 'pointer' | 'keyboard', pointerId?: number) => {
    const held = active.current;
    if (!held) return;
    if (source === 'pointer' && (held.kind !== 'pointer' || held.pointerId !== pointerId)) return;
    if (source === 'keyboard' && held.kind !== 'keyboard') return;
    active.current = null;
    onCancelingChange(false);
    if (held.kind === 'pointer' && held.canceling) return;
    onStop();
  };
  const busy = starting || recording || checking;
  const label = canceling ? '已取消 · 松开结束' : starting ? '正在连接麦克风 · 松开取消' : recording ? '正在录音 · 松开结束' : checking ? '正在评估…' : idleLabel;

  return <div className={`hold-record-control ${className}`}>
    <button
      className={`hold-record-button${recording ? ' is-recording' : ''}${canceling ? ' is-canceling' : ''}`}
      type="button"
      disabled={checking}
      aria-label={label}
      aria-pressed={starting || recording || canceling}
      onPointerDown={(event) => {
        if (event.button !== 0) return;
        try { event.currentTarget.setPointerCapture(event.pointerId); } catch { /* Continue with bubbling pointer events. */ }
        begin('pointer', event.pointerId, event.clientY);
      }}
      onPointerUp={(event) => end('pointer', event.pointerId)}
      onPointerMove={(event) => {
        const held = active.current;
        if (held?.kind === 'pointer' && held.pointerId === event.pointerId && !held.canceling && held.originY - event.clientY >= 72) {
          held.canceling = true;
          onCancel();
          onCancelingChange(true);
        }
      }}
      onPointerCancel={(event) => {
        const held = active.current;
        if (held?.kind !== 'pointer' || held.pointerId !== event.pointerId) return;
        if (!held.canceling) onCancel();
        active.current = null;
        onCancelingChange(false);
      }}
      onLostPointerCapture={(event) => {
        const held = active.current;
        if (held?.kind !== 'pointer' || held.pointerId !== event.pointerId) return;
        if (!held.canceling) onCancel();
        active.current = null;
        onCancelingChange(false);
      }}
      onKeyDown={(event) => {
        if (event.code !== 'Space' && event.code !== 'Enter') return;
        event.preventDefault();
        if (!event.repeat) begin('keyboard');
      }}
      onKeyUp={(event) => {
        if (event.code !== 'Space' && event.code !== 'Enter') return;
        event.preventDefault();
        end('keyboard');
      }}
      onBlur={() => end()}
      onContextMenu={(event) => event.preventDefault()}
    >
      <svg viewBox="0 0 24 24" aria-hidden="true"><rect x="9" y="2.5" width="6" height="12" rx="3" /><path d="M5.5 11a6.5 6.5 0 0 0 13 0M12 17.5V22m-4 0h8" /></svg>
      {recording && <i className="record-indicator" />}
    </button>
    <span className="hold-record-label" aria-live="polite">{busy ? label : idleLabel}</span>
  </div>;
}

function PronunciationFeedback({ result, onRetry, onStop, onCancel, onCancelingChange, starting, recording, checking, canceling }: { result: PronunciationResult; onRetry: () => void; onStop: () => void; onCancel: () => void; onCancelingChange: (canceling: boolean) => void; starting: boolean; recording: boolean; checking: boolean; canceling: boolean }) {
  const localPhoneme = result.method === 'local-phoneme-assessment';
  const scored = localPhoneme || result.method === 'azure-pronunciation-assessment';
  const title = result.status === 'recognized'
    ? (scored ? '本次发音分数较高' : '识别到了目标词')
    : result.status === 'needs_practice'
      ? (scored ? '这个词的发音还可以练习' : '识别结果与目标不同')
      : '这次结果不确定';

  return <div className={`pron-result ${result.status}`}>
    <div>
      <b>{title}</b>
      <small>
        {localPhoneme
          ? result.transcript
            ? <>听到“{result.transcript}” · 本地音素评分 {Math.round(result.similarity * 100)} 分 · ASR 识别信心 {Math.round(result.confidence * 100)}%</>
            : <>ASR 暂未获得转写 · 本地音素评分 {Math.round(result.similarity * 100)} 分</>
          : result.method === 'azure-pronunciation-assessment'
            ? result.transcript
              ? <>听到“{result.transcript}” · 发音准确度 {Math.round(result.similarity * 100)}% · 识别信心 {Math.round(result.confidence * 100)}%</>
              : <>ASR 暂未获得转写 · 发音准确度 {Math.round(result.similarity * 100)}%</>
            : <>听到“{result.transcript || '暂未获得转写'}” · 识别置信度 {Math.round(result.confidence * 100)}%</>}
      </small>
    </div>
    <span>{Math.round(result.similarity * 100)}%</span>
    {result.scores && <div className="pron-score-row">
      {result.scores.overall !== undefined && <i>综合 {Math.round(result.scores.overall)}</i>}
      <i>准确度 {Math.round(result.scores.accuracy)}</i>
      {result.scores.fluency !== undefined && <i>流利度 {Math.round(result.scores.fluency)}</i>}
      {result.scores.prosody !== undefined && <i>韵律 {Math.round(result.scores.prosody)}</i>}
      {result.scores.completeness !== undefined && <i>完整度 {Math.round(result.scores.completeness)}</i>}
    </div>}
    {result.words && result.words.length > 0 && <div className="pron-words">
      {result.words.map((word, index) => <div className="pron-word" key={`${word.word}-${index}`}>
        <i className={word.accuracy >= 80 ? 'good' : word.accuracy >= 60 ? 'mid' : 'low'}>{word.word} {Math.round(word.accuracy)}</i>
        {word.phonemes && word.phonemes.length > 0 && <div className="pron-phonemes">
          {word.phonemes.map((phoneme, phonemeIndex) => <i className={phoneme.accuracy >= 80 ? 'good' : phoneme.accuracy >= 60 ? 'mid' : 'low'} key={`${phoneme.phoneme}-${phonemeIndex}`}>{phoneme.phoneme} {Math.round(phoneme.accuracy)}</i>)}
        </div>}
      </div>)}
    </div>}
    <p>{result.notice}</p>
    <HoldToRecordButton className="pron-retry" starting={starting} recording={recording} checking={checking} canceling={canceling} onStart={onRetry} onStop={onStop} onCancel={onCancel} onCancelingChange={onCancelingChange} idleLabel="按住重新录音，松开提交评估" />
  </div>;
}

function LibraryView({ cards, total, search, setSearch, onBrowse, onEdit, onDelete, onAdd }: { cards: Card[]; total: number; search: string; setSearch: (value: string) => void; onBrowse: (card: Card) => void; onEdit: (card: Card) => void; onDelete: (card: Card) => void; onAdd: () => void }) {
  return <section className="library-page"><div className="page-heading"><div><span className="eyebrow">YOUR COLLECTION</span><h1>我的词库 <span className="heading-count">{total}</span></h1></div><button className="button button-primary desktop-add" onClick={onAdd}>＋ 新增单词</button></div><div className="library-toolbar"><label className="search-box"><span>⌕</span><input value={search} onChange={(event) => setSearch(event.target.value)} placeholder="搜索单词、释义或原句" /><kbd>⌘ K</kbd></label><span className="toolbar-count">共 {total} 个词</span></div>{cards.length === 0 ? <div className="empty-library"><span>⌕</span><h2>{search ? '没有找到匹配的单词' : '词库还是空的'}</h2><p>{search ? '试试另一个关键词。' : '把读到的词语和原句一起保存，复习会更有上下文。'}</p>{!search && <button className="button button-primary" onClick={onAdd}>开始添加</button>}</div> : <div className="word-list">{cards.map((card) => <article className="word-row" key={card.id}><div className="word-row-main"><div className="list-word-line"><button className="word-row-browse" onClick={() => onBrowse(card)} title="浏览这张单词卡">{card.word}</button>{card.ipa && <span className="ipa">/{card.ipa}/</span>}</div><p>{card.definition || '未填写释义'}{card.contextText && <span className="context-snippet"> · {card.contextText}</span>}</p>{card.source && <small>来源 · {card.source}</small>}</div><div className="word-row-side"><span className={`due-label ${Date.parse(card.dueAt) <= Date.now() ? 'due-now' : ''}`}>{Date.parse(card.dueAt) <= Date.now() ? '待复习' : formatDue(card.dueAt)}</span><div className="row-actions"><button title="编辑" onClick={() => onEdit(card)}>编辑</button><button title="删除" onClick={() => onDelete(card)}>删除</button></div></div></article>)}</div>}</section>;
}

function BrowseView({ cards, initialCardId, onAdd, notify }: { cards: Card[]; initialCardId: string; onAdd: () => void; notify: (message: string) => void }) {
  const [index, setIndex] = useState(() => Math.max(0, cards.findIndex((card) => card.id === initialCardId)));
  const [playing, setPlaying] = useState(false);
  const audioRef = useRef<HTMLAudioElement | null>(null);
  const urlRef = useRef<string | null>(null);
  const speechRequestRef = useRef(0);
  useEffect(() => {
    const selected = cards.findIndex((card) => card.id === initialCardId);
    setIndex(selected >= 0 ? selected : 0);
  }, [cards, initialCardId]);
  useEffect(() => {
    speechRequestRef.current += 1;
    audioRef.current?.pause();
    audioRef.current = null;
    if (urlRef.current) URL.revokeObjectURL(urlRef.current);
    urlRef.current = null;
    setPlaying(false);
  }, [cards[index]?.id]);
  useEffect(() => () => {
    speechRequestRef.current += 1;
    audioRef.current?.pause();
    if (urlRef.current) URL.revokeObjectURL(urlRef.current);
  }, []);
  const card = cards[index];
  const play = async () => {
    if (!card || playing) return;
    setPlaying(true);
    const requestId = ++speechRequestRef.current;
    try {
      const url = await fetchSpeechAudio(card.word);
      if (speechRequestRef.current !== requestId) { URL.revokeObjectURL(url); return; }
      urlRef.current = url;
      const audio = new Audio(url);
      audioRef.current = audio;
      const finish = () => {
        if (audioRef.current === audio) audioRef.current = null;
        if (urlRef.current === url) urlRef.current = null;
        URL.revokeObjectURL(url);
        if (speechRequestRef.current === requestId) setPlaying(false);
      };
      audio.onended = finish;
      audio.onerror = () => { finish(); notify('音频播放失败，请重试'); };
      await audio.play();
    } catch (error) {
      if (speechRequestRef.current === requestId) {
        setPlaying(false);
        if (urlRef.current) URL.revokeObjectURL(urlRef.current);
        urlRef.current = null;
        audioRef.current = null;
        notify((error as Error).message || 'TTS 暂不可用；请检查 AI 服务与模型状态');
      }
    }
  };
  if (!card) return <section className="browse-page"><div className="page-heading"><div><span className="eyebrow">BROWSE YOUR WORDS</span><h1>浏览单词卡</h1></div></div><div className="empty-library"><span>▤</span><h2>词库还是空的</h2><p>添加单词后，可以在这里逐张浏览。</p><button className="button button-primary" onClick={onAdd}>开始添加</button></div></section>;
  return <section className="browse-page"><div className="page-heading"><div><span className="eyebrow">BROWSE YOUR WORDS</span><h1>浏览单词卡 <span className="heading-count">{index + 1} / {cards.length}</span></h1></div><button className="text-button" onClick={onAdd}>＋ 新增</button></div><article className="flash-card browse-card"><div className="flash-top"><span className="pill">单词浏览</span><span className="card-index">{card.state === 'new' ? '新词' : `复习 ${card.reps} 次`}</span></div><div className="word-face browse-word-face"><div className="word-title-row"><h2>{card.word}</h2><button className={`sound-button ${playing ? 'is-loading' : ''}`} title="播放发音" aria-label={`播放 ${card.word} 的发音`} disabled={playing} onClick={() => void play()}><SpeakerGlyph /></button></div>{card.ipa && <span className="ipa">/{card.ipa}/</span>}</div><div className="answer-face browse-answer"><div className="answer-divider" /><h3>{card.definition || '未填写释义'}</h3>{card.contextMeaning && <p className="context-meaning">{card.contextMeaning}</p>}{card.contextText && <blockquote>{card.contextText}</blockquote>}{card.source && <small className="source-line">来源 · {card.source}</small>}</div><div className="browse-controls"><button className="button button-secondary" disabled={index === 0} onClick={() => setIndex((value) => Math.max(0, value - 1))}>← 上一个</button><button className="button button-secondary" disabled={index >= cards.length - 1} onClick={() => setIndex((value) => Math.min(cards.length - 1, value + 1))}>下一个 →</button></div></article></section>;
}

function formatBytes(value: number | undefined | null) {
  if (value === undefined || value === null || !Number.isFinite(value)) return '—';
  if (value < 1024) return `${value} B`;
  const units = ['KB', 'MB', 'GB', 'TB'];
  let amount = value / 1024;
  let unit = 0;
  while (amount >= 1024 && unit < units.length - 1) { amount /= 1024; unit += 1; }
  return `${amount.toFixed(amount >= 100 ? 0 : 1)} ${units[unit]}`;
}

function SpeechStorageCard({ notify }: { notify: (message: string) => void }) {
  const [status, setStatus] = useState<SpeechStorageStatus | null>(null);
  const [loading, setLoading] = useState(false);
  const [clearing, setClearing] = useState(false);
  const refresh = useCallback(async () => {
    setLoading(true);
    try { setStatus(await api<SpeechStorageStatus>('/audio')); }
    catch (error) { notify((error as Error).message || '无法读取读音存储状态'); }
    finally { setLoading(false); }
  }, [notify]);
  useEffect(() => { void refresh(); }, [refresh]);
  const clear = async () => {
    if (!window.confirm(`清理已保存的 ${status?.files ?? 0} 个读音文件？词卡和复习记录不会删除；之后播放时可以重新生成。`)) return;
    setClearing(true);
    try {
      const result = await api<{ deletedFiles: number; deletedBytes: number; cancelledTasks: number }>('/audio', { method: 'DELETE' }, 7 * 60 * 1000);
      notify(`已清理 ${result.deletedFiles} 个读音（${formatBytes(result.deletedBytes)}）`);
      await refresh();
    } catch (error) { notify((error as Error).message || '读音清理失败'); }
    finally { setClearing(false); }
  };
  return <article className="settings-card"><div className="settings-card-head"><span className="settings-icon">♫</span><div><h2>读音存储</h2><p>语音文件保存在服务器持久化目录；可以清理并在下次播放时重新生成。</p></div></div><div className="storage-summary"><span><b>{status?.files ?? '—'}</b><small>个文件</small></span><span><b>{status ? formatBytes(status.bytes) : '—'}</b><small>占用空间</small></span></div><code className="storage-path">{status?.path || '/audio'}</code><div className="button-row"><button className="button button-secondary" onClick={() => void refresh()} disabled={loading}>{loading ? '读取中…' : '刷新状态'}</button><button className="button button-quiet" onClick={() => void clear()} disabled={clearing || !status?.files}>{clearing ? '正在清理…' : '清理已保存读音'}</button></div><small className="muted">NAS 挂载路径通过部署目录的 AUDIO_STORAGE_PATH 配置；API 容器内路径为 /audio。</small></article>;
}

function MonitorView({ notify }: { notify: (message: string) => void }) {
  const [metrics, setMetrics] = useState<OpsMetrics | null>(null);
  const [logs, setLogs] = useState<OpsLogs | null>(null);
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);
  const [logQuery, setLogQuery] = useState('');
  const refresh = useCallback(async () => {
    setLoading(true);
    try {
      const [nextMetrics, nextLogs] = await Promise.all([api<OpsMetrics>('/ops/metrics'), api<OpsLogs>('/ops/logs')]);
      setMetrics(nextMetrics);
      setLogs(nextLogs);
      setError('');
    } catch (caught) {
      const message = (caught as Error).message || '无法读取服务状态';
      setError(message);
      notify(message);
    } finally { setLoading(false); }
  }, [notify]);
  useEffect(() => {
    void refresh();
    const timer = window.setInterval(() => void refresh(), 10000);
    return () => window.clearInterval(timer);
  }, [refresh]);
  const taskEntries = Object.entries(metrics?.ai?.tasks || {});
  const filterLogs = (lines: string[]) => lines.filter((line) => line.toLowerCase().includes(logQuery.trim().toLowerCase()));
  const aiMemory = metrics?.ai?.containerMemory;
  const app = metrics?.app;
  const process = metrics?.ai?.process;
  const tts = metrics?.tts;
  const model = metrics?.ai?.models;
  return <section className="monitor-page"><div className="page-heading"><div><span className="eyebrow">SYSTEM HEALTH</span><h1>服务监控</h1></div><button className="button button-secondary" onClick={() => void refresh()} disabled={loading}>{loading ? '刷新中…' : '↻ 刷新'}</button></div>{error && <div className="collect-notice">暂时无法刷新：{error}</div>}<div className="monitor-grid"><article className="monitor-card"><span>应用内存</span><b>{formatBytes(app?.heapAllocBytes)}</b><small>Go RSS {formatBytes(app?.rssBytes)} · 容器 {formatBytes(app?.containerMemory?.usageBytes)} / {formatBytes(app?.containerMemory?.limitBytes)} · {app?.goroutines ?? '—'} 个协程</small></article><article className="monitor-card"><span>AI 容器内存</span><b>{formatBytes(aiMemory?.usageBytes)}</b><small>上限 {formatBytes(aiMemory?.limitBytes)} · 进程 RSS {formatBytes(process?.rssBytes)}</small></article><article className="monitor-card"><span>AI CPU</span><b>{process?.cpuPercent == null ? '采样中' : `${process.cpuPercent}%`}</b><small>进程累计 {process?.cpuSeconds?.toFixed(1) ?? '—'} 秒</small></article><article className="monitor-card"><span>持久化读音</span><b>{metrics?.speechStorage.files ?? '—'} 个 · {formatBytes(metrics?.speechStorage.bytes)}</b><small>{metrics?.speechStorage.path || '/audio'}</small></article><article className="monitor-card"><span>词库数据库</span><b>{formatBytes(metrics?.databaseStorage?.bytes)}</b><small>{metrics?.databaseStorage?.path || '/data/vocab.db'}</small></article></div><div className="monitor-panels"><article className="settings-card monitor-status-card"><div className="settings-card-head"><span className="settings-icon">♫</span><div><h2>任务与模型</h2><p>语音队列按优先级运行，播放按钮任务优先于后台预生成。</p></div></div><div className="task-status-row"><span>语音队列</span><b>{tts?.active ? '正在生成' : '空闲'} · 等待 {tts?.queued ?? 0}</b><small>播放优先 {tts?.interactiveQueued ?? 0} · 后台 {tts?.backgroundQueued ?? 0}</small></div><div className="task-status-row"><span>语音任务累计</span><b>完成 {tts?.completed ?? 0} · 失败 {tts?.failed ?? 0}</b><small>最后一次 {tts?.lastDurationSeconds?.toFixed(1) ?? '—'} 秒{tts?.lastError ? ` · ${tts.lastError}` : ''}</small></div><div className="task-status-row"><span>Kokoro</span><b>{model?.tts?.state || '未知'}</b><small>{model?.tts?.error || '语音合成模型状态'}</small></div><div className="task-status-row"><span>发音评分模型</span><b>{model?.pronunciation?.state || '未知'}</b><small>ASR {model?.asrLoaded ? '已加载' : '未加载'} · 音素评分 {model?.phonemeScorerLoaded ? '已加载' : '未加载'}</small></div>{taskEntries.map(([name, task]) => <div className="task-status-row" key={name}><span>AI · {name}</span><b>运行 {task.running} · 完成 {task.completed} · 失败 {task.failed}</b><small>请求 {task.requests} · 最近 {task.lastDurationSeconds.toFixed(1)} 秒</small></div>)}{metrics?.aiError && <small className="muted">AI 指标暂不可用：{metrics.aiError}</small>}</article><article className="settings-card logs-card"><div className="settings-card-head"><span className="settings-icon">≋</span><div><h2>近期日志</h2><p>显示应用和 AI 服务进程内最近保留的日志，不替代 Docker 完整日志。</p></div></div><label className="search-box log-search"><span>⌕</span><input value={logQuery} onChange={(event) => setLogQuery(event.target.value)} placeholder="筛选日志内容" /></label><div className="log-columns"><section><h3>应用</h3><pre>{filterLogs(logs?.app || []).join('\n') || '暂无应用日志'}</pre></section><section><h3>AI 服务</h3><pre>{filterLogs(logs?.ai || []).join('\n') || logs?.aiError || '暂无 AI 日志'}</pre></section></div></article></div><small className="muted monitor-updated">上次采样 {metrics?.sampledAt ? new Date(metrics.sampledAt).toLocaleString() : '—'} · 每 10 秒自动刷新</small></section>;
}

function EditorView({ card, onSave, onCancel, notify }: { card: Card | null; onSave: (input: CardInput) => Promise<void>; onCancel: () => void; notify: (message: string) => void }) {
  const [word, setWord] = useState(card?.word || '');
  const [ipa, setIpa] = useState(card?.ipa || '');
  const [definition, setDefinition] = useState(card?.definition || '');
  const [contextText, setContextText] = useState(card?.contextText || '');
  const [contextMeaning, setContextMeaning] = useState(card?.contextMeaning || '');
  const [source, setSource] = useState(card?.source || '');
  const [saving, setSaving] = useState(false);
  const [suggestions, setSuggestions] = useState<DictionaryEntry[]>([]);
  const [looking, setLooking] = useState(false);
  const [aiLoading, setAiLoading] = useState(false);
  const passageRef = useRef<HTMLTextAreaElement>(null);
  const selectedRange = useRef<{ start: number; end: number } | null>(null);

  useEffect(() => {
    const value = word.trim();
    if (!value || value.length > 60) { setSuggestions([]); return; }
    const local = getWordCache()[value.toLowerCase()];
    if (local) {
      setSuggestions([local]);
      if (!ipa) setIpa(local.ipa);
      if (!definition) setDefinition(local.definition);
    }
    const timer = window.setTimeout(async () => {
      setLooking(true);
      try {
        // The API serializes an empty result as JSON null; guard before use.
        const entries = (await api<DictionaryEntry[]>(`/dictionary?q=${encodeURIComponent(value)}`)) || [];
        setSuggestions(entries);
        saveWordCache(entries);
        const exact = entries.find((entry) => entry.word.toLowerCase() === value.toLowerCase());
        if (exact) {
          setIpa((old) => old || exact.ipa);
          setDefinition((old) => old || exact.definition);
        }
      } catch { /* Recent local dictionary cache remains available offline. */ }
      finally { setLooking(false); }
    }, 300);
    return () => window.clearTimeout(timer);
  }, [definition, ipa, word]);

  const useSelection = () => {
    const element = passageRef.current;
    if (!element) return;
    const range = selectedRange.current || { start: element.selectionStart, end: element.selectionEnd };
    const selected = element.value.slice(range.start, range.end).trim().replace(/^[^\p{L}]+|[^\p{L}'-]+$/gu, '');
    if (!selected) { notify('先在原句中选中一个词，再点这里'); return; }
    setWord(selected);
  };

  const chooseEntry = (entry: DictionaryEntry) => {
    setWord(entry.word);
    setIpa(entry.ipa);
    setDefinition(entry.definition);
    setSuggestions([]);
  };

  const askAI = async () => {
    if (!word.trim()) { notify('先填写要学习的单词'); return; }
    setAiLoading(true);
    try {
      const result = await api<{ definition: string; contextMeaning: string }>('/gloss', { method: 'POST', body: JSON.stringify({ word, contextText }) });
      setDefinition(result.definition);
      if (result.contextMeaning) setContextMeaning(result.contextMeaning);
      notify('释义已生成，请核对后保存');
    } catch (error) { notify((error as Error).message || 'AI 释义服务暂不可用'); }
    finally { setAiLoading(false); }
  };

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    const input: CardInput = { id: card?.id || id(), word: word.trim(), ipa: ipa.trim(), definition: definition.trim(), contextText: contextText.trim(), contextMeaning: contextMeaning.trim(), source: source.trim() };
    if (!input.word) { notify('单词不能为空'); return; }
    setSaving(true);
    try { await onSave(input); } catch (error) { notify((error as Error).message || '保存失败'); }
    finally { setSaving(false); }
  };

  return <section className="editor-page"><div className="page-heading"><div><span className="eyebrow">{card ? 'EDIT A WORD' : 'CAPTURE A WORD'}</span><h1>{card ? '编辑单词' : '收集新词'}</h1></div><button className="text-button" onClick={onCancel}>取消</button></div><div className="editor-layout"><form className="editor-form" onSubmit={submit}>
    <div className="form-section"><div className="section-title"><span className="step-number">01</span><div><h2>单词与释义</h2><p>写下想记住的词，发音和释义可以再补充。</p></div></div><label htmlFor="word">单词 <i>*</i></label><div className="word-input-wrap"><input id="word" value={word} onChange={(event) => setWord(event.target.value)} placeholder="例如：significant" required autoFocus autoCapitalize="none" autoCorrect="off" /><span className="lookup-indicator">{looking ? '查词中…' : suggestions.length ? '本地词典' : ''}</span></div>{suggestions.length > 0 && <div className="dictionary-suggestions">{suggestions.slice(0, 5).map((entry) => <button type="button" key={entry.word} onClick={() => chooseEntry(entry)}><b>{entry.word}</b><span>/{entry.ipa}/</span><small>{entry.definition}</small></button>)}</div>}<label htmlFor="ipa">美式 IPA</label><input id="ipa" value={ipa} onChange={(event) => setIpa(event.target.value)} placeholder="ˌsɪɡˈnɪfɪkənt" /><label htmlFor="definition">释义</label><textarea id="definition" value={definition} onChange={(event) => setDefinition(event.target.value)} placeholder="简洁地解释这个词的意思" rows={3} /><button type="button" className="ai-fill" onClick={askAI} disabled={aiLoading}>{aiLoading ? '正在连接 AI…' : '✦ 用 AI 补充释义'} <span>需配置本地或兼容服务</span></button></div>
    <div className="form-section"><div className="section-title"><span className="step-number">02</span><div><h2>原句与语境</h2><p>保留你读到这个词时的句子，复习时更容易想起来。</p></div></div><label htmlFor="passage">粘贴原句或段落</label><textarea ref={passageRef} id="passage" value={contextText} onChange={(event) => setContextText(event.target.value)} onSelect={(event) => { selectedRange.current = { start: event.currentTarget.selectionStart, end: event.currentTarget.selectionEnd }; }} onKeyUp={(event) => { selectedRange.current = { start: event.currentTarget.selectionStart, end: event.currentTarget.selectionEnd }; }} onMouseUp={(event) => { selectedRange.current = { start: event.currentTarget.selectionStart, end: event.currentTarget.selectionEnd }; }} placeholder="从论文、书籍或网页中复制一段文字，选中目标词后添加…" rows={5} /><button type="button" className="selection-button" onClick={useSelection}>⌗ 使用选中的词</button><label htmlFor="contextMeaning">这个语境里的意思 <span className="optional">可选</span></label><textarea id="contextMeaning" value={contextMeaning} onChange={(event) => setContextMeaning(event.target.value)} placeholder="你对这句话中词义的理解" rows={2} /><label htmlFor="source">来源 <span className="optional">文章名、页码或链接</span></label><input id="source" value={source} onChange={(event) => setSource(event.target.value)} placeholder="例如：Attention Is All You Need · p. 3" /></div>
    <div className="form-actions"><button type="button" className="button button-quiet" onClick={onCancel}>取消</button><button type="submit" className="button button-primary" disabled={saving}>{saving ? '保存中…' : card ? '保存更改' : '保存到词库'} <span>→</span></button></div>
  </form><aside className="editor-aside"><div className="preview-card"><span className="eyebrow">CARD PREVIEW</span><h2>{word || 'your word'}</h2>{ipa && <span className="ipa">/{ipa}/</span>}<p>{definition || '释义会显示在单词翻面后'}</p>{contextText && <blockquote>{contextText}</blockquote>}<small>复习时先看到单词，再翻开释义</small></div><div className="privacy-note"><span>✦</span><p>单词、原句和来源只保存在你的服务器；添加和复习支持临时离线。</p></div></aside></div></section>;
}

function SettingsView({ bundle, pending, syncing, onSync, onImport, notify }: { bundle: Bundle; pending: number; syncing: boolean; onSync: () => void; onImport: (file: File) => Promise<void>; notify: (message: string) => void }) {
  const backupRef = useRef<HTMLInputElement>(null);
  const dictRef = useRef<HTMLInputElement>(null);
  const [dictLoading, setDictLoading] = useState(false);
  const exportBackup = async () => {
    let backup = bundle;
    try { backup = await api<Bundle>('/export'); }
    catch (error) {
      if (!isNetworkError(error)) { notify((error as Error).message || '备份导出失败'); return; }
      backup = { ...bundle, dictionary: Object.values(getWordCache()), exportedAt: new Date().toISOString() };
      notify('当前离线：备份包含本机缓存的卡片和最近查过的词条');
    }
    const data = new Blob([JSON.stringify({ ...backup, exportedAt: new Date().toISOString() }, null, 2)], { type: 'application/json' });
    const url = URL.createObjectURL(data);
    const anchor = document.createElement('a');
    anchor.href = url;
    anchor.download = `wordwell-backup-${new Date().toISOString().slice(0, 10)}.json`;
    anchor.click();
    URL.revokeObjectURL(url);
    notify('备份文件已导出');
  };
  const handleBackupFile = async (event: ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0];
    if (!file) return;
    try { await onImport(file); } catch (error) { notify((error as Error).message || '无法导入备份'); }
    event.target.value = '';
  };
  const handleDictionary = async (event: ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0];
    if (!file) return;
    setDictLoading(true);
    try {
      const text = await file.text();
      let entries: DictionaryEntry[];
      if (file.name.toLowerCase().endsWith('.json')) {
        const decoded = JSON.parse(text) as DictionaryEntry[] | { entries: DictionaryEntry[] };
        entries = Array.isArray(decoded) ? decoded : decoded.entries;
        if (!Array.isArray(entries)) throw new Error('JSON 应为词条数组或包含 entries 数组');
      } else {
        entries = text.split(/\r?\n/).map((line) => line.split('\t')).filter((fields) => fields.length >= 3 && fields[0].trim()).map((fields) => ({ word: fields[0].trim(), ipa: (fields[1] || '').trim(), definition: fields[2].trim(), locale: (fields[3] || 'en-US').trim() }));
      }
      const result = await api<{ imported: number }>('/dictionary/import', { method: 'POST', body: JSON.stringify({ entries }) });
      saveWordCache(entries);
      notify(`已导入 ${result.imported} 条本地词典记录`);
    } catch (error) { notify((error as Error).message || '词典导入失败，请检查 JSON/TSV 格式'); }
    finally { setDictLoading(false); event.target.value = ''; }
  };
  return <section className="settings-page"><div className="page-heading"><div><span className="eyebrow">PREFERENCES & DATA</span><h1>设置与数据</h1></div></div><div className="settings-grid"><article className="settings-card"><div className="settings-card-head"><span className="settings-icon">↻</span><div><h2>设备同步</h2><p>桌面和手机连接同一个自托管词库。离线时的新增与复习会在重新联网后自动上传。</p></div></div><div className="settings-status"><span className="status-dot" />{pending ? `${pending} 项等待同步` : '本机与服务器已同步'}</div><button className="button button-secondary" onClick={onSync} disabled={syncing}>{syncing ? '正在同步…' : '立即同步'}</button></article><article className="settings-card"><div className="settings-card-head"><span className="settings-icon">▣</span><div><h2>备份与恢复</h2><p>导出包含卡片、语境和复习记录的 JSON。恢复会合并同一 ID 的卡片，并加入新卡片。</p></div></div><div className="button-row"><button className="button button-secondary" onClick={exportBackup}>↓ 导出备份</button><button className="button button-quiet" onClick={() => backupRef.current?.click()}>↑ 导入备份</button><input ref={backupRef} type="file" accept="application/json,.json" hidden onChange={handleBackupFile} /></div></article><article className="settings-card"><div className="settings-card-head"><span className="settings-icon">⌕</span><div><h2>本地词典</h2><p>导入 JSON 或 TSV（每行：word、IPA、definition、locale），查词记录保存在 SQLite 中。</p></div></div><button className="button button-secondary" onClick={() => dictRef.current?.click()} disabled={dictLoading}>{dictLoading ? '正在导入…' : '选择词典文件'}</button><input ref={dictRef} type="file" accept=".json,.tsv,.txt" hidden onChange={handleDictionary} /><small className="muted">当前浏览器缓存 {Object.keys(getWordCache()).length} 条最近查词</small></article><article className="settings-card install-card"><div className="settings-card-head"><span className="settings-icon">⌂</span><div><h2>添加到手机主屏幕</h2><p>本应用支持 PWA。iPhone 上用 Safari 打开，点“分享”→“添加到主屏幕”，即可像 App 一样使用。</p></div></div><span className="muted">录音需要 HTTPS，或在本机用 localhost 打开。</span></article><AiTextSettingsCard notify={notify} /><PronunciationSettingsCard notify={notify} /><SpeechStorageCard notify={notify} /><article className="settings-card ai-card"><div className="settings-card-head"><span className="settings-icon">✦</span><div><h2>语音与 AI</h2><p>Kokoro 美式 TTS、本地 ASR 和音素评分由独立 AI 服务提供；发音模型首次评分时下载并缓存。AI 文本释义可在上方「AI 文本释义」卡片直接配置，或使用部署时的环境变量。</p></div></div><VoiceTestCard /><span className="muted">未启用 Azure 时默认使用本地音素模型；模型首次调用下载，模型或词典无法评分时明确回退到 ASR 转写比较。</span></article></div></section>;
}

function PronunciationSettingsCard({ notify }: { notify: (message: string) => void }) {
  const [provider, setProvider] = useState<'' | 'local' | 'azure'>('');
  const [region, setRegion] = useState('');
  const [apiKey, setApiKey] = useState('');
  const [hasKey, setHasKey] = useState(false);
  const [keyHint, setKeyHint] = useState('');
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [modelStatus, setModelStatus] = useState<PronunciationModelStatus | null>(null);
  const [modelStatusError, setModelStatusError] = useState('');
  const [requestingModel, setRequestingModel] = useState(false);

  const refreshModelStatus = useCallback(async () => {
    try {
      const status = await api<PronunciationModelStatus>('/pronunciation/model');
      setModelStatus(status);
      setModelStatusError('');
      return status;
    } catch (error) {
      setModelStatusError((error as Error).message || '无法读取模型状态');
      return null;
    }
  }, []);

  useEffect(() => {
    let cancelled = false;
    api<AiTextSettings & { pronunciation?: PronunciationSettings }>('/settings')
      .then((value) => {
        if (cancelled) return;
        const pron = value.pronunciation;
        if (pron) {
          setProvider(pron.provider);
          setRegion(pron.azureRegion);
          setHasKey(pron.hasKey);
          setKeyHint(pron.keyHint);
        }
        setLoading(false);
      })
      .catch(() => { if (!cancelled) { setLoading(false); } });
    return () => { cancelled = true; };
  }, []);

  useEffect(() => { void refreshModelStatus(); }, [refreshModelStatus]);

  useEffect(() => {
    if (modelStatus?.state !== 'downloading') return;
    let cancelled = false;
    let timer = 0;
    const poll = async () => {
      const status = await refreshModelStatus();
      if (!cancelled && status?.state === 'downloading') timer = window.setTimeout(() => void poll(), 1500);
    };
    timer = window.setTimeout(() => void poll(), 1500);
    return () => { cancelled = true; window.clearTimeout(timer); };
  }, [modelStatus?.state, refreshModelStatus]);

  const prepareModel = async () => {
    setRequestingModel(true);
    setModelStatusError('');
    try {
      const status = await api<PronunciationModelStatus>('/pronunciation/model', { method: 'POST' });
      setModelStatus(status);
    } catch (error) {
      setModelStatusError((error as Error).message || '模型下载或加载失败');
    } finally { setRequestingModel(false); }
  };

  const save = async () => {
    setSaving(true);
    try {
      const saved = await api<AiTextSettings & { pronunciation?: PronunciationSettings }>('/settings', { method: 'PUT', body: JSON.stringify({
        pronProvider: provider,
        pronAzureRegion: region.trim(),
        pronAzureApiKey: apiKey.trim() || undefined,
      }) });
      const pron = saved.pronunciation;
      if (pron) {
        setProvider(pron.provider);
        setRegion(pron.azureRegion);
        setHasKey(pron.hasKey);
        setKeyHint(pron.keyHint);
      }
      setApiKey('');
      notify('发音评估配置已保存');
    } catch (error) {
      notify((error as Error).message || '保存发音评估配置失败');
    } finally { setSaving(false); }
  };

  const modelPartState = (state?: string) => state === 'ready' ? '已加载' : state === 'downloaded' ? '已缓存' : state === 'not_downloaded' ? '未缓存' : state === 'loading' || state === 'downloading' ? '加载中' : state === 'failed' ? '失败' : '等待启动';
  const modelError = modelStatus?.state === 'failed' ? modelStatus.error : modelStatus?.asr?.state === 'failed' ? modelStatus.asr.error : modelStatus?.tts?.state === 'failed' ? modelStatus.tts.error : '';
  const modelStateText = modelStatus
    ? `音素评分 ${modelPartState(modelStatus.state)} · ASR ${modelPartState(modelStatus.asr?.state)} · Kokoro ${modelPartState(modelStatus.tts?.state)}${modelError ? `；${modelError}` : ''}`
    : modelStatusError || '正在读取模型状态…';

  return <article className="settings-card ai-config-card"><div className="settings-card-head"><span className="settings-icon">◎</span><div><h2>发音评估</h2><p>选择「录音并检查读音」使用的评估后端。本地音素评分、ASR 转写和 Kokoro 朗读模型会在 AI 服务启动时预加载并常驻内存。Azure 录音会上传到 Azure 并按用量计费，Key 只保存在服务器。</p></div></div>
    {loading ? <span className="muted">正在读取配置…</span> : <>
      <label className="ai-config-row"><input type="radio" name="pron-provider" checked={provider !== 'azure'} onChange={() => setProvider('local')} /> 本地音素评分（默认）</label>
      <label className="ai-config-row"><input type="radio" name="pron-provider" checked={provider === 'azure'} onChange={() => setProvider('azure')} /> Azure 发音评估（在线，音素级评分）</label>
      {provider === 'azure' && <div className="ai-config-fields">
        <div>
          <label className="ai-config-label">Azure 区域</label>
          <input value={region} onChange={(event) => setRegion(event.target.value)} placeholder="例如：eastasia" autoCapitalize="none" autoCorrect="off" />
        </div>
        <div>
          <label className="ai-config-label">订阅 Key <span>{hasKey ? `已保存（${keyHint}）` : '未设置'}</span></label>
          <input type="password" value={apiKey} onChange={(event) => setApiKey(event.target.value)} placeholder={hasKey ? '留空保持不变' : 'Azure 语音服务 Key'} autoCapitalize="none" autoCorrect="off" />
        </div>
      </div>}
      <div className="button-row"><button className="button button-secondary" onClick={() => void save()} disabled={saving}>{saving ? '保存中…' : '保存'}</button></div>
      <div className="pron-model-control"><div className={`pron-model-state ${modelStatus?.state === 'failed' ? 'is-error' : ''}`}><span className={`model-state-dot ${modelStatus?.state || 'unknown'}`} />{modelStateText}</div><button className="button button-secondary" onClick={() => void prepareModel()} disabled={requestingModel || modelStatus?.state === 'downloading' || modelStatus?.state === 'ready'}>{requestingModel ? '正在启动…' : modelStatus?.state === 'downloading' ? '正在准备模型…' : modelStatus?.state === 'ready' ? '模型已就绪' : modelStatus?.state === 'downloaded' ? '加载本地模型' : modelStatus?.state === 'failed' ? '重试模型准备' : '下载本地模型'}</button></div>
      {provider === 'azure' && <small className="muted">需要启用 Azure Speech 的订阅。浏览器录音会先转换为 16 kHz PCM WAV，再发送到所填区域；Azure 会按服务用量计费。</small>}
    </>}
  </article>;
}

function VoiceTestCard() {
  const [testText, setTestText] = useState('The resilience of a self-hosted vocabulary.');
  const [testing, setTesting] = useState(false);
  const [testResult, setTestResult] = useState('');
  const [testOk, setTestOk] = useState(false);
  const [mic] = useState(() => microphoneState());

  const playTest = async () => {
    const text = testText.trim() || 'Hello';
    setTesting(true);
    setTestResult('');
    try {
      const url = await fetchSpeechAudio(text);
      const audio = new Audio(url);
      audio.onended = () => URL.revokeObjectURL(url);
      await audio.play();
      setTestOk(true);
      setTestResult('语音合成并播放成功；生成的 WAV 已缓存在服务器，复习页可直接复用。');
    } catch (error) {
      setTestOk(false);
      setTestResult(`语音测试失败：${(error as Error).message || '未知错误'}（常见原因：AI 容器未启动、Kokoro 模型尚未下载完成、AI_SERVICE_URL 配置错误）`);
    } finally { setTesting(false); }
  };

  return <div className="voice-test">
    <div className="voice-test-row">
      <input
        value={testText}
        onChange={(event) => setTestText(event.target.value)}
        placeholder="输入要合成的英文句子"
        aria-label="语音测试文本"
      />
      <button className="button button-secondary" onClick={() => void playTest()} disabled={testing}>{testing ? '合成中…' : '▶ 生成并播放'}</button>
    </div>
    {testResult && <small className={`muted ${testOk ? 'ai-test-ok' : 'ai-test-fail'}`}>{testResult}</small>}
    <small className="muted">麦克风（录音检查读音）：{mic.supported ? '✓ ' : '✗ '}{mic.reason}</small>
  </div>;
}

function AiTextSettingsCard({ notify }: { notify: (message: string) => void }) {
  const [settings, setSettings] = useState<AiTextSettings | null>(null);
  const [enabled, setEnabled] = useState(false);
  const [baseUrl, setBaseUrl] = useState('');
  const [model, setModel] = useState('');
  const [apiKey, setApiKey] = useState('');
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [testing, setTesting] = useState(false);
  const [testResult, setTestResult] = useState('');

  const apply = (value: AiTextSettings) => {
    setSettings(value);
    setEnabled(value.provider === 'openai-compatible');
    setBaseUrl(value.baseUrl);
    setModel(value.model);
    setApiKey('');
  };

  useEffect(() => {
    let cancelled = false;
    api<AiTextSettings>('/settings')
      .then((value) => { if (!cancelled) { apply(value); setLoading(false); } })
      .catch(() => { if (!cancelled) { setLoading(false); setTestResult('当前离线，无法读取服务器上的 AI 配置'); } });
    return () => { cancelled = true; };
  }, []);

  const save = async (extra?: { clearApiKey?: boolean }): Promise<boolean> => {
    setSaving(true);
    try {
      const saved = await api<AiTextSettings>('/settings', { method: 'PUT', body: JSON.stringify({
        provider: enabled ? 'openai-compatible' : 'disabled',
        baseUrl: baseUrl.trim(),
        model: model.trim(),
        apiKey: apiKey.trim() || undefined,
        clearApiKey: extra?.clearApiKey ?? false,
      }) });
      apply(saved);
      setTestResult('');
      notify('AI 释义配置已保存');
      return true;
    } catch (error) {
      notify((error as Error).message || '保存 AI 配置失败');
      return false;
    } finally { setSaving(false); }
  };

  const saveAndTest = async () => {
    if (!(await save())) return;
    setTesting(true);
    try {
      const result = await api<{ definition: string }>('/gloss', { method: 'POST', body: JSON.stringify({ word: 'serendipity', contextText: 'A serendipity encounter brightened an ordinary afternoon.' }) });
      setTestResult(`测试成功：${result.definition.slice(0, 100)}`);
    } catch (error) {
      setTestResult(`测试失败：${(error as Error).message}`);
    } finally { setTesting(false); }
  };

  return <article className="settings-card ai-config-card"><div className="settings-card-head"><span className="settings-icon">⚙</span><div><h2>AI 文本释义</h2><p>词典没有的单词和词组由 OpenAI 兼容服务按语境生成释义。配置保存在你的服务器数据库中，优先于 .env；API Key 只保存在服务器，不会回传浏览器。</p></div></div>
    {loading ? <span className="muted">正在读取配置…</span> : <>
      <label className="ai-config-row"><input type="checkbox" checked={enabled} onChange={(event) => setEnabled(event.target.checked)} /> 启用 OpenAI 兼容释义服务</label>
      <div className="ai-config-fields">
        <label className="ai-config-label">Base URL</label>
        <input value={baseUrl} onChange={(event) => setBaseUrl(event.target.value)} placeholder="http://ollama:11434/v1" disabled={!enabled} />
        <label className="ai-config-label">模型名</label>
        <input value={model} onChange={(event) => setModel(event.target.value)} placeholder="qwen2.5:3b" disabled={!enabled} autoCapitalize="none" autoCorrect="off" />
        <label className="ai-config-label">API Key <span>{settings?.hasKey ? `已保存（${settings.keyHint}）` : '未设置'}</span></label>
        <input type="password" value={apiKey} onChange={(event) => setApiKey(event.target.value)} placeholder={settings?.hasKey ? '留空保持不变' : 'sk-…（本地 Ollama 可留空）'} disabled={!enabled} autoCapitalize="none" autoCorrect="off" />
      </div>
      <div className="button-row">
        <button className="button button-secondary" onClick={() => void save()} disabled={saving}>{saving ? '保存中…' : '保存'}</button>
        <button className="button button-quiet" onClick={() => void saveAndTest()} disabled={saving || testing}>{testing ? '测试中…' : '保存并测试'}</button>
        {settings?.hasKey && <button className="button button-quiet" onClick={() => void save({ clearApiKey: true })} disabled={saving}>清除 Key</button>}
      </div>
      {testResult && <small className={`muted ${testResult.startsWith('测试成功') ? 'ai-test-ok' : 'ai-test-fail'}`}>{testResult}</small>}
      {!enabled && <small className="muted">停用后本地词典释义不受影响；批量收词与「用 AI 补充释义」会明确报错，不会猜造释义。</small>}
    </>}
  </article>;
}

