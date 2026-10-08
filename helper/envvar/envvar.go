// Package envvar validates per-pool environment variables that are passed to
// the runner inside the VM.
package envvar

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var validName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// reserved are set by the agent for the runner process and can't be overridden.
var reserved = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "UID": true, "GID": true,
}

// reservedPrefixes belong to the runner and Actions; setting them would confuse the runner.
var reservedPrefixes = []string{"GITHUB_", "ACTIONS_", "RUNNER_"}

// Validate checks that every name is a valid, non-reserved variable name and
// that no value contains a NUL byte. Errors name the variable, never its value.
func Validate(env map[string]string) error {
	for _, name := range Names(env) {
		if !validName.MatchString(name) {
			return fmt.Errorf("env %q: invalid variable name", name)
		}
		if reserved[name] {
			return fmt.Errorf("env %q: reserved variable", name)
		}
		for _, prefix := range reservedPrefixes {
			if strings.HasPrefix(name, prefix) {
				return fmt.Errorf("env %q: %s* variables are reserved", name, prefix)
			}
		}
		if strings.ContainsRune(env[name], 0) {
			return fmt.Errorf("env %q: value contains a NUL byte", name)
		}
	}

	return nil
}

// Names returns the variable names, sorted.
func Names(env map[string]string) []string {
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Pairs returns NAME=VALUE pairs sorted by name.
func Pairs(env map[string]string) []string {
	pairs := make([]string, 0, len(env))
	for _, name := range Names(env) {
		pairs = append(pairs, name+"="+env[name])
	}
	return pairs
}
