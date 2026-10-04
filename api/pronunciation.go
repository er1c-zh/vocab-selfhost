package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// azureSTTTimeout bounds one pronunciation-assessment call. Review recordings
// are short, and a longer request is unlikely to recover usefully.
const azureSTTTimeout = 60 * time.Second

type PronunciationRequest struct {
	Expected    string `json:"expected"`
	AudioMime   string `json:"audioMime"`
	AudioBase64 string `json:"audioBase64"`
}

func (s *server) handlePronunciationModel(w http.ResponseWriter, r *http.Request) {
	var method string
	var body []byte
	switch r.Method {
	case http.MethodGet:
		method = http.MethodGet
	case http.MethodPost:
		method = http.MethodPost
		body = []byte(`{}`)
	default:
		methodNotAllowed(w)
		return
	}

	response, status, err := s.callAIRequest(r.Context(), method, "/v1/pronunciation/model", body)
	if err != nil {
		writeError(w, status, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(response)
}

func (s *server) handlePronunciation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<20))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "request is too large")
		return
	}
	var req PronunciationRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	req.Expected = strings.TrimSpace(req.Expected)
	if req.Expected == "" || len(req.Expected) > 100 {
		writeError(w, http.StatusBadRequest, "expected word must contain 1-100 characters")
		return
	}
	started := time.Now()
	provider := "local"
	defer func() {
		log.Printf("Pronunciation assessment provider=%s duration=%s", provider, time.Since(started).Round(time.Millisecond))
	}()

	cfg := s.loadPronunciation(r.Context())
	if cfg == nil || cfg.Provider != "azure" {
		// Local path: the self-hosted AI service scores target phones and returns
		// ASR transcript matching only when local phoneme scoring is unavailable.
		response, status, err := s.callAIBytes(r.Context(), "/v1/pronunciation", raw)
		if err != nil {
			writeError(w, status, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(response)
		return
	}
	provider = "azure"

	contentType, err := azureContentType(req.AudioMime)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	audio, err := base64.StdEncoding.DecodeString(req.AudioBase64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "audioBase64 is not valid base64")
		return
	}
	if len(audio) < 800 {
		writeJSON(w, http.StatusOK, azureInconclusive(req.Expected))
		return
	}
	if len(audio) > 12<<20 {
		writeError(w, http.StatusBadRequest, "recording is too large; keep it under 12 MB")
		return
	}
	if cfg.AzureKey == "" || !validAzureRegion(cfg.AzureRegion) {
		writeError(w, http.StatusServiceUnavailable, "Azure pronunciation assessment is not configured; check the region and subscription key in settings")
		return
	}
	result, err := s.azurePronAssess(r.Context(), req.Expected, audio, contentType, cfg)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func azureContentType(mime string) (string, error) {
	mediaType := strings.ToLower(strings.TrimSpace(strings.SplitN(mime, ";", 2)[0]))
	switch mediaType {
	case "audio/wav":
		return "audio/wav; codecs=audio/pcm; samplerate=16000", nil
	case "audio/ogg":
		return "audio/ogg; codecs=opus", nil
	default:
		return "", fmt.Errorf("Azure accepts 16 kHz PCM WAV or OGG Opus audio; got %q", mime)
	}
}

func azureInconclusive(expected string) map[string]any {
	return map[string]any{
		"expected":   expected,
		"transcript": "",
		"similarity": 0,
		"confidence": 0,
		"status":     "inconclusive",
		"method":     "azure-pronunciation-assessment",
		"notice":     "没有识别到可评估的语音；请对着麦克风清楚地读一次。",
	}
}

type azureScoreFields struct {
	AccuracyScore     *float64 `json:"AccuracyScore"`
	FluencyScore      *float64 `json:"FluencyScore"`
	ProsodyScore      *float64 `json:"ProsodyScore"`
	CompletenessScore *float64 `json:"CompletenessScore"`
	PronScore         *float64 `json:"PronScore"`
}

type azurePhoneme struct {
	Phoneme                 string           `json:"Phoneme"`
	AccuracyScore           *float64         `json:"AccuracyScore"`
	PronunciationAssessment azureScoreFields `json:"PronunciationAssessment"`
}

type azureWord struct {
	Word                    string           `json:"Word"`
	AccuracyScore           *float64         `json:"AccuracyScore"`
	PronunciationAssessment azureScoreFields `json:"PronunciationAssessment"`
	Phonemes                []azurePhoneme   `json:"Phonemes"`
}

type azureNBest struct {
	Confidence              float64          `json:"Confidence"`
	Display                 string           `json:"Display"`
	Lexical                 string           `json:"Lexical"`
	AccuracyScore           *float64         `json:"AccuracyScore"`
	FluencyScore            *float64         `json:"FluencyScore"`
	ProsodyScore            *float64         `json:"ProsodyScore"`
	CompletenessScore       *float64         `json:"CompletenessScore"`
	PronScore               *float64         `json:"PronScore"`
	PronunciationAssessment azureScoreFields `json:"PronunciationAssessment"`
	Words                   []azureWord      `json:"Words"`
}

type azureRecognition struct {
	RecognitionStatus json.RawMessage `json:"RecognitionStatus"`
	DisplayText       string          `json:"DisplayText"`
	NBest             []azureNBest    `json:"NBest"`
}

func (r azureRecognition) succeeded() bool {
	var status string
	if json.Unmarshal(r.RecognitionStatus, &status) == nil {
		return strings.EqualFold(status, "Success")
	}
	// Some Speech SDK response shapes encode the enum numerically; Success is 0.
	var numeric int
	return json.Unmarshal(r.RecognitionStatus, &numeric) == nil && numeric == 0
}

func firstScore(primary, nested *float64) (float64, bool) {
	if primary != nil {
		return *primary, true
	}
	if nested != nil {
		return *nested, true
	}
	return 0, false
}

func (s *server) azurePronAssess(ctx context.Context, expected string, audio []byte, contentType string, cfg *pronunciationSettings) (map[string]any, error) {
	params, err := json.Marshal(map[string]any{
		"ReferenceText":           expected,
		"GradingSystem":           "HundredMark",
		"Granularity":             "Phoneme",
		"Dimension":               "Comprehensive",
		"EnableMiscue":            "True",
		"EnableProsodyAssessment": "True",
	})
	if err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("https://%s.stt.speech.microsoft.com/speech/recognition/conversation/cognitiveservices/v1", cfg.AzureRegion)
	if s.azureSTTURL != "" { // overridable for tests
		endpoint = s.azureSTTURL
	}
	parsedURL, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid Azure Speech endpoint: %w", err)
	}
	query := parsedURL.Query()
	query.Set("language", "en-US")
	query.Set("format", "detailed")
	parsedURL.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, parsedURL.String(), bytes.NewReader(audio))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Ocp-Apim-Subscription-Key", cfg.AzureKey)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Pronunciation-Assessment", base64.StdEncoding.EncodeToString(params))
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: azureSTTTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Azure pronunciation service unavailable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("Azure rejected the subscription key (check the key and region)")
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("Azure rate limit reached; try again later")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("Azure pronunciation assessment failed (%d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed azureRecognition
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("Azure returned an unexpected response format: %w", err)
	}
	notice := "Azure 将读音与目标词的发音进行自动比对；这些分数用于练习反馈，不代表口音诊断或母语水平认证。"
	if !parsed.succeeded() || len(parsed.NBest) == 0 {
		result := azureInconclusive(expected)
		result["notice"] = notice
		return result, nil
	}

	best := parsed.NBest[0]
	accuracy, ok := firstScore(best.AccuracyScore, best.PronunciationAssessment.AccuracyScore)
	if !ok {
		return nil, fmt.Errorf("Azure response did not include pronunciation accuracy scores")
	}
	fluency, hasFluency := firstScore(best.FluencyScore, best.PronunciationAssessment.FluencyScore)
	prosody, hasProsody := firstScore(best.ProsodyScore, best.PronunciationAssessment.ProsodyScore)
	completeness, hasCompleteness := firstScore(best.CompletenessScore, best.PronunciationAssessment.CompletenessScore)
	overall, hasOverall := firstScore(best.PronScore, best.PronunciationAssessment.PronScore)
	transcript := parsed.DisplayText
	if transcript == "" {
		transcript = best.Display
	}
	if transcript == "" {
		transcript = best.Lexical
	}
	confidence := math.Max(0, math.Min(1, best.Confidence))
	status := "needs_practice"
	if accuracy >= 80 {
		status = "recognized"
	}

	words := make([]map[string]any, 0, len(best.Words))
	for i, word := range best.Words {
		if i >= 12 {
			break
		}
		wordAccuracy, hasWordAccuracy := firstScore(word.AccuracyScore, word.PronunciationAssessment.AccuracyScore)
		if !hasWordAccuracy {
			continue
		}
		phonemes := make([]map[string]any, 0, len(word.Phonemes))
		for j, phoneme := range word.Phonemes {
			if j >= 32 {
				break
			}
			phonemeAccuracy, hasPhonemeAccuracy := firstScore(phoneme.AccuracyScore, phoneme.PronunciationAssessment.AccuracyScore)
			if hasPhonemeAccuracy && phoneme.Phoneme != "" {
				phonemes = append(phonemes, map[string]any{"phoneme": phoneme.Phoneme, "accuracy": phonemeAccuracy})
			}
		}
		words = append(words, map[string]any{
			"word":     strings.Trim(word.Word, ".,!?;:"),
			"accuracy": wordAccuracy,
			"phonemes": phonemes,
		})
	}
	scores := map[string]float64{"accuracy": accuracy}
	if hasOverall {
		scores["overall"] = overall
	}
	if hasFluency {
		scores["fluency"] = fluency
	}
	if hasProsody {
		scores["prosody"] = prosody
	}
	if hasCompleteness {
		scores["completeness"] = completeness
	}
	return map[string]any{
		"expected":   expected,
		"transcript": strings.TrimSpace(transcript),
		"similarity": math.Max(0, math.Min(1, accuracy/100)),
		"confidence": confidence,
		"status":     status,
		"method":     "azure-pronunciation-assessment",
		"notice":     notice,
		"scores":     scores,
		"words":      words,
	}, nil
}
