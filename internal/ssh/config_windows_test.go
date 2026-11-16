package ssh

import (
	"path/filepath"
	"testing"
)

func TestWindowsProgramDataPath(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "shared data %folder")
	c := &connectionConfig{options: Options{Directory: t.TempDir(), Environment: []string{"pRoGrAmDaTa=" + directory}}}
	for _, token := range []string{"__PROGRAMDATA__", "__programdata__"} {
		for _, suffix := range []string{`\ssh/ssh_known_hosts`, `/ssh/ssh_known_hosts2`, ""} {
			got, err := c.expand(token + suffix)
			if err != nil {
				t.Fatal(err)
			}
			if got != directory+suffix || !filepath.IsAbs(got) {
				t.Fatalf("expanded path %q, want %q", got, directory+suffix)
			}
		}
	}
	c.options.Environment = nil
	if _, err := c.expand(`__PROGRAMDATA__\ssh/ssh_known_hosts`); err == nil {
		t.Fatal("missing ProgramData accepted")
	}
}
