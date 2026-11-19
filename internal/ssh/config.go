package ssh

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Options describes one owned SSH connection. Executable is used only for ssh -G.
// Environment and Directory belong to the initiating command, never daemon startup.
type Options struct {
	Host, Command, Executable, Directory string
	Args, Environment                    []string
	// Diagnostic receives negotiated algorithms only when SSH LogLevel is DEBUG or higher.
	Diagnostic io.Writer
	// Confirm receives a host key fingerprint. A nil callback requires an existing trusted key.
	Confirm func(context.Context, string) (bool, error)
	// Secret is available only through the initiating interactive frontend.
	Secret func(context.Context, string) ([]byte, error)
}

type connectionConfig struct {
	options                   Options
	values                    map[string][]string
	host, user, port, network string
	timeout                   time.Duration
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
	if o.Executable == "" {
		o.Executable = environment(o.Environment, "SSHORE_SSH")
		if o.Executable == "" {
			o.Executable = "ssh"
		}
	}
	// SSH settings are evaluated with the actual remote command for Match command.
	args := []string{"-G", "-T", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "ControlPersist=no", "-o", "RequestTTY=no", "-o", "RemoteCommand=none", "-o", "ClearAllForwardings=yes"}
	c := &connectionConfig{options: o, values: map[string][]string{}, network: "tcp", timeout: 15 * time.Second}
	for args := o.Args; len(args) > 0; {
		name, value, n := SplitOption(args)
		if ForwardingOption(name, value) {
			return nil, fmt.Errorf("SSH configuration forwarding is unsupported; use sshore forward")
		}
		args = args[max(n, 1):]
	}
	args = append(args, o.Args...)
	args = append(args, "--", o.Host)
	if o.Command != "" {
		args = append(args, o.Command)
	}
	cmd := exec.CommandContext(ctx, o.Executable, args...)
	// Match exec children may retain output pipes after the resolver is canceled.
	cmd.WaitDelay = 100 * time.Millisecond
	cmd.Env = o.Environment
	cmd.Dir = o.Directory
	hideConfigCommand(cmd)
	var diagnostic bytes.Buffer
	cmd.Stderr = &diagnostic
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("SSH configuration: %w: %s", err, strings.TrimSpace(diagnostic.String()))
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), " ")
		if ok {
			key = strings.ToLower(key)
			c.values[key] = append(c.values[key], value)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	c.host, c.user, c.port = c.get("hostname"), c.get("user"), c.get("port")
	n, err := strconv.Atoi(c.port)
	if c.host == "" || c.user == "" || err != nil || n < 1 || n > 65535 {
		return nil, fmt.Errorf("ssh -G returned an invalid connection configuration")
	}
	// ssh -G preserves IPv6 brackets. Go's endpoint helpers add their own.
	if strings.HasPrefix(c.host, "[") && strings.HasSuffix(c.host, "]") && strings.Contains(c.host, ":") {
		c.host = c.host[1 : len(c.host)-1]
	}
	switch c.get("addressfamily") {
	case "inet":
		c.network = "tcp4"
	case "inet6":
		c.network = "tcp6"
	}
	if value := c.get("connecttimeout"); value != "" && value != "none" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || int64(n) > int64((1<<63-1)/time.Second) {
			return nil, fmt.Errorf("invalid ConnectTimeout")
		}
		c.timeout = time.Duration(n) * time.Second
	}
	for _, key := range []string{"proxycommand", "knownhostscommand", "pkcs11provider", "revokedhostkeys"} {
		if v := c.get(key); v != "" && v != "none" {
			return nil, fmt.Errorf("%s is not supported by the built-in SSH client", key)
		}
	}
	for _, key := range []string{"forwardx11", "gssapiauthentication", "hostbasedauthentication", "permitlocalcommand", "compression", "verifyhostkeydns"} {
		if c.enabled(key) {
			return nil, fmt.Errorf("%s is not supported by the built-in SSH client", key)
		}
	}
	if v := c.get("forwardagent"); v != "" && v != "no" && v != "false" {
		return nil, fmt.Errorf("ForwardAgent is not supported")
	}
	if c.get("tunnel") == "yes" || c.get("tunnel") == "point-to-point" || c.get("tunnel") == "ethernet" {
		return nil, fmt.Errorf("Tunnel is not supported")
	}
	if v := c.get("permitremoteopen"); v != "" && v != "any" {
		return nil, fmt.Errorf("PermitRemoteOpen restrictions are not supported by the built-in SSH client")
	}
	if v := c.get("bindinterface"); v != "" {
		return nil, fmt.Errorf("BindInterface is not supported by the built-in SSH client")
	}
	policy := c.get("stricthostkeychecking")
	if policy != "yes" && policy != "true" && policy != "ask" && policy != "accept-new" {
		return nil, fmt.Errorf("StrictHostKeyChecking must be yes, ask or accept-new")
	}
	return c, nil
}
func (c *connectionConfig) expand(value string) (string, error) {
	home := environment(c.options.Environment, "HOME")
	if runtime.GOOS == "windows" {
		home = environment(c.options.Environment, "USERPROFILE")
	}
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] == '%' {
			i++
			if i == len(value) {
				return "", fmt.Errorf("incomplete SSH path token")
			}
			switch value[i] {
			case '%':
				out.WriteByte('%')
			case 'd':
				out.WriteString(home)
			case 'h':
				out.WriteString(c.host)
			case 'r':
				out.WriteString(c.user)
			case 'p':
				out.WriteString(c.port)
			case 'n':
				out.WriteString(c.options.Host)
			default:
				return "", fmt.Errorf("unsupported SSH path token %%%c", value[i])
			}
			continue
		}
		if value[i] == '$' && i+1 < len(value) && value[i+1] == '{' {
			end := strings.IndexByte(value[i+2:], '}')
			if end < 0 {
				return "", fmt.Errorf("incomplete SSH environment variable")
			}
			key := value[i+2 : i+2+end]
			v := environment(c.options.Environment, key)
			if v == "" {
				return "", fmt.Errorf("SSH environment variable %s is not set", key)
			}
			out.WriteString(v)
			i += end + 2
			continue
		}
		out.WriteByte(value[i])
	}
	result, err := c.expandSystemPath(out.String())
	if err != nil {
		return "", err
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
