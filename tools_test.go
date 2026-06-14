package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestReadReturnsCurrentContents(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "hello.txt")
	writeFile(t, p, "hi there")

	out, err := ReadDefinition.Function(mustJSON(t, map[string]string{"path": p}))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if out != "hi there" {
		t.Fatalf("read = %q, want %q", out, "hi there")
	}
}

func TestEditWritesFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "out.txt")

	if _, err := EditDefinition.Function(mustJSON(t, map[string]string{"path": p, "content": "written"})); err != nil {
		t.Fatalf("edit: %v", err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(data) != "written" {
		t.Fatalf("file = %q, want %q", data, "written")
	}
}

func TestEditThenReadReflectsChange(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	writeFile(t, p, "old")

	if _, err := EditDefinition.Function(mustJSON(t, map[string]string{"path": p, "content": "new"})); err != nil {
		t.Fatalf("edit: %v", err)
	}
	out, err := ReadDefinition.Function(mustJSON(t, map[string]string{"path": p}))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if out != "new" {
		t.Fatalf("read after edit = %q, want %q", out, "new")
	}
}

func TestRunReturnsCombinedOutput(t *testing.T) {
	out, err := RunDefinition.Function(mustJSON(t, map[string]string{"command": "echo hello"}))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if out != "hello\n" {
		t.Fatalf("run = %q, want %q", out, "hello\n")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
