package main

// Session and request selection share the existing line reader, with no second
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
	return chooseLocalItem(input, names, names, "Session (#number/name, /filter; Enter cancels): ", "session")
}

// Labels are display-only; selection always returns an original identity.
func chooseLocalItem(input setupInput, names, labels []string, prompt, kind string) (string, error) {
	visible := make([]bool, len(names))
	show := func(filter string) {
		matches := 0
		for i := range names {
			visible[i] = strings.Contains(strings.ToLower(labels[i]), strings.ToLower(filter))
			if visible[i] {
				logf(os.Stderr, logInfo, "  #%d  %s\n", i+1, cleanText(labels[i]))
				matches++
			}
		}
		if matches == 0 {
			logf(os.Stderr, logWarning, "No matches. / clears the filter.\n")
		}
	}
	show("")
	for {
		answer, err := input.Read(prompt, false)
		if err != nil {
			return "", fmt.Errorf("%s selection cancelled: %w", kind, err)
		}
		answer = strings.TrimSpace(answer)
		if answer == "" {
			return "", fmt.Errorf("%s selection cancelled", kind)
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
		logf(os.Stderr, logWarning, "Choose a listed #number or exact identity; /text filters.\n")
	}
}
