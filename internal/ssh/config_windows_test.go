package ssh

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestWindowsProgramDataPath(t *testing.T) {
	programData := filepath.Join(t.TempDir(), "shared data %folder")
	system := filepath.Join(programData, "ssh")
	if err := os.MkdirAll(system, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(system, "ssh_config"), []byte("Host fixture\n Port 2222\n User alice\n UserKnownHostsFile none\n"), 0600); err != nil {
		t.Fatal(err)
	}
	config, err := resolveConfig(t.Context(), Options{Host: "fixture", Environment: []string{
		"USERPROFILE=" + t.TempDir(), "pRoGrAmDaTa=" + programData,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if config.port != "2222" || config.user != "alice" {
		t.Fatalf("ProgramData ssh_config was not applied: port %s, user %s", config.port, config.user)
	}
	paths, _, err := config.knownHostsPaths()
	want := []string{filepath.Join(system, "ssh_known_hosts"), filepath.Join(system, "ssh_known_hosts2")}
	if err != nil || !slices.Equal(paths, want) {
		t.Fatalf("global known_hosts paths = %q, %v, want %q", paths, err, want)
	}
}
