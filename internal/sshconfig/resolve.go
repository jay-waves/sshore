package sshconfig

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Source describes one configuration file. Include paths relative to Base use
// the same directory throughout that source, including in nested files.
type Source struct {
	Path, Base string
	Optional   bool
	CheckMode  bool
}

type Request struct {
	Host, LocalUser, Home, Command string
	LookupEnv                      func(string) string
	Overrides                      []Option
	Sources                        []Source
}

type Result struct {
	Values                  map[string][]string
	Origins                 map[string]string
	OriginalHost, LocalUser string
}

type resolver struct {
	Request
	Result
	host             string
	final, wantFinal bool
	includes         map[string]bool
}

// Resolve applies explicit options, files in priority order, and a final pass
// when requested. Transport defaults belong to the caller.
func Resolve(ctx context.Context, req Request) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	r := resolver{
		Request: req,
		Result: Result{
			Values: make(map[string][]string), Origins: make(map[string]string),
			OriginalHost: req.Host, LocalUser: req.LocalUser,
		},
		host: req.Host, includes: make(map[string]bool),
	}
	for _, opt := range req.Overrides {
		if opt.Key == "host" || opt.Key == "match" || opt.Key == "include" {
			return Result{}, fmt.Errorf("%s is only allowed in a configuration file", opt.Key)
		}
		if err := r.set(opt, "command line"); err != nil {
			return Result{}, err
		}
	}
	if err := r.readSources(ctx); err != nil {
		return Result{}, err
	}
	if hostname := r.get("hostname"); hostname != "" {
		var err error
		r.host, err = expand(hostname, map[byte]string{'h': r.host}, nil)
		if err != nil {
			return Result{}, fmt.Errorf("%s: HostName: %w", r.Origins["hostname"], err)
		}
	}
	if !strings.Contains(r.host, ":") {
		r.host = strings.ToLower(r.host)
	}
	r.Values["hostname"] = []string{r.host}
	if r.wantFinal {
		r.final = true
		// Match host and Host stanzas now see the resolved host. A second
		// pass fills unset options without discarding earlier values.
		if err := r.readSources(ctx); err != nil {
			return Result{}, err
		}
	}
	if r.get("user") == "" {
		r.Values["user"] = []string{r.Request.LocalUser}
	}
	return r.Result, nil
}

func (r *resolver) get(key string) string {
	if values := r.Values[key]; len(values) > 0 {
		return values[0]
	}
	return ""
}

func (r *resolver) readSources(ctx context.Context) error {
	for _, source := range r.Sources {
		if err := r.read(ctx, source, 0); err != nil {
			return err
		}
	}
	return nil
}

func (r *resolver) read(ctx context.Context, source Source, depth int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth > 16 || r.includes[source.Path] {
		return fmt.Errorf("%s: recursive SSH Include", source.Path)
	}
	file, err := os.Open(source.Path)
	if source.Optional && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	if source.CheckMode {
		if err := checkPermissions(file); err != nil {
			return err
		}
	}
	r.includes[source.Path] = true
	defer delete(r.includes, source.Path)
	active := true
	lines := bufio.NewScanner(file)
	for number := 1; lines.Scan(); number++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := lines.Text()
		if number == 1 {
			line = strings.TrimPrefix(line, "\uFEFF")
		}
		at := source.Path + ":" + strconv.Itoa(number)
		opt, err := ParseOption(line)
		if err != nil {
			return fmt.Errorf("%s: %w", at, err)
		}
		switch opt.Key {
		case "":
		case "host":
			active = matchPatterns(opt.Values, r.host)
		case "match":
			active, err = r.match(opt.Values)
			if err != nil {
				return fmt.Errorf("%s: %w", at, err)
			}
		case "include":
			if active {
				if err := r.include(ctx, source.Base, opt.Values, depth+1); err != nil {
					return fmt.Errorf("%s: Include: %w", at, err)
				}
			}
		default:
			if active {
				if err := r.set(opt, at); err != nil {
					return err
				}
			}
		}
	}
	if err := lines.Err(); err != nil {
		return fmt.Errorf("%s: %w", source.Path, err)
	}
	return nil
}

func (r *resolver) include(ctx context.Context, base string, patterns []string, depth int) error {
	host, err := r.matchTarget("host")
	if err != nil {
		return err
	}
	user, port := r.get("user"), r.get("port")
	if user == "" {
		user = r.Request.LocalUser
	}
	if port == "" {
		port = "22"
	}
	tokens := map[byte]string{
		'd': r.Home, 'h': host, 'n': r.OriginalHost, 'p': port, 'r': user, 'u': r.Request.LocalUser,
	}
	for _, pattern := range patterns {
		path, err := expand(pattern, tokens, r.LookupEnv)
		if err != nil {
			return err
		}
		if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") {
			path = filepath.Join(r.Home, path[2:])
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(base, path)
		}
		if !strings.ContainsAny(path, "*?[") {
			if err := r.read(ctx, Source{Path: path, Base: base, Optional: true, CheckMode: true}, depth); err != nil {
				return err
			}
			continue
		}
		names, err := filepath.Glob(path)
		if err != nil {
			return err
		}
		for _, name := range names {
			if err := r.read(ctx, Source{Path: name, Base: base, CheckMode: true}, depth); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *resolver) match(values []string) (bool, error) {
	matched := true
	for i := 0; i < len(values); i++ {
		name, negated := strings.CutPrefix(strings.ToLower(values[i]), "!")
		var current bool
		switch name {
		case "all":
			if i != len(values)-1 || i > 1 || i == 1 && !strings.EqualFold(strings.TrimPrefix(values[0], "!"), "final") {
				return false, fmt.Errorf("Match all must appear alone or after final")
			}
			current = true
		case "final":
			r.wantFinal = true
			current = r.final
		case "host", "originalhost", "user", "localuser", "command":
			i++
			if i >= len(values) || values[i] == "" && name != "command" {
				return false, fmt.Errorf("Match %s requires a pattern", name)
			}
			target, err := r.matchTarget(name)
			if err != nil {
				return false, err
			}
			pattern := values[i]
			if name == "host" || name == "originalhost" {
				pattern, target = strings.ToLower(pattern), strings.ToLower(target)
			}
			current = matchPatterns(strings.Split(pattern, ","), target)
		default:
			return false, fmt.Errorf("Match %s is unsupported", name)
		}
		matched = matched && current != negated
	}
	return matched, nil
}

func (r *resolver) matchTarget(name string) (string, error) {
	switch name {
	case "host":
		if hostname := r.get("hostname"); hostname != "" && !r.final {
			return expand(hostname, map[byte]string{'h': r.host}, nil)
		}
		return r.host, nil
	case "originalhost":
		return r.OriginalHost, nil
	case "localuser":
		return r.Request.LocalUser, nil
	case "user":
		if user := r.get("user"); user != "" {
			return user, nil
		}
		return r.Request.LocalUser, nil
	default:
		return r.Command, nil
	}
}

func (r *resolver) set(opt Option, at string) error {
	opt.Key = optionKey(opt.Key)
	if err := r.apply(opt); err != nil {
		return fmt.Errorf("%s: %s: %w", at, opt.Key, err)
	}
	if len(r.Values[opt.Key]) != 0 && r.Origins[opt.Key] == "" {
		r.Origins[opt.Key] = at
	}
	return nil
}

func (r *resolver) apply(opt Option) error {
	key := opt.Key
	values := opt.Values
	kind, known := options[key]
	if !known {
		if matchPatterns(r.Values["ignoreunknown"], key) {
			return nil
		}
		return fmt.Errorf("unsupported SSH configuration option")
	}
	if len(values) == 0 || slices.Contains(values, "") && kind != command {
		return fmt.Errorf("requires a non-empty value")
	}
	switch kind {
	case list, repeated:
	case command:
		values = []string{strings.Join(values, " ")}
	default:
		if len(values) != 1 {
			return fmt.Errorf("requires a single value")
		}
	}
	if kind != repeated && len(r.Values[key]) != 0 {
		return nil
	}
	values, err := normalize(key, kind, values)
	if err != nil {
		return err
	}
	switch key {
	case "proxyjump":
		if r.get("proxycommand") != "" {
			return nil
		}
	case "proxycommand":
		if r.get("proxyjump") != "" {
			return nil
		}
	}
	if kind == repeated {
		for _, value := range values {
			if !slices.Contains(r.Values[key], value) {
				r.Values[key] = append(r.Values[key], value)
			}
		}
	} else {
		r.Values[key] = values
	}
	return nil
}

func expand(value string, tokens map[byte]string, lookup func(string) string) (string, error) {
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		switch {
		case value[i] == '%':
			i++
			if i == len(value) {
				return "", fmt.Errorf("incomplete %% expansion")
			}
			if value[i] == '%' {
				out.WriteByte('%')
				continue
			}
			token, ok := tokens[value[i]]
			if !ok {
				return "", fmt.Errorf("unsupported expansion %%%c", value[i])
			}
			out.WriteString(token)
		case lookup != nil && strings.HasPrefix(value[i:], "${"):
			key, _, ok := strings.Cut(value[i+2:], "}")
			if !ok || key == "" {
				return "", fmt.Errorf("invalid environment expansion")
			}
			env := lookup(key)
			if env == "" {
				return "", fmt.Errorf("environment variable %s is not set", key)
			}
			out.WriteString(env)
			i += len(key) + 2
		default:
			out.WriteByte(value[i])
		}
	}
	return out.String(), nil
}
