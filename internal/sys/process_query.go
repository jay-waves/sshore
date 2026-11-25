//go:build linux || windows || (darwin && arm64)

package sys

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/shirou/gopsutil/v4/common"
	"github.com/shirou/gopsutil/v4/process"
)

const procRoot = "/proc"

// Match the procfs used for native Linux ownership and identity checks.
var processContext = context.WithValue(context.Background(), common.EnvKey, common.EnvMap{
	common.HostProcEnvKey: procRoot,
})

// WorkingDirectory reads the native current directory of a live process.
// PowerShell runspace locations are independent of its native process cwd.
func WorkingDirectory(pid int) (string, error) {
	if _, err := LiveIdentity(pid); err != nil {
		return "", err
	}
	p := process.Process{Pid: int32(pid)}
	if runtime.GOOS == "windows" {
		executable, err := p.ExeWithContext(processContext)
		if err != nil {
			return "", err
		}
		switch strings.ToLower(filepath.Base(executable)) {
		case "pwsh.exe", "powershell.exe", "powershell_ise.exe":
			return "", fmt.Errorf("PowerShell runspace directory is unavailable")
		}
	}
	return p.CwdWithContext(processContext)
}
