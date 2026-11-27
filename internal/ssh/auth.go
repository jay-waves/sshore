package ssh

import (
	"context"
	"crypto/rsa"
	json "encoding/json/v2"
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

// Keep authentication prompts local to this handshake.
func (c connectionConfig) configureAuthentication(ctx context.Context, cc *cryptossh.ClientConfig, budgets *authenticationBudgets, authenticatedWith *string) error {
	if confirm := c.options.Confirm; confirm != nil {
		c.options.Confirm = func(ctx context.Context, prompt string) (bool, error) {
			resume := budgets.pause()
			defer resume()
			return confirm(ctx, prompt)
		}
	}
	hostKey, algorithms, err := c.hostKeys(ctx)
	if err != nil {
		return err
	}
	cc.HostKeyAlgorithms = algorithms
	cc.HostKeyCallback = func(host string, remote net.Addr, key cryptossh.PublicKey) error {
		if c.debugEnabled() {
			c.debugf(ctx, "Server host key: %s", describePublicKey(key))
		}
		err := hostKey(host, remote, key)
		if err == nil {
			c.debugf(ctx, "Server host key verified")
		}
		return err
	}

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
	var publicKeys []cryptossh.Signer
	var publicKeysLoaded bool
	readSecret := func(prompt string) ([]byte, error) {
		if promptErr != nil {
			return nil, promptErr
		}
		resume := budgets.pause()
		defer resume()
		secret, err := c.options.Secret(ctx, prompt)
		if err != nil {
			promptErr = err
		}
		return secret, err
	}
	ask := func(prompt string) (string, error) {
		secret, err := readSecret(prompt)
		defer clear(secret)
		return string(secret), err
	}
	if !c.enabled("batchmode") && c.options.Secret != nil {
		c.passphrase, c.passphraseAttempts = readSecret, limit
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
				if !publicKeysLoaded {
					publicKeysLoaded = true
					publicKeys, identityErr = c.publicKeySigners(ctx)
					if promptErr != nil {
						return nil, promptErr
					}
				}
				if len(publicKeys) == 0 {
					continue
				}
				// One signer per attempt lets the next identity run even when
				// the previous signer failed locally after its public-key probe.
				signer := publicKeys[0]
				publicKeys = publicKeys[1:]
				if c.debugEnabled() {
					key := describePublicKey(signer.PublicKey())
					*authenticatedWith = "publickey " + key
					c.debugf(ctx, "Offering public key: %s", key)
				}
				return cryptossh.PublicKeys(signer), nil
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
				*authenticatedWith = method
				c.debugf(ctx, "Trying %s authentication", method)
				return cryptossh.PasswordCallback(func() (string, error) {
					return ask("\x00password:" + c.user + "@" + c.host)
				}), nil
			case "keyboard-interactive":
				if c.get("kbdinteractiveauthentication") == "no" || c.get("kbdinteractiveauthentication") == "false" {
					continue
				}
				attempts[method]++
				*authenticatedWith = method
				c.debugf(ctx, "Trying %s authentication", method)
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
	return nil
}

func describePublicKey(key cryptossh.PublicKey) string {
	return key.Type() + " " + cryptossh.FingerprintSHA256(key)
}

func (c *connectionConfig) publicKeySigners(ctx context.Context) ([]cryptossh.Signer, error) {
	minimumRSA, err := c.minimumRSASize()
	if err != nil {
		return nil, err
	}
	if c.get("pubkeyauthentication") == "false" || c.get("pubkeyauthentication") == "no" {
		return nil, fmt.Errorf("public-key authentication is disabled")
	}
	algorithms := c.values["pubkeyacceptedalgorithms"]
	if algorithms != nil && len(algorithms) == 0 {
		return nil, errors.New("PubkeyAcceptedAlgorithms permits no supported SSH public-key signature algorithm")
	}
	paths, pathErr := c.identityPaths()
	signers, privateErr := c.privateIdentities(ctx, paths)
	signers = slices.DeleteFunc(signers, func(signer cryptossh.Signer) bool {
		return checkRSAKeySize(signer.PublicKey(), minimumRSA) != nil
	})
	byKey := make(map[string]cryptossh.Signer, len(signers))
	for _, signer := range signers {
		byKey[string(signer.PublicKey().Marshal())] = signer
	}
	var certificates []cryptossh.Signer
	seen := make(map[string]bool)
	for _, cert := range c.identityCertificates(paths) {
		signer := byKey[string(cert.Key.Marshal())]
		id := string(cert.Marshal())
		if signer != nil && !seen[id] {
			if certSigner, err := cryptossh.NewCertSigner(cert, signer); err == nil {
				seen[id] = true
				certificates = append(certificates, certSigner)
			}
		}
	}
	constrained, err := constrainIdentitySigners(append(certificates, signers...), algorithms)
	if err != nil {
		return nil, err
	}
	if len(constrained) == 0 {
		detail := errors.Join(pathErr, privateErr, errors.New("check IdentityFile and public-key authentication settings"))
		return nil, fmt.Errorf("no usable SSH public-key identity: %s", strings.ReplaceAll(detail.Error(), "\n", "; "))
	}
	return constrained, nil
}

func constrainIdentitySigners(signers []cryptossh.Signer, configured []string) ([]cryptossh.Signer, error) {
	selected := signers[:0]
	for _, signer := range signers {
		key := signer.PublicKey()
		algorithmSigner, ok := signer.(cryptossh.AlgorithmSigner)
		if !ok {
			if slices.Contains(configured, key.Type()) {
				selected = append(selected, signer)
			}
			continue
		}
		cert, certified := key.(*cryptossh.Certificate)
		if certified {
			key = cert.Key
		}
		supported := []string{key.Type()}
		if multi, ok := signer.(cryptossh.MultiAlgorithmSigner); ok {
			supported = multi.Algorithms()
		} else if key.Type() == cryptossh.KeyAlgoRSA {
			supported = []string{cryptossh.KeyAlgoRSASHA256, cryptossh.KeyAlgoRSASHA512}
		}
		names := supported
		if certified {
			names = make([]string, len(supported))
			for i, algorithm := range supported {
				names[i] = certificateAlgorithm(algorithm)
			}
		}
		var allowed []string
		for _, name := range configured {
			if i := slices.Index(names, name); i >= 0 {
				allowed = append(allowed, supported[i])
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

func certificateAlgorithm(algorithm string) string {
	return strings.TrimSuffix(algorithm, "@openssh.com") + "-cert-v01@openssh.com"
}

var knownHostsMu sync.Mutex

func (c *connectionConfig) knownHostsPaths() (paths []string, userPath string, err error) {
	for _, key := range []string{"userknownhostsfile", "globalknownhostsfile"} {
		first := len(paths)
		if key == "globalknownhostsfile" && len(c.values[key]) == 0 {
			system := systemConfigDirectory(c.options.Environment)
			paths = append(paths, filepath.Join(system, "ssh_known_hosts"), filepath.Join(system, "ssh_known_hosts2"))
			continue
		}
		for _, value := range c.values[key] {
			if value == "" || value == "none" {
				continue
			}
			path, err := c.expand(value)
			if err != nil {
				return nil, "", err
			}
			paths = append(paths, path)
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
	canRemember := userPath != "" && !c.options.RequireKnownHost && (policy == "ask" || policy == "accept-new")
	ask := canRemember && policy == "ask"
	hashHosts, batch := c.enabled("hashknownhosts"), c.enabled("batchmode")
	confirm := c.options.Confirm
	caPolicy := c.values["casignaturealgorithms"]
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
		if cert, ok := key.(*cryptossh.Certificate); ok {
			if caPolicy != nil &&
				(cert.Signature == nil || !slices.Contains(caPolicy, cert.Signature.Format)) {
				return fmt.Errorf("SSH host certificate signature is disallowed by CASignatureAlgorithms")
			}
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
	if explicit := c.values["hostkeyalgorithms"]; explicit != nil {
		// Keep the resolved configuration's order within the supported, trusted set.
		var ordered []string
		for _, algorithm := range explicit {
			if slices.Contains(allowed, algorithm) {
				ordered = append(ordered, algorithm)
			}
		}
		allowed = ordered
	}
	if caPolicy != nil && len(caPolicy) == 0 {
		allowed = slices.DeleteFunc(slices.Clone(allowed), func(algorithm string) bool {
			return strings.HasSuffix(algorithm, "-cert-v01@openssh.com")
		})
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
