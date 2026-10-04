package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// Settings stored in SQLite override the AI_* env vars once the user saves
// them from the web UI. An absent "text_ai_provider" row keeps the env-driven
// behavior for deployments that never touch the settings page.
const (
	settingTextAIProvider = "text_ai_provider" // "" | disabled | openai-compatible
	settingTextAIBaseURL  = "text_ai_base_url"
	settingTextAIModel    = "text_ai_model"
	settingTextAIAPIKey   = "text_ai_api_key"

	settingPronProvider    = "pron_provider" // "" | local | azure
	settingPronAzureRegion = "pron_azure_region"
	settingPronAzureKey    = "pron_azure_key"
)

type textAISettings struct {
	Provider string
	BaseURL  string
	Model    string
	APIKey   string
}

func (s *server) loadTextAI(ctx context.Context) *textAISettings {
	values, err := s.store.allSettings(ctx)
	if err != nil || values[settingTextAIProvider] == "" {
		return nil
	}
	return &textAISettings{
		Provider: values[settingTextAIProvider],
		BaseURL:  values[settingTextAIBaseURL],
		Model:    values[settingTextAIModel],
		APIKey:   values[settingTextAIAPIKey],
	}
}

// pronunciationSettings selects the assessment backend: an unset or "local"
// provider keeps the self-hosted ASR similarity flow; "azure" sends the
// recording to Azure Speech for phoneme-level scoring.
type pronunciationSettings struct {
	Provider    string
	AzureRegion string
	AzureKey    string
}

func (s *server) loadPronunciation(ctx context.Context) *pronunciationSettings {
	values, err := s.store.allSettings(ctx)
	if err != nil || values[settingPronProvider] == "" {
		return nil
	}
	return &pronunciationSettings{
		Provider:    values[settingPronProvider],
		AzureRegion: values[settingPronAzureRegion],
		AzureKey:    values[settingPronAzureKey],
	}
}

func (s *server) handleSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.writeSettingsState(w, r)
	case http.MethodPut:
		var input struct {
			Provider    *string `json:"provider"`
			BaseURL     *string `json:"baseUrl"`
			Model       *string `json:"model"`
			APIKey      *string `json:"apiKey"`
			ClearAPIKey bool    `json:"clearApiKey"`

			PronProvider    *string `json:"pronProvider"`
			PronAzureRegion *string `json:"pronAzureRegion"`
			PronAzureAPIKey *string `json:"pronAzureApiKey"`
			PronClearKey    bool    `json:"pronClearAzureKey"`
		}
		if !decodeJSON(w, r, &input, 16<<10) {
			return
		}
		current, err := s.store.allSettings(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		values := current
		if values == nil {
			values = map[string]string{}
		}
		set := func(input *string, key string) {
			if input != nil {
				values[key] = strings.TrimSpace(*input)
			}
		}
		set(input.Provider, settingTextAIProvider)
		set(input.BaseURL, settingTextAIBaseURL)
		set(input.Model, settingTextAIModel)
		set(input.PronProvider, settingPronProvider)
		set(input.PronAzureRegion, settingPronAzureRegion)
		values[settingPronAzureRegion] = strings.ToLower(values[settingPronAzureRegion])

		provider := values[settingTextAIProvider]
		if provider != "" && provider != "disabled" && provider != "openai-compatible" {
			writeError(w, http.StatusBadRequest, "provider must be empty, disabled or openai-compatible")
			return
		}
		baseURL := strings.TrimRight(values[settingTextAIBaseURL], "/")
		model := values[settingTextAIModel]
		if provider == "openai-compatible" {
			if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
				writeError(w, http.StatusBadRequest, "baseUrl must start with http:// or https://")
				return
			}
			if model == "" {
				writeError(w, http.StatusBadRequest, "model is required for openai-compatible")
				return
			}
		}
		pronProvider := values[settingPronProvider]
		if pronProvider != "" && pronProvider != "local" && pronProvider != "azure" {
			writeError(w, http.StatusBadRequest, "pronProvider must be empty, local or azure")
			return
		}
		region := values[settingPronAzureRegion]
		if pronProvider == "azure" && !validAzureRegion(region) {
			writeError(w, http.StatusBadRequest, "pronAzureRegion must be a valid Azure region such as eastasia")
			return
		}
		if input.PronClearKey {
			values[settingPronAzureKey] = ""
		} else if input.PronAzureAPIKey != nil && strings.TrimSpace(*input.PronAzureAPIKey) != "" {
			values[settingPronAzureKey] = strings.TrimSpace(*input.PronAzureAPIKey)
		}
		if pronProvider == "azure" && values[settingPronAzureKey] == "" {
			writeError(w, http.StatusBadRequest, "pronAzureApiKey is required for the azure pronunciation provider")
			return
		}
		values[settingTextAIBaseURL] = baseURL
		if input.ClearAPIKey {
			values[settingTextAIAPIKey] = ""
		} else if input.APIKey != nil && strings.TrimSpace(*input.APIKey) != "" {
			values[settingTextAIAPIKey] = strings.TrimSpace(*input.APIKey)
		}
		if err := s.store.putSettings(r.Context(), values); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.writeSettingsState(w, r)
	default:
		methodNotAllowed(w)
	}
}

func validAzureRegion(region string) bool {
	if len(region) < 2 || len(region) > 40 {
		return false
	}
	for _, char := range region {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-' {
			continue
		}
		return false
	}
	return true
}

// writeSettingsState reports the saved text-AI and pronunciation settings.
// API keys never leave the server; the UI only receives whether one exists
// plus a masked hint.
func (s *server) writeSettingsState(w http.ResponseWriter, r *http.Request) {
	response := map[string]any{"provider": "", "baseUrl": "", "model": "", "hasKey": false, "keyHint": ""}
	if cfg := s.loadTextAI(r.Context()); cfg != nil {
		response["provider"] = cfg.Provider
		response["baseUrl"] = cfg.BaseURL
		response["model"] = cfg.Model
		response["hasKey"] = cfg.APIKey != ""
		if len(cfg.APIKey) > 4 {
			response["keyHint"] = "••••" + cfg.APIKey[len(cfg.APIKey)-4:]
		}
	}
	pron := map[string]any{"provider": "", "azureRegion": "", "hasKey": false, "keyHint": ""}
	if cfg := s.loadPronunciation(r.Context()); cfg != nil {
		pron["provider"] = cfg.Provider
		pron["azureRegion"] = cfg.AzureRegion
		pron["hasKey"] = cfg.AzureKey != ""
		if len(cfg.AzureKey) > 4 {
			pron["keyHint"] = "••••" + cfg.AzureKey[len(cfg.AzureKey)-4:]
		}
	}
	response["pronunciation"] = pron
	writeJSON(w, http.StatusOK, response)
}

// injectTextAI merges the saved web-UI settings into a gloss request body so
// the AI service can override its process env for this call. The key travels
// only on the internal compose network between the Go API and the AI service.
func (s *server) injectTextAI(ctx context.Context, payload []byte) ([]byte, error) {
	cfg := s.loadTextAI(ctx)
	if cfg == nil {
		return payload, nil
	}
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		return payload, nil
	}
	body["textAI"] = map[string]string{"provider": cfg.Provider, "baseUrl": cfg.BaseURL, "model": cfg.Model, "apiKey": cfg.APIKey}
	return json.Marshal(body)
}
