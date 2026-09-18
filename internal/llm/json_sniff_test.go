package llm

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openai/openai-go/v3"
)

// modelListJSON is a minimal OpenAI-shaped catalog payload.
const modelListJSON = `{"object":"list","data":[{"id":"model-a","object":"model"}]}`

// TestListModelsToleratesNonJSONContentType pins the reported failure: a
// server that answers GET /models with a valid JSON body under the wrong
// media type used to fail the whole catalog fetch with openai-go's opaque
// "expected destination type of 'string' or '[]byte' for responses with
// content-type ... that is not 'application/json'". A Go backend that writes
// the body without setting Content-Type gets exactly that
// "text/plain; charset=utf-8" header from net/http's content sniffing, so
// /models in the TUI showed the error instead of the model picker.
func TestListModelsToleratesNonJSONContentType(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
	}{
		{name: "text/plain from Go content sniffing", contentType: "text/plain; charset=utf-8"},
		{name: "text/html", contentType: "text/html"},
		{name: "empty", contentType: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/models" {
					http.NotFound(w, r)
					return
				}
				if tt.contentType != "" {
					w.Header().Set("Content-Type", tt.contentType)
				}
				_, _ = w.Write([]byte(modelListJSON))
			}))
			defer srv.Close()

			models, err := listModelsFromServer(t, srv.URL)
			if err != nil {
				t.Fatalf("listModels: %v", err)
			}
			if len(models) != 1 || models[0].model.ID != "model-a" {
				t.Fatalf("unexpected models: %+v", models)
			}
		})
	}
}

// TestListModelsNonJSONBodyStillErrors pins that the normalization is
// conditional: a 200 whose body is genuinely not JSON (an HTML portal page,
// a proxy notice) is passed through and still fails the fetch rather than
// being silently coerced into an empty catalog.
func TestListModelsNonJSONBodyStillErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("upstream gateway error"))
	}))
	defer srv.Close()

	models, err := listModelsFromServer(t, srv.URL)
	if err == nil {
		t.Fatalf("expected an error for a non-JSON body, got models %+v", models)
	}
}

// TestListModelsKeepsJSONContentTypeResponseWorking guards the sniffer
// against breaking the ordinary path.
func TestListModelsKeepsJSONContentTypeResponseWorking(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   []map[string]any{{"id": "model-a", "object": "model"}},
		})
	}))
	defer srv.Close()

	models, err := listModelsFromServer(t, srv.URL)
	if err != nil {
		t.Fatalf("listModels: %v", err)
	}
	if len(models) != 1 || models[0].model.ID != "model-a" {
		t.Fatalf("unexpected models: %+v", models)
	}
}

// TestListModelsToleratesUnrequestedGzip pins the second quirk of the same
// class of endpoint (ai.h-bomb.nl): the catalog body is gzipped even though
// the client never advertised Accept-Encoding, and served under a non-JSON
// media type. The catalog client must advertise gzip (so the standard library
// decodes it) and normalize the media type, otherwise the fetch fails with
// openai-go's content-type error.
func TestListModelsToleratesUnrequestedGzip(t *testing.T) {
	payload, err := json.Marshal(map[string]any{
		"object": "list",
		"data":   []map[string]any{{"id": "model-a", "object": "model"}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var sawAcceptEncoding string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAcceptEncoding = r.Header.Get("Accept-Encoding")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		gz := gzip.NewWriter(w)
		_, _ = gz.Write(payload)
		_ = gz.Close()
	}))
	defer srv.Close()

	models, err := listModelsFromServer(t, srv.URL)
	if err != nil {
		t.Fatalf("listModels: %v (Accept-Encoding sent: %q)", err, sawAcceptEncoding)
	}
	if len(models) != 1 || models[0].model.ID != "model-a" {
		t.Fatalf("unexpected models: %+v", models)
	}
	if !strings.Contains(sawAcceptEncoding, "gzip") {
		t.Fatalf("Accept-Encoding = %q, want gzip advertised so the transport decodes it", sawAcceptEncoding)
	}
}

// listModelsFromServer builds a provider the way production does (through
// newClientPair, so the catalog client carries the real transport stack) and
// lists its models.
func listModelsFromServer(t *testing.T, baseURL string) ([]catalogModel, error) {
	t.Helper()
	stream, catalog := newClientPair(baseURL+"/", "test", nil)
	p := &OpenAIProvider{
		profiles:    []*providerProfile{{name: "default", stream: stream, catalog: catalog}},
		modelClient: make(map[ModelRef]*openai.Client),
	}
	return p.listModels(context.Background())
}
