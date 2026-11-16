package ssh

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/skeema/knownhosts"
	cryptossh "golang.org/x/crypto/ssh"
	xknownhosts "golang.org/x/crypto/ssh/knownhosts"
)

func (c *connectionConfig) authentication(ctx context.Context) ([]cryptossh.AuthMethod, net.Conn, error) {
	if preferred := c.get("preferredauthentications"); preferred != "" && !slices.Contains(strings.Split(preferred, ","), "publickey") {
		return nil, nil, fmt.Errorf("PreferredAuthentications excludes publickey; built-in SSH requires public-key authentication")
	}
	minimumRSA, err := c.minimumRSASize()
	if err != nil {
		return nil, nil, err
	}
	if c.get("pubkeyauthentication") == "false" || c.get("pubkeyauthentication") == "no" {
		return nil, nil, fmt.Errorf("built-in SSH requires public-key authentication")
	}
	signers, conn, agentErr := c.agentSigners(ctx)
	files, allowed, certificates, fileErr := c.fileIdentities(ctx)
	selected := make([]cryptossh.Signer, 0, len(signers)+len(files))
	seen := map[string]bool{}
	add := func(signer cryptossh.Signer) {
		key, parseErr := cryptossh.ParsePublicKey(signer.PublicKey().Marshal())
		if parseErr != nil {
			return
		}
		if checkRSAKeySize(key, minimumRSA) != nil || seen[string(key.Marshal())] {
			return
		}
		seen[string(key.Marshal())] = true
		selected = append(selected, signer)
	}
	var others []cryptossh.Signer
	for _, signer := range signers {
		key, parseErr := cryptossh.ParsePublicKey(signer.PublicKey().Marshal())
		if parseErr != nil {
			continue
		}
		matches := allowed[string(key.Marshal())]
		if cert, ok := key.(*cryptossh.Certificate); ok {
			matches = matches || allowed[string(cert.Key.Marshal())]
		}
		if matches {
			add(signer)
		} else {
			others = append(others, signer)
		}
	}
	for _, signer := range files {
		add(signer)
	}
	// Certificates reuse the chosen agent or file signer for their underlying key.
	for _, cert := range certificates {
		for _, signer := range append([]cryptossh.Signer(nil), selected...) {
			if string(cert.Key.Marshal()) == string(signer.PublicKey().Marshal()) {
				certSigner, e := cryptossh.NewCertSigner(cert, signer)
				if e == nil {
					add(certSigner)
				}
				break
			}
		}
	}
	// Try configured certificates before their raw identity and unrelated agent keys.
	slices.SortStableFunc(selected, func(a, b cryptossh.Signer) int {
		ak, _ := cryptossh.ParsePublicKey(a.PublicKey().Marshal())
		bk, _ := cryptossh.ParsePublicKey(b.PublicKey().Marshal())
		_, ac := ak.(*cryptossh.Certificate)
		_, bc := bk.(*cryptossh.Certificate)
		if ac == bc {
			return 0
		}
		if ac {
			return -1
		}
		return 1
	})
	if !c.enabled("identitiesonly") {
		for _, signer := range others {
			add(signer)
		}
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
				if conn != nil {
					conn.Close()
				}
				return nil, nil, err
			}
			constrained = append(constrained, wrapped)
		} else if slices.Contains(allowedAlgorithms, signer.PublicKey().Type()) && c.acceptsSignature(signer.PublicKey(), signer.PublicKey().Type()) {
			constrained = append(constrained, signer)
		}
	}
	selected = constrained
	if len(selected) == 0 {
		if conn != nil {
			conn.Close()
		}
		return nil, nil, fmt.Errorf("no usable SSH public-key identity: %w", errors.Join(agentErr, fileErr, errors.New("check IdentityFile and SSH agent keys")))
	}
	return []cryptossh.AuthMethod{cryptossh.PublicKeys(selected...)}, conn, nil
}

func (c *connectionConfig) minimumRSASize() (int, error) {
	value := c.get("requiredrsasize")
	if value == "" {
		return 1024, nil
	}
	minimum, err := strconv.Atoi(value)
	if err != nil || minimum < 1024 {
		return 0, fmt.Errorf("RequiredRSASize must be at least 1024 bits")
	}
	return minimum, nil
}

func checkRSAKeySize(key cryptossh.PublicKey, minimum int) error {
	if certificate, ok := key.(*cryptossh.Certificate); ok {
		key = certificate.Key
	}
	if public, ok := key.(cryptossh.CryptoPublicKey); ok {
		if rsaKey, ok := public.CryptoPublicKey().(*rsa.PublicKey); ok && rsaKey.N.BitLen() < minimum {
			return fmt.Errorf("RSA key has %d bits; RequiredRSASize requires %d", rsaKey.N.BitLen(), minimum)
		}
	}
	return nil
}

func (c *connectionConfig) acceptsSignature(key cryptossh.PublicKey, algorithm string) bool {
	configured := c.get("pubkeyacceptedalgorithms")
	if configured == "" {
		configured = c.get("pubkeyacceptedkeytypes")
	}
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
					absolute := *c
					absolute.options.Directory = ""
					for _, entry := range entries {
						path, err := absolute.expand(entry)
						if err != nil {
							return nil, nil, err
						}
						if !filepath.IsAbs(path) {
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
	verify := net.JoinHostPort(c.host, c.port)
	if alias := c.get("hostkeyalias"); alias != "" && alias != "none" {
		verify = alias
		if _, _, err := net.SplitHostPort(alias); err != nil {
			verify = net.JoinHostPort(alias, "22")
		}
	}
	minimumRSA, err := c.minimumRSASize()
	if err != nil {
		return nil, nil, err
	}
	callback := func(_ string, remote net.Addr, key cryptossh.PublicKey) error {
		if err := checkRSAKeySize(key, minimumRSA); err != nil {
			return err
		}
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
			if c.enabled("batchmode") {
				return fmt.Errorf("unknown SSH host %s; BatchMode disables host key confirmation", verify)
			}
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
		f, e := os.OpenFile(userPath, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0600)
		if e != nil {
			return e
		}
		defer f.Close()
		info, e := f.Stat()
		if e != nil {
			return e
		}
		if info.Size() > 0 {
			var last [1]byte
			if _, e = f.ReadAt(last[:], info.Size()-1); e != nil {
				return e
			}
			if last[0] != '\n' {
				if _, e = f.WriteString("\n"); e != nil {
					return e
				}
			}
		}
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
