package forge

// RenderActionsSection returns the [actions] section of Forgejo's app.ini.
//
// Enabling Actions is what activates Forgejo's fork pull request approval
// gate (FORGE-003): a workflow run triggered by a pull request author who
// has neither write access to the repository nor a prior merged
// contribution to it is held for maintainer approval before it runs.
// Forgejo enforces that gate unconditionally once Actions is on — it
// exposes no app.ini key, and no per-repository setting, to loosen or
// disable it. Enabling Actions is therefore both necessary and sufficient
// for the requirement to hold; there is no separate "approval" key to
// render. The trust boundary this protects is recorded in spec.md.
//
// FORGE-001 composes this section into the fully rendered app.ini alongside
// domain, database, mailer, and session config.
func RenderActionsSection() string {
	return "[actions]\nENABLED = true\n"
}
