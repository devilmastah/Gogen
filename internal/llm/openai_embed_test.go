package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPostEmbeddings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("empty key sent an Authorization header")
		}
		var body struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Model != "bge" || len(body.Input) != 2 {
			t.Fatalf("body = %+v", body)
		}
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"index": 1, "embedding": []float64{0, 1}},
			{"index": 0, "embedding": []float64{1, 0}},
		}})
	}))
	defer srv.Close()
	got, err := PostEmbeddings(context.Background(), srv.URL+"/v1", "", "bge", []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0][0] != 1 || got[1][1] != 1 {
		t.Fatalf("vectors = %v", got)
	}
	if _, err := PostEmbeddings(context.Background(), srv.URL, "k", "", []string{"a"}); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty model err = %v", err)
	}
}

func TestPostEmbeddingsRejectsDuplicateIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"index": 0, "embedding": []float64{0, 1}},
			{"index": 0, "embedding": []float64{1, 0}},
		}})
	}))
	defer srv.Close()
	_, err := PostEmbeddings(context.Background(), srv.URL+"/v1", "", "bge", []string{"a", "b"})
	if err == nil || !strings.Contains(err.Error(), "indexes") {
		t.Fatalf("err = %v", err)
	}
}

func TestPostRerank(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/rerank" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer sk" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		var body struct {
			Model     string   `json:"model"`
			Query     string   `json:"query"`
			Documents []string `json:"documents"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Model != "bge" || body.Query != "where" || len(body.Documents) != 2 || body.Documents[0] != "a" {
			t.Fatalf("body = %+v", body)
		}
		json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{
			{"index": 1, "relevance_score": 0.9},
			{"index": 0, "relevance_score": 0.2},
		}})
	}))
	defer srv.Close()
	got, err := PostRerank(context.Background(), srv.URL+"/v1", "sk", "bge", "where", []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Index != 1 || got[0].Score != 0.9 || got[1].Index != 0 {
		t.Fatalf("results = %+v", got)
	}
	if _, err := PostRerank(context.Background(), srv.URL+"/v1", "", "bge", "q", nil); err != nil {
		t.Fatal(err)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{"))
	}))
	defer bad.Close()
	if _, err := PostRerank(context.Background(), bad.URL+"/v1", "", "bge", "q", []string{"a"}); err == nil || !strings.Contains(err.Error(), "rerank response") {
		t.Fatalf("bad body err = %v", err)
	}
}
