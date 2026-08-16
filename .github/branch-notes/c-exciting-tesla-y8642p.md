# Branch: c/exciting-tesla-y8642p

Tracking branch for XCUT-003 — every failure an operator sees names what
failed, why it failed, and what to do about it. The two named test cases:
exhausted SSH authentication explains that Farrier authenticates through
the operator's SSH agent or a key file they name, that a key on disk is
not enough on its own, and that `ssh-add -l` lists what the agent holds;
a remote directory that cannot be created or written says the default is
not writable by an ordinary user and that a writable path can be given
instead.

See `docs/functional-requirements.md` § XCUT and `docs/status.json`.
