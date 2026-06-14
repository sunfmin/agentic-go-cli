package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/google/go-cmp/cmp"
)

func TestRequestToCurl(t *testing.T) {
	const body = `{"model":"claude","note":"don't panic"}`
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("authorization", "Bearer sk-secret-xyz")

	got, err := requestToCurl(req)
	if err != nil {
		t.Fatalf("requestToCurl: %v", err)
	}

	// Headers are emitted in sorted order; the bearer token is rewritten to a
	// shell variable; the JSON body rides verbatim in a quoted heredoc.
	want := "curl 'https://api.anthropic.com/v1/messages' \\\n" +
		"  -X POST \\\n" +
		"  -H 'Anthropic-Beta: oauth-2025-04-20' \\\n" +
		"  -H \"Authorization: Bearer $ANTHROPIC_AUTH_TOKEN\" \\\n" +
		"  -H 'Content-Type: application/json' \\\n" +
		"  --data-binary @- <<'JSON'\n" +
		body + "\nJSON\n"
	if diff := cmp.Diff(want, string(got)); diff != "" {
		t.Errorf("requestToCurl mismatch (-want +got):\n%s", diff)
	}
	if strings.Contains(string(got), "sk-secret-xyz") {
		t.Errorf("requestToCurl leaked the bearer token:\n%s", got)
	}
}

func TestPersistAPIExchange(t *testing.T) {
	tests := []struct {
		name     string
		req      string
		resp     string
		wantReq  bool // expect api/003.request.curl
		wantResp bool // expect api/003.response.json
	}{
		{name: "full exchange", req: "curl ...", resp: `{"id":"x"}`, wantReq: true, wantResp: true},
		{name: "response only", req: "", resp: `{"id":"x"}`, wantReq: false, wantResp: true},
		{name: "nothing captured", req: "", resp: "", wantReq: false, wantResp: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			a := New(&capturingModel{req: []byte(tt.req), resp: []byte(tt.resp)}, nil, nil)
			a.sessionDir = dir
			a.round = 3
			if err := a.persistAPIExchange(); err != nil {
				t.Fatalf("persistAPIExchange: %v", err)
			}

			assertFile(t, filepath.Join(dir, "api", "003.request.curl"), tt.wantReq, tt.req)
			assertFile(t, filepath.Join(dir, "api", "003.response.json"), tt.wantResp, tt.resp)
		})
	}
}

func TestPersistAPIExchangeNonCapturingModel(t *testing.T) {
	dir := t.TempDir()
	// fakeModel does not expose LastExchange, so there is nothing to persist.
	a := New(&fakeModel{}, nil, nil)
	a.sessionDir = dir
	a.round = 1
	if err := a.persistAPIExchange(); err != nil {
		t.Fatalf("persistAPIExchange: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "api")); !os.IsNotExist(err) {
		t.Errorf("api/ created for a model that captures nothing")
	}
}

// assertFile checks a written exchange file: present with the expected content,
// or absent.
func assertFile(t *testing.T, path string, want bool, content string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if !want {
		if !os.IsNotExist(err) {
			t.Errorf("%s exists, want absent", path)
		}
		return
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if diff := cmp.Diff(content, string(got)); diff != "" {
		t.Errorf("%s content mismatch (-want +got):\n%s", path, diff)
	}
}

// TestAnthropicModelCapturesExchange drives the real anthropicModel against a
// stub server to prove the capture middleware records the wire request and the
// verbatim response, and still lets the SDK decode the response (body restored).
func TestAnthropicModelCapturesExchange(t *testing.T) {
	const respBody = `{"id":"msg_1","type":"message","role":"assistant","model":"claude","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)

	client := anthropic.NewClient(
		option.WithBaseURL(srv.URL),
		option.WithAuthToken("sk-secret-xyz"),
		option.WithMaxRetries(0),
	)
	m := NewAnthropicModel(&client)

	msg, err := m.Next(context.Background(), []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock("hi")),
	}, nil)
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	// Response body was restored, so the SDK still decoded the message.
	if msg == nil || msg.ID != "msg_1" {
		t.Fatalf("decoded message = %+v, want ID msg_1", msg)
	}

	capturer := m.(interface {
		LastExchange() (request, response []byte)
	})
	req, resp := capturer.LastExchange()
	if diff := cmp.Diff(respBody, string(resp)); diff != "" {
		t.Errorf("captured response mismatch (-want +got):\n%s", diff)
	}
	curl := string(req)
	if !strings.Contains(curl, srv.URL+"/v1/messages") {
		t.Errorf("curl missing endpoint:\n%s", curl)
	}
	if !strings.Contains(curl, `$ANTHROPIC_AUTH_TOKEN`) || strings.Contains(curl, "sk-secret-xyz") {
		t.Errorf("curl did not redact the token:\n%s", curl)
	}
	// The captured request body is the verbatim payload the server received.
	if !strings.Contains(curl, gotBody) {
		t.Errorf("curl body %q does not match what the server received %q", curl, gotBody)
	}
}

// capturingModel is a Model that exposes a canned exchange via LastExchange, so
// persistAPIExchange can be driven without real network I/O.
type capturingModel struct {
	req  []byte
	resp []byte
}

func (c *capturingModel) Next(context.Context, []anthropic.MessageParam, []anthropic.ToolUnionParam) (*anthropic.Message, error) {
	return &anthropic.Message{}, nil
}

func (c *capturingModel) Summarize(context.Context, string) (string, error) { return "", nil }

func (c *capturingModel) LastExchange() (request, response []byte) {
	return c.req, c.resp
}
