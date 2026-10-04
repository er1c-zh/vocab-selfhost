package main

import (
	"testing"
	"time"
)

func TestFSRSFirstReviewSchedulesInTheFuture(t *testing.T) {
	now := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	card := applyFSRS(Card{ID: "card-1", DueAt: now, State: "new"}, 3, now)
	if card.Stability <= 0 || card.Difficulty < 1 || card.Difficulty > 10 {
		t.Fatalf("invalid first-review memory state: %+v", card)
	}
	if !card.DueAt.After(now) || card.Reps != 1 || card.Lapses != 0 {
		t.Fatalf("unexpected first-review scheduling: %+v", card)
	}
}

func TestFSRSAgainIncrementsLapseAndKeepsDueAtLeastOneDay(t *testing.T) {
	now := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	card := Card{ID: "card-1", State: "review", Stability: 2, Difficulty: 5, Reps: 2, DueAt: now, LastReview: now.Add(-24 * time.Hour)}
	updated := applyFSRS(card, 1, now)
	if updated.Lapses != 1 || updated.Reps != 3 {
		t.Fatalf("expected one lapse and three reviews, got %+v", updated)
	}
	if updated.DueAt.Before(now.Add(24 * time.Hour)) {
		t.Fatalf("Again must schedule no sooner than one day in this day-based MVP: %s", updated.DueAt)
	}
}
