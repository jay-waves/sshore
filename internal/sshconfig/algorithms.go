package sshconfig

import (
	"fmt"
	"slices"
	"strings"
)

// Algorithms applies a configured list to the implementation's supported
// algorithms. An explicit policy matching nothing returns a non-nil empty list.
func Algorithms(supported []string, spec string) ([]string, error) {
	if spec == "" {
		return supported, nil
	}
	mode := spec[0]
	if mode == '-' || mode == '+' || mode == '^' {
		spec = spec[1:]
	}
	patterns := strings.Split(spec, ",")
	for _, pattern := range patterns {
		if pattern == "" || strings.ContainsAny(pattern, " \t\r\n") {
			return nil, fmt.Errorf("invalid SSH algorithm list")
		}
	}
	if mode == '+' {
		// All supported algorithms are already in the default set.
		return supported, nil
	}

	result := []string{}
	if mode == '-' {
		for _, algorithm := range supported {
			if !matchPatterns(patterns, algorithm) {
				result = append(result, algorithm)
			}
		}
	} else {
		for _, pattern := range patterns {
			for _, algorithm := range supported {
				if wildcard(pattern, algorithm) && !slices.Contains(result, algorithm) {
					result = append(result, algorithm)
				}
			}
		}
		if mode == '^' {
			for _, algorithm := range supported {
				if !slices.Contains(result, algorithm) {
					result = append(result, algorithm)
				}
			}
		}
	}
	return result, nil
}
