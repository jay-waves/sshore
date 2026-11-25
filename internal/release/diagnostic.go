package release

import "fmt"

const InstallHelp = "Install the same full release on both machines.\n" +
	"Linux installer: https://raw.githubusercontent.com/jay-waves/sshore/main/install.sh\n" +
	"Windows installer: https://raw.githubusercontent.com/jay-waves/sshore/main/install.ps1"

func VersionMismatch(local, peer, role string) error {
	if peer == "" {
		peer = "unknown"
	}
	return fmt.Errorf("sshore version mismatch: this process=%s, %s=%q.\n%s", local, role, peer, InstallHelp)
}
