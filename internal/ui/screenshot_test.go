package ui

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Screenshot generation for the UI tests. Each rendering test feeds its captured
// frame to shoot, which writes an image under screenshot/ at the module root,
// named after the test. It is gated behind the UISHOT env var and degrades
// gracefully (a missing tool is logged, never fatal), so plain `go test` stays
// fast and CI-safe whether or not freeze/Chrome are installed.
//
//	UISHOT unset|off|0  → no images (default)
//	UISHOT=svg          → freeze renders <test>.svg (fast, pure Go)
//	UISHOT=1|png        → also rasterise <test>.png via headless Chrome
//
// freeze's own PNG export hangs on some machines, so PNGs go through Chrome.

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
	svgPath := base + ".svg"
	if out, err := exec.Command(freeze, ansiPath,
		"--language", "ansi", "--output", svgPath,
		"--background", "#0d1117", "--margin", "0", "--padding", "30,40,30,30",
		"--window", "--border.radius", "10", "--font.size", "13", "--line-height", "1.35",
	).CombinedOutput(); err != nil {
		t.Logf("screenshot: freeze failed: %v\n%s", err, out)
		return
	}
	if mode == "png" {
		if err := rasterise(svgPath, base+".png"); err != nil {
			t.Logf("screenshot: png skipped (%v); kept %s", err, filepath.Base(svgPath))
		}
	}
}

// rasterise turns a freeze SVG into a 2x PNG with headless Chrome.
func rasterise(svgPath, pngPath string) error {
	chrome := chromeBin()
	if chrome == "" {
		return fmt.Errorf("chrome not found")
	}
	w, h, err := svgSize(svgPath)
	if err != nil {
		return err
	}
	profile, err := os.MkdirTemp("", "uishot-chrome-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(profile)
	out, err := exec.Command(chrome,
		"--headless=new", "--disable-gpu", "--hide-scrollbars",
		"--force-device-scale-factor=2",
		fmt.Sprintf("--window-size=%d,%d", w, h),
		"--default-background-color=00000000",
		"--user-data-dir="+profile,
		"--screenshot="+pngPath,
		"file://"+svgPath,
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}

var svgSizeRe = regexp.MustCompile(`width="([0-9.]+)" height="([0-9.]+)"`)

// svgSize reads the intrinsic width/height (CSS px, rounded up) from a freeze SVG.
func svgSize(path string) (int, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	m := svgSizeRe.FindSubmatch(data)
	if m == nil {
		return 0, 0, fmt.Errorf("no width/height in %s", filepath.Base(path))
	}
	w, _ := strconv.ParseFloat(string(m[1]), 64)
	h, _ := strconv.ParseFloat(string(m[2]), 64)
	return int(math.Ceil(w)), int(math.Ceil(h)), nil
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

// chromeBin locates a Chrome/Chromium binary for SVG rasterisation.
func chromeBin() string {
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	for _, p := range []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
