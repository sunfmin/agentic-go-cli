package agent

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// apiSubdir is the per-Session directory holding the raw API exchange of each
// Round: a curl-runnable request and the server's verbatim response.
const apiSubdir = "api"

// persistAPIExchange writes the most recent request/response to api/NNN.* in the
// Session directory, keyed by the current Round so files never collide and stay
// in send order. It is a no-op for a Model that captures nothing (the test fake).
func (a *Agent) persistAPIExchange() error {
	capturer, ok := a.model.(interface {
		LastExchange() (request, response []byte)
	})
	if !ok {
		return nil
	}
	req, resp := capturer.LastExchange()
	if len(req) == 0 && len(resp) == 0 {
		return nil
	}
	dir, err := a.ensureDir()
	if err != nil {
		return err
	}
	apiDir := filepath.Join(dir, apiSubdir)
	if err := os.MkdirAll(apiDir, 0o755); err != nil {
		return err
	}
	if len(req) > 0 {
		p := filepath.Join(apiDir, fmt.Sprintf("%03d.request.curl", a.round))
		if err := os.WriteFile(p, req, 0o644); err != nil {
			return err
		}
	}
	if len(resp) > 0 {
		p := filepath.Join(apiDir, fmt.Sprintf("%03d.response.json", a.round))
		if err := os.WriteFile(p, resp, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// requestToCurl rebuilds the live HTTP request as a curl command that reproduces
// it byte-for-byte — same URL, method, headers and JSON body that go over the
// wire — except the bearer token, which is rewritten to a shell variable so the
// saved command stays runnable (export the var) without writing the credential
// to disk. The body rides in a quoted heredoc, so it is preserved verbatim and
// needs no escaping.
func requestToCurl(req *http.Request) ([]byte, error) {
	body, err := requestBody(req)
	if err != nil {
		return nil, err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "curl %s \\\n", shellSingleQuote(req.URL.String()))
	if req.Method != "" && req.Method != http.MethodGet {
		fmt.Fprintf(&b, "  -X %s \\\n", req.Method)
	}

	keys := make([]string, 0, len(req.Header))
	for k := range req.Header {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range req.Header[k] {
			if redacted, ok := redactHeader(k, v); ok {
				// Double-quoted so the shell expands the credential variable.
				fmt.Fprintf(&b, "  -H \"%s: %s\" \\\n", k, redacted)
				continue
			}
			fmt.Fprintf(&b, "  -H %s \\\n", shellSingleQuote(k+": "+v))
		}
	}

	if len(body) > 0 {
		b.WriteString("  --data-binary @- <<'JSON'\n")
		b.Write(body)
		b.WriteString("\nJSON\n")
	} else {
		// Trim the trailing continuation when there is no body to append.
		return []byte(strings.TrimSuffix(b.String(), " \\\n") + "\n"), nil
	}
	return []byte(b.String()), nil
}

// requestBody reads the request body without consuming it. The SDK sets GetBody
// (it must, to support retries), so the common path is side-effect free; the
// fallback reads and restores Body for the rare case GetBody is unset.
func requestBody(req *http.Request) ([]byte, error) {
	if req.GetBody != nil {
		rc, err := req.GetBody()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(rc)
	}
	if req.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(req.Body)
	req.Body = io.NopCloser(strings.NewReader(string(body)))
	return body, err
}

// redactHeader rewrites a credential-bearing header value to a shell variable
// placeholder, preserving any auth scheme. It returns ok=false for headers that
// carry no secret, which are then emitted verbatim.
func redactHeader(key, value string) (string, bool) {
	switch strings.ToLower(key) {
	case "authorization":
		scheme := "Bearer"
		if parts := strings.SplitN(value, " ", 2); len(parts) == 2 {
			scheme = parts[0]
		}
		return scheme + " $ANTHROPIC_AUTH_TOKEN", true
	case "x-api-key":
		return "$ANTHROPIC_API_KEY", true
	}
	return "", false
}

// shellSingleQuote wraps s in single quotes for safe use in a shell command,
// escaping any embedded single quotes.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
