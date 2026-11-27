package ssh

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/jay-waves/sshore/internal/sshconfig"
)

// FormatEndpoint joins an SSH destination and its operand, bracketing IPv6 addresses.
func FormatEndpoint(host, operand string) string {
	prefix, addr := "", host
	if i := strings.LastIndexByte(host, '@'); i >= 0 {
		prefix, addr = host[:i+1], host[i+1:]
	}
	if strings.Contains(addr, ":") && !strings.HasPrefix(addr, "[") {
		addr = "[" + addr + "]"
	}
	return prefix + addr + ":" + operand
}

// Options describes one owned SSH connection.
// Environment and Directory belong to the initiating command, never daemon startup.
type Options struct {
	Host, Command, Directory string
	Args, Environment        []string
	// OpenCommand starts a managed command on the authenticated connection.
	OpenCommand func(context.Context, *Client, string, io.Writer) (*Process, error)
	// IgnoredForwarding reports the host whose configured forwards were skipped.
	IgnoredForwarding func(context.Context, string)
	// Diagnostic reports SSH progress when debug logging is enabled.
	Diagnostic func(context.Context, string)
	// RequireKnownHost prevents all hops from learning new host keys.
	RequireKnownHost bool
	// Confirm receives a host key fingerprint when StrictHostKeyChecking is ask.
	Confirm func(context.Context, string) (bool, error)
	// Secret is available only through the initiating interactive frontend.
	Secret func(context.Context, string) ([]byte, error)
}

type connectionConfig struct {
	options                   Options
	values                    map[string][]string
	origins                   map[string]string
	originalHost, localUser   string
	host, user, port, network string
	passphrase                func(string) ([]byte, error)
	passphraseAttempts        int
}

func environment(env []string, key string) string {
	for _, e := range slices.Backward(env) {
		k, v, ok := strings.Cut(e, "=")
		if ok && (k == key || runtime.GOOS == "windows" && strings.EqualFold(k, key)) {
			return v
		}
	}
	return ""
}
func (c *connectionConfig) get(key string) string {
	v := c.values[key]
	if len(v) > 0 {
		return v[0]
	}
	return ""
}
func (c *connectionConfig) debugEnabled() bool {
	return c.options.Diagnostic != nil && strings.HasPrefix(strings.ToUpper(c.get("loglevel")), "DEBUG")
}

func (c *connectionConfig) debugf(ctx context.Context, format string, args ...any) {
	if c.debugEnabled() {
		c.options.Diagnostic(ctx, fmt.Sprintf("SSH %q: ", c.options.Host)+fmt.Sprintf(format, args...))
	}
}

func (c *connectionConfig) enabled(key string) bool {
	v := c.get(key)
	return v == "yes" || v == "true"
}

func resolveConfig(ctx context.Context, o Options) (*connectionConfig, error) {
	if o.Host == "" || strings.HasPrefix(o.Host, "-") || strings.ContainsAny(o.Host, "\x00\r\n\t ") {
		return nil, fmt.Errorf("invalid SSH destination")
	}
	if o.Environment == nil {
		o.Environment = os.Environ()
	}
	resolved, err := resolveNativeConfig(ctx, o)
	if err != nil {
		return nil, fmt.Errorf("SSH configuration: %w", err)
	}
	c := &connectionConfig{
		options: o, network: "tcp",
		values: resolved.Values, origins: resolved.Origins,
		originalHost: resolved.OriginalHost, localUser: resolved.LocalUser,
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *connectionConfig) validate() error {
	c.host, c.user, c.port = c.get("hostname"), c.get("user"), c.get("port")
	n, err := strconv.Atoi(c.port)
	if c.host == "" || c.user == "" || err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("invalid SSH hostname, user, or port")
	}
	// Go's endpoint helpers add their own IPv6 brackets.
	if strings.HasPrefix(c.host, "[") && strings.HasSuffix(c.host, "]") && strings.Contains(c.host, ":") {
		c.host = c.host[1 : len(c.host)-1]
	}
	switch c.get("addressfamily") {
	case "inet":
		c.network = "tcp4"
	case "inet6":
		c.network = "tcp6"
	}
	for _, key := range []string{"proxycommand", "knownhostscommand", "revokedhostkeys"} {
		if v := c.get(key); v != "" && v != "none" {
			return c.unsupported(key)
		}
	}
	for _, key := range []string{"forwardx11", "gssapikeyexchange", "nohostauthenticationforlocalhost", "permitlocalcommand", "compression", "verifyhostkeydns"} {
		if c.enabled(key) {
			return c.unsupported(key)
		}
	}
	if v := c.get("forwardagent"); v != "" && v != "no" && v != "false" {
		return fmt.Errorf("ForwardAgent is not supported")
	}
	if c.get("tunnel") == "yes" || c.get("tunnel") == "point-to-point" || c.get("tunnel") == "ethernet" {
		return fmt.Errorf("Tunnel is not supported")
	}
	if v := c.get("permitremoteopen"); v != "" && (v != "any" || len(c.values["permitremoteopen"]) != 1) {
		return fmt.Errorf("PermitRemoteOpen restrictions are not supported by the built-in SSH client")
	}
	if v := c.get("bindinterface"); v != "" {
		return fmt.Errorf("BindInterface is not supported by the built-in SSH client")
	}
	if v := c.get("canonicalizehostname"); v != "" && v != "no" {
		return c.unsupported("canonicalizehostname")
	}
	policy := c.get("stricthostkeychecking")
	if policy != "yes" && policy != "true" && policy != "ask" && policy != "accept-new" {
		return fmt.Errorf("StrictHostKeyChecking must be yes, ask or accept-new")
	}
	return nil
}

func (c *connectionConfig) unsupported(key string) error {
	if where := c.origins[key]; where != "" {
		return fmt.Errorf("%s: %s is not supported by the built-in SSH client", where, key)
	}
	return fmt.Errorf("%s is not supported by the built-in SSH client", key)
}

func (c *connectionConfig) expand(value string) (string, error) {
	home := homeDirectory(c.options.Environment)
	tokens := map[byte]string{
		'd': home, 'h': c.host, 'n': c.originalHost, 'p': c.port, 'r': c.user, 'u': c.localUser,
	}
	result, err := sshconfig.Expand(value, tokens, func(key string) string { return environment(c.options.Environment, key) })
	if err != nil {
		return "", fmt.Errorf("SSH path: %w", err)
	}
	if result == "~" {
		result = home
	} else if strings.HasPrefix(result, "~/") || strings.HasPrefix(result, "~\\") {
		result = filepath.Join(home, result[2:])
	}
	if !filepath.IsAbs(result) && c.options.Directory != "" {
		result = filepath.Join(c.options.Directory, result)
	}
	return result, nil
}

// Challenge carries a server authentication prompt to the initiating frontend.
type Challenge struct {
	Title       string
	Instruction string
	Question    string
	Echo        bool
}
