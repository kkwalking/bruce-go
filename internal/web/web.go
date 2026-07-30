package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"bruce-go/internal/config"
	"bruce-go/internal/sandbox"
	"bruce-go/internal/tool"
)

const (
	defaultMaxResults = 5
	defaultMaxBytes   = 2 << 20
	defaultMaxText    = 20000
)

type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
	Content string `json:"content,omitempty"`
}

type Page struct {
	URL        string
	FinalURL   string
	StatusCode int
	Title      string
	Text       string
}

type Searcher interface {
	Search(ctx context.Context, query string, maxResults int) ([]Result, error)
}

type Fetcher struct {
	Client   *http.Client
	Policy   NetworkPolicy
	MaxBytes int64
	MaxText  int
}

func NewFetcher(client *http.Client, policy NetworkPolicy) Fetcher {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	return Fetcher{Client: client, Policy: policy, MaxBytes: defaultMaxBytes, MaxText: defaultMaxText}
}

func (f Fetcher) Fetch(ctx context.Context, rawURL string) (Page, error) {
	checked, err := f.Policy.Check(rawURL)
	if err != nil {
		return Page{}, err
	}
	maxBytes := f.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checked, nil)
	if err != nil {
		return Page{}, err
	}
	req.Header.Set("User-Agent", "bruce-go/0.1")
	resp, err := f.client().Do(req)
	if err != nil {
		return Page{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Page{}, fmt.Errorf("web fetch HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return Page{}, err
	}
	if int64(len(data)) > maxBytes {
		return Page{}, errors.New("web fetch 响应过大")
	}
	title, text, err := ExtractHTML(bytes.NewReader(data), f.MaxText)
	if err != nil {
		return Page{}, err
	}
	return Page{URL: checked, FinalURL: resp.Request.URL.String(), StatusCode: resp.StatusCode, Title: title, Text: text}, nil
}

func (f Fetcher) client() *http.Client {
	if f.Client != nil {
		return f.Client
	}
	return &http.Client{Timeout: 20 * time.Second}
}

func ExtractHTML(r io.Reader, maxText int) (title, text string, err error) {
	doc, err := goquery.NewDocumentFromReader(r)
	if err != nil {
		return "", "", err
	}
	doc.Find("script,style,noscript,svg,canvas,template").Each(func(_ int, s *goquery.Selection) {
		s.Remove()
	})
	doc.Find("br,p,h1,h2,h3,h4,h5,h6,li,div,section,article,main").Each(func(_ int, s *goquery.Selection) {
		_ = s.AppendHtml(" ")
	})
	title = cleanSpace(doc.Find("title").First().Text())
	body := doc.Find("main").Text()
	if strings.TrimSpace(body) == "" {
		body = doc.Find("article").Text()
	}
	if strings.TrimSpace(body) == "" {
		body = doc.Find("body").Text()
	}
	text = cleanSpace(body)
	if maxText <= 0 {
		maxText = defaultMaxText
	}
	if len(text) > maxText {
		text = text[:maxText] + "\n... 页面内容过长，已截断 ..."
	}
	return title, text, nil
}

type NetworkPolicy struct {
	AllowPrivateNetworks bool
	AllowedSchemes       []string
}

func DefaultNetworkPolicy() NetworkPolicy {
	return NetworkPolicy{AllowedSchemes: []string{"http", "https"}}
}

func (p NetworkPolicy) Check(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", errors.New("URL 不能为空")
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", errors.New("URL 必须包含 scheme 和 host")
	}
	allowed := p.AllowedSchemes
	if len(allowed) == 0 {
		allowed = []string{"http", "https"}
	}
	ok := false
	for _, scheme := range allowed {
		if strings.EqualFold(u.Scheme, scheme) {
			ok = true
			break
		}
	}
	if !ok {
		return "", errors.New("不允许的 URL scheme: " + u.Scheme)
	}
	if !p.AllowPrivateNetworks {
		host := u.Hostname()
		ips, err := net.LookupIP(host)
		if err != nil {
			if ip := net.ParseIP(host); ip != nil {
				ips = []net.IP{ip}
			}
		}
		for _, ip := range ips {
			if isPrivateIP(ip) {
				return "", errors.New("默认不允许访问本地或内网地址")
			}
		}
		if strings.EqualFold(host, "localhost") {
			return "", errors.New("默认不允许访问 localhost")
		}
	}
	return u.String(), nil
}

func isPrivateIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

type Manager struct {
	Enabled  bool
	Searcher Searcher
	Fetcher  Fetcher
}

func NewManager(settings config.WebSearchSettings, client *http.Client) *Manager {
	searcher := NewSearcher(settings, client)
	return &Manager{Enabled: true, Searcher: searcher, Fetcher: NewFetcher(client, DefaultNetworkPolicy())}
}

func (m *Manager) SetEnabled(enabled bool) {
	m.Enabled = enabled
}

func (m *Manager) Status() string {
	if m == nil {
		return "Web: unavailable"
	}
	state := "off"
	if m.Enabled {
		state = "on"
	}
	provider := "none"
	if m.Searcher != nil {
		provider = fmt.Sprintf("%T", m.Searcher)
	}
	return "Web: " + state + " (" + provider + ")"
}

func (m *Manager) Search(ctx context.Context, query string, maxResults int) ([]Result, error) {
	if m == nil || !m.Enabled {
		return nil, errors.New("WebSearch 已关闭")
	}
	if m.Searcher == nil {
		return nil, errors.New("未配置 WebSearch provider")
	}
	if maxResults <= 0 {
		maxResults = defaultMaxResults
	}
	return m.Searcher.Search(ctx, query, maxResults)
}

func (m *Manager) Fetch(ctx context.Context, rawURL string) (Page, error) {
	if m == nil || !m.Enabled {
		return Page{}, errors.New("WebFetch 已关闭")
	}
	return m.Fetcher.Fetch(ctx, rawURL)
}

func RegisterTools(registry *tool.Registry, manager *Manager) {
	registry.Register(tool.Tool{
		Name:        "web_search",
		Description: "联网搜索并返回标题、URL 和摘要",
		Parameters:  rawSchema("query", "搜索关键词", "max_results", "最大结果数"),
		Exec: func(ctx context.Context, args map[string]string) (string, error) {
			results, err := manager.Search(ctx, args["query"], parsePositive(args["max_results"], defaultMaxResults))
			if err != nil {
				return "", err
			}
			return formatResults(results), nil
		},
		PromptSnippet: "Search the web when local context is insufficient or freshness matters",
		Policy:        tool.Policy{Source: tool.SourceWeb, MinimumMode: sandbox.ModeReadOnly, ParallelSafe: true},
	})
	registry.Register(tool.Tool{
		Name:        "web_fetch",
		Description: "抓取 URL 的网页正文",
		Parameters:  rawSchema("url", "要抓取的 http/https URL"),
		Exec: func(ctx context.Context, args map[string]string) (string, error) {
			page, err := manager.Fetch(ctx, args["url"])
			if err != nil {
				return "", err
			}
			return formatPage(page), nil
		},
		PromptSnippet: "Fetch and extract readable page text from a URL",
		Policy:        tool.Policy{Source: tool.SourceWeb, MinimumMode: sandbox.ModeReadOnly, ParallelSafe: true},
	})
}

type ZhipuSearcher struct {
	APIKey       string
	Endpoint     string
	SearchEngine string
	ContentSize  string
	Client       *http.Client
}

type SerpAPISearcher struct {
	APIKey string
	Client *http.Client
}

type SearxngSearcher struct {
	URL    string
	Client *http.Client
}

func NewSearcher(settings config.WebSearchSettings, client *http.Client) Searcher {
	provider := strings.ToLower(strings.TrimSpace(settings.Provider))
	if provider == "" {
		switch {
		case strings.TrimSpace(settings.Searxng.URL) != "":
			provider = "searxng"
		case strings.TrimSpace(settings.SerpAPI.APIKey) != "":
			provider = "serpapi"
		default:
			provider = "zhipu"
		}
	}
	switch provider {
	case "searx", "searxng":
		if settings.Searxng.URL != "" {
			return SearxngSearcher{URL: settings.Searxng.URL, Client: client}
		}
	case "serpapi":
		if settings.SerpAPI.APIKey != "" {
			return SerpAPISearcher{APIKey: settings.SerpAPI.APIKey, Client: client}
		}
	case "glm", "zhipu", "bigmodel":
		return ZhipuSearcher{
			APIKey:       settings.Zhipu.APIKey,
			Endpoint:     defaultString(settings.Zhipu.Endpoint, "https://open.bigmodel.cn/api/paas/v4/web_search"),
			SearchEngine: defaultString(settings.Zhipu.SearchEngine, "search_std"),
			ContentSize:  defaultString(settings.Zhipu.ContentSize, "medium"),
			Client:       client,
		}
	}
	return ZhipuSearcher{
		APIKey:       settings.Zhipu.APIKey,
		Endpoint:     defaultString(settings.Zhipu.Endpoint, "https://open.bigmodel.cn/api/paas/v4/web_search"),
		SearchEngine: defaultString(settings.Zhipu.SearchEngine, "search_std"),
		ContentSize:  defaultString(settings.Zhipu.ContentSize, "medium"),
		Client:       client,
	}
}

func (s ZhipuSearcher) Search(ctx context.Context, query string, maxResults int) ([]Result, error) {
	if strings.TrimSpace(s.APIKey) == "" {
		return nil, errors.New("缺少 zhipu webSearch.apiKey")
	}
	body, _ := json.Marshal(map[string]any{
		"search_query":  query,
		"search_engine": s.SearchEngine,
		"content_size":  s.ContentSize,
		"count":         maxResults,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.APIKey)
	req.Header.Set("Content-Type", "application/json")
	data, err := doJSON(s.Client, req)
	if err != nil {
		return nil, err
	}
	return extractSearchResults(data, maxResults), nil
}

func (s SerpAPISearcher) Search(ctx context.Context, query string, maxResults int) ([]Result, error) {
	if strings.TrimSpace(s.APIKey) == "" {
		return nil, errors.New("缺少 serpapi.apiKey")
	}
	u, _ := url.Parse("https://serpapi.com/search.json")
	q := u.Query()
	q.Set("engine", "google")
	q.Set("q", query)
	q.Set("api_key", s.APIKey)
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	data, err := doJSON(s.Client, req)
	if err != nil {
		return nil, err
	}
	return extractSearchResults(data, maxResults), nil
}

func (s SearxngSearcher) Search(ctx context.Context, query string, maxResults int) ([]Result, error) {
	if strings.TrimSpace(s.URL) == "" {
		return nil, errors.New("缺少 searxng.url")
	}
	base, err := url.Parse(s.URL)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(base.Path, "/search") {
		base.Path = strings.TrimRight(base.Path, "/") + "/search"
	}
	q := base.Query()
	q.Set("q", query)
	q.Set("format", "json")
	base.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if err != nil {
		return nil, err
	}
	data, err := doJSON(s.Client, req)
	if err != nil {
		return nil, err
	}
	return extractSearchResults(data, maxResults), nil
}

func doJSON(client *http.Client, req *http.Request) (map[string]any, error) {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("search HTTP %d", resp.StatusCode)
	}
	var data map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	return data, nil
}

func extractSearchResults(data map[string]any, maxResults int) []Result {
	var results []Result
	var walk func(any)
	walk = func(value any) {
		if len(results) >= maxResults {
			return
		}
		switch v := value.(type) {
		case []any:
			for _, item := range v {
				walk(item)
				if len(results) >= maxResults {
					return
				}
			}
		case map[string]any:
			result := resultFromMap(v)
			if result.URL != "" && result.Title != "" {
				results = append(results, result)
				return
			}
			keys := make([]string, 0, len(v))
			for k := range v {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(v[k])
				if len(results) >= maxResults {
					return
				}
			}
		}
	}
	walk(data)
	return dedupeResults(results)
}

func resultFromMap(m map[string]any) Result {
	urlValue := firstString(m, "url", "link", "href", "source_url")
	return Result{
		Title:   firstString(m, "title", "name"),
		URL:     urlValue,
		Snippet: firstString(m, "snippet", "content", "summary", "description"),
		Content: firstString(m, "content"),
	}
}

func dedupeResults(results []Result) []Result {
	seen := map[string]bool{}
	var out []Result
	for _, result := range results {
		if seen[result.URL] {
			continue
		}
		seen[result.URL] = true
		out = append(out, result)
	}
	return out
}

func firstString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := m[key].(string); ok && strings.TrimSpace(v) != "" {
			return cleanSpace(v)
		}
	}
	return ""
}

func formatResults(results []Result) string {
	if len(results) == 0 {
		return "没有搜索结果"
	}
	var b strings.Builder
	for i, result := range results {
		fmt.Fprintf(&b, "%d. %s\nURL: %s\n摘要: %s\n", i+1, result.Title, result.URL, result.Snippet)
		if result.Content != "" && result.Content != result.Snippet {
			b.WriteString("内容: " + result.Content + "\n")
		}
		if i != len(results)-1 {
			b.WriteString("\n")
		}
	}
	return strings.TrimSpace(b.String())
}

func formatPage(page Page) string {
	var b strings.Builder
	fmt.Fprintf(&b, "URL: %s\nStatus: %d\n", page.FinalURL, page.StatusCode)
	if page.Title != "" {
		b.WriteString("Title: " + page.Title + "\n")
	}
	b.WriteString("\n" + page.Text)
	return strings.TrimSpace(b.String())
}

func rawSchema(items ...string) []byte {
	var props, required []string
	for i := 0; i+1 < len(items); i += 2 {
		name, desc := items[i], items[i+1]
		props = append(props, `"`+name+`":{"type":"string","description":"`+desc+`"}`)
		required = append(required, `"`+name+`"`)
	}
	return []byte(`{"type":"object","properties":{` + strings.Join(props, ",") + `},"required":[` + strings.Join(required, ",") + `]}`)
}

func parsePositive(value string, fallback int) int {
	var n int
	_, _ = fmt.Sscanf(strings.TrimSpace(value), "%d", &n)
	if n <= 0 {
		return fallback
	}
	return n
}

func cleanSpace(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
