package main

// Line-based session selection uses the existing setup reader, with no second
// input loop or screen ownership. Filters only change visibility; original
// numbers remain stable. Bare input always denotes an exact name;
// #N unambiguously selects an index. Filtering never attaches automatically.
// Setup validates remote names and closes the reader before sending a selection.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

func chooseSetupSession(input setupInput, names []string) (string, error) {
	visible := make([]bool, len(names))
	show := func(filter string) {
		matches := 0
		for i, name := range names {
			visible[i] = strings.Contains(strings.ToLower(name), strings.ToLower(filter))
			if visible[i] {
				logf(os.Stderr, logInfo, "  #%d  %s\n", i+1, name)
				matches++
			}
		}
		if matches == 0 {
			logf(os.Stderr, logWarning, "No matches. / clears the filter.\n")
		}
	}
	show("")
	for {
		answer, err := input.Read("Session (#number/name, /filter; Enter cancels): ", false)
		if err != nil {
			return "", fmt.Errorf("session selection cancelled: %w", err)
		}
		answer = strings.TrimSpace(answer)
		if answer == "" {
			return "", fmt.Errorf("session selection cancelled")
		}
		if strings.HasPrefix(answer, "/") {
			show(strings.TrimSpace(answer[1:]))
			continue
		}
		for i, name := range names {
			if visible[i] && answer == name {
				return name, nil
			}
		}
		n, err := strconv.Atoi(strings.TrimPrefix(answer, "#"))
		if strings.HasPrefix(answer, "#") && err == nil && n >= 1 && n <= len(names) && visible[n-1] {
			return names[n-1], nil
		}
		logf(os.Stderr, logWarning, "Choose a listed #number or name; /text filters.\n")
	}
}
