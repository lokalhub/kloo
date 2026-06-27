package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
)

const modelsPath = "/models"

type modelsResponse struct {
	Data []modelData `json:"data"`
}

type modelData struct {
	ID            string `json:"id"`
	ContextLength int    `json:"context_length"`
	TopProvider   struct {
		ContextLength int `json:"context_length"`
	} `json:"top_provider"`
}

// Models fetches the OpenAI-compatible /models listing for the configured
// endpoint. It supports both OpenAI-style context_length and OpenRouter's
// top_provider.context_length fallback.
func (c *Client) Models(ctx context.Context) ([]ModelInfo, error) {
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+modelsPath, nil)
	if err != nil {
		return nil, fmt.Errorf("llm: build models request: %w", err)
	}
	httpReq.Header.Set("Accept", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("llm: request %s: %w", c.endpoint+modelsPath, err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return nil, &APIError{
			StatusCode: httpResp.StatusCode,
			Status:     httpResp.Status,
			Body:       readAPIErrorBody(httpResp.Body, c.apiKey),
		}
	}
	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("llm: read models response body: %w", err)
	}

	var parsed modelsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("llm: decode models response: %w", err)
	}

	models := make([]ModelInfo, 0, len(parsed.Data))
	for _, item := range parsed.Data {
		contextLength := item.ContextLength
		if contextLength == 0 {
			contextLength = item.TopProvider.ContextLength
		}
		models = append(models, ModelInfo{
			ID:            item.ID,
			ContextLength: contextLength,
		})
	}
	sort.Slice(models, func(i, j int) bool {
		return models[i].ID < models[j].ID
	})
	return models, nil
}
