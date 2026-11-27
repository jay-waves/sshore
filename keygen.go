package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jay-waves/sshore/internal/sys"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

func runKeygen(args cliArguments) (int, error) {
	target := args.operand(0)
	if target == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return 1, err
		}
		target = filepath.Join(home, ".ssh", "id_ed25519")
	}
	target, err := sys.ExpandHome(target)
	if err != nil {
		return 2, err
	}
	for _, name := range []string{target, target + ".pub"} {
		if _, err := os.Lstat(name); err == nil {
			return 1, fmt.Errorf("key file already exists: %s", cleanText(name))
		} else if !os.IsNotExist(err) {
			return 1, err
		}
	}
	pub, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		return 1, err
	}
	defer clear(private)
	var block *pem.Block
	if args.has("--no-passphrase") {
		block, err = ssh.MarshalPrivateKey(private, "")
	} else {
		passphrase, readErr := readKeyPassphrase()
		if readErr != nil {
			return 1, readErr
		}
		defer clear(passphrase)
		block, err = ssh.MarshalPrivateKeyWithPassphrase(private, "", passphrase)
	}
	if err != nil {
		return 1, err
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		return 1, err
	}
	if args.operand(0) == "" {
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return 1, err
		}
	}
	privatePEM := pem.EncodeToMemory(block)
	defer clear(privatePEM)
	if err := writeKeyFile(target, privatePEM); err != nil {
		return 1, err
	}
	if err := writeKeyFile(target+".pub", ssh.MarshalAuthorizedKey(key)); err != nil {
		return 1, errors.Join(err, os.Remove(target))
	}
	fmt.Fprintf(os.Stdout, "%s  %s.pub\n", ssh.FingerprintSHA256(key), cleanText(target))
	return 0, nil
}

func readKeyPassphrase() ([]byte, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return nil, fmt.Errorf("keygen requires a terminal to read a passphrase; use --no-passphrase for an unencrypted key")
	}
	fmt.Fprint(os.Stderr, "Passphrase: ")
	passphrase, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return nil, err
	}
	if len(passphrase) == 0 {
		return nil, fmt.Errorf("empty passphrase; use --no-passphrase for an unencrypted key")
	}
	fmt.Fprint(os.Stderr, "Confirm passphrase: ")
	confirm, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	defer clear(confirm)
	if err != nil || !bytes.Equal(passphrase, confirm) {
		clear(passphrase)
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("passphrases do not match")
	}
	return passphrase, nil
}

func writeKeyFile(name string, data []byte) error {
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	err = sys.PrivateStatePath(name, false)
	if err == nil {
		err = writeFull(file, data)
	}
	if err := errors.Join(err, file.Close()); err != nil {
		return errors.Join(err, os.Remove(name))
	}
	return nil
}
