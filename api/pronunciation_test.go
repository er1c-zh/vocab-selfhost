package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func pronCall(t *testing.T, s *server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/pronunciation", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	return w
}

func TestPronunciationLocalFallback(t *testing.T) {
	ai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/pronunciation" {
			http.NotFound(w, r)
			return
		}
		var payload PronunciationRequest
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if payload.Expected != "hello" {
			t.Fatalf("local AI should receive the original payload, got %+v", payload)
		}
		writeJSON(w, 200, map[string]any{"expected": "hello", "transcript": "hello", "similarity": 1.0, "confidence": 0.9, "status": "recognized", "method": "asr-transcript-match", "notice": "n"})
	}))
	defer ai.Close()

	store, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := &server{store: store, aiClient: ai.Client(), aiURL: ai.URL}

	w := pronCall(t, s, `{"expected":"hello","audioMime":"audio/webm;codecs=opus","audioBase64":"AAAA"}`)
	if w.Code != 200 {
		t.Fatalf("local pronunciation returned %d: %s", w.Code, w.Body.String())
	}
	var result map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result["method"] != "asr-transcript-match" {
		t.Fatalf("expected passthrough of local ASR result: %s", w.Body.String())
	}
}

func TestPronunciationModelStatusAndDownloadRoutes(t *testing.T) {
	var gotMethods []string
	ai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/pronunciation/model" {
			http.NotFound(w, r)
			return
		}
		gotMethods = append(gotMethods, r.Method)
		if r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, map[string]any{"state": "downloaded", "downloaded": true, "loaded": false, "error": ""})
			return
		}
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if len(payload) != 0 {
			t.Errorf("model preparation should have an empty object body, got %+v", payload)
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"state": "downloading", "downloaded": false, "loaded": false, "error": ""})
	}))
	defer ai.Close()

	store, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := &server{store: store, aiClient: ai.Client(), aiURL: ai.URL}
	call := func(method string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/api/pronunciation/model", nil)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		return w
	}

	status := call(http.MethodGet)
	if status.Code != http.StatusOK || !strings.Contains(status.Body.String(), `"state":"downloaded"`) {
		t.Fatalf("model status returned %d: %s", status.Code, status.Body.String())
	}
	started := call(http.MethodPost)
	if started.Code != http.StatusAccepted || !strings.Contains(started.Body.String(), `"state":"downloading"`) {
		t.Fatalf("model preparation returned %d: %s", started.Code, started.Body.String())
	}
	if len(gotMethods) != 2 || gotMethods[0] != http.MethodGet || gotMethods[1] != http.MethodPost {
		t.Fatalf("unexpected upstream methods: %v", gotMethods)
	}
}

func TestPronunciationAzureProvider(t *testing.T) {
	var gotParams map[string]any
	var gotContentType string
	var gotQuery string
	var gotKey string
	azure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		gotQuery = r.URL.RawQuery
		gotKey = r.Header.Get("Ocp-Apim-Subscription-Key")
		decoded, err := base64.StdEncoding.DecodeString(r.Header.Get("Pronunciation-Assessment"))
		if err != nil {
			t.Fatalf("Pronunciation-Assessment header is not base64: %v", err)
		}
		if err := json.Unmarshal(decoded, &gotParams); err != nil {
			t.Fatal(err)
		}
		writeJSON(w, 200, map[string]any{
			"RecognitionStatus": "Success",
			"DisplayText":       "Hello.",
			"NBest": []map[string]any{{
				"Confidence":        0.96,
				"AccuracyScore":     85,
				"FluencyScore":      90,
				"ProsodyScore":      70,
				"CompletenessScore": 100,
				"PronScore":         88,
				"Words": []map[string]any{
					{"Word": "Hello", "AccuracyScore": 85, "Phonemes": []map[string]any{
						{"Phoneme": "h", "PronunciationAssessment": map[string]float64{"AccuracyScore": 92}},
					}},
				},
			}},
		})
	}))
	defer azure.Close()

	store, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := &server{store: store, aiClient: http.DefaultClient, aiURL: "http://localhost:1", azureSTTURL: azure.URL}

	// Save text-AI settings first, then configure Azure pronunciation as the UI does.
	textPutReq := httptest.NewRequest(http.MethodPut, "/api/settings", bytes.NewBufferString(`{"provider":"openai-compatible","baseUrl":"http://ollama:11434/v1","model":"qwen2.5:3b"}`))
	textPutW := httptest.NewRecorder()
	s.ServeHTTP(textPutW, textPutReq)
	if textPutW.Code != 200 {
		t.Fatalf("text settings PUT returned %d: %s", textPutW.Code, textPutW.Body.String())
	}
	putReq := httptest.NewRequest(http.MethodPut, "/api/settings", bytes.NewBufferString(`{"pronProvider":"azure","pronAzureRegion":"eastasia","pronAzureApiKey":"azure-key-4321"}`))
	putW := httptest.NewRecorder()
	s.ServeHTTP(putW, putReq)
	if putW.Code != 200 {
		t.Fatalf("settings PUT returned %d: %s", putW.Code, putW.Body.String())
	}
	var state struct {
		Pronunciation struct {
			Provider string `json:"provider"`
			HasKey   bool   `json:"hasKey"`
			KeyHint  string `json:"keyHint"`
		} `json:"pronunciation"`
	}
	if err := json.Unmarshal(putW.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.Pronunciation.Provider != "azure" || !state.Pronunciation.HasKey || state.Pronunciation.KeyHint != "••••4321" {
		t.Fatalf("pronunciation settings wrong: %+v", state.Pronunciation)
	}
	var settingsAfterAzure map[string]any
	if err := json.Unmarshal(putW.Body.Bytes(), &settingsAfterAzure); err != nil {
		t.Fatal(err)
	}
	if settingsAfterAzure["provider"] != "openai-compatible" || settingsAfterAzure["model"] != "qwen2.5:3b" {
		t.Fatalf("saving pronunciation settings erased text-AI settings: %+v", settingsAfterAzure)
	}

	w := pronCall(t, s, `{"expected":"hello","audioMime":"audio/wav","audioBase64":"`+base64.StdEncoding.EncodeToString(make([]byte, 1024))+`"}`)
	if w.Code != 200 {
		t.Fatalf("azure pronunciation returned %d: %s", w.Code, w.Body.String())
	}
	var result struct {
		Method     string  `json:"method"`
		Status     string  `json:"status"`
		Similarity float64 `json:"similarity"`
		Confidence float64 `json:"confidence"`
		Transcript string  `json:"transcript"`
		Scores     struct {
			Accuracy float64 `json:"accuracy"`
			Fluency  float64 `json:"fluency"`
			Prosody  float64 `json:"prosody"`
			Overall  float64 `json:"overall"`
		} `json:"scores"`
		Words []struct {
			Word     string  `json:"word"`
			Accuracy float64 `json:"accuracy"`
			Phonemes []struct {
				Phoneme  string  `json:"phoneme"`
				Accuracy float64 `json:"accuracy"`
			} `json:"phonemes"`
		} `json:"words"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Method != "azure-pronunciation-assessment" || result.Status != "recognized" || result.Similarity != 0.85 || result.Confidence != 0.96 {
		t.Fatalf("unexpected azure result: %+v", result)
	}
	if result.Scores.Accuracy != 85 || result.Scores.Fluency != 90 || result.Scores.Prosody != 70 || result.Scores.Overall != 88 || len(result.Words) != 1 || result.Words[0].Word != "Hello" || len(result.Words[0].Phonemes) != 1 || result.Words[0].Phonemes[0].Phoneme != "h" {
		t.Fatalf("score details missing: %+v", result)
	}
	if gotParams["ReferenceText"] != "hello" || gotParams["Granularity"] != "Phoneme" || gotParams["EnableProsodyAssessment"] != "True" || gotParams["EnableMiscue"] != "True" {
		t.Fatalf("assessment params wrong: %+v", gotParams)
	}
	if gotContentType != "audio/wav; codecs=audio/pcm; samplerate=16000" {
		t.Fatalf("content type not mapped: %q", gotContentType)
	}
	if gotKey != "azure-key-4321" {
		t.Fatalf("subscription key not sent to Azure: %q", gotKey)
	}
	query, err := url.ParseQuery(gotQuery)
	if err != nil || query.Get("language") != "en-US" || query.Get("format") != "detailed" {
		t.Fatalf("unexpected Azure request query: %q", gotQuery)
	}

	// Saving only the text-AI settings later must preserve the Azure provider.
	textUpdate := httptest.NewRequest(http.MethodPut, "/api/settings", bytes.NewBufferString(`{"provider":"disabled"}`))
	textUpdateW := httptest.NewRecorder()
	s.ServeHTTP(textUpdateW, textUpdate)
	if textUpdateW.Code != 200 {
		t.Fatalf("text settings update returned %d: %s", textUpdateW.Code, textUpdateW.Body.String())
	}
	var afterTextUpdate struct {
		Pronunciation struct {
			Provider string `json:"provider"`
			HasKey   bool   `json:"hasKey"`
		} `json:"pronunciation"`
	}
	if err := json.Unmarshal(textUpdateW.Body.Bytes(), &afterTextUpdate); err != nil {
		t.Fatal(err)
	}
	if afterTextUpdate.Pronunciation.Provider != "azure" || !afterTextUpdate.Pronunciation.HasKey {
		t.Fatalf("saving text-AI settings erased Azure settings: %+v", afterTextUpdate.Pronunciation)
	}
}

func TestPronunciationAzureFailures(t *testing.T) {
	// Recognition failure -> inconclusive result, not an error.
	azure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"RecognitionStatus": "NoSpeech"})
	}))
	defer azure.Close()
	store, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := &server{store: store, aiClient: http.DefaultClient, azureSTTURL: azure.URL}

	putReq := httptest.NewRequest(http.MethodPut, "/api/settings", bytes.NewBufferString(`{"pronProvider":"azure","pronAzureRegion":"eastasia","pronAzureApiKey":"k"}`))
	s.ServeHTTP(httptest.NewRecorder(), putReq)

	w := pronCall(t, s, `{"expected":"word","audioMime":"audio/wav","audioBase64":"`+base64.StdEncoding.EncodeToString(make([]byte, 1024))+`"}`)
	if w.Code != 200 {
		t.Fatalf("recognition failure should be a 200 inconclusive result, got %d", w.Code)
	}
	var result map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	if result["status"] != "inconclusive" {
		t.Fatalf("expected inconclusive, got %+v", result)
	}

	// Azure auth failure surfaces as 502 with a clear message.
	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer denied.Close()
	s.azureSTTURL = denied.URL
	w = pronCall(t, s, `{"expected":"word","audioMime":"audio/wav","audioBase64":"`+base64.StdEncoding.EncodeToString(make([]byte, 1024))+`"}`)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("azure 401 should map to 502, got %d", w.Code)
	}

	// Validation: an unsupported provider format is rejected before upstream use.
	unsupported := pronCall(t, s, `{"expected":"word","audioMime":"audio/webm","audioBase64":"`+base64.StdEncoding.EncodeToString(make([]byte, 1024))+`"}`)
	if unsupported.Code != http.StatusBadRequest {
		t.Fatalf("unsupported Azure audio content type should return 400, got %d", unsupported.Code)
	}

	// Validation: Azure cannot be enabled without a region and a key.
	badStore, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer badStore.Close()
	badServer := &server{store: badStore, aiClient: http.DefaultClient}
	badReq := httptest.NewRequest(http.MethodPut, "/api/settings", bytes.NewBufferString(`{"pronProvider":"azure"}`))
	badW := httptest.NewRecorder()
	badServer.ServeHTTP(badW, badReq)
	if badW.Code != 400 {
		t.Fatalf("azure without region should return 400, got %d", badW.Code)
	}
}
