package ui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Screenshot generation for the UI tests. Each rendering test feeds its captured
// frame to shoot, which writes an image under screenshot/ at the module root,
// named after the test. It is gated behind the UISHOT env var and degrades
// gracefully (a missing tool is logged, never fatal), so plain `go test` stays
// fast and CI-safe whether or not freeze is installed.
//
//	UISHOT unset|off|0  → no images (default)
//	UISHOT=svg          → freeze renders <test>.svg
//	UISHOT=1|png        → freeze renders <test>.png
//
// freeze rasterises PNGs itself — via librsvg (rsvg-convert) when present, else a
// resvg-wasm module on wazero — so no headless browser is involved. Both paths
// render a full frame in well under a second; the call is still wrapped in a
// timeout so a pathological render can never hang the whole suite.
const shootTimeout = 60 * time.Second

// shootMode parses UISHOT into "" (off), "svg", or "png".
func shootMode() string {
	switch strings.ToLower(os.Getenv("UISHOT")) {
	case "", "0", "off", "no", "false":
		return ""
	case "svg":
		return "svg"
	default:
		return "png"
	}
}

// shoot records frame as an image named after the running test.
func shoot(t *testing.T, frame string) {
	t.Helper()
	mode := shootMode()
	if mode == "" {
		return
	}
	dir, err := screenshotDir()
	if err != nil {
		t.Logf("screenshot: %v", err)
		return
	}
	base := filepath.Join(dir, screenshotName(t.Name()))
	ansiPath := base + ".ansi"
	if err := os.WriteFile(ansiPath, []byte(frame), 0o644); err != nil {
		t.Logf("screenshot: write %s: %v", ansiPath, err)
		return
	}
	freeze := freezeBin()
	if freeze == "" {
		t.Logf("screenshot: freeze not found on PATH or in GOPATH/bin; kept %s only", filepath.Base(ansiPath))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), shootTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, freeze, ansiPath,
		"--language", "ansi", "--output", base+"."+mode,
		"--background", "#0d1117", "--margin", "0", "--padding", "30,40,30,30",
		"--window", "--border.radius", "10", "--font.size", "13", "--line-height", "1.35",
	).CombinedOutput()
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		t.Logf("screenshot: freeze timed out after %s; kept %s", shootTimeout, filepath.Base(ansiPath))
	case err != nil:
		t.Logf("screenshot: freeze failed: %v\n%s", err, out)
	}
}

// screenshotName turns a test name (which may include subtest paths) into a
// filesystem-safe base name.
func screenshotName(testName string) string {
	return strings.NewReplacer("/", "__", " ", "_").Replace(testName)
}

// screenshotDir is screenshot/ at the module root, created if absent.
func screenshotDir() (string, error) {
	root, err := repoRoot()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, "screenshot")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// repoRoot walks up from the working directory to the directory holding go.mod.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above %s", dir)
		}
		dir = parent
	}
}

// freezeBin locates the freeze binary on PATH or in GOPATH/bin.
func freezeBin() string {
	if p, err := exec.LookPath("freeze"); err == nil {
		return p
	}
	if out, err := exec.Command("go", "env", "GOPATH").Output(); err == nil {
		cand := filepath.Join(strings.TrimSpace(string(out)), "bin", "freeze")
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	return ""
}
