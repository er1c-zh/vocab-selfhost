package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCardCreateReviewIdempotencyAndExport(t *testing.T) {
	store, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := &server{store: store, aiClient: http.DefaultClient}
	call := func(method, path string, payload any) *httptest.ResponseRecorder {
		var body bytes.Buffer
		if payload != nil {
			if err := json.NewEncoder(&body).Encode(payload); err != nil {
				t.Fatal(err)
			}
		}
		req := httptest.NewRequest(method, path, &body)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		return w
	}

	created := call(http.MethodPost, "/api/cards", CardInput{ID: "card-test", Word: "evidence", Definition: "information supporting a conclusion", ContextText: "The evidence supports the claim."})
	if created.Code != http.StatusCreated {
		t.Fatalf("create returned %d: %s", created.Code, created.Body.String())
	}
	due := call(http.MethodGet, "/api/cards?scope=due", nil)
	var cards []Card
	if err := json.Unmarshal(due.Body.Bytes(), &cards); err != nil || len(cards) != 1 {
		t.Fatalf("expected one due card, cards=%+v err=%v", cards, err)
	}

	reviewedAt := time.Now().UTC().Format(time.RFC3339Nano)
	reviewBody := map[string]any{"id": "review-event-1", "rating": 3, "reviewedAt": reviewedAt}
	review := call(http.MethodPost, "/api/cards/card-test/review", reviewBody)
	if review.Code != http.StatusOK {
		t.Fatalf("review returned %d: %s", review.Code, review.Body.String())
	}
	var first struct {
		Card      Card `json:"card"`
		Duplicate bool `json:"duplicate"`
	}
	if err := json.Unmarshal(review.Body.Bytes(), &first); err != nil || first.Card.Reps != 1 || first.Duplicate {
		t.Fatalf("unexpected review response: %+v err=%v", first, err)
	}
	again := call(http.MethodPost, "/api/cards/card-test/review", reviewBody)
	var second struct {
		Card      Card `json:"card"`
		Duplicate bool `json:"duplicate"`
	}
	if err := json.Unmarshal(again.Body.Bytes(), &second); err != nil || !second.Duplicate || second.Card.Reps != 1 {
		t.Fatalf("duplicate review should apply once: %+v err=%v", second, err)
	}

	export := call(http.MethodGet, "/api/export", nil)
	var bundle BackupBundle
	if err := json.Unmarshal(export.Body.Bytes(), &bundle); err != nil || len(bundle.Cards) != 1 || len(bundle.Reviews) != 1 || len(bundle.Dictionary) == 0 {
		t.Fatalf("export should include card, review history and local dictionary: cards=%d reviews=%d dictionary=%d err=%v", len(bundle.Cards), len(bundle.Reviews), len(bundle.Dictionary), err)
	}
}

func TestAPIHealthz(t *testing.T) {
	store, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := &server{store: store}
	req := httptest.NewRequest(http.MethodGet, "/api/healthz", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/healthz returned %d, want %d", w.Code, http.StatusOK)
	}
}

func TestBackupImportRestoresCardReviewAndDictionary(t *testing.T) {
	store, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	bundle := BackupBundle{
		Version:    1,
		Cards:      []Card{{ID: "restore-1", Word: "retention", Definition: "continued possession", CreatedAt: now, UpdatedAt: now, DueAt: now.Add(24 * time.Hour), State: "review", Difficulty: 4, Stability: 3.5, LastReview: now, Reps: 2}},
		Reviews:    []ReviewEvent{{ID: "restore-review-1", CardID: "restore-1", Rating: 3, ReviewedAt: now}},
		Dictionary: []DictionaryEntry{{Word: "retention", IPA: "rɪˈtɛnʃən", Definition: "the act of keeping something", Locale: "en-US"}},
	}
	if err := store.importBundle(t.Context(), bundle); err != nil {
		t.Fatal(err)
	}
	card, err := store.getCard(t.Context(), "restore-1")
	if err != nil || card.Reps != 2 || card.Stability != 3.5 || card.Word != "retention" {
		t.Fatalf("card state did not round-trip: %+v err=%v", card, err)
	}
	entries, err := store.searchDictionary(t.Context(), "retention")
	if err != nil || len(entries) == 0 || entries[0].IPA != "rɪˈtɛnʃən" {
		t.Fatalf("dictionary entry did not import: %+v err=%v", entries, err)
	}
	exported, err := store.export(t.Context())
	if err != nil || len(exported.Reviews) != 1 {
		t.Fatalf("review log did not restore: %+v err=%v", exported.Reviews, err)
	}
}
