package reviewengine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

type LLMClient interface {
	ChatCompletion(ctx context.Context, req ChatRequest) (string, error)
}

type ChatRequest struct {
	BaseURL     string
	APIKey      string
	Model       string
	Temperature float64
	System      string
	User        string
}

type OpenAICompatClient struct {
	HTTP *http.Client
}

func NewOpenAICompatClient() *OpenAICompatClient {
	return &OpenAICompatClient{
		HTTP: &http.Client{Timeout: 90 * time.Second},
	}
}

func (c *OpenAICompatClient) ChatCompletion(ctx context.Context, req ChatRequest) (string, error) {
	payload := map[string]any{
		"model": req.Model,
		"messages": []map[string]string{
			{"role": "system", "content": req.System},
			{"role": "user", "content": req.User},
		},
		"temperature": req.Temperature,
	}
	body, _ := json.Marshal(payload)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, req.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if req.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+req.APIKey)
	}

	res, err := c.HTTP.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	var decoded struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(res.Body).Decode(&decoded); err != nil {
		return "", fmt.Errorf("decode llm response: %w", err)
	}
	if len(decoded.Choices) == 0 {
		return "", fmt.Errorf("llm response has no choices")
	}
	return decoded.Choices[0].Message.Content, nil
}

