package release

import (
	"embed"
	"errors"
)

func RunInstaller(_ string, _ embed.FS) error {
	return errors.New("install the experimental macOS client manually")
}
