import { useMemo, useRef, useState } from 'react';
import type { ReactNode } from 'react';
import { getWordCache, id } from './api';
import type { BatchCollectRequest, BatchCollectResponse, Card } from './api';
import { buildPhrase, normalizeWord, tokenize, tokensInRange } from './tokenize';
import type { Token } from './tokenize';

// Mirrors the server-side batch limit in api/batch.go.
const MAX_ITEMS = 50;

type PendingItem = {
  /** stable id used for React keys and updates; the text may be edited */
  itemId: string;
  /** normalized text; the de-duplication key within the list */
  key: string;
  surfaceText: string;
  normalizedText: string;
  ipa: string;
  definition: string;
  contextMeaning: string;
  start: number;
  end: number;
  isPhrase: boolean;
  expanded: boolean;
};

type Props = {
  cards: Card[];
  onSubmitBatch: (request: BatchCollectRequest) => Promise<BatchCollectResponse | null>;
  notify: (message: string) => void;
};

export default function BatchCollectView({ cards, onSubmitBatch, notify }: Props) {
  const [text, setText] = useState('');
  const [source, setSource] = useState('');
  const [items, setItems] = useState<PendingItem[]>([]);
  const [submitting, setSubmitting] = useState(false);
  const [result, setResult] = useState<BatchCollectResponse | null>(null);
  const [queuedOffline, setQueuedOffline] = useState(false);
  const suppressClick = useRef(false);
  const containerRef = useRef<HTMLDivElement>(null);

  // Parsed automatically whenever the text changes; tokenize keeps the exact
  // offsets so selections always map back to the pasted sentence.
  const tokens = useMemo(() => tokenize(text), [text]);
  const selectedKeys = useMemo(() => new Set(items.map((item) => item.key)), [items]);
  // Tokens covered by any selected phrase keep a persistent highlight after
  // the native drag selection is cleared.
  const phraseTokenIndexes = useMemo(() => {
    const covered = new Set<number>();
    for (const item of items) {
      if (!item.isPhrase) continue; // single words already get is-selected
      for (const token of tokens) {
        if (token.start >= item.start && token.end <= item.end) covered.add(token.index);
      }
    }
    return covered;
  }, [items, tokens]);
  const existingWords = useMemo(() => new Set(cards.map((card) => card.word.trim().toLowerCase())), [cards]);

  const atCapacity = (count: number) => {
    if (count >= MAX_ITEMS) {
      notify(`一次最多批量添加 ${MAX_ITEMS} 项`);
      return true;
    }
    return false;
  };

  const makeItem = (surfaceText: string, normalizedText: string, start: number, end: number, isPhrase: boolean): PendingItem => {
    // Pre-fill from the browser's recent dictionary lookups; the server still
    // fills remaining gaps from the full dictionary or the optional AI gloss.
    const cached = getWordCache()[normalizedText] || (isPhrase ? undefined : getWordCache()[normalizedText.split(' ')[0]]);
    return {
      itemId: id(),
      key: normalizedText,
      surfaceText,
      normalizedText,
      ipa: cached?.ipa || '',
      definition: cached?.definition || '',
      contextMeaning: '',
      start,
      end,
      isPhrase,
      expanded: true,
    };
  };

  const toggleWord = (token: Token) => {
    setResult(null);
    setQueuedOffline(false);
    setItems((prev) => {
      if (prev.some((item) => item.key === token.normalized)) return prev.filter((item) => item.key !== token.normalized);
      if (atCapacity(prev.length)) return prev;
      return [...prev, makeItem(token.surface, token.normalized, token.start, token.end, false)];
    });
  };

  const togglePhrase = (phrase: { surfaceText: string; normalizedText: string; start: number; end: number }) => {
    setResult(null);
    setQueuedOffline(false);
    setItems((prev) => {
      if (prev.some((item) => item.key === phrase.normalizedText)) return prev.filter((item) => item.key !== phrase.normalizedText);
      if (atCapacity(prev.length)) return prev;
      return [...prev, makeItem(phrase.surfaceText, phrase.normalizedText, phrase.start, phrase.end, true)];
    });
  };

  const onTokenClick = (token: Token) => {
    if (suppressClick.current) {
      suppressClick.current = false;
      return;
    }
    toggleWord(token);
  };

  // Runs on mouseup / touchend: a non-collapsed native text selection over
  // several tokens becomes a phrase. Collapsed selections are left to the
  // click handler so single words still toggle.
  const handleSelectionEnd = () => {
    const container = containerRef.current;
    const selection = window.getSelection();
    if (!container || !selection || selection.rangeCount === 0 || selection.isCollapsed) return;
    const range = selection.getRangeAt(0);
    if (!container.contains(range.commonAncestorContainer)) return;
    // The container renders the pasted text verbatim, so text-content offsets
    // equal offsets in the original input.
    const before = range.cloneRange();
    before.selectNodeContents(container);
    before.setEnd(range.startContainer, range.startOffset);
    const after = range.cloneRange();
    after.selectNodeContents(container);
    after.setStart(range.endContainer, range.endOffset);
    const start = before.toString().length;
    const end = (container.textContent || '').length - after.toString().length;
    const covered = tokensInRange(tokens, start, end).filter((token) => token.isWord);
    if (covered.length < 2) return;
    const phrase = buildPhrase(text, covered);
    if (!phrase) return;
    togglePhrase(phrase);
    suppressClick.current = true;
    window.setTimeout(() => { suppressClick.current = false; }, 400);
    selection.removeAllRanges();
  };

  const patchItem = (itemId: string, patch: Partial<PendingItem>) => {
    setItems((prev) => prev.map((item) => (item.itemId === itemId ? { ...item, ...patch } : item)));
  };

  const renameItem = (itemId: string, surfaceText: string) => {
    setItems((prev) => prev.map((item) => {
      if (item.itemId !== itemId) return item;
      const normalizedText = item.isPhrase
        ? surfaceText.split(/\s+/).filter(Boolean).map(normalizeWord).join(' ')
        : normalizeWord(surfaceText);
      return { ...item, key: normalizedText || item.key, surfaceText, normalizedText: normalizedText || item.normalizedText };
    }));
  };

  const submit = async () => {
    if (!items.length || submitting) return;
    setSubmitting(true);
    try {
      const request: BatchCollectRequest = {
        requestId: id(),
        contextText: text.trim(),
        source: source.trim(),
        items: items.map((item) => ({
          id: id(),
          surfaceText: item.surfaceText,
          normalizedText: item.normalizedText,
          start: item.start,
          end: item.end,
          ipa: item.ipa.trim(),
          definition: item.definition.trim(),
          contextMeaning: item.contextMeaning.trim(),
        })),
      };
      const response = await onSubmitBatch(request);
      if (response === null) {
        setQueuedOffline(true);
        setItems([]);
        return;
      }
      setResult(response);
      setQueuedOffline(false);
      const failedKeys = new Set(response.results.filter((item) => item.status === 'error').map((item) => item.normalizedText));
      setItems((prev) => prev.filter((item) => failedKeys.has(item.normalizedText)));
    } catch (error) {
      notify((error as Error).message || '批量添加失败');
    } finally {
      setSubmitting(false);
    }
  };

  const renderInteractiveText = (): ReactNode[] => {
    const nodes: ReactNode[] = [];
    let cursor = 0;
    for (const token of tokens) {
      if (token.start > cursor) nodes.push(<span key={`gap-${cursor}`} className="token-gap">{text.slice(cursor, token.start)}</span>);
      if (token.isWord) {
        nodes.push(
          <span
            key={token.index}
            className={`token ${phraseTokenIndexes.has(token.index) ? 'is-in-phrase' : ''} ${selectedKeys.has(token.normalized) ? 'is-selected' : ''}`}
            onClick={() => onTokenClick(token)}
            title={existingWords.has(token.normalized) ? '已在词库' : '点击选择'}
          >{token.surface}</span>,
        );
      } else {
        nodes.push(<span key={`raw-${token.index}`} className="token-gap">{token.surface}</span>);
      }
      cursor = token.end;
    }
    if (cursor < text.length) nodes.push(<span key={`gap-${cursor}`} className="token-gap">{text.slice(cursor)}</span>);
    return nodes;
  };

  const statusFor = (item: PendingItem): 'created' | 'duplicate' | 'error' | null => {
    if (!result) return null;
    return result.results.find((r) => r.normalizedText === item.normalizedText)?.status || null;
  };

  return <section className="collect-page">
    <div className="page-heading"><div><span className="eyebrow">BATCH COLLECTION</span><h1>文本收词</h1></div><span className="muted">粘贴一段英文，点选单词或拖选词组，一次收进词库</span></div>

    <div className="collect-input card-block">
      <label htmlFor="collect-text">原句或段落 <span className="optional">粘贴后自动分词</span></label>
      <textarea
        id="collect-text"
        value={text}
        onChange={(event) => { setText(event.target.value); setResult(null); setQueuedOffline(false); }}
        placeholder="从论文、书籍或网页中复制一段英文粘贴到这里，例如：The momentum phenomenon is driven in large part by persistence in common return factors…"
        rows={5}
      />
      <div className="collect-input-row">
        <div className="collect-source-field">
          <label htmlFor="collect-source">来源 <span className="optional">文章名、页码或链接</span></label>
          <input id="collect-source" value={source} onChange={(event) => setSource(event.target.value)} placeholder="例如：Asset Pricing · p. 12" />
        </div>
        {text && <button type="button" className="button button-quiet" onClick={() => { setText(''); setItems([]); setResult(null); }}>清空</button>}
      </div>
    </div>

    {text.trim() && <div className="collect-text card-block">
      <div className="section-title"><span className="step-number">02</span><div><h2>选择要收集的词</h2><p>单击选择或取消一个单词；按住拖动可选中连续词组。</p></div></div>
      <div ref={containerRef} className="collect-tokens" onMouseUp={handleSelectionEnd} onTouchEnd={handleSelectionEnd}>{renderInteractiveText()}</div>
    </div>}

    <div className="collect-pending card-block">
      <div className="section-title"><span className="step-number">03</span><div><h2>待添加列表 <span className="heading-count">{items.length}</span></h2><p>每个词都有可折叠的编辑卡片：可修改文字并补填 IPA、释义和语境释义；留空的字段由本地词典或可选 AI 在提交时补齐。</p></div></div>
      {queuedOffline && <div className="collect-notice">当前离线：整批词语已保存在本机，联网后将自动同步到服务器。</div>}
      {items.length === 0 && !result && <p className="muted collect-empty">还没有选择任何词。点击上方文本中的单词，或拖动选择一个词组。</p>}
      {items.length > 0 && <div className="collect-list">
        {items.map((item) => {
          const exists = existingWords.has(item.normalizedText);
          const status = statusFor(item);
          return <div className={`collect-card ${item.expanded ? 'is-open' : ''}`} key={item.itemId}>
            <div className="collect-card-head">
              <button
                type="button"
                className="collect-card-toggle"
                onClick={() => patchItem(item.itemId, { expanded: !item.expanded })}
                aria-expanded={item.expanded}
              >
                <span className="collect-chevron">▸</span>
                <b className="collect-card-word">{item.surfaceText || item.normalizedText}</b>
                <span className="badge">{item.isPhrase ? '词组' : '单词'}</span>
                {exists && <span className="badge badge-dim">已在词库</span>}
                {status === 'created' && <span className="badge">已添加</span>}
                {status === 'duplicate' && <span className="badge badge-dim">重复</span>}
                {status === 'error' && <span className="badge badge-error">失败</span>}
                {!item.expanded && item.definition && <span className="collect-card-hint">{item.definition}</span>}
              </button>
              <button type="button" className="collect-card-delete" title="删除" onClick={() => { setItems((prev) => prev.filter((entry) => entry.itemId !== item.itemId)); }}>删除</button>
            </div>
            {item.expanded && <div className="collect-card-body">
              <div className="collect-field">
                <label>词汇</label>
                <input
                  value={item.surfaceText}
                  onChange={(event) => renameItem(item.itemId, event.target.value)}
                  aria-label="词汇"
                />
              </div>
              <div className="collect-field">
                <label>美式 IPA <span className="optional">留空则由词典或 AI 补齐</span></label>
                <input
                  value={item.ipa}
                  onChange={(event) => patchItem(item.itemId, { ipa: event.target.value })}
                  placeholder="ˌsɪɡˈnɪfɪkənt"
                  aria-label="美式 IPA"
                />
              </div>
              <div className="collect-field">
                <label>释义 <span className="optional">留空则由词典或 AI 补齐</span></label>
                <textarea
                  value={item.definition}
                  onChange={(event) => patchItem(item.itemId, { definition: event.target.value })}
                  placeholder="简洁地解释这个词的意思"
                  rows={2}
                  aria-label="释义"
                />
              </div>
              <div className="collect-field">
                <label>这个语境里的意思 <span className="optional">可选</span></label>
                <textarea
                  value={item.contextMeaning}
                  onChange={(event) => patchItem(item.itemId, { contextMeaning: event.target.value })}
                  placeholder="你对这句话中词义的理解"
                  rows={2}
                  aria-label="这个语境里的意思"
                />
              </div>
            </div>}
          </div>;
        })}
      </div>}
      {items.length > 0 && <button type="button" className="button button-primary" onClick={() => void submit()} disabled={submitting}>{submitting ? `正在批量添加 ${items.length} 项…` : `批量添加 ${items.length} 项`}</button>}
      {result && <div className="collect-result">
        <b>完成：{result.created} 个新词 · {result.duplicate} 个重复 · {result.failed} 个失败</b>
        <ul>
          {result.results.map((item) => <li key={`${item.id}-${item.normalizedText}`}>
            <b>{item.normalizedText}</b>
            {item.status === 'created' && <span className="badge">已添加</span>}
            {item.status === 'duplicate' && <span className="badge badge-dim">重复，未重复创建</span>}
            {item.status === 'error' && <span className="badge badge-error">{item.error || '添加失败'}</span>}
          </li>)}
        </ul>
      </div>}
    </div>
  </section>;
}
