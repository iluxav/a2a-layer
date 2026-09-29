package mcpproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeUpstream is an MCP server with three tools; each echoes the headers its call arrived with
// and whether the client offered elicitation (a proxy must not).
func fakeUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "gateway", Version: "1"}, nil)
	for _, name := range []string{"linear__get_issue", "linear__save_issue", "vercel__buy_domain"} {
		server.AddTool(&mcp.Tool{Name: name, Description: "does " + name, InputSchema: map[string]any{"type": "object"}},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				h := req.Extra.Header
				elicit := req.Session.InitializeParams().Capabilities.Elicitation != nil
				text := name + " auth=" + h.Get("Authorization") + " session=" + h.Get("X-Parent-Session")
				if elicit {
					text += " elicitation-offered"
				}
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil
			})
	}
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	t.Cleanup(ts.Close)
	return ts
}

func connectTo(t *testing.T, url string) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "cli", Version: "1"}, nil)
	s, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: url}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestProxyServesOnlyTheAllowedTools(t *testing.T) {
	up := fakeUpstream(t)
	var p *Proxy
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { p.ServeHTTP(w, r) }))
	defer front.Close()
	p = New(front.URL)

	served, closeProxy, err := p.Open(context.Background(), []Upstream{{
		Name: "gateway", URL: up.URL,
		Headers: map[string]string{"Authorization": "Bearer dgk_pm", "X-Parent-Session": "sess_1"},
		Tools:   []string{"linear__get_issue", "linear__save_issue", "linear__gone"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(served) != 1 || !strings.HasPrefix(served[0].URL, front.URL+PathPrefix) {
		t.Fatalf("served = %+v", served)
	}
	if got := strings.Join(served[0].Tools, ","); got != "linear__get_issue,linear__save_issue" {
		t.Errorf("tools = %s", got)
	}
	if got := strings.Join(served[0].Missing, ","); got != "linear__gone" {
		t.Errorf("missing = %s", got)
	}

	cli := connectTo(t, served[0].URL)
	var names []string
	for tool, err := range cli.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	if got := strings.Join(names, ","); got != "linear__get_issue,linear__save_issue" {
		t.Errorf("the CLI sees %s", got)
	}
	res, err := cli.CallTool(context.Background(), &mcp.CallToolParams{Name: "linear__get_issue", Arguments: map[string]any{"id": "TES-1"}})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if text != "linear__get_issue auth=Bearer dgk_pm session=sess_1" {
		t.Errorf("upstream saw %q (headers must be forwarded; elicitation must not be offered)", text)
	}
	if res, err := cli.CallTool(context.Background(), &mcp.CallToolParams{Name: "vercel__buy_domain"}); err == nil && !res.IsError {
		t.Error("a tool outside the allowlist was callable through the proxy")
	}

	closeProxy()
	resp, err := http.Post(served[0].URL, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("after close the endpoint should be gone, got %d", resp.StatusCode)
	}
}

func TestProxyWithoutAnAllowlistServesEverything(t *testing.T) {
	up := fakeUpstream(t)
	p := New("http://unused")
	served, closeProxy, err := p.Open(context.Background(), []Upstream{{Name: "gw", URL: up.URL}})
	if err != nil {
		t.Fatal(err)
	}
	defer closeProxy()
	if len(served[0].Tools) != 3 || len(served[0].Missing) != 0 {
		t.Errorf("served = %+v", served[0])
	}
}

func TestProxyReportsAnUnreachableServer(t *testing.T) {
	p := New("http://unused")
	_, _, err := p.Open(context.Background(), []Upstream{{Name: "gw", URL: "http://127.0.0.1:1/mcp"}})
	if err == nil || !strings.Contains(err.Error(), "mcp server gw") {
		t.Errorf("err = %v", err)
	}
}
