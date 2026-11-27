package main

import (
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	deployment "github.com/jay-waves/sshore/internal/release/remote"
	sshio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/sys"
	bolt "go.etcd.io/bbolt"
)

const maxDeploymentRecordBytes = 1 << 20
const shutdownDestinationEnv = "SSHORE_SHUTDOWN_DESTINATION"

type deploymentEndpoint struct {
	Host, Directory, Home string
	Destination           deploymentDestination
	SSH                   []string
	Review                bool `json:",omitempty"`
}

// Deployment records are account-scoped across releases and state directories.
func deploymentHistoryTransaction(write bool, run func(*bolt.Tx) error) error {
	dir, err := daemonLocation()
	if err != nil {
		return err
	}
	return stateDBTransaction(filepath.Dir(dir), filepath.Join("deployments", "history.db"), write, run)
}

type deploymentDestination struct {
	User, Host, Port string
}

func deploymentEndpointFor(connection sshio.Options, destination sshio.Destination) (deploymentEndpoint, error) {
	endpoint := deploymentEndpoint{
		Host: connection.Host, Directory: connection.Directory, SSH: connection.Args,
		Destination: deploymentDestination{destination.User, destination.Host, destination.Port},
	}
	if endpoint.Directory == "" {
		var err error
		endpoint.Directory, err = os.Getwd()
		if err != nil {
			return deploymentEndpoint{}, err
		}
	}
	return endpoint, nil
}

func (e deploymentEndpoint) sameRoute(other deploymentEndpoint) bool {
	return e.Host == other.Host && e.Directory == other.Directory && slices.Equal(e.SSH, other.SSH)
}

func (e deploymentEndpoint) cached() (entry deploymentEndpoint, err error) {
	key, err := json.Marshal(e.Destination)
	if err != nil {
		return entry, err
	}
	err = deploymentHistoryTransaction(false, func(tx *bolt.Tx) error {
		if tx == nil {
			return nil
		}
		bucket := tx.Bucket([]byte(version))
		if bucket == nil {
			return nil
		}
		if data := bucket.Get(key); data != nil {
			return json.Unmarshal(data, &entry)
		}
		return nil
	})
	return entry, err
}

func (e deploymentEndpoint) remember(home string, review bool) error {
	key, err := json.Marshal(e.Destination)
	if err != nil {
		return err
	}
	return deploymentHistoryTransaction(true, func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte(version))
		if err != nil {
			return err
		}
		if data := bucket.Get(key); data != nil {
			var previous deploymentEndpoint
			if err := json.Unmarshal(data, &previous); err != nil {
				return err
			}
			if previous.Review && !review {
				e = previous
			}
		}
		e.Home, e.Review = home, review || e.Review
		data, err := marshalBoundedStateJSON("deployment endpoint", e, maxDeploymentRecordBytes)
		if err != nil {
			return err
		}
		return bucket.Put(key, data)
	})
}

// Use the installed client so the shutdown request selects its own deployment.
func stopInstalledDeployments(executable, release string) error {
	dir, err := daemonLocation()
	if err != nil {
		return err
	}
	// Pre-bbolt releases cannot enumerate their deployments for this installer.
	// Require manual shutdown before replacing a client with a legacy ledger.
	legacy := filepath.Join(filepath.Dir(dir), "deployments", release+".json")
	if _, err := os.Lstat(legacy); err == nil {
		return fmt.Errorf("legacy deployment history for sshore %s: stop its remote tasks with the installed client and remove %q before upgrading", cleanText(release), legacy)
	} else if !os.IsNotExist(err) {
		return err
	}
	var endpoints []deploymentEndpoint
	err = deploymentHistoryTransaction(false, func(tx *bolt.Tx) error {
		if tx == nil {
			return nil
		}
		bucket := tx.Bucket([]byte(release))
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(_, data []byte) error {
			var endpoint deploymentEndpoint
			if err := json.Unmarshal(data, &endpoint); err != nil {
				return err
			}
			endpoints = append(endpoints, endpoint)
			return nil
		})
	})
	if err != nil {
		return err
	}
	for _, endpoint := range endpoints {
		if !endpoint.Review {
			continue
		}
		if validSSHDestination(endpoint.Host) != nil || !filepath.IsAbs(endpoint.Directory) {
			return fmt.Errorf("invalid deployment history entry")
		}
		args := []string{"kill", endpoint.Host, "--all"}
		if len(endpoint.SSH) > 0 {
			args = append(append(args, "--ssh"), endpoint.SSH...)
		}
		command := exec.Command(executable, args...)
		command.Dir = endpoint.Directory
		destination, err := json.Marshal(endpoint.Destination)
		if err != nil {
			return err
		}
		command.Env = append(command.Environ(), shutdownDestinationEnv+"="+string(destination))
		command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
		fmt.Fprintf(os.Stderr, "Review sshore %s tasks on %s.\n", release, cleanText(endpoint.Host))
		if err := command.Run(); err != nil {
			return fmt.Errorf("retiring sshore %s on %s: %w", release, cleanText(endpoint.Host), err)
		}
	}
	return nil
}

var deploymentReleaseBase = "https://github.com/jay-waves/sshore/releases/download"

// The binary's startup location selects state for service, daemon and CLI.
var deploymentCoordinates = sync.OnceValues(func() (directory, clientID string) {
	exe, err := os.Executable()
	if err != nil {
		return "", ""
	}
	dir := filepath.Dir(exe)
	parent := filepath.Dir(dir)
	id := filepath.Base(parent)
	if filepath.Base(dir) != version || !validMachineID(id) {
		return "", ""
	}
	account, err := user.Current()
	if err != nil {
		return "", ""
	}
	root := filepath.Join(account.HomeDir, filepath.FromSlash(deployment.ClientDirectory))
	if !sameDeploymentRoot(filepath.Dir(parent), root) {
		home, err := filepath.EvalSymlinks(account.HomeDir)
		if err != nil || !sameDeploymentRoot(filepath.Dir(parent), filepath.Join(home, filepath.FromSlash(deployment.ClientDirectory))) {
			return "", ""
		}
	}
	return dir, id
})

func sameDeploymentRoot(path, root string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(path, root)
	}
	return path == root
}

func deployedStateRoot() string {
	if dir, _ := deploymentCoordinates(); dir != "" {
		return filepath.Join(dir, "state")
	}
	return ""
}

func deploymentClientID() (string, error) {
	if _, id := deploymentCoordinates(); id != "" {
		return id, nil
	}
	daemon, err := daemonLocation()
	if err != nil {
		return "", err
	}
	root := filepath.Dir(daemon)
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	if err := sys.PrivateStatePath(root, true); err != nil {
		return "", err
	}
	lock, err := lockStateFile(filepath.Join(root, "deployment-id.lock"))
	if err != nil {
		return "", err
	}
	defer lock.Close()
	name := filepath.Join(root, "deployment-id")
	id, err := os.ReadFile(name)
	if errors.Is(err, fs.ErrNotExist) {
		value := resourceToken()[:32]
		return value, writeStateFile(name, []byte(value))
	}
	if err != nil {
		return "", err
	}
	if !validMachineID(string(id)) {
		return "", fmt.Errorf("invalid deployment ID in %s", name)
	}
	return string(id), nil
}
