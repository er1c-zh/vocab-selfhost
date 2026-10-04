import { describe, expect, it } from 'vitest';
import { buildPhrase, normalizeWord, tokenize, tokensInRange } from './tokenize';

const words = (text: string) => tokenize(text).map((token) => token.surface);

describe('tokenize', () => {
  it('splits a sentence into word tokens with offsets', () => {
    const text = 'The momentum phenomenon is driven.';
    const tokens = tokenize(text);
    expect(tokens.map((t) => t.surface)).toEqual(['The', 'momentum', 'phenomenon', 'is', 'driven']);
    const momentum = tokens[1];
    expect(text.slice(momentum.start, momentum.end)).toBe('momentum');
    expect(tokens[0].normalized).toBe('the');
  });

  it('strips trailing punctuation but keeps offsets valid', () => {
    const tokens = tokenize('performance, stock; market!');
    expect(tokens.map((t) => t.surface)).toEqual(['performance', 'stock', 'market']);
    for (const token of tokens) {
      expect(token.surface).not.toMatch(/[.,;!]/);
    }
  });

  it('keeps apostrophized words intact, straight and curly', () => {
    expect(words("don't stop believing")).toEqual(["don't", 'stop', 'believing']);
    expect(words('it’s fine')).toEqual(['it’s', 'fine']);
    expect(tokenize("don't")[0].normalized).toBe("don't");
    // A possessive apostrophe at the end is not part of the word.
    expect(words("the dogs' bowls")).toEqual(['the', 'dogs', 'bowls']);
  });

  it('keeps hyphenated compounds as one token', () => {
    expect(words('a characteristic-based model')).toEqual(['a', 'characteristic-based', 'model']);
    // A standalone hyphen between spaces does not become a token.
    expect(words('risk - adjusted')).toEqual(['risk', 'adjusted']);
  });

  it('handles numbers mixed with letters', () => {
    expect(words('COVID-19 changed 2020 markets')).toEqual(['COVID-19', 'changed', '2020', 'markets']);
  });

  it('survives PDF line breaks, CRLF and repeated whitespace', () => {
    const text = 'persistence\r\nin   common\n\t\treturn factors';
    const tokens = tokenize(text);
    expect(tokens.map((t) => t.surface)).toEqual(['persistence', 'in', 'common', 'return', 'factors']);
    for (const token of tokens) {
      expect(text.slice(token.start, token.end)).toBe(token.surface);
    }
  });

  it('keeps Unicode letters as words', () => {
    const tokens = tokenize('café naïve über');
    expect(tokens.map((t) => t.normalized)).toEqual(['café', 'naïve', 'über']);
  });

  it('marks number-only tokens as non-words', () => {
    const tokens = tokenize('page 42 shows');
    expect(tokens[1].isWord).toBe(false);
    expect(tokens[0].isWord).toBe(true);
  });
});

describe('normalizeWord', () => {
  it('lowercases and trims punctuation edges', () => {
    expect(normalizeWord('Momentum')).toBe('momentum');
    expect(normalizeWord("don't")).toBe("don't");
    expect(normalizeWord("'quoted-")).toBe('quoted');
  });
});

describe('buildPhrase', () => {
  it('builds a phrase preserving internal spaces from the original text', () => {
    const text = 'driven in large part by persistence';
    const tokens = tokenize(text);
    const phrase = buildPhrase(text, tokens.slice(2, 4)); // "large part"
    expect(phrase?.surfaceText).toBe('large part');
    expect(phrase?.normalizedText).toBe('large part');
    expect(phrase?.start).toBe(text.indexOf('large'));
    expect(phrase?.end).toBe(text.indexOf('large') + 'large part'.length);
  });

  it('collapses PDF line breaks inside a phrase', () => {
    const text = 'common\nreturn factors';
    const tokens = tokenize(text);
    const phrase = buildPhrase(text, tokens);
    expect(phrase?.surfaceText).toBe('common return factors');
  });

  it('keeps hyphens and apostrophes inside a phrase', () => {
    const text = "risk-adjusted don't stop";
    const phrase = buildPhrase(text, tokenize(text));
    expect(phrase?.surfaceText).toBe("risk-adjusted don't stop");
  });

  it('returns null for an empty token list', () => {
    expect(buildPhrase('any text', [])).toBeNull();
  });
});

describe('tokensInRange', () => {
  const text = 'The momentum phenomenon is driven';
  const tokens = tokenize(text);

  it('returns tokens fully covered by the selection', () => {
    const start = text.indexOf('momentum');
    const end = text.indexOf('phenomenon') + 'phenomenon'.length;
    expect(tokensInRange(tokens, start, end).map((t) => t.surface)).toEqual(['momentum', 'phenomenon']);
  });

  it('includes a token that is only partially selected', () => {
    const start = text.indexOf('mo');
    const end = start + 2;
    expect(tokensInRange(tokens, start, end).map((t) => t.surface)).toEqual(['momentum']);
  });

  it('returns nothing for a collapsed selection', () => {
    expect(tokensInRange(tokens, 5, 5)).toEqual([]);
  });
});
