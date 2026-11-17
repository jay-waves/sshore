package ssh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

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

func (c *connectionConfig) publicIdentities() (map[string]bool, []*cryptossh.Certificate, error) {
	var certificates []*cryptossh.Certificate
	allowed := map[string]bool{}
	var failures []error
	var missing []string
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
		if key := publicIdentity(path); key != nil {
			remember(key)
		} else if publicIdentity(path+".pub") == nil {
			missing = append(missing, path+".pub (load the private key with ssh-add)")
		}
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
	if len(allowed) == 0 && len(failures) == 0 {
		if len(missing) > 0 {
			failures = append(failures, fmt.Errorf("IdentityFile not found: %s", strings.Join(missing, ", ")))
		} else {
			failures = append(failures, fmt.Errorf("no IdentityFile configured"))
		}
	}
	return allowed, certificates, errors.Join(failures...)
}
