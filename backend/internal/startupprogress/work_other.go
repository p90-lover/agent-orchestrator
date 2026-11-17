//go:build !windows && !unix

package startupprogress

// Self reports no work where the platform has no way to read it; progress
// lines still name the stage and its elapsed time.
func Self() Work { return Work{} }
