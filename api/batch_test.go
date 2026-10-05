package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func batchCall(t *testing.T, s *server, req BatchRequest) (*httptest.ResponseRecorder, BatchResponse) {
	t.Helper()
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(req); err != nil {
		t.Fatal(err)
	}
	httpReq := httptest.NewRequest("POST", "/api/cards/batch", &body)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httpReq)
	var response BatchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("batch response is not JSON: %v body=%s", err, w.Body.String())
	}
	return w, response
}

func sampleBatchRequest(requestID string) BatchRequest {
	return BatchRequest{
		RequestID:   requestID,
		ContextText: "The momentum phenomenon is driven in large part by persistence in common return factors rather than solely by idiosyncratic stock performance.",
		Source:      "Asset Pricing · p. 12",
		Items: []BatchItemInput{
			{ID: "i1", SurfaceText: "momentum", NormalizedText: "momentum", Start: 4, End: 12},
			{ID: "i2", SurfaceText: "idiosyncratic", NormalizedText: "idiosyncratic", Start: 110, End: 123},
			{ID: "i3", SurfaceText: "common return factors", NormalizedText: "common return factors", Start: 78, End: 99},
		},
	}
}

func TestBatchCollectCreatesCardsWithContextAndSource(t *testing.T) {
	store, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := &server{store: store, aiClient: http.DefaultClient}

	w, response := batchCall(t, s, sampleBatchRequest("req-1"))
	if w.Code != 200 {
		t.Fatalf("batch returned %d: %s", w.Code, w.Body.String())
	}
	if response.Created != 3 || response.Duplicate != 0 || response.Failed != 0 || len(response.Results) != 3 {
		t.Fatalf("unexpected summary: created=%d duplicate=%d failed=%d", response.Created, response.Duplicate, response.Failed)
	}
	for _, result := range response.Results {
		if result.Status != "created" || result.CardID == "" {
			t.Fatalf("item should be created: %+v", result)
		}
	}
	cards, err := store.listCards(t.Context(), "", "")
	if err != nil || len(cards) != 3 {
		t.Fatalf("expected 3 cards, got %d err=%v", len(cards), err)
	}
	for _, card := range cards {
		if !strings.Contains(card.ContextText, "momentum phenomenon") {
			t.Fatalf("card lost the source sentence: %+v", card)
		}
		if card.Source != "Asset Pricing · p. 12" {
			t.Fatalf("card lost the source: %+v", card)
		}
		if card.State != "new" || card.Reps != 0 {
			t.Fatalf("new card should be reviewable immediately: %+v", card)
		}
	}
}

func TestBatchCollectIsIdempotent(t *testing.T) {
	store, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := &server{store: store, aiClient: http.DefaultClient}

	_, first := batchCall(t, s, sampleBatchRequest("same-request"))
	_, replay := batchCall(t, s, sampleBatchRequest("same-request"))
	if replay.Created != first.Created || replay.Duplicate != first.Duplicate || replay.Failed != first.Failed {
		t.Fatalf("replay should return the stored response verbatim: %+v", replay)
	}
	for i, result := range replay.Results {
		if result != first.Results[i] {
			t.Fatalf("replay result %d differs: %+v vs %+v", i, result, first.Results[i])
		}
	}
	cards, _ := store.listCards(t.Context(), "", "")
	if len(cards) != 3 {
		t.Fatalf("replay must not create new cards, got %d", len(cards))
	}
}

func TestBatchCollectSkipsDuplicateWordWithSameContext(t *testing.T) {
	store, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := &server{store: store, aiClient: http.DefaultClient}

	if _, response := batchCall(t, s, sampleBatchRequest("req-a")); response.Created != 3 {
		t.Fatalf("first batch should create 3 cards: %+v", response)
	}
	// Same words, same sentence, different request id: all duplicates.
	req := sampleBatchRequest("req-b")
	req.Items[1].SurfaceText = "IDIOSYNCRATIC" // case difference must still match
	_, response := batchCall(t, s, req)
	if response.Created != 0 || response.Duplicate != 3 {
		t.Fatalf("expected all duplicates: %+v", response)
	}
	for _, result := range response.Results {
		if result.Status != "duplicate" || result.CardID == "" {
			t.Fatalf("duplicate should reference the existing card: %+v", result)
		}
	}
	cards, _ := store.listCards(t.Context(), "", "")
	if len(cards) != 3 {
		t.Fatalf("duplicate batch must not create cards, got %d", len(cards))
	}
}

func TestBatchCollectDuplicateWithinOneRequest(t *testing.T) {
	store, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := &server{store: store, aiClient: http.DefaultClient}

	req := sampleBatchRequest("req-c")
	req.Items = append(req.Items, BatchItemInput{ID: "i4", SurfaceText: "Momentum", NormalizedText: "momentum", Start: 4, End: 12})
	_, response := batchCall(t, s, req)
	if response.Created != 3 || response.Duplicate != 1 {
		t.Fatalf("second occurrence in one batch should be a duplicate: created=%d duplicate=%d", response.Created, response.Duplicate)
	}
	last := response.Results[len(response.Results)-1]
	if last.Status != "duplicate" {
		t.Fatalf("unexpected status for in-batch duplicate: %+v", last)
	}
}

func TestBatchCollectPartialFailureKeepsOtherItems(t *testing.T) {
	store, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := &server{store: store, aiClient: http.DefaultClient}

	req := sampleBatchRequest("req-d")
	req.Items = append(req.Items, BatchItemInput{ID: "bad", SurfaceText: "ok", NormalizedText: strings.Repeat("x", 200), Start: 0, End: 1})
	w, response := batchCall(t, s, req)
	// The oversized item is rejected by request validation, which also rejects
	// the valid items; that is the contract for malformed requests.
	if w.Code != 400 {
		t.Fatalf("invalid item should fail the whole request with 400, got %d", w.Code)
	}
	_ = response

	// An item that fails during processing (empty normalized text) is reported
	// individually and does not block the rest.
	req2 := BatchRequest{RequestID: "req-e", ContextText: "Persistence matters.", Items: []BatchItemInput{
		{ID: "ok1", SurfaceText: "persistence", NormalizedText: "persistence", Start: 0, End: 11},
		{ID: "bad2", SurfaceText: "''", NormalizedText: "''", Start: 0, End: 0},
		{ID: "ok2", SurfaceText: "matters", NormalizedText: "matters", Start: 12, End: 19},
	}}
	_, response2 := batchCall(t, s, req2)
	if response2.Created != 2 || response2.Failed != 1 || len(response2.Results) != 3 {
		t.Fatalf("one bad item must not block the others: %+v", response2)
	}
	if response2.Results[1].Status != "error" {
		t.Fatalf("bad item should be reported as error: %+v", response2.Results[1])
	}
}

func TestBatchCollectRejectsInvalidRequests(t *testing.T) {
	store, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := &server{store: store, aiClient: http.DefaultClient}

	cases := map[string]func(*BatchRequest){
		"missing request id": func(r *BatchRequest) { r.RequestID = "" },
		"no items":           func(r *BatchRequest) { r.Items = nil },
		"too many items": func(r *BatchRequest) {
			r.Items = nil
			for i := 0; i < 51; i++ {
				r.Items = append(r.Items, BatchItemInput{SurfaceText: "w", NormalizedText: "w", Start: 0, End: 1})
			}
		},
	}
	for name, mutate := range cases {
		req := sampleBatchRequest("req-invalid")
		mutate(&req)
		w, _ := batchCall(t, s, req)
		if w.Code != 400 {
			t.Fatalf("%s: expected 400, got %d", name, w.Code)
		}
	}
}

func TestBatchCollectUsesAIGlossForUnknownWords(t *testing.T) {
	glossRequests := 0
	ai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/gloss" {
			http.NotFound(w, r)
			return
		}
		glossRequests++
		var payload struct {
			Word        string `json:"word"`
			ContextText string `json:"contextText"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if payload.ContextText == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		writeJSON(w, 200, map[string]string{"definition": "glossed meaning of " + payload.Word, "contextMeaning": "in-context sense"})
	}))
	defer ai.Close()

	store, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := &server{store: store, aiClient: ai.Client(), aiURL: ai.URL}

	req := sampleBatchRequest("req-gloss-1")
	_, response := batchCall(t, s, req)
	if response.Created != 3 || response.Failed != 0 {
		t.Fatalf("batch should succeed with AI gloss: %+v", response)
	}
	if glossRequests != 3 {
		t.Fatalf("every word missing from the dictionary should be glossed, got %d calls", glossRequests)
	}
	for _, card := range response.Results {
		stored, err := store.getCard(t.Context(), card.CardID)
		if err != nil || stored.Definition != "glossed meaning of "+stored.Word {
			t.Fatalf("definition was not filled from AI gloss: %+v err=%v", card, err)
		}
	}

	// A broken AI service must not lose the cards; definitions stay empty.
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer broken.Close()
	s.aiURL = broken.URL
	req2 := sampleBatchRequest("req-gloss-2")
	req2.Items = []BatchItemInput{
		{ID: "j1", SurfaceText: "serendipity", NormalizedText: "serendipity", Start: 0, End: 11},
		{ID: "j2", SurfaceText: "ephemeral", NormalizedText: "ephemeral", Start: 12, End: 21},
	}
	_, response2 := batchCall(t, s, req2)
	if response2.Created != 2 || response2.Failed != 0 {
		t.Fatalf("AI failure must not fail the batch: %+v", response2)
	}
	for _, result := range response2.Results {
		stored, err := store.getCard(t.Context(), result.CardID)
		if err != nil || stored.Definition != "" {
			t.Fatalf("card should survive a broken AI service without a definition: %+v err=%v", stored, err)
		}
	}
}

func TestBatchCollectPrefersUserSuppliedContent(t *testing.T) {
	glossRequests := 0
	ai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/gloss" {
			glossRequests++
			writeJSON(w, 200, map[string]string{"definition": "AI gloss that must not override the user"})
			return
		}
		if r.URL.Path == "/v1/tts" {
			wav := append([]byte("RIFF"), make([]byte, 40)...)
			writeJSON(w, 200, map[string]string{"audioBase64": base64.StdEncoding.EncodeToString(wav)})
			return
		}
		http.NotFound(w, r)
	}))
	defer ai.Close()

	store, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := &server{store: store, aiClient: ai.Client(), aiURL: ai.URL}

	req := BatchRequest{RequestID: "req-custom", ContextText: "Custom definitions win.", Items: []BatchItemInput{
		{ID: "c1", SurfaceText: "custom", NormalizedText: "custom", Start: 0, End: 6, IPA: "ˈkʌstəm", Definition: "user-written definition", ContextMeaning: "user context sense"},
		{ID: "c2", SurfaceText: "auto", NormalizedText: "auto", Start: 24, End: 28},
	}}
	_, response := batchCall(t, s, req)
	if response.Created != 2 || response.Failed != 0 {
		t.Fatalf("unexpected batch summary: %+v", response)
	}
	if glossRequests != 1 {
		t.Fatalf("AI gloss should run only for items without a user definition, got %d calls", glossRequests)
	}
	custom, err := store.getCard(t.Context(), response.Results[0].CardID)
	if err != nil || custom.IPA != "ˈkʌstəm" || custom.Definition != "user-written definition" || custom.ContextMeaning != "user context sense" {
		t.Fatalf("user-supplied content was not saved: %+v err=%v", custom, err)
	}
	auto, err := store.getCard(t.Context(), response.Results[1].CardID)
	if err != nil || auto.Definition != "AI gloss that must not override the user" {
		t.Fatalf("AI gloss should fill the empty item: %+v err=%v", auto, err)
	}
}

func TestSettingsDriveGlossOverride(t *testing.T) {
	var gotTextAI map[string]string
	ai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if override, ok := payload["textAI"].(map[string]any); ok {
			gotTextAI = map[string]string{}
			for k, v := range override {
				gotTextAI[k], _ = v.(string)
			}
		}
		writeJSON(w, 200, map[string]string{"definition": "glossed", "contextMeaning": ""})
	}))
	defer ai.Close()

	store, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := &server{store: store, aiClient: ai.Client(), aiURL: ai.URL}

	// Save UI settings: provider enabled with a custom key.
	put := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(body))
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		return w
	}
	if w := put(`{"provider":"openai-compatible","baseUrl":"http://internal:11434/v1","model":"qwen2.5:3b","apiKey":"secret-abcd1234"}`); w.Code != 200 {
		t.Fatalf("PUT /api/settings returned %d: %s", w.Code, w.Body.String())
	}
	get := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, get)
	var state map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state["provider"] != "openai-compatible" || state["hasKey"] != true || state["keyHint"] != "••••1234" {
		t.Fatalf("settings state wrong or key not masked: %+v", state)
	}

	// Batch gloss must forward the UI settings to the AI service.
	req := BatchRequest{RequestID: "req-settings", ContextText: "Settings drive the gloss.", Items: []BatchItemInput{
		{ID: "s1", SurfaceText: "setting", NormalizedText: "setting", Start: 0, End: 7},
	}}
	_, response := batchCall(t, s, req)
	if response.Created != 1 {
		t.Fatalf("batch with UI settings should create a card: %+v", response)
	}
	if gotTextAI == nil || gotTextAI["provider"] != "openai-compatible" || gotTextAI["model"] != "qwen2.5:3b" || gotTextAI["apiKey"] != "secret-abcd1234" {
		t.Fatalf("AI service did not receive the UI settings: %+v", gotTextAI)
	}

	// Disabling in the UI must win over an env-configured deployment.
	if w := put(`{"provider":"disabled"}`); w.Code != 200 {
		t.Fatalf("disable returned %d", w.Code)
	}
	gotTextAI = nil
	req.RequestID = "req-settings-off"
	req.Items = []BatchItemInput{{ID: "s2", SurfaceText: "another", NormalizedText: "another", Start: 0, End: 7}}
	req.ContextText = "A different sentence for the disabled case."
	_, response = batchCall(t, s, req)
	if response.Created != 1 {
		t.Fatalf("disabled provider must still create cards: %+v", response)
	}
	if gotTextAI == nil || gotTextAI["provider"] != "disabled" {
		t.Fatalf("disabled provider must be forwarded so it wins over env config: %+v", gotTextAI)
	}

	// Invalid provider values are rejected.
	if w := put(`{"provider":"bogus"}`); w.Code != 400 {
		t.Fatalf("bogus provider should return 400, got %d", w.Code)
	}
	if w := put(`{"provider":"openai-compatible","baseUrl":"ftp://x","model":"m"}`); w.Code != 400 {
		t.Fatalf("non-http baseUrl should return 400, got %d", w.Code)
	}
}
