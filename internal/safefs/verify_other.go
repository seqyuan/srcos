//go:build !linux

package safefs

import "os"

// VerifyOpen is a no-op on hosts without /proc/self/fd. SRCOS's runtime is
// Linux-only (it uses /proc and systemd), so this exists to keep the package
// buildable elsewhere, not to claim the check succeeds there.
func VerifyOpen(*os.File, string) error { return nil }
