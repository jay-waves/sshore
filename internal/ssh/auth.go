package ssh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/skeema/knownhosts"
	cryptossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	xknownhosts "golang.org/x/crypto/ssh/knownhosts"
)

func (c *connectionConfig) authentication(ctx context.Context) ([]cryptossh.AuthMethod, net.Conn, error) {
	if c.get("pubkeyauthentication") == "false" || c.get("pubkeyauthentication") == "no" {
		return nil, nil, fmt.Errorf("built-in SSH requires public-key authentication through an agent")
	}
	address := c.get("identityagent")
	if address == "none" {
		return nil, nil, fmt.Errorf("SSH agent is disabled")
	}
	if address == "" || address == "SSH_AUTH_SOCK" {
		address = environment(c.options.Environment, "SSH_AUTH_SOCK")
	}
	if address == "" {
		address = defaultAgent()
	}
	if address == "" {
		return nil, nil, fmt.Errorf("SSH agent unavailable; start an agent and load the key using ssh-add")
	}
	address, err := c.expand(address)
	if err != nil {
		return nil, nil, err
	}
	conn, err := connectAgent(ctx, address)
	if err != nil {
		return nil, nil, fmt.Errorf("SSH agent unavailable: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	signers, err := agent.NewClient(conn).Signers()
	if err != nil {
		conn.Close()
		return nil, nil, fmt.Errorf("list SSH agent keys: %w", err)
	}
	// Match configured public identities without ever reading private key material.
	allowed := map[string]bool{}
	for i, name := range append(append([]string(nil), c.values["identityfile"]...), c.values["certificatefile"]...) {
		if name == "none" {
			continue
		}
		path, err := c.expand(name)
		if err != nil {
			conn.Close()
			return nil, nil, err
		}
		paths := []string{path + ".pub"}
		if i >= len(c.values["identityfile"]) || strings.HasSuffix(path, ".pub") {
			paths = []string{path}
		}
		for _, path := range paths {
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			key, _, _, _, err := cryptossh.ParseAuthorizedKey(data)
			if err == nil {
				allowed[string(key.Marshal())] = true
			}
		}
	}
	selected := make([]cryptossh.Signer, 0, len(signers))
	others := make([]cryptossh.Signer, 0, len(signers))
	for _, signer := range signers {
		key, err := cryptossh.ParsePublicKey(signer.PublicKey().Marshal())
		if err != nil {
			conn.Close()
			return nil, nil, err
		}
		matches := allowed[string(key.Marshal())]
		if certificate, ok := key.(*cryptossh.Certificate); ok {
			matches = matches || allowed[string(certificate.Key.Marshal())]
		}
		if matches {
			selected = append(selected, signer)
		} else {
			others = append(others, signer)
		}
	}
	if !c.enabled("identitiesonly") {
		selected = append(selected, others...)
	}
	constrained := make([]cryptossh.Signer, 0, len(selected))
	allowedAlgorithms := cryptossh.SupportedAlgorithms().PublicKeyAuths
	for _, signer := range selected {
		if algorithmSigner, ok := signer.(cryptossh.AlgorithmSigner); ok {
			var supported []string
			if multi, ok := signer.(cryptossh.MultiAlgorithmSigner); ok {
				supported = multi.Algorithms()
			} else {
				keyType := strings.TrimSuffix(signer.PublicKey().Type(), "-cert-v01@openssh.com")
				supported = []string{keyType}
				if keyType == cryptossh.KeyAlgoRSA {
					supported = []string{cryptossh.KeyAlgoRSASHA256, cryptossh.KeyAlgoRSASHA512}
				}
			}
			var allowed []string
			for _, algorithm := range allowedAlgorithms {
				if slices.Contains(supported, algorithm) && c.acceptsSignature(signer.PublicKey(), algorithm) {
					allowed = append(allowed, algorithm)
				}
			}
			if len(allowed) == 0 {
				continue
			}
			wrapped, err := cryptossh.NewSignerWithAlgorithms(algorithmSigner, allowed)
			if err != nil {
				conn.Close()
				return nil, nil, err
			}
			constrained = append(constrained, wrapped)
		} else if slices.Contains(allowedAlgorithms, signer.PublicKey().Type()) && c.acceptsSignature(signer.PublicKey(), signer.PublicKey().Type()) {
			constrained = append(constrained, signer)
		}
	}
	selected = constrained
	if len(selected) == 0 {
		conn.Close()
		return nil, nil, fmt.Errorf("no matching SSH agent key; load the configured identity using ssh-add")
	}
	return []cryptossh.AuthMethod{cryptossh.PublicKeys(selected...)}, conn, nil
}

func (c *connectionConfig) acceptsSignature(key cryptossh.PublicKey, algorithm string) bool {
	configured := c.get("pubkeyacceptedalgorithms")
	if configured == "" {
		return true
	}
	if strings.HasSuffix(key.Type(), "-cert-v01@openssh.com") {
		algorithm += "-cert-v01@openssh.com"
	}
	return slices.Contains(strings.Split(configured, ","), algorithm)
}

var knownHostsWrites sync.Mutex

func (c *connectionConfig) hostKeys(ctx context.Context) (cryptossh.HostKeyCallback, []string, error) {
	var paths []string
	userPath := ""
	for _, key := range []string{"userknownhostsfile", "globalknownhostsfile"} {
		for _, value := range c.values[key] {
			if value == "none" {
				continue
			}
			// ssh -G does not quote path lists. Prefer a whole existing filename, then
			// resolve separate entries; ambiguous missing paths are rejected.
			whole, err := c.expand(value)
			if err != nil {
				return nil, nil, err
			}
			entries := []string{value}
			if _, err := os.Stat(whole); err != nil {
				entries = strings.Fields(value)
				if len(entries) > 1 {
					for _, entry := range entries {
						if !filepath.IsAbs(entry) && !strings.HasPrefix(entry, "~/") && !strings.HasPrefix(entry, "~\\") {
							return nil, nil, fmt.Errorf("ambiguous known_hosts path list from ssh -G: %q; use existing absolute files", value)
						}
					}
				}
			}
			for _, entry := range entries {
				path, err := c.expand(entry)
				if err != nil {
					return nil, nil, err
				}
				if key == "userknownhostsfile" && userPath == "" {
					userPath = path
				}
				if _, err = os.Stat(path); err == nil {
					paths = append(paths, path)
				} else if !errors.Is(err, os.ErrNotExist) {
					return nil, nil, err
				}
			}
		}
	}
	db, err := knownhosts.NewDB(paths...)
	if err != nil {
		return nil, nil, err
	}
	verifyHost := c.host
	if alias := c.get("hostkeyalias"); alias != "" && alias != "none" {
		verifyHost = alias
	}
	verify := net.JoinHostPort(verifyHost, c.port)
	callback := func(_ string, remote net.Addr, key cryptossh.PublicKey) error {
		err := db.HostKeyCallback()(verify, remote, key)
		if err == nil {
			return nil
		}
		if !knownhosts.IsHostUnknown(err) {
			return err
		}
		policy := c.get("stricthostkeychecking")
		if policy == "true" || policy == "yes" || userPath == "" {
			return err
		}
		if policy == "ask" {
			if c.options.Confirm == nil {
				return fmt.Errorf("unknown SSH host %s (%s); connect interactively to trust it", verify, cryptossh.FingerprintSHA256(key))
			}
			ok, e := c.options.Confirm(ctx, fmt.Sprintf("Trust SSH host %s (%s)?", verify, cryptossh.FingerprintSHA256(key)))
			if e != nil {
				return e
			}
			if !ok {
				return fmt.Errorf("SSH host key not trusted")
			}
		}
		knownHostsWrites.Lock()
		defer knownHostsWrites.Unlock()
		latestPaths := append([]string(nil), paths...)
		if _, e := os.Stat(userPath); e == nil && !slices.Contains(latestPaths, userPath) {
			latestPaths = append(latestPaths, userPath)
		}
		latest, e := knownhosts.NewDB(latestPaths...)
		if e != nil {
			return e
		}
		e = latest.HostKeyCallback()(verify, remote, key)
		if e == nil {
			return nil
		}
		if !knownhosts.IsHostUnknown(e) {
			return e
		}
		if e = os.MkdirAll(filepath.Dir(userPath), 0700); e != nil {
			return e
		}
		f, e := os.OpenFile(userPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if e != nil {
			return e
		}
		defer f.Close()
		address := knownhosts.Normalize(verify)
		if c.enabled("hashknownhosts") {
			address = xknownhosts.HashHostname(address)
		}
		_, e = f.WriteString(knownhosts.Line([]string{address}, key) + "\n")
		return e
	}
	allowed := cryptossh.SupportedAlgorithms().HostKeys
	preferred := db.HostKeyAlgorithms(verify)
	if len(preferred) > 0 {
		allowed = slices.DeleteFunc(append([]string(nil), preferred...), func(a string) bool { return !slices.Contains(cryptossh.SupportedAlgorithms().HostKeys, a) })
	}
	if explicit := c.get("hostkeyalgorithms"); explicit != "" {
		// Keep the resolved configuration's order within the supported, trusted set.
		var ordered []string
		for _, algorithm := range strings.Split(explicit, ",") {
			if slices.Contains(allowed, algorithm) && !slices.Contains(ordered, algorithm) {
				ordered = append(ordered, algorithm)
			}
		}
		allowed = ordered
	}
	if len(allowed) == 0 {
		return nil, nil, fmt.Errorf("no supported modern SSH host key algorithm")
	}
	return callback, allowed, nil
}
