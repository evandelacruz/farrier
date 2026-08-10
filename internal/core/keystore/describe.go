package keystore

import "strings"

// Describer is the optional self-description side of a Driver: it names
// where the driver keeps the key material called keyName — a path on disk,
// an item in a secret manager — so a caller that has just stored something
// can tell the operator where it went (INIT-006).
//
// Implementations report the destination and never the value (KEY-003).
// A destination is not a secret: it is the same non-secret driver config
// the manifest already carries in plain sight (CORE-001).
//
// It is optional because not every driver knows. The command driver
// (KEY-002) hands resolution to an operator-supplied command and has no
// idea where that command reads from or writes to, and an out-of-tree
// driver reached over the CORE-003 exec protocol speaks a fixed method set
// with no describe in it — adding one would change a published protocol
// third parties implement against, to ask a question those drivers mostly
// cannot answer. Target covers both by reporting nothing rather than
// guessing, and its callers say what they know instead.
type Describer interface {
	// DescribeTarget returns where keyName is kept, or an empty string when
	// the driver cannot say — including when keyName is one it would refuse.
	DescribeTarget(keyName string) string
}

// Target reports where driver keeps keyName, as the driver itself
// describes it, or "" when the driver does not implement Describer or
// declines to answer. An empty result is the normal answer for a driver
// whose storage is someone else's — it means "unknown here", not "error",
// and a caller reports the driver alone rather than inventing a location.
//
// Callers pass the Driver they got from New, guard wrapping included:
// guardedDriver forwards DescribeTarget, so the rotation guard never
// costs a caller the location report.
func Target(driver Driver, keyName string) string {
	describer, ok := driver.(Describer)
	if !ok {
		return ""
	}
	return strings.TrimSpace(describer.DescribeTarget(keyName))
}

// DescribeTarget returns the file the key material lives in — the same
// path Resolve reads and Store writes — so the report an operator sees
// names the exact file to back up, not just the directory. A key name the
// driver would refuse (empty, or one escaping the configured directory)
// has no path to name, so it describes nothing and the caller falls back
// to naming the driver.
func (d FileDriver) DescribeTarget(keyName string) string {
	full, err := d.resolvePath(keyName)
	if err != nil {
		return ""
	}
	return full
}

var _ Describer = FileDriver{}
