package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExtractHTML(t *testing.T) {
	html := `<html><head><title>Hello</title><script>bad()</script></head><body><main><h1>Title</h1><p>Readable text.</p></main></body></html>`
	title, text, err := ExtractHTML(strings.NewReader(html), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if title != "Hello" || text != "Title Readable text." {
		t.Fatalf("title=%q text=%q", title, text)
	}
}

func TestFetcherUsesNetworkPolicyAndExtractsPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<html><title>Fetch</title><body><article>Fetched body</article></body></html>`))
	}))
	defer server.Close()

	blocked := NewFetcher(server.Client(), DefaultNetworkPolicy())
	if _, err := blocked.Fetch(context.Background(), server.URL); err == nil {
		t.Fatal("expected localhost fetch to be blocked by default policy")
	}

	fetcher := NewFetcher(server.Client(), NetworkPolicy{AllowPrivateNetworks: true})
	page, err := fetcher.Fetch(context.Background(), server.URL)
	if err != nil {
		t.Fatal(err)
	}
	if page.Title != "Fetch" || page.Text != "Fetched body" {
		t.Fatalf("page = %+v", page)
	}
}

func TestSearxngSearcherParsesResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" || r.URL.Query().Get("format") != "json" {
			http.Error(w, "unexpected request: "+r.URL.String(), http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"title":"A","url":"https://example.com/a","content":"alpha"},{"title":"B","url":"https://example.com/b","content":"beta"}]}`))
	}))
	defer server.Close()

	results, err := (SearxngSearcher{URL: server.URL, Client: server.Client()}).Search(context.Background(), "query", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Title != "A" || results[0].Snippet != "alpha" {
		t.Fatalf("results = %+v", results)
	}
}
