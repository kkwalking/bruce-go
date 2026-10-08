package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"bruce-go/internal/config"
)

// DiscoveredModel is one entry of a provider's model list, with the capability
// hints the endpoint chooses to publish.
type DiscoveredModel struct {
	ID string
	// ContextWindow and MaxOutputTokens are zero when the endpoint does not
	// publish them; a gateway is free to omit either.
	ContextWindow   int
	MaxOutputTokens int
}

// DiscoverModels asks a provider which models it serves, which doubles as the
// connectivity test for an API key and base URL.
//
// It is deliberately tolerant about response shape: gateways in the wild return
// the OpenAI list shape with varying extra fields, and the Anthropic list shape
// differs only in which names the capability hints use. Anything that cannot be
// read as a model list is reported as an error so the caller can fall back to
// entering model names by hand.
func DiscoverModels(ctx context.Context, protocol, baseURL, apiKey string) ([]DiscoveredModel, error) {
	endpoint, headers, err := discoveryRequest(protocol, baseURL, apiKey)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(response.Body)
		return nil, &APIError{Provider: protocol, StatusCode: response.StatusCode, Status: response.Status, Body: strings.TrimSpace(string(body))}
	}
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	return parseModelList(data)
}

// discoveryRequest builds the model-list endpoint and its authentication
// headers. Each protocol authenticates differently, so the header set is not
// interchangeable.
func discoveryRequest(protocol, baseURL, apiKey string) (string, map[string]string, error) {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return "", nil, errors.New("a base URL is required to list models")
	}
	headers := map[string]string{"Accept": "application/json"}
	switch protocol {
	case config.ProtocolAnthropic:
		headers["x-api-key"] = apiKey
		headers["anthropic-version"] = anthropicVersion
		return modelsURL(base, "/v1"), headers, nil
	default:
		headers["Authorization"] = "Bearer " + apiKey
		return modelsURL(base, ""), headers, nil
	}
}

// modelsURL joins a base URL with the model-list path. A base that already
// names the endpoint is used as-is; an Anthropic base carries its version
// prefix, because that API is rooted at /v1 while OpenAI-compatible gateways
// usually include the prefix in the configured base URL.
func modelsURL(base, versionPrefix string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if strings.HasSuffix(base, "/models") {
		return base
	}
	if versionPrefix != "" && !strings.HasSuffix(base, versionPrefix) {
		return base + versionPrefix + "/models"
	}
	return base + "/models"
}

// parseModelList reads the {data:[...]} shape both APIs return.
func parseModelList(data []byte) ([]DiscoveredModel, error) {
	var root struct {
		Data []struct {
			ID string `json:"id"`
			// The capability hints are published under two spellings: a
			// gateway uses context_window/max_output_tokens, while the
			// Anthropic API uses max_input_tokens/max_tokens.
			ContextWindow   int `json:"context_window"`
			ContextLength   int `json:"context_length"`
			MaxOutputTokens int `json:"max_output_tokens"`
			MaxInputTokens  int `json:"max_input_tokens"`
			MaxTokens       int `json:"max_tokens"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	if len(root.Data) == 0 {
		return nil, errors.New("the endpoint returned no models")
	}
	models := make([]DiscoveredModel, 0, len(root.Data))
	seen := make(map[string]bool, len(root.Data))
	for _, entry := range root.Data {
		id := strings.TrimSpace(entry.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		models = append(models, DiscoveredModel{
			ID:              id,
			ContextWindow:   firstPositive(entry.ContextWindow, entry.ContextLength, entry.MaxInputTokens),
			MaxOutputTokens: firstPositive(entry.MaxOutputTokens, entry.MaxTokens),
		})
	}
	if len(models) == 0 {
		return nil, errors.New("the endpoint returned no usable model identifiers")
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models, nil
}

func firstPositive(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

// ModelCapabilities converts discovered hints into the settings shape, omitting
// models whose endpoint published no capability at all.
func ModelCapabilities(models []DiscoveredModel) map[string]config.ModelCapability {
	capabilities := make(map[string]config.ModelCapability)
	for _, model := range models {
		if model.ContextWindow == 0 && model.MaxOutputTokens == 0 {
			continue
		}
		capabilities[model.ID] = config.ModelCapability{
			ContextWindow:   model.ContextWindow,
			MaxOutputTokens: model.MaxOutputTokens,
		}
	}
	return capabilities
}

// ModelIDs returns just the identifiers, in the order given.
func ModelIDs(models []DiscoveredModel) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	return ids
}
