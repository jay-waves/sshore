package ssh

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// Keep callbacks and their agent connection local to this handshake.
func (c connectionConfig) configureAuthentication(ctx context.Context, cc *cryptossh.ClientConfig, budgets *authenticationBudgets) (func(), error) {
	if confirm := c.options.Confirm; confirm != nil {
		c.options.Confirm = func(ctx context.Context, prompt string) (bool, error) {
			resume := budgets.pause()
			defer resume()
			return confirm(ctx, prompt)
		}
	}
	hostKey, algorithms, err := c.hostKeys(ctx)
	if err != nil {
		return nil, err
	}
	cc.HostKeyCallback, cc.HostKeyAlgorithms = hostKey, algorithms

	var agent io.Closer
	// Let the server accept none authentication before requiring local identities.
	attempts := map[string]int{}
	preferred := c.get("preferredauthentications")
	if preferred == "" {
		preferred = "publickey,keyboard-interactive,password"
	}
	limit, e := strconv.Atoi(c.get("numberofpasswordprompts"))
	if e != nil {
		limit = 3
	}
	var identityErr, promptErr error
	ask := func(prompt string) (string, error) {
		resume := budgets.pause()
		defer resume()
		secret, err := c.options.Secret(ctx, prompt)
		defer clear(secret)
		if err != nil {
			promptErr = err
			return "", err
		}
		return string(secret), nil
	}
	cc.AuthCallback = func(state *cryptossh.ClientAuthContext) (cryptossh.AuthMethod, error) {
		// SSH may ask for another method after a method callback fails.
		// A failed local prompt ends authentication for this connection.
		if promptErr != nil {
			return nil, promptErr
		}
		for method := range strings.SplitSeq(preferred, ",") {
			if !slices.Contains(state.AllowedMethods, method) {
				continue
			}
			if method == "publickey" {
				if attempts[method] > 0 {
					continue
				}
				attempts[method]++
				method, agentConn, err := c.authentication(ctx)
				identityErr = err
				if err != nil {
					continue
				}
				agent = agentConn
				return method, nil
			}
			if c.enabled("batchmode") || c.options.Secret == nil || attempts[method] >= limit {
				continue
			}
			switch method {
			case "password":
				if c.get("passwordauthentication") == "no" || c.get("passwordauthentication") == "false" {
					continue
				}
				attempts[method]++
				return cryptossh.PasswordCallback(func() (string, error) { return ask("\x00password:" + c.user + "@" + c.host) }), nil
			case "keyboard-interactive":
				if c.get("kbdinteractiveauthentication") == "no" || c.get("kbdinteractiveauthentication") == "false" {
					continue
				}
				attempts[method]++
				return cryptossh.KeyboardInteractive(func(name, instruction string, questions []string, echoes []bool) ([]string, error) {
					if len(questions) != len(echoes) {
						return nil, fmt.Errorf("invalid SSH authentication challenge")
					}
					answers := make([]string, len(questions))
					for i, question := range questions {
						prompt := Challenge{Title: name, Instruction: instruction, Question: question, Echo: echoes[i]}
						if i > 0 {
							prompt.Title, prompt.Instruction = "", ""
						}
						data, err := json.Marshal(prompt)
						if err != nil {
							return nil, err
						}
						answers[i], err = ask("\x00challenge:" + string(data))
						if err != nil {
							return nil, err
						}
					}
					return answers, nil
				}), nil
			}
		}
		if attempts["password"] > 0 || attempts["keyboard-interactive"] > 0 {
			return nil, fmt.Errorf("server rejected SSH authentication for %s@%s; check the login credentials and server authentication policy", c.user, c.host)
		}
		return nil, identityErr
	}
	return func() {
		if agent != nil {
			_ = agent.Close()
		}
	}, nil
}

func (c *connectionConfig) authentication(ctx context.Context) (_ cryptossh.AuthMethod, agentConn io.Closer, _ error) {
	if preferred := c.get("preferredauthentications"); preferred != "" && !slices.Contains(strings.Split(preferred, ","), "publickey") {
		return nil, nil, fmt.Errorf("PreferredAuthentications excludes publickey; public-key authentication is disabled")
	}
	minimumRSA, err := c.minimumRSASize()
	if err != nil {
		return nil, nil, err
	}
	if c.get("pubkeyauthentication") == "false" || c.get("pubkeyauthentication") == "no" {
		return nil, nil, fmt.Errorf("public-key authentication is disabled")
	}
	signers, conn, agentErr := c.agentSigners(ctx)
	defer func() {
		if conn != nil && agentConn == nil {
			conn.Close()
		}
	}()
	allowed, certificates, fileErr := c.publicIdentities()
	selected := selectIdentitySigners(signers, allowed, certificates, c.enabled("identitiesonly"), minimumRSA)
	constrained, err := constrainIdentitySigners(selected, c.signatureAlgorithms())
	if err != nil {
		return nil, nil, err
	}
	if len(constrained) == 0 {
		detail := errors.Join(fileErr, agentErr, errors.New("load a key with ssh-add; check IdentityFile public keys and SSH agent"))
		return nil, nil, fmt.Errorf("no usable SSH public-key identity: %s", strings.ReplaceAll(detail.Error(), "\n", "; "))
	}
	return cryptossh.PublicKeys(constrained...), conn, nil
}

func selectIdentitySigners(signers []cryptossh.Signer, allowed map[string]bool, certificates []*cryptossh.Certificate, identitiesOnly bool, minimumRSA int) []cryptossh.Signer {
	var preferredCertificates, preferredKeys, others []cryptossh.Signer
	// Every entry records an observed key. Only allowed raw keys retain a
	// signer for use with a configured certificate.
	keys := make(map[string]cryptossh.Signer, len(signers))
	for _, signer := range signers {
		// Agent keys expose wire blobs. Decode each once for identity matching,
		// certificate classification and the minimum RSA size check.
		key, err := cryptossh.ParsePublicKey(signer.PublicKey().Marshal())
		if err != nil || checkRSAKeySize(key, minimumRSA) != nil {
			continue
		}
		id := string(key.Marshal())
		if _, seen := keys[id]; seen {
			continue
		}
		keys[id] = nil
		cert, certified := key.(*cryptossh.Certificate)
		switch {
		case certified && (allowed[id] || allowed[string(cert.Key.Marshal())]):
			preferredCertificates = append(preferredCertificates, signer)
		case allowed[id]:
			preferredKeys = append(preferredKeys, signer)
			keys[id] = signer
		case !identitiesOnly:
			others = append(others, signer)
		}
	}
	// Certificates reuse the chosen agent signer for their underlying key.
	for _, cert := range certificates {
		signer := keys[string(cert.Key.Marshal())]
		if signer == nil {
			continue
		}
		id := string(cert.Marshal())
		if _, seen := keys[id]; seen {
			continue
		}
		certSigner, err := cryptossh.NewCertSigner(cert, signer)
		if err == nil {
			keys[id] = nil
			preferredCertificates = append(preferredCertificates, certSigner)
		}
	}
	return slices.Concat(preferredCertificates, preferredKeys, others)
}

func constrainIdentitySigners(signers []cryptossh.Signer, configured []string) ([]cryptossh.Signer, error) {
	selected := signers[:0]
	modern := cryptossh.SupportedAlgorithms().PublicKeyAuths
	for _, signer := range signers {
		keyType := signer.PublicKey().Type()
		algorithmSigner, ok := signer.(cryptossh.AlgorithmSigner)
		if !ok {
			if slices.Contains(modern, keyType) && allowsSignature(configured, keyType, keyType) {
				selected = append(selected, signer)
			}
			continue
		}
		var supported []string
		if multi, ok := signer.(cryptossh.MultiAlgorithmSigner); ok {
			supported = multi.Algorithms()
		} else {
			algorithm := strings.TrimSuffix(keyType, "-cert-v01@openssh.com")
			supported = []string{algorithm}
			if algorithm == cryptossh.KeyAlgoRSA {
				supported = []string{cryptossh.KeyAlgoRSASHA256, cryptossh.KeyAlgoRSASHA512}
			}
		}
		var allowed []string
		for _, algorithm := range modern {
			if slices.Contains(supported, algorithm) && allowsSignature(configured, keyType, algorithm) {
				allowed = append(allowed, algorithm)
			}
		}
		if len(allowed) == 0 {
			continue
		}
		wrapped, err := cryptossh.NewSignerWithAlgorithms(algorithmSigner, allowed)
		if err != nil {
			return nil, err
		}
		selected = append(selected, wrapped)
	}
	return selected, nil
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

func (c *connectionConfig) signatureAlgorithms() []string {
	configured := c.get("pubkeyacceptedalgorithms")
	if configured == "" {
		configured = c.get("pubkeyacceptedkeytypes")
	}
	if configured == "" {
		return nil
	}
	return strings.Split(configured, ",")
}

func allowsSignature(configured []string, keyType, algorithm string) bool {
	if configured == nil {
		return true
	}
	if strings.HasSuffix(keyType, "-cert-v01@openssh.com") {
		algorithm += "-cert-v01@openssh.com"
	}
	return slices.Contains(configured, algorithm)
}

var knownHostsMu sync.Mutex

func (c *connectionConfig) knownHostsPaths() (paths []string, userPath string, err error) {
	absolute := *c
	absolute.options.Directory = ""
	for _, key := range []string{"userknownhostsfile", "globalknownhostsfile"} {
		first := len(paths)
		for _, value := range c.values[key] {
			if value == "" || value == "none" {
				continue
			}
			whole, err := c.expand(value)
			if err != nil {
				return nil, "", err
			}
			// ssh -G does not quote path lists. An existing whole filename
			// wins; otherwise a list must contain unambiguous absolute paths.
			entries := strings.Fields(value)
			if len(entries) <= 1 {
				paths = append(paths, whole)
				continue
			}
			if _, err = os.Stat(whole); err == nil {
				paths = append(paths, whole)
				continue
			} else if !errors.Is(err, os.ErrNotExist) && !isInvalidFilename(err) {
				return nil, "", err
			}
			for _, entry := range entries {
				path, err := absolute.expand(entry)
				if err != nil {
					return nil, "", err
				}
				if !filepath.IsAbs(path) {
					return nil, "", fmt.Errorf("ambiguous known_hosts path list from ssh -G: %q; use existing absolute files", value)
				}
				paths = append(paths, path)
			}
		}
		if key == "userknownhostsfile" && len(paths) > first {
			userPath = paths[first]
		}
	}
	return paths, userPath, nil
}

func loadKnownHosts(paths []string) (*knownhosts.HostKeyDB, error) {
	var existing []string
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			existing = append(existing, path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return knownhosts.NewDB(existing...)
}

func (c *connectionConfig) hostKeys(ctx context.Context) (cryptossh.HostKeyCallback, []string, error) {
	paths, userPath, err := c.knownHostsPaths()
	if err != nil {
		return nil, nil, err
	}
	knownHostsMu.Lock()
	db, err := loadKnownHosts(paths)
	knownHostsMu.Unlock()
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
	policy := c.get("stricthostkeychecking")
	canRemember := userPath != "" && policy != "true" && policy != "yes"
	ask := canRemember && policy == "ask"
	hashHosts, batch := c.enabled("hashknownhosts"), c.enabled("batchmode")
	confirm := c.options.Confirm
	check := func(remote net.Addr, key cryptossh.PublicKey, remember bool) error {
		knownHostsMu.Lock()
		defer knownHostsMu.Unlock()
		latest, err := loadKnownHosts(paths)
		if err != nil {
			return err
		}
		err = latest.HostKeyCallback()(verify, remote, key)
		if !remember || !knownhosts.IsHostUnknown(err) {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		address := knownhosts.Normalize(verify)
		if hashHosts {
			address = xknownhosts.HashHostname(address)
		}
		return appendKnownHost(userPath, knownhosts.Line([]string{address}, key))
	}
	callback := func(_ string, remote net.Addr, key cryptossh.PublicKey) error {
		if err := checkRSAKeySize(key, minimumRSA); err != nil {
			return err
		}
		err := check(remote, key, canRemember && !ask)
		if !ask || !knownhosts.IsHostUnknown(err) {
			return err
		}
		if batch {
			return fmt.Errorf("unknown SSH host %s; BatchMode disables host key confirmation", verify)
		}
		if confirm == nil {
			return fmt.Errorf("unknown SSH host %s (%s); connect interactively to trust it", verify, cryptossh.FingerprintSHA256(key))
		}
		ok, err := confirm(ctx, fmt.Sprintf("Trust SSH host %s (%s)?", verify, cryptossh.FingerprintSHA256(key)))
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("SSH host key not trusted")
		}
		return check(remote, key, true)
	}
	supported := cryptossh.SupportedAlgorithms().HostKeys
	allowed := supported
	if preferred := db.HostKeyAlgorithms(verify); len(preferred) > 0 {
		allowed = slices.DeleteFunc(preferred, func(a string) bool { return !slices.Contains(supported, a) })
	}
	if explicit := c.get("hostkeyalgorithms"); explicit != "" {
		// Keep the resolved configuration's order within the supported, trusted set.
		var ordered []string
		for algorithm := range strings.SplitSeq(explicit, ",") {
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

func appendKnownHost(path, line string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], info.Size()-1); err != nil {
			return err
		}
		if last[0] != '\n' {
			line = "\n" + line
		}
	}
	_, err = f.WriteString(line + "\n")
	return err
}
