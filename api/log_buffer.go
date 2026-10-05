package main

import (
	"strings"
	"sync"
)

type lineLogBuffer struct {
	mu    sync.Mutex
	limit int
	lines []string
}

func newLineLogBuffer(limit int) *lineLogBuffer {
	return &lineLogBuffer{limit: limit}
}

func (b *lineLogBuffer) Write(p []byte) (int, error) {
	text := strings.TrimSpace(string(p))
	if text == "" {
		return len(p), nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > 4000 {
			line = line[:4000] + "…"
		}
		b.lines = append(b.lines, line)
	}
	if len(b.lines) > b.limit {
		b.lines = append([]string(nil), b.lines[len(b.lines)-b.limit:]...)
	}
	return len(p), nil
}

func (b *lineLogBuffer) snapshot(limit int) []string {
	if b == nil {
		return []string{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if limit <= 0 || limit > len(b.lines) {
		limit = len(b.lines)
	}
	return append([]string(nil), b.lines[len(b.lines)-limit:]...)
}
