// Package sshore embeds the installation scripts distributed at the repository root.
package sshore

import "embed"

// InstallerScripts contains the Linux and Windows installers.
//
//go:embed install.sh install.ps1
var InstallerScripts embed.FS
