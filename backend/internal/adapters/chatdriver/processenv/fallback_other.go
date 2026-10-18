//go:build !windows

package processenv

// systemDefault has nothing to add off Windows; Merge hardens only Windows
// environments.
func systemDefault(string) string { return "" }
