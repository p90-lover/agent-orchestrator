//go:build !windows

package shellterm

import "errors"

// systemWindowsDirectory has no answer off Windows, where the SSH terminal
// route is off anyway (see sshTerminalPlatform).
func systemWindowsDirectory() (string, error) {
	return "", errors.New("shellterm: no Windows directory on this platform")
}
