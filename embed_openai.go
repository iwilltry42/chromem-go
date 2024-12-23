package chromem

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const BaseURLOpenAI = "https://api.openai.com/v1"

type EmbeddingModelOpenAI string

const (
	EmbeddingModelOpenAI2Ada EmbeddingModelOpenAI = "text-embedding-ada-002"

	EmbeddingModelOpenAI3Small EmbeddingModelOpenAI = "text-embedding-3-small"
	EmbeddingModelOpenAI3Large EmbeddingModelOpenAI = "text-embedding-3-large"
)

type OpenAIResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// NewEmbeddingFuncDefault returns a function that creates embeddings for a text
// using OpenAI`s "text-embedding-3-small" model via their API.
// The model supports a maximum text length of 8191 tokens.
// The API key is read from the environment variable "OPENAI_API_KEY".
func NewEmbeddingFuncDefault() EmbeddingFunc {
	apiKey := os.Getenv("OPENAI_API_KEY")
	return NewEmbeddingFuncOpenAI(apiKey, EmbeddingModelOpenAI3Small)
}

// NewEmbeddingFuncOpenAI returns a function that creates embeddings for a text
// using the OpenAI API.
func NewEmbeddingFuncOpenAI(apiKey string, model EmbeddingModelOpenAI) EmbeddingFunc {
	// OpenAI embeddings are normalized
	return NewEmbeddingFuncOpenAICompat(NewOpenAICompatConfig(BaseURLOpenAI, apiKey, string(model)).WithNormalized(true))
}

// NewEmbeddingFuncOpenAICompat returns a function that creates embeddings for a text
// using an OpenAI compatible API. For example:
//   - Azure OpenAI: https://azure.microsoft.com/en-us/products/ai-services/openai-service
//   - LitLLM: https://github.com/BerriAI/litellm
//   - Ollama: https://github.com/ollama/ollama/blob/main/docs/openai.md
//   - etc.
//
// It offers options to set request headers and query parameters
// e.g. to pass the `api-key` header and the `api-version` query parameter for Azure OpenAI.
//
// The `normalized` parameter indicates whether the vectors returned by the embedding
// model are already normalized, as is the case for OpenAI's and Mistral's models.
// The flag is optional. If it's nil, it will be autodetected on the first request
// (which bears a small risk that the vector just happens to have a length of 1).
func NewEmbeddingFuncOpenAICompat(config *OpenAICompatConfig) EmbeddingFunc {
	if config == nil {
		panic("config must not be nil")
	}

	// We don't set a default timeout here, although it's usually a good idea.
	// In our case though, the library user can set the timeout on the context,
	// and it might have to be a long timeout, depending on the text length.
	client := &http.Client{
		Timeout: 120 * time.Second,
	}

	var checkedNormalized bool
	checkNormalized := sync.Once{}

	return func(ctx context.Context, text string) ([]float32, error) {
		// Prepare the request body.
		reqBody, err := json.Marshal(map[string]string{
			"input": text,
			"model": config.model,
		})
		if err != nil {
			return nil, fmt.Errorf("couldn't marshal request body: %w", err)
		}

		fullURL, err := url.JoinPath(config.baseURL, config.embeddingsEndpoint)
		if err != nil {
			return nil, fmt.Errorf("couldn't join base URL and endpoint: %w", err)
		}

		// Create the request. Creating it with context is important for a timeout
		// to be possible, because the client is configured without a timeout.
		req, err := http.NewRequestWithContext(ctx, "POST", fullURL, bytes.NewBuffer(reqBody))
		if err != nil {
			return nil, fmt.Errorf("couldn't create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+config.apiKey)

		// Add headers
		for k, v := range config.headers {
			req.Header.Add(k, v)
		}

		// Add query parameters
		q := req.URL.Query()
		for k, v := range config.queryParams {
			q.Add(k, v)
		}
		req.URL.RawQuery = q.Encode()

		// Send the request and get the body.
		body, err := requestWithExponentialBackoff(ctx, client, req, 5, true)
		if err != nil {
			return nil, fmt.Errorf("error sending request(s): %w", err)
		}

		var embeddingResponse OpenAIResponse
		err = json.Unmarshal(body, &embeddingResponse)
		if err != nil {
			return nil, fmt.Errorf("couldn't unmarshal response body: %w", err)
		}

		// Check if the response contains embeddings.
		if len(embeddingResponse.Data) == 0 || len(embeddingResponse.Data[0].Embedding) == 0 {
			return nil, errors.New("no embeddings found in the response")
		}

		v := embeddingResponse.Data[0].Embedding
		if config.normalized != nil {
			if *config.normalized {
				return v, nil
			}
			return NormalizeVector(v), nil
		}
		checkNormalized.Do(func() {
			if IsNormalized(v) {
				checkedNormalized = true
			} else {
				checkedNormalized = false
			}
		})
		if !checkedNormalized {
			v = NormalizeVector(v)
		}

		return v, nil
	}
}

type OpenAICompatConfig struct {
	baseURL string
	apiKey  string
	model   string

	// Optional
	normalized         *bool
	embeddingsEndpoint string
	headers            map[string]string
	queryParams        map[string]string
}

func NewOpenAICompatConfig(baseURL, apiKey, model string) *OpenAICompatConfig {
	return &OpenAICompatConfig{
		baseURL: baseURL,
		apiKey:  apiKey,
		model:   model,

		embeddingsEndpoint: "/embeddings",
	}
}

func (c *OpenAICompatConfig) WithEmbeddingsEndpoint(endpoint string) *OpenAICompatConfig {
	c.embeddingsEndpoint = endpoint
	return c
}

func (c *OpenAICompatConfig) WithHeaders(headers map[string]string) *OpenAICompatConfig {
	c.headers = headers
	return c
}

func (c *OpenAICompatConfig) WithQueryParams(queryParams map[string]string) *OpenAICompatConfig {
	c.queryParams = queryParams
	return c
}

func (c *OpenAICompatConfig) WithNormalized(normalized bool) *OpenAICompatConfig {
	c.normalized = &normalized
	return c
}

func requestWithExponentialBackoff(ctx context.Context, client *http.Client, req *http.Request, maxRetries int, handleRateLimit bool) ([]byte, error) {

	const baseDelay = time.Millisecond * 200
	var resp *http.Response
	var err error

	var failures []string

	// Save the original request body
	var bodyBytes []byte
	if req.Body != nil {
		bodyBytes, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read request body: %v", err)
		}
	}

	for i := 0; i < maxRetries; i++ {
		// Reset body to the original request body
		if bodyBytes != nil {
			req.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
		}

		resp, err = client.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			if resp.Body == nil {
				return nil, fmt.Errorf("response body is nil")
			}
			defer resp.Body.Close()

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				// Request was OK, but we hit an error reading the response body.
				// This is likely a transient error, so we retry.
				failures = append(failures, fmt.Sprintf("#%d/%d: failed to read response body: %v", i+1, maxRetries, err))
				continue
			}

			return body, nil
		}

		if resp != nil {
			var bodystr string
			if resp.Body != nil {
				body, rerr := io.ReadAll(resp.Body)
				if rerr == nil {
					bodystr = string(body)
				}
				resp.Body.Close()
			}
			failures = append(failures, fmt.Sprintf("#%d/%d: %d <%s> (err: %v)", i+1, maxRetries, resp.StatusCode, bodystr, err))

			if resp.StatusCode >= 500 || (handleRateLimit && resp.StatusCode == http.StatusTooManyRequests) {
				// Retry for 5xx (Server Errors)
				// We're also handling rate limit here (without checking the Retry-After header), if handleRateLimit is true,
				// since it's what e.g. OpenAI recommends (see https://github.com/openai/openai-cookbook/blob/457f4310700f93e7018b1822213ca99c613dbd1b/examples/How_to_handle_rate_limits.ipynb).
				delay := baseDelay * time.Duration(1<<i)
				jitter := time.Duration(rand.Int63n(int64(baseDelay)))
				time.Sleep(delay + jitter)
				continue
			} else {
				// Don't retry for other status codes
				break
			}
		}

	}

	return nil, fmt.Errorf("requesting embeddings - retry limit (%d) exceeded or failed with non-retriable error: %v", maxRetries, strings.Join(failures, "; "))
}
