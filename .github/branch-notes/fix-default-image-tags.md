# Branch: claude/fix-default-image-tags

Defect fix, not a requirement: `initialize.DefaultImageRefs` pins all three
default images to `:latest`. Forgejo publishes no `latest` tag, so
`farrier init` fails at the `resolve-images` step with a registry 404 unless
the operator passes `-image forgejo=...`. This branch replaces the floating
defaults with real, resolvable tags.

No requirement ID; `docs/status.json` is untouched.

This file exists for conductor tracking and can be deleted once the PR merges.
