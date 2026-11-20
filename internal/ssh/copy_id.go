package ssh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	cryptossh "golang.org/x/crypto/ssh"
)

// Install the canonical key line under an exclusive lock. Fingerprinting also
// recognizes existing authorizations whose lines contain options or comments.
const copyIDScript = `set -eu
umask 077
for tool in flock ssh-keygen awk tail; do
    command -v "$tool" >/dev/null || { printf 'copy-id requires %s on the remote host\n' "$tool" >&2; exit 1; }
done
IFS= read -r fingerprint
IFS= read -r key
directory="${HOME:?}/.ssh"
file="$directory/authorized_keys"
mkdir -p "$directory"
chmod 700 "$directory"
if [ -L "$file" ]; then
    printf 'copy-id: %s is a symbolic link; use a regular authorized_keys file\n' "$file" >&2
    exit 1
fi
if [ -e "$file" ] && [ ! -f "$file" ]; then
    printf 'copy-id: %s exists but is not a regular file; check the remote path\n' "$file" >&2
    exit 1
fi
exec 9>>"$file"
flock -x 9
chmod 600 "$file"
if ssh-keygen -E sha256 -lf "$file" 2>/dev/null | awk -v fingerprint="$fingerprint" '$2 == fingerprint { found = 1 } END { exit !found }'; then
    exit 0
fi
if [ -s "$file" ] && [ -n "$(tail -c 1 "$file")" ]; then
    printf '\n' >&9
fi
printf '%s\n' "$key" >&9`

type installIdentity struct {
	path string
	key  cryptossh.PublicKey
}

func (c *connectionConfig) installIdentities() ([]installIdentity, error) {
	paths, pathErr := c.identityPaths()
	var identities []installIdentity
	seen := make(map[string]bool)
	for _, path := range paths {
		publicPath := path + ".pub"
		key := publicIdentity(publicPath)
		if key == nil {
			publicPath = path
			key = publicIdentity(path)
		}
		if key == nil {
			signer, err := privateIdentity(path)
			var encrypted *cryptossh.PassphraseMissingError
			if err == nil {
				key = signer.PublicKey()
			} else if errors.As(err, &encrypted) {
				key = encrypted.PublicKey
			}
		}
		if key == nil {
			continue
		}
		if cert, ok := key.(*cryptossh.Certificate); ok {
			key = cert.Key
		}
		id := string(key.Marshal())
		if !seen[id] {
			seen[id] = true
			identities = append(identities, installIdentity{path: publicPath, key: key})
		}
	}
	return identities, pathErr
}

// CopyID installs one configured public key on the remote account. Select is
// called only when several distinct configured keys are available.
func CopyID(ctx context.Context, options Options, selectKey func([]string) (int, error), output, diagnostic io.Writer) error {
	command := "sh -c '" + strings.ReplaceAll(copyIDScript, "'", "'\"'\"'") + "'"
	options.Command, options.Diagnostic = command, diagnostic
	cfg, err := resolveDialConfig(ctx, options)
	if err != nil {
		return err
	}
	identities, pathErr := cfg.installIdentities()
	if len(identities) == 0 {
		return fmt.Errorf("no usable public key from IdentityFile: %w", errors.Join(pathErr, errors.New("check the SSH configuration and key files")))
	}
	choice := 0
	if len(identities) > 1 {
		paths := make([]string, len(identities))
		for i, identity := range identities {
			paths[i] = identity.path
		}
		choice, err = selectKey(paths)
		if err != nil {
			return err
		}
		if choice < 0 || choice >= len(identities) {
			return fmt.Errorf("invalid public key selection")
		}
	}
	var budgets authenticationBudgets
	client, err := cfg.dial(ctx, 0, &budgets)
	if err != nil {
		return err
	}
	defer client.Close()
	stop := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stop()
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()
	key := identities[choice].key
	session.Stdin = strings.NewReader(cryptossh.FingerprintSHA256(key) + "\n" + string(cryptossh.MarshalAuthorizedKey(key)))
	session.Stdout, session.Stderr = output, diagnostic
	if err := session.Run(command); err != nil {
		return fmt.Errorf("install public key on %s: %w", options.Host, err)
	}
	return nil
}
