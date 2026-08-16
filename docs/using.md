# Working on your forge

Day-two work: your instance is up, your code is on it, and now you use it. Standing one up is the [README](../README.md); protecting and recovering it is [operating.md](operating.md); what you are accepting is [security.md](security.md).

**This covers what is different because it is Farrier. It does not teach Forgejo.** Pull requests, review, branch protection, teams, and the Actions syntax are Forgejo's, unchanged, and Forgejo documents them at [forgejo.org/docs](https://forgejo.org/docs/latest/). What lands here is the short list of places where a habit carried over from GitHub, or a Farrier deployment decision, produces a surprise. When a section here starts explaining Forgejo, it belongs in a link instead.

## CI

Farrier deploys Forgejo Actions with a runner already registered, so a workflow runs on a fresh instance with nothing to install. Four things decide whether your first one works.

### Put workflows in `.github/workflows`

Forgejo looks for workflows in `.forgejo/workflows`, then `.gitea/workflows`, then `.github/workflows`, and uses **the first of those that exists — only that one**. It is a fallback chain, not a merge.

So for a project moving off GitHub, `.github/workflows` is the two-way door. GitHub never looks at `.forgejo/`, Forgejo falls back to `.github/` when `.forgejo/` is absent, and one set of files runs in both places.

The trap is on the other side of that. **The moment you add `.forgejo/workflows`, your instance stops reading `.github/workflows` entirely** — not just for the file you duplicated. Anything you still want running on the instance moves too.

Add `.forgejo/workflows` when the two forges genuinely need different content, and then treat it as the instance's complete set. With the colocated runner answering to `ubuntu-latest`, most GitHub workflows need no `runs-on` change.

### `runs-on: ubuntu-latest` works

The colocated runner answers to both `ubuntu-latest` and `docker`, each mapped to the same Node container image. A workflow written for GitHub — `runs-on: ubuntu-latest` — schedules on a fresh instance without rewriting the file.

That label match is not GitHub's runner VM. Every step still runs as root inside a plain Node image, not GitHub's `ubuntu-latest` image with a hundred tools baked in. Expect to install what your job needs — you are already root inside that container, so nothing needs `sudo` and `sudo` is not there anyway. Name a different image per job with `container:` when installing the same packages every run is the wrong trade.

`runs-on: docker` is the Forgejo-shaped spelling of the same thing. Check what your own instance answers to under **Site Administration → Actions → Runners**, which lists each runner with its labels.

### Actions resolve from `code.forgejo.org`, not GitHub

`uses: actions/checkout@v4` fetches from `code.forgejo.org/actions/checkout` — Forgejo's default source, which Farrier leaves alone. The common ones are mirrored there, `checkout` and `setup-node` among them. Most of the marketplace is not.

This is the next thing a ported GitHub workflow hits after `runs-on`, and it fails rather than queues: the step errors on a repository that does not exist, naming `code.forgejo.org`, which reads like an outage and is not one. Every `uses:` line in a workflow you carried over needs to be either mirrored or spelled out.

Name a full URL for an action that lives elsewhere:

```yaml
- uses: https://github.com/some-org/some-action@v1
```

**The runner host needs outbound HTTPS**, both to that source and to the registry holding job-container images. A host firewalled to inbound-only runs the forge fine and fails every workflow at its first step.

### The runner can take the forge host

Jobs run on the host's Docker socket, which is the same host holding your git data and database. Read [security.md](security.md), "CI can take the forge host" — it states the trade and what to do instead.

## Secrets

Set them in Forgejo's web UI, per repository under **Settings → Actions → Secrets**, or once for everything under an organization or user account. Workflows read them as `${{ secrets.NAME }}`. Farrier has no secrets command: they are Forgejo's, and they are set where Forgejo sets them.

They are stored in the database, encrypted with the bundle's `SECRET_KEY`, so they travel in every snapshot and come back with a restore or a promote. What they do not survive is a host lost with no snapshot — see [operating.md](operating.md), "What a backup does not carry."

## More than one project on one instance

An instance hosts as many repositories as you put on it. Publishing a second project is one command from that project's folder, pointed at the first instance's bundle:

```bash
cd my-other-project
farrier publish -bundle ../my-project/.farrier
```

The bundle supplies the instance — its address, its host key, its identity. The folder you run in supplies the code, and the repository takes that folder's name unless you pass `-name`. `FARRIER_TARGET_TOKEN` is read from the environment exactly as in the quick start, and a nameless instance needs `-target` as well, having no public URL of its own to default to.

Nothing about the instance changes and no code path differs. One instance for ten projects is one address, one backup, one drill. The reasoning, and why the bundle then belongs on its own rather than inside one of the ten, is [spec.md](spec.md), "The unit: a forge, and the projects on it."

## What is different from GitHub

**No issues, no wikis, no project management.** By design and by scope, not by omission — Farrier is git hosting, pull requests, review, secrets, and CI. Forgejo ships issues and wikis; that they are outside what this project supports is the reason nothing here documents them. Keep the tracker you already have.

**Nothing is public by default.** `publish` creates private repositories. An instance built for a team's own work has no discovery, no stars, and no contributor identity anyone arrives with — the network effects are what you traded away, and they are not coming back through a setting.

Three Forgejo-shaped differences you will hit in review and CI:

- **Review comments are a flat list, not threads.** Comments on the same line group visually, but there is no thread object and nothing to resolve. A follow-up review supersedes an earlier one; nothing gets ticked off.
- **CI reports commit statuses, not check runs.** The pull request shows a green or red mark per commit. GitHub's checks UI, with its per-check reruns and annotations, has no equivalent.
- **Draft is a title prefix.** Open a pull request with `WIP:` in front of the title to mark it not-ready. There is no draft flag and no button.
