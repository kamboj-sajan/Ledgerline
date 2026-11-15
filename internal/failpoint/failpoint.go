//go:build !chaos

// Package failpoint marks places where the chaos tests crash the process on
// purpose. In a normal build Inject is an empty function the compiler inlines
// away; the real implementation exists only with the chaos build tag.
package failpoint

// Inject does nothing without the chaos build tag.
func Inject(string) {}
