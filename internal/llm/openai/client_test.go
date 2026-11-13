package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pulseaiclub/phi/internal/llm"
)

func TestBuildRequestImages(t *testing.T) {
	cfg := llm.ModelConfig{Name: "gpt-4o", APIKey: "k", BaseURL: "https://api.openai.com/v1"}
	req := BuildRequest(cfg, "sys", []llm.Message{
		{
			Role:    llm.RoleUser,
			Content: "what is this?",
			Images: []llm.Image{
				{Data: "QUJD", MimeType: "image/png"},
			},
		},
	}, nil)

	require.Len(t, req.Messages, 2) // system + user
	parts, ok := req.Messages[1].Content.([]any)
	require.True(t, ok, "expected content parts array with images, got %T", req.Messages[1].Content)
	require.Len(t, parts, 2)

	text := parts[0].(map[string]string)
	assert.Equal(t, "text", text["type"])
	assert.Equal(t, "what is this?", text["text"])

	img := parts[1].(map[string]any)
	assert.Equal(t, "image_url", img["type"])
	imageURL := img["image_url"].(map[string]string)
	assert.Equal(t, "data:image/png;base64,QUJD", imageURL["url"])

	body, err := json.Marshal(req)
	require.NoError(t, err)
	assert.Contains(t, string(body), `"image_url"`)
	assert.NotContains(t, string(body), `"images"`)
}

func TestBuildRequestNoImagesKeepsStringContent(t *testing.T) {
	cfg := llm.ModelConfig{Name: "gpt-4o", APIKey: "k", BaseURL: "https://api.openai.com/v1"}
	req := BuildRequest(cfg, "", []llm.Message{{Role: llm.RoleUser, Content: "hi"}}, nil)

	body, err := json.Marshal(req)
	require.NoError(t, err)
	// content must stay a JSON string, not an array, when there are no images.
	var raw struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(body, &raw))
	require.Len(t, raw.Messages, 1)
	assert.Equal(t, `"hi"`, string(raw.Messages[0].Content))
}

// prompt_tokens is the whole prompt, so the parser splits cache reads and
// writes out of it. Vendors disagree on which field carries the hits.
func TestNormalizeUsageSplitsCacheBuckets(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		wire usageWire
		want llm.Usage
	}{
		"openai details": {
			wire: usageWire{
				PromptTokens: 10, CompletionTokens: 5,
				PromptTokensDetails: &usageWireDetails{CachedTokens: 4},
			},
			// 10 - 4 cached, plus the 5-token reply.
			want: llm.Usage{
				PromptTokens: 6, CompletionTokens: 5, TotalTokens: 15,
				PromptTokensDetails: &llm.PromptTokensDetails{CachedTokens: 4},
			},
		},
		"deepseek hit field": {
			wire: usageWire{PromptTokens: 100, CompletionTokens: 20, PromptCacheHitTokens: 80},
			want: llm.Usage{
				PromptTokens: 20, CompletionTokens: 20, TotalTokens: 120,
				PromptTokensDetails: &llm.PromptTokensDetails{CachedTokens: 80},
			},
		},
		"cache write": {
			wire: usageWire{
				PromptTokens: 100, CompletionTokens: 10,
				PromptTokensDetails: &usageWireDetails{CachedTokens: 30, CacheWriteTokens: 20},
			},
			want: llm.Usage{
				PromptTokens: 50, CompletionTokens: 10, TotalTokens: 110,
				PromptTokensDetails: &llm.PromptTokensDetails{CachedTokens: 30, CacheWriteTokens: 20},
			},
		},
		"hit field and details agree": {
			wire: usageWire{
				PromptTokens: 100, CompletionTokens: 1, PromptCacheHitTokens: 60,
				PromptTokensDetails: &usageWireDetails{CachedTokens: 60},
			},
			want: llm.Usage{
				PromptTokens: 40, CompletionTokens: 1, TotalTokens: 101,
				PromptTokensDetails: &llm.PromptTokensDetails{CachedTokens: 60},
			},
		},
		"cache larger than prompt clamps at zero": {
			wire: usageWire{
				PromptTokens: 3, CompletionTokens: 1,
				PromptTokensDetails: &usageWireDetails{CachedTokens: 9},
			},
			want: llm.Usage{
				PromptTokens: 0, CompletionTokens: 1, TotalTokens: 10,
				PromptTokensDetails: &llm.PromptTokensDetails{CachedTokens: 9},
			},
		},
		"no usage": {wire: usageWire{}, want: llm.Usage{}},
	}
	for name, tt := range tests {
		assert.Equal(t, tt.want, normalizeUsage(tt.wire), name)
	}
}

func TestBuildRequestDoesNotInferDeepSeekThinking(t *testing.T) {
	cfg := llm.ModelConfig{Name: "deepseek-flash", Think: llm.ThinkConfig{Enabled: true, Mode: llm.High}}
	req := BuildRequest(cfg, "", []llm.Message{{Role: llm.RoleUser, Content: "hi"}}, nil)
	assert.Nil(t, req.Thinking)
	assert.Equal(t, "high", req.ReasoningEffort)
}

func TestCompactRequestCarriesUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"role":"assistant","content":"summary"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":12,"completion_tokens":8,"total_tokens":20,
				"prompt_tokens_details":{"cached_tokens":2}}
		}`))
	}))
	defer srv.Close()

	cfg := llm.ModelConfig{Name: "gpt-4o", BaseURL: srv.URL, APIKey: "k"}
	res, err := CompactRequest(
		context.Background(), srv.Client(), cfg, NewCompactRequest(cfg, "sum", 100),
	)
	require.NoError(t, err)
	assert.True(t, res.UsageReported())
	assert.Equal(t, 8, res.Usage.CompletionTokens)
	assert.Equal(t, 10, res.Usage.PromptTokens, "uncached prompt (12 - 2 cached)")
	assert.Equal(t, 2, res.Usage.CachedTokens())
	assert.Equal(t, 20, res.Usage.TotalTokens)
}

func TestCompactRequestWithoutUsageStaysZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"summary"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	cfg := llm.ModelConfig{Name: "gpt-4o", BaseURL: srv.URL, APIKey: "k"}
	res, err := CompactRequest(
		context.Background(), srv.Client(), cfg, NewCompactRequest(cfg, "sum", 100),
	)
	require.NoError(t, err)
	assert.False(t, res.UsageReported())
}
