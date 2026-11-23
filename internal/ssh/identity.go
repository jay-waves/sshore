package ssh

import (
	"bytes"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"os"

	cryptossh "golang.org/x/crypto/ssh"
)

// privateFileSigner defers unlocking until the server accepts the public key.
type privateFileSigner struct {
	key  cryptossh.PublicKey
	load func() (cryptossh.Signer, error)
}

func (s privateFileSigner) PublicKey() cryptossh.PublicKey { return s.key }

func (s privateFileSigner) Sign(random io.Reader, data []byte) (*cryptossh.Signature, error) {
	return s.SignWithAlgorithm(random, data, "")
}

func (s privateFileSigner) SignWithAlgorithm(random io.Reader, data []byte, algorithm string) (*cryptossh.Signature, error) {
	signer, err := s.load()
	if err != nil {
		return nil, err
	}
	if algorithm == "" || algorithm == signer.PublicKey().Type() {
		return signer.Sign(random, data)
	}
	if algorithmSigner, ok := signer.(cryptossh.AlgorithmSigner); ok {
		return algorithmSigner.SignWithAlgorithm(random, data, algorithm)
	}
	return nil, fmt.Errorf("IdentityFile key does not support %s", algorithm)
}

func privateIdentity(path string) (cryptossh.Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	defer clear(data)
	return cryptossh.ParsePrivateKey(data)
}

func (c *connectionConfig) unlockIdentity(path string) (cryptossh.Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	defer clear(data)
	for i := 0; i < c.passphraseAttempts; i++ {
		secret, err := c.passphrase("Passphrase for " + path)
		if err == nil {
			var signer cryptossh.Signer
			signer, err = cryptossh.ParsePrivateKeyWithPassphrase(data, secret)
			clear(secret)
			if !errors.Is(err, x509.IncorrectPasswordError) {
				return signer, err
			}
		} else {
			clear(secret)
			return nil, err
		}
	}
	return nil, fmt.Errorf("incorrect passphrase for IdentityFile %q", path)
}

func (c *connectionConfig) privateIdentities(paths []string) ([]cryptossh.Signer, error) {
	seen := make(map[string]bool)
	var signers []cryptossh.Signer
	var failures []error
	for _, path := range paths {
		public := publicIdentity(path + ".pub")
		signer, err := privateIdentity(path)
		switch {
		case err == nil:
			public = signer.PublicKey()
		default:
			var encrypted *cryptossh.PassphraseMissingError
			if !errors.As(err, &encrypted) || c.passphrase == nil || c.passphraseAttempts <= 0 {
				failures = append(failures, fmt.Errorf("IdentityFile %q: %w", path, err))
				continue
			}
			if encrypted.PublicKey != nil {
				public = encrypted.PublicKey
			}
			if public == nil {
				signer, err = c.unlockIdentity(path)
				if err != nil {
					failures = append(failures, err)
					continue
				}
				public = signer.PublicKey()
			}
		}
		id := string(public.Marshal())
		if seen[id] {
			continue
		}
		seen[id] = true
		signers = append(signers, privateFileSigner{key: public, load: func() (cryptossh.Signer, error) {
			if signer == nil {
				loaded, err := c.unlockIdentity(path)
				if err != nil {
					return nil, err
				}
				if !bytes.Equal(public.Marshal(), loaded.PublicKey().Marshal()) {
					return nil, fmt.Errorf("IdentityFile %q does not match its public key", path)
				}
				signer = loaded
			}
			return signer, nil
		}})
	}
	return signers, errors.Join(failures...)
}

func publicIdentity(path string) cryptossh.PublicKey {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	defer clear(data)
	key, _, _, _, err := cryptossh.ParseAuthorizedKey(data)
	if err != nil {
		return nil
	}
	return key
}

func (c *connectionConfig) identityPaths() ([]string, error) {
	var paths []string
	var failures []error
	for _, name := range c.values["identityfile"] {
		if name == "none" {
			continue
		}
		path, err := c.expand(name)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		paths = append(paths, path)
	}
	return paths, errors.Join(failures...)
}

func (c *connectionConfig) identityCertificates(paths []string) []*cryptossh.Certificate {
	var certificates []*cryptossh.Certificate
	for _, path := range paths {
		if cert, ok := publicIdentity(path + "-cert.pub").(*cryptossh.Certificate); ok {
			certificates = append(certificates, cert)
		}
	}
	for _, name := range c.values["certificatefile"] {
		if name != "none" {
			if path, err := c.expand(name); err == nil {
				if cert, ok := publicIdentity(path).(*cryptossh.Certificate); ok {
					certificates = append(certificates, cert)
				}
			}
		}
	}
	return certificates
}
