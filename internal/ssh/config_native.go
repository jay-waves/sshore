package ssh

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/jay-waves/sshore/internal/sshconfig"
	cryptossh "golang.org/x/crypto/ssh"
)

func homeDirectory(env []string) string {
	key := "HOME"
	if runtime.GOOS == "windows" {
		key = "USERPROFILE"
	}
	if home := environment(env, key); home != "" {
		return home
	}
	home, _ := os.UserHomeDir()
	return home
}

func systemConfigDirectory(env []string) string {
	if runtime.GOOS != "windows" {
		return "/etc/ssh"
	}
	dir := environment(env, "ProgramData")
	if dir == "" {
		dir = `C:\ProgramData`
	}
	return filepath.Join(dir, "ssh")
}

func resolveNativeConfig(ctx context.Context, o Options) (sshconfig.Result, error) {
	home := homeDirectory(o.Environment)
	system := systemConfigDirectory(o.Environment)
	lookup := func(key string) string { return environment(o.Environment, key) }
	req := sshconfig.Request{Home: home, LookupEnv: lookup, Command: o.Command}
	target := o.Host
	var login string
	if account, host, ok := strings.Cut(target, "@"); ok {
		login, target = account, host
		if login == "" || strings.Contains(target, "@") {
			return sshconfig.Result{}, fmt.Errorf("invalid SSH destination")
		}
	}
	if target == "" || strings.HasPrefix(target, "-") {
		return sshconfig.Result{}, fmt.Errorf("invalid SSH destination")
	}
	req.Host = target
	if account, err := user.Current(); err == nil {
		req.LocalUser = account.Username
		if runtime.GOOS == "windows" {
			_, name, ok := strings.Cut(req.LocalUser, `\`)
			if ok {
				req.LocalUser = name
			}
		}
	}

	config, overrides, err := nativeOptions(o.Args)
	if err != nil {
		return sshconfig.Result{}, err
	}
	if login != "" {
		overrides = append(overrides, sshconfig.Option{Key: "user", Values: []string{login}})
	}
	req.Overrides = overrides

	userDir := filepath.Join(home, ".ssh")
	switch {
	case strings.EqualFold(config, "none"):
	case config != "":
		if !filepath.IsAbs(config) && o.Directory != "" {
			config = filepath.Join(o.Directory, config)
		}
		req.Sources = []sshconfig.Source{{Path: config, Base: userDir}}
	default:
		if home != "" {
			req.Sources = append(req.Sources, sshconfig.Source{
				Path: filepath.Join(userDir, "config"), Base: userDir, Optional: true, CheckMode: true,
			})
		}
		req.Sources = append(req.Sources, sshconfig.Source{Path: filepath.Join(system, "ssh_config"), Base: system, Optional: true})
	}

	result, err := sshconfig.Resolve(ctx, req)
	if err != nil {
		return sshconfig.Result{}, err
	}
	for key, values := range map[string][]string{
		"port": {"22"}, "connecttimeout": {"15"}, "stricthostkeychecking": {"ask"},
		"serveraliveinterval": {"15"}, "serveralivecountmax": {"3"},
		"identityfile":       {"~/.ssh/id_rsa", "~/.ssh/id_ecdsa", "~/.ssh/id_ed25519"},
		"userknownhostsfile": {"~/.ssh/known_hosts", "~/.ssh/known_hosts2"},
	} {
		if len(result.Values[key]) == 0 {
			result.Values[key] = values
		}
	}

	modern := cryptossh.SupportedAlgorithms()
	signatures := slices.Clone(modern.PublicKeyAuths)
	for _, algorithm := range modern.PublicKeyAuths {
		signatures = append(signatures, certificateAlgorithm(algorithm))
	}
	for _, policy := range []struct {
		key       string
		allowed   []string
		transport bool
	}{
		{"ciphers", modern.Ciphers, true}, {"kexalgorithms", modern.KeyExchanges, true},
		{"macs", modern.MACs, true}, {"hostkeyalgorithms", modern.HostKeys, true},
		{"pubkeyacceptedalgorithms", signatures, false},
		{"casignaturealgorithms", modern.PublicKeyAuths, false},
	} {
		spec := ""
		if configured := result.Values[policy.key]; len(configured) != 0 {
			spec = configured[0]
		} else if policy.key == "hostkeyalgorithms" || policy.key == "casignaturealgorithms" {
			// Absent an explicit policy, host-key verification keeps its
			// known_hosts preference and the SSH library's CA checks.
			continue
		}
		algorithms, err := sshconfig.Algorithms(policy.allowed, spec)
		if err != nil {
			return sshconfig.Result{}, fmt.Errorf("%s: %s: %w", result.Origins[policy.key], policy.key, err)
		}
		if policy.transport && len(algorithms) == 0 {
			return sshconfig.Result{}, fmt.Errorf("%s: %s: no supported SSH algorithm remains", result.Origins[policy.key], policy.key)
		}
		result.Values[policy.key] = algorithms
	}
	return result, nil
}

func nativeOptions(args []string) (string, []sshconfig.Option, error) {
	var config string
	var overrides []sshconfig.Option
	verbose := ""
	setFlag := func(key, value string) {
		overrides = slices.DeleteFunc(overrides, func(opt sshconfig.Option) bool { return opt.Key == key })
		overrides = append(overrides, sshconfig.Option{Key: key, Values: []string{value}})
	}
	for len(args) != 0 {
		name, value, count := SplitOption(args)
		if count == 0 || name == "" {
			return "", nil, fmt.Errorf("invalid SSH option %q", args[0])
		}
		args = args[count:]
		switch name {
		case "-L", "-R", "-D":
			overrides = append(overrides, sshconfig.Option{Key: forwardingKey(name, value), Values: []string{value}})
		case "-F":
			config = value
		case "-o":
			option, err := sshconfig.ParseOption(value)
			if err != nil {
				return "", nil, fmt.Errorf("invalid SSH -o option: %w", err)
			}
			if option.Key == "" {
				return "", nil, fmt.Errorf("SSH -o requires a configuration option")
			}
			overrides = append(overrides, option)
		case "-v", "-vv", "-vvv":
			verbose = fmt.Sprintf("DEBUG%d", len(name)-1)
		case "-4":
			setFlag("addressfamily", "inet")
		case "-6":
			setFlag("addressfamily", "inet6")
		case "-p":
			setFlag("port", value)
		case "-l":
			setFlag("user", value)
		case "-c":
			setFlag("ciphers", value)
		case "-m":
			setFlag("macs", value)
		case "-i":
			overrides = append(overrides, sshconfig.Option{Key: "identityfile", Values: []string{value}})
		case "-J":
			overrides = append(overrides, sshconfig.Option{Key: "proxyjump", Values: []string{value}})
		case "-C":
			return "", nil, fmt.Errorf("SSH compression is unsupported")
		default:
			return "", nil, fmt.Errorf("unsupported SSH option %s", name)
		}
	}
	if verbose != "" {
		overrides = append([]sshconfig.Option{{Key: "loglevel", Values: []string{verbose}}}, overrides...)
	}
	return config, overrides, nil
}
