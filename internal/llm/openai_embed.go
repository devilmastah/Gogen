package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// embedHTTP is the client for embeddings requests. The context cancels the
// call; there is no separate short timeout, because a local model can take
// longer than a catalog probe.
var embedHTTP = &http.Client{}

type embeddingResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// EmbeddingEndpoint returns the base URL, key, and the provider's own model
// for name. An empty name is the chat provider (the current profile). ok is
// false when that name is not configured. The key may be empty for a local
// server.
func (p *OpenAIProvider) EmbeddingEndpoint(name string) (baseURL, apiKey, model string, ok bool) {
	if p == nil {
		return "", "", "", false
	}
	p.modelsMu.RLock()
	defer p.modelsMu.RUnlock()
	want := strings.TrimSpace(name)
	if want == "" {
		want = p.currentModelProfile
		if want == "" {
			want = DefaultProfileName
		}
	}
	for _, pr := range p.profiles {
		if pr != nil && pr.name == want {
			return pr.baseURL, pr.apiKey, pr.model, true
		}
	}
	return "", "", "", false
}

// PostEmbeddings calls POST {baseURL}/embeddings. An empty key sends no
// Authorization header. The returned vectors follow the input order.
func PostEmbeddings(ctx context.Context, baseURL, apiKey, model string, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("embedding model is empty")
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("embedding endpoint is empty")
	}
	body, err := json.Marshal(map[string]any{"model": model, "input": texts})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := embedHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("embeddings endpoint returned %s", resp.Status)
	}
	var parsed embeddingResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("embeddings response: %w", err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return nil, fmt.Errorf("embeddings endpoint: %s", parsed.Error.Message)
	}
	if len(parsed.Data) != len(texts) {
		return nil, fmt.Errorf("embeddings response length mismatch")
	}
	sort.SliceStable(parsed.Data, func(i, j int) bool {
		return parsed.Data[i].Index < parsed.Data[j].Index
	})
	out := make([][]float32, len(parsed.Data))
	for i, row := range parsed.Data {
		if row.Index != i {
			return nil, fmt.Errorf("embeddings response indexes do not match the input")
		}
		vec := make([]float32, len(row.Embedding))
		for j, v := range row.Embedding {
			vec[j] = float32(v)
		}
		out[i] = vec
	}
	return out, nil
}

type rerankResponse struct {
	Results []struct {
		Index          int     `json:"index"`
		RelevanceScore float64 `json:"relevance_score"`
	} `json:"results"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// RerankResult is one document from a rerank response, in the order the
// endpoint returned. Index refers to the documents that were sent.
type RerankResult struct {
	Index int
	Score float64
}

// PostRerank calls POST {baseURL}/rerank with the query, the model, and the
// documents. An empty key sends no Authorization header.
func PostRerank(ctx context.Context, baseURL, apiKey, model, query string, documents []string) ([]RerankResult, error) {
	if len(documents) == 0 {
		return nil, nil
	}
	if strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("rerank model is empty")
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("rerank endpoint is empty")
	}
	body, err := json.Marshal(map[string]any{
		"model":     model,
		"query":     query,
		"documents": documents,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/rerank", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := embedHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("rerank endpoint returned %s", resp.Status)
	}
	var parsed rerankResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("rerank response: %w", err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return nil, fmt.Errorf("rerank endpoint: %s", parsed.Error.Message)
	}
	if len(parsed.Results) == 0 {
		return nil, fmt.Errorf("rerank response could not be read")
	}
	out := make([]RerankResult, len(parsed.Results))
	for i, row := range parsed.Results {
		out[i] = RerankResult{Index: row.Index, Score: row.RelevanceScore}
	}
	return out, nil
}
