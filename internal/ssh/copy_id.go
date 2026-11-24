package ssh

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"strings"

	cryptossh "golang.org/x/crypto/ssh"
)

const copyIDHeader = "sshore-copy-id"

// Hold the lock across the snapshot and the client's append decision.
const copyIDScript = `set -eu
umask 077
for tool in flock base64 tail; do
    command -v "$tool" >/dev/null || { printf 'copy-id requires %s on the remote host\n' "$tool" >&2; exit 1; }
done
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
printf '%s\n' '` + copyIDHeader + `'
base64 "$file"
printf '.\n'
IFS= read -r key
[ -n "$key" ] || exit 0
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
func CopyID(ctx context.Context, options Options, selectKey func([]string) (int, error), diagnostic io.Writer) error {
	command := "exec sh -c '" + strings.ReplaceAll(copyIDScript, "'", "'\"'\"'") + "'"
	options.Command = command
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
		if selectKey == nil {
			return errors.New("multiple IdentityFile keys require a public key selection")
		}
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
	stop := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stop()
	stream, err := client.Start(command, diagnostic)
	if err != nil {
		client.Close()
		return err
	}
	defer stream.Wait()
	defer client.Close()

	reader := bufio.NewReader(stream)
	line, err := copyIDKeyLine(reader, identities[choice].key)
	if err != nil {
		return fmt.Errorf("read authorized_keys on %s: %w", options.Host, err)
	}
	if _, err := stream.Write(line); err != nil {
		return fmt.Errorf("install public key on %s: %w", options.Host, err)
	}
	if _, err := io.Copy(io.Discard, reader); err != nil {
		return fmt.Errorf("install public key on %s: %w", options.Host, err)
	}
	if err := stream.Wait(); err != nil {
		return fmt.Errorf("install public key on %s: %w", options.Host, err)
	}
	return nil
}

// A blank reply keeps the existing entry. Read through the end marker before
// replying, so incomplete snapshots cannot lead to an append.
func copyIDKeyLine(reader *bufio.Reader, key cryptossh.PublicKey) ([]byte, error) {
	header, err := reader.ReadSlice('\n')
	if err != nil {
		return nil, err
	}
	if string(header) != copyIDHeader+"\n" {
		return nil, errors.New("unexpected output from remote shell")
	}
	// Base64 keeps the dot terminator separate from arbitrary file contents.
	encoded := textproto.NewReader(reader).DotReader()
	lines := bufio.NewScanner(base64.NewDecoder(base64.StdEncoding, encoded))
	wanted := key.Marshal()
	present := false
	for lines.Scan() {
		if present {
			continue
		}
		existing, _, _, _, err := cryptossh.ParseAuthorizedKey(lines.Bytes())
		if err != nil {
			continue
		}
		if cert, ok := existing.(*cryptossh.Certificate); ok {
			existing = cert.Key
		}
		present = bytes.Equal(existing.Marshal(), wanted)
	}
	if err := lines.Err(); err != nil {
		return nil, err
	}
	if present {
		return []byte("\n"), nil
	}
	return cryptossh.MarshalAuthorizedKey(key), nil
}
