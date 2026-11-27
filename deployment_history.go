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

	"github.com/jay-waves/sshore/internal/release"
	sshio "github.com/jay-waves/sshore/internal/ssh"
	"github.com/jay-waves/sshore/internal/sys"
)

const maxDeploymentHistory = 1 << 20
const shutdownDestinationEnv = "SSHORE_SHUTDOWN_DESTINATION"

type deploymentEndpoint struct {
	Host, Directory, Home string
	Destination           deploymentDestination
	SSH                   []string
	Review                bool `json:",omitempty"`
}

func deploymentHistoryPath(release string) (string, error) {
	dir, err := daemonLocation()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(dir), "deployments", release+".json"), nil
}

func readDeploymentHistory(name string) ([]deploymentEndpoint, error) {
	var endpoints []deploymentEndpoint
	if err := readStateJSON(name, maxDeploymentHistory, &endpoints); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return endpoints, nil
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

func (e deploymentEndpoint) cached() (deploymentEndpoint, error) {
	name, err := deploymentHistoryPath(version)
	if err != nil {
		return deploymentEndpoint{}, err
	}
	endpoints, err := readDeploymentHistory(name)
	if err != nil {
		return deploymentEndpoint{}, err
	}
	for _, entry := range endpoints {
		if e.Destination == entry.Destination {
			return entry, nil
		}
	}
	return deploymentEndpoint{}, nil
}

func (e deploymentEndpoint) remember(home string, review bool) error {
	name, err := deploymentHistoryPath(version)
	if err != nil {
		return err
	}
	dir := filepath.Dir(name)
	if _, err := prepareStateDir(filepath.Dir(dir), filepath.Base(dir)); err != nil {
		return err
	}
	lock, err := lockStateFile(name + ".lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	endpoints, err := readDeploymentHistory(name)
	if err != nil {
		return err
	}
	for i, entry := range endpoints {
		if e.Destination != entry.Destination {
			continue
		}
		if entry.Review && !review {
			e = entry
		}
		e.Home, e.Review = home, review || entry.Review
		endpoints[i] = e
		return writeBoundedStateJSON(name, endpoints, maxDeploymentHistory)
	}
	e.Home, e.Review = home, review
	return writeBoundedStateJSON(name, append(endpoints, e), maxDeploymentHistory)
}

// Use the installed client so the shutdown request selects its own deployment.
func stopInstalledDeployments(executable, release string) error {
	name, err := deploymentHistoryPath(release)
	if err != nil {
		return err
	}
	endpoints, err := readDeploymentHistory(name)
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
	root := filepath.Join(account.HomeDir, filepath.FromSlash(release.ClientDirectory))
	if !sameDeploymentRoot(filepath.Dir(parent), root) {
		home, err := filepath.EvalSymlinks(account.HomeDir)
		if err != nil || !sameDeploymentRoot(filepath.Dir(parent), filepath.Join(home, filepath.FromSlash(release.ClientDirectory))) {
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
