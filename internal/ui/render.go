// Package ui renders tool calls and their output for the terminal.
package ui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// PrintToolCall renders a tool invocation in a human-readable form instead of
// raw JSON, e.g.:
//
//	● read(path: main.go)
//	● edit
//	    path: hello.txt
//	    content:
//	      hello
func PrintToolCall(name string, input []byte) {
	var args map[string]any
	_ = json.Unmarshal(input, &args)

	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	inline := true
	for _, k := range keys {
		s := fmt.Sprint(args[k])
		if strings.Contains(s, "\n") || len(s) > 60 {
			inline = false
			break
		}
	}

	// Single-argument tools don't need the key label — show the value directly.
	if len(keys) == 1 {
		s := fmt.Sprint(args[keys[0]])
		if inline {
			fmt.Printf("\x1b[92m●\x1b[0m \x1b[1m%s\x1b[0m(%s)\n", name, s)
			return
		}
		fmt.Printf("\x1b[92m●\x1b[0m \x1b[1m%s\x1b[0m\n", name)
		for _, line := range strings.Split(s, "\n") {
			fmt.Printf("    %s\n", line)
		}
		return
	}

	if inline {
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s: %v", k, args[k]))
		}
		fmt.Printf("\x1b[92m●\x1b[0m \x1b[1m%s\x1b[0m(%s)\n", name, strings.Join(parts, ", "))
		return
	}

	fmt.Printf("\x1b[92m●\x1b[0m \x1b[1m%s\x1b[0m\n", name)
	for _, k := range keys {
		s := fmt.Sprint(args[k])
		if strings.Contains(s, "\n") {
			fmt.Printf("    %s:\n", k)
			for _, line := range strings.Split(s, "\n") {
				fmt.Printf("      %s\n", line)
			}
		} else {
			fmt.Printf("    %s: %s\n", k, s)
		}
	}
}

// PrintToolResult prints the tool's output verbatim (original formatting
// preserved), indented so it reads as output, red on error.
func PrintToolResult(response string, isError bool) {
	for _, line := range strings.Split(strings.TrimRight(response, "\n"), "\n") {
		if isError {
			fmt.Printf("  \x1b[91m%s\x1b[0m\n", line)
		} else {
			fmt.Printf("  %s\n", line)
		}
	}
}
