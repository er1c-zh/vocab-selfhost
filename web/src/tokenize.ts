// Tokenizer for the batch collection view. It splits English text into
// clickable word tokens while keeping exact character offsets into the
// original text, so a token or a multi-token phrase can always be mapped back
// to the user's pasted sentence (including PDF line breaks and odd spacing).

export type Token = {
  /** Ordinal of the token within the text. */
  index: number;
  /** Inclusive start offset into the original text. */
  start: number;
  /** Exclusive end offset into the original text. */
  end: number;
  /** The word exactly as it appears in the source text. */
  surface: string;
  /** Lowercased form used for dictionary lookup and de-duplication. */
  normalized: string;
  /** False for numbers and tokens without letters (they are not selectable). */
  isWord: boolean;
};

/**
 * A word is letters/digits, optionally joined by apostrophes or hyphens:
 * don't, it’s, characteristic-based, COVID-19. Trailing separators are not
 * consumed, so `dogs'` keeps the apostrophe out and `word -` stays two tokens.
 */
const WORD_RE = /[\p{L}\p{N}]+(?:['’\u2019-][\p{L}\p{N}]+)*/gu;

export function normalizeWord(surface: string): string {
  return surface
    .normalize('NFKC')
    .replace(/[\u2019\u02BC]/g, "'")
    .toLowerCase()
    .trim()
    .replace(/^['-]+|['-]+$/g, '');
}

export function tokenize(text: string): Token[] {
  const tokens: Token[] = [];
  for (const match of text.matchAll(WORD_RE)) {
    const surface = match[0];
    const normalized = normalizeWord(surface);
    if (!normalized) continue;
    tokens.push({
      index: tokens.length,
      start: match.index,
      end: match.index + surface.length,
      surface,
      normalized,
      isWord: /\p{L}/u.test(surface),
    });
  }
  return tokens;
}

/**
 * Build a phrase from a run of tokens. The surface text is sliced from the
 * original input and whitespace runs are collapsed to single spaces, so PDF
 * line breaks inside a phrase still read naturally. The normalized form joins
 * the per-token normalized words, dropping punctuation-only gaps.
 */
export function buildPhrase(text: string, tokens: Token[]): { surfaceText: string; normalizedText: string; start: number; end: number } | null {
  if (tokens.length === 0) return null;
  const start = tokens[0].start;
  const end = tokens[tokens.length - 1].end;
  const surfaceText = text.slice(start, end).replace(/\s+/g, ' ').trim();
  const normalizedText = tokens
    .map((token) => token.normalized)
    .filter(Boolean)
    .join(' ')
    .trim();
  if (!surfaceText || !normalizedText) return null;
  return { surfaceText, normalizedText, start, end };
}

/**
 * Map a DOM selection to the tokens it covers. `selectionStart`/`End` are
 * offsets inside the rendered container, which mirrors the original text
 * because the container renders every character of it in order.
 */
export function tokensInRange(tokens: Token[], selectionStart: number, selectionEnd: number): Token[] {
  if (selectionEnd <= selectionStart) return [];
  return tokens.filter((token) => token.end > selectionStart && token.start < selectionEnd);
}
