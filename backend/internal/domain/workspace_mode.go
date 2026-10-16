package domain

// WorkspaceMode is where a single-repo project's worker sessions work
// (ProjectConfig.WorkspaceMode).
type WorkspaceMode string

const (
	// WorkspaceModeWorktree gives each session its own git worktree (the default).
	WorkspaceModeWorktree WorkspaceMode = "worktree"
	// WorkspaceModeShared gives every session of the project one long-lived git
	// worktree, created once and reused; AO never removes it.
	WorkspaceModeShared WorkspaceMode = "shared"
	// WorkspaceModeFolder runs sessions in the project directory itself, with
	// whatever branch and uncommitted work it holds; AO never removes, resets or
	// provisions it.
	WorkspaceModeFolder WorkspaceMode = "folder"
)

// WithDefault returns WorkspaceModeWorktree for an unset mode.
func (m WorkspaceMode) WithDefault() WorkspaceMode {
	if m == "" {
		return WorkspaceModeWorktree
	}
	return m
}

// IsKnown reports whether m is empty or one of the defined modes.
func (m WorkspaceMode) IsKnown() bool {
	switch m {
	case "", WorkspaceModeWorktree, WorkspaceModeShared, WorkspaceModeFolder:
		return true
	}
	return false
}
