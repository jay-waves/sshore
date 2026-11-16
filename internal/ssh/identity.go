package ssh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"

	cryptossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

func (c *connectionConfig) agentSigners(ctx context.Context) ([]cryptossh.Signer, net.Conn, error) {
	address := c.get("identityagent")
	if address == "none" {
		return nil, nil, nil
	}
	if strings.HasPrefix(address, "$") && !strings.HasPrefix(address, "${") {
		name := strings.TrimPrefix(address, "$")
		address = environment(c.options.Environment, name)
		if address == "" {
			return nil, nil, fmt.Errorf("IdentityAgent environment variable %s is unset or empty", name)
		}
	}
	if address == "" || address == "SSH_AUTH_SOCK" {
		address = environment(c.options.Environment, "SSH_AUTH_SOCK")
	}
	if address == "" {
		address = defaultAgent()
	}
	if address == "" {
		return nil, nil, nil
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
		return nil, nil, err
	}
	return signers, conn, nil
}

func publicIdentity(path string) cryptossh.PublicKey {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	key, _, _, _, err := cryptossh.ParseAuthorizedKey(data)
	if err != nil {
		return nil
	}
	return key
}

func (c *connectionConfig) fileIdentities(ctx context.Context) ([]cryptossh.Signer, map[string]bool, []*cryptossh.Certificate, error) {
	var signers []cryptossh.Signer
	var certificates []*cryptossh.Certificate
	allowed := map[string]bool{}
	var failures []error
	remember := func(key cryptossh.PublicKey) {
		if key == nil {
			return
		}
		allowed[string(key.Marshal())] = true
		if cert, ok := key.(*cryptossh.Certificate); ok {
			certificates = append(certificates, cert)
			allowed[string(cert.Key.Marshal())] = true
		}
	}
	for _, name := range c.values["identityfile"] {
		if name == "none" {
			continue
		}
		path, err := c.expand(name)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		remember(publicIdentity(path + ".pub"))
		remember(publicIdentity(path + "-cert.pub"))
		data, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				failures = append(failures, fmt.Errorf("identity %s: %w", path, err))
			}
			continue
		}
		if key, _, _, _, e := cryptossh.ParseAuthorizedKey(data); e == nil {
			remember(key)
			continue
		}
		if runtime.GOOS != "windows" {
			info, statErr := os.Stat(path)
			if statErr != nil || info.Mode().Perm()&0077 != 0 {
				failures = append(failures, fmt.Errorf("identity %s: private key permissions must exclude group and other users", path))
				continue
			}
		}
		// OpenSSH encrypted keys embed the public key even without a sidecar.
		_, parseErr := cryptossh.ParseRawPrivateKey(data)
		var encrypted *cryptossh.PassphraseMissingError
		if errors.As(parseErr, &encrypted) {
			remember(encrypted.PublicKey)
		}
		signer, err := c.fileSigner(ctx, path, data)
		if err != nil {
			failures = append(failures, fmt.Errorf("identity %s: %w", path, err))
			continue
		}
		remember(signer.PublicKey())
		signers = append(signers, signer)
	}
	for _, name := range c.values["certificatefile"] {
		if name == "none" {
			continue
		}
		path, err := c.expand(name)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		remember(publicIdentity(path))
	}
	return signers, allowed, certificates, errors.Join(failures...)
}

func (c *connectionConfig) fileSigner(ctx context.Context, path string, data []byte) (cryptossh.Signer, error) {
	if c.options.IdentityCache != nil {
		if signer, ok := c.options.IdentityCache.Get(path, data); ok {
			return signer, nil
		}
	}
	key, err := cryptossh.ParseRawPrivateKey(data)
	if err == nil {
		return cryptossh.NewSignerFromKey(key)
	}
	var encrypted *cryptossh.PassphraseMissingError
	if !errors.As(err, &encrypted) {
		return nil, err
	}
	if c.enabled("batchmode") || c.options.Passphrase == nil {
		return nil, fmt.Errorf("encrypted key requires an interactive passphrase")
	}
	public := encrypted.PublicKey
	if public == nil {
		public = publicIdentity(path + ".pub")
	}
	if public == nil {
		return nil, fmt.Errorf("encrypted key requires a public .pub file")
	}
	return &fileSigner{ctx: ctx, path: path, data: data, public: public, prompt: c.options.Passphrase, cache: c.options.IdentityCache}, nil
}

// The public key is offered before unlocking: rejected identities never prompt.
type fileSigner struct {
	ctx    context.Context
	path   string
	data   []byte
	public cryptossh.PublicKey
	prompt func(context.Context, string) ([]byte, error)
	cache  *IdentityCache
	mu     sync.Mutex
	signer cryptossh.Signer
}

func (s *fileSigner) PublicKey() cryptossh.PublicKey { return s.public }
func (s *fileSigner) unlock() (cryptossh.Signer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.signer != nil {
		return s.signer, nil
	}
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	if budgets, _ := s.ctx.Value(authenticationBudgetKey{}).(*authenticationBudgets); budgets != nil {
		resume := budgets.pause()
		defer resume()
	}
	if s.cache != nil {
		release, err := s.cache.Acquire(s.ctx, s.path)
		if err != nil {
			return nil, err
		}
		defer release()
		if signer, ok := s.cache.Get(s.path, s.data); ok {
			s.signer = signer
			return signer, nil
		}
	}
	secret, err := s.prompt(s.ctx, s.path)
	defer clear(secret)
	if err != nil {
		return nil, err
	}
	key, err := cryptossh.ParseRawPrivateKeyWithPassphrase(s.data, secret)
	if err != nil {
		return nil, fmt.Errorf("unlock identity %s: %w", s.path, err)
	}
	signer, err := cryptossh.NewSignerFromKey(key)
	if err != nil {
		return nil, err
	}
	if string(signer.PublicKey().Marshal()) != string(s.public.Marshal()) {
		return nil, fmt.Errorf("identity public key does not match %s", s.path)
	}
	if s.cache != nil {
		signer, err = s.cache.Add(s.path, s.data, key)
		if err != nil {
			return nil, err
		}
	}
	s.signer = signer
	return signer, nil
}
func (s *fileSigner) Sign(random io.Reader, data []byte) (*cryptossh.Signature, error) {
	signer, err := s.unlock()
	if err != nil {
		return nil, err
	}
	return signer.Sign(random, data)
}
func (s *fileSigner) SignWithAlgorithm(random io.Reader, data []byte, algorithm string) (*cryptossh.Signature, error) {
	signer, err := s.unlock()
	if err != nil {
		return nil, err
	}
	if a, ok := signer.(cryptossh.AlgorithmSigner); ok {
		return a.SignWithAlgorithm(random, data, algorithm)
	}
	if algorithm != "" && algorithm != signer.PublicKey().Type() {
		return nil, fmt.Errorf("unsupported signature algorithm %s", algorithm)
	}
	return signer.Sign(random, data)
}
