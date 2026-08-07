// Package forge renders Forgejo configuration from the bundle manifest, runs
// admin bootstrap, and reconciles CI state (FORGE-001 through FORGE-004).
//
// FORGE-001 (full app.ini rendering across domain, database, mailer, and
// session config), FORGE-002 (admin bootstrap), and FORGE-004 (CI
// reconciliation) are not yet built. Today the package holds the app.ini
// section that establishes the CI trust boundary (FORGE-003); later work
// composes it into the fully rendered app.ini.
package forge
