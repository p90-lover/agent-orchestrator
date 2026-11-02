//go:build windows

package processenv

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// systemDefault is Windows' own answer for an essential variable the daemon's
// environment lacks (it was started with a stripped environment).
func systemDefault(name string) string {
	switch strings.ToUpper(name) {
	case "SYSTEMROOT", "WINDIR":
		if dir, err := windows.GetSystemWindowsDirectory(); err == nil {
			return dir
		}
	case "SYSTEMDRIVE":
		if dir, err := windows.GetSystemWindowsDirectory(); err == nil {
			return filepath.VolumeName(dir)
		}
	case "TEMP", "TMP":
		return os.TempDir()
	case "USERPROFILE":
		if dir, err := windows.GetCurrentProcessToken().GetUserProfileDirectory(); err == nil {
			return dir
		}
	case "LOCALAPPDATA":
		if dir, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, 0); err == nil {
			return dir
		}
	case "APPDATA":
		if dir, err := windows.KnownFolderPath(windows.FOLDERID_RoamingAppData, 0); err == nil {
			return dir
		}
	case "PROGRAMDATA":
		if dir, err := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0); err == nil {
			return dir
		}
	}
	return ""
}
