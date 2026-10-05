package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Batch collection limits: keep a single request bounded so one paste cannot
// stall the single-connection SQLite database or the optional AI service.
const (
	batchMaxItems        = 50
	batchMaxContextChars = 10000
	batchMaxSourceChars  = 500
	batchMaxWordChars    = 100
	batchMaxGlossCalls   = 10
	batchGlossTimeout    = 30 * time.Second
)

type BatchItemInput struct {
	ID             string `json:"id"`
	SurfaceText    string `json:"surfaceText"`
	NormalizedText string `json:"normalizedText"`
	Start          int    `json:"start"`
	End            int    `json:"end"`
	// Optional per-item content supplied by the user in the batch UI.
	// A user-provided definition takes precedence over dictionary and AI gloss.
	IPA            string `json:"ipa,omitempty"`
	Definition     string `json:"definition,omitempty"`
	ContextMeaning string `json:"contextMeaning,omitempty"`
}

type BatchRequest struct {
	RequestID   string           `json:"requestId"`
	ContextText string           `json:"contextText"`
	Source      string           `json:"source"`
	Items       []BatchItemInput `json:"items"`
}

type BatchItemResult struct {
	ID             string `json:"id"`
	NormalizedText string `json:"normalizedText"`
	Status         string `json:"status"` // created | duplicate | error
	CardID         string `json:"cardId,omitempty"`
	Word           string `json:"word,omitempty"`
	Error          string `json:"error,omitempty"`
}

type BatchResponse struct {
	RequestID string            `json:"requestId"`
	Results   []BatchItemResult `json:"results"`
	Created   int               `json:"created"`
	Duplicate int               `json:"duplicate"`
	Failed    int               `json:"failed"`
}

func (s *server) handleCardsBatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var req BatchRequest
	if !decodeJSON(w, r, &req, 1<<20) {
		return
	}
	if err := validateBatchRequest(req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Idempotency: replays of the same request return the stored response
	// instead of creating a second set of cards.
	if cached, ok, err := s.store.loadBatchResponse(r.Context(), req.RequestID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	} else if ok {
		writeJSON(w, http.StatusOK, cached)
		return
	}
	response := s.processBatch(r, req)
	if err := s.store.saveBatchResponse(r.Context(), req.RequestID, response); err != nil {
		// Cards were already created; report the results but flag the storage failure.
		response.Failed++
		response.Results = append(response.Results, BatchItemResult{ID: "batch-record", NormalizedText: "", Status: "error", Error: "batch result could not be recorded; retrying this request may create duplicates: " + err.Error()})
	}
	writeJSON(w, http.StatusOK, response)
}

func validateBatchRequest(req BatchRequest) error {
	if strings.TrimSpace(req.RequestID) == "" || len(req.RequestID) > 100 {
		return errors.New("requestId is required (max 100 characters)")
	}
	if len(req.ContextText) > batchMaxContextChars {
		return fmt.Errorf("contextText must contain at most %d characters", batchMaxContextChars)
	}
	if len(req.Source) > batchMaxSourceChars {
		return fmt.Errorf("source must contain at most %d characters", batchMaxSourceChars)
	}
	if len(req.Items) == 0 {
		return errors.New("items must contain at least one entry")
	}
	if len(req.Items) > batchMaxItems {
		return fmt.Errorf("items must contain at most %d entries", batchMaxItems)
	}
	for _, item := range req.Items {
		if len(item.NormalizedText) == 0 || len(item.NormalizedText) > batchMaxWordChars {
			return errors.New("each item needs a normalizedText of 1-100 characters")
		}
		if strings.TrimSpace(item.SurfaceText) == "" {
			return errors.New("each item needs a surfaceText")
		}
		if item.Start < 0 || item.End < item.Start {
			return errors.New("item offsets must satisfy 0 <= start <= end")
		}
		if len(item.IPA) > 200 || len(item.Definition) > 5000 || len(item.ContextMeaning) > 5000 {
			return errors.New("one or more item fields are too long")
		}
	}
	return nil
}

func (s *server) processBatch(r *http.Request, req BatchRequest) BatchResponse {
	ctx := r.Context()
	response := BatchResponse{RequestID: req.RequestID, Results: make([]BatchItemResult, 0, len(req.Items))}
	contextText := strings.TrimSpace(req.ContextText)
	source := strings.TrimSpace(req.Source)
	seen := map[string]string{} // normalized text -> status of the earlier item in this batch
	aiAvailable := s.aiURL != ""
	glossCalls := 0

	for _, item := range req.Items {
		result := BatchItemResult{ID: item.ID, NormalizedText: item.NormalizedText}
		normalized := normalizeBatchText(item.NormalizedText)
		if normalized == "" {
			result.Status = "error"
			result.Error = "normalized text is empty after cleanup"
			response.Failed++
			response.Results = append(response.Results, result)
			continue
		}
		result.NormalizedText = normalized
		if prior, ok := seen[normalized]; ok {
			result.Status = "duplicate"
			result.CardID = prior
			if prior == "" {
				result.Error = "duplicated within the same batch"
			}
			response.Duplicate++
			response.Results = append(response.Results, result)
			continue
		}
		seen[normalized] = ""

		surface := collapseSpaces(item.SurfaceText)
		existing, found, err := s.store.findCardByWord(ctx, normalized)
		if err != nil {
			result.Status = "error"
			result.Error = err.Error()
			response.Failed++
			response.Results = append(response.Results, result)
			continue
		}
		if found && collapseSpaces(existing.ContextText) == contextText {
			result.Status = "duplicate"
			result.CardID = existing.ID
			result.Word = existing.Word
			seen[normalized] = existing.ID
			response.Duplicate++
			response.Results = append(response.Results, result)
			continue
		}

		input := CardInput{Word: surface, ContextText: contextText, Source: source, IPA: item.IPA, Definition: item.Definition, ContextMeaning: item.ContextMeaning}
		if entry, ok, err := s.store.dictionaryExact(ctx, normalized); err != nil {
			result.Status = "error"
			result.Error = err.Error()
			response.Failed++
			response.Results = append(response.Results, result)
			continue
		} else if ok {
			if input.IPA == "" {
				input.IPA = entry.IPA
			}
			if input.Definition == "" {
				input.Definition = entry.Definition
			}
		}
		// Any word or phrase still missing a definition gets an in-context
		// gloss from the optional AI service. A failure here must not lose the
		// card, so it is tolerated silently.
		if input.Definition == "" && aiAvailable && glossCalls < batchMaxGlossCalls {
			glossCalls++
			glossCtx, cancel := context.WithTimeout(ctx, batchGlossTimeout)
			definition, contextMeaning, err := s.glossWord(glossCtx, surface, contextText)
			cancel()
			if err != nil {
				aiAvailable = false // stop asking for the rest of this batch
			} else {
				input.Definition = definition
				input.ContextMeaning = contextMeaning
			}
		}
		card, err := s.store.createCard(ctx, input, randomID())
		if err != nil {
			result.Status = "error"
			result.Error = err.Error()
			response.Failed++
			response.Results = append(response.Results, result)
			continue
		}
		s.enqueueCardSpeech(card)
		result.Status = "created"
		result.CardID = card.ID
		result.Word = card.Word
		seen[normalized] = card.ID
		response.Created++
		response.Results = append(response.Results, result)
	}
	return response
}

// glossWord calls the optional AI gloss endpoint with any text-AI settings
// saved from the web UI; errors are surfaced so the caller can skip remaining
// AI calls for this batch.
func (s *server) glossWord(ctx context.Context, word, contextText string) (string, string, error) {
	body, err := json.Marshal(map[string]string{"word": word, "contextText": contextText})
	if err != nil {
		return "", "", err
	}
	body, err = s.injectTextAI(ctx, body)
	if err != nil {
		return "", "", err
	}
	payload, _, err := s.callAIBytes(ctx, "/v1/gloss", body)
	if err != nil {
		return "", "", err
	}
	var result struct {
		Definition     string `json:"definition"`
		ContextMeaning string `json:"contextMeaning"`
	}
	if err := json.Unmarshal(payload, &result); err != nil || strings.TrimSpace(result.Definition) == "" {
		return "", "", errors.New("AI gloss returned no definition")
	}
	return strings.TrimSpace(result.Definition), strings.TrimSpace(result.ContextMeaning), nil
}

func normalizeBatchText(value string) string {
	value = strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(value))), " ")
	return strings.Trim(value, "'-")
}

func collapseSpaces(value string) string {
	return strings.Join(strings.Fields(value), " ")
}
