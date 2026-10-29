# Rumil

![Rumil: an elven scribe reading a scroll with a quill, the word RUMIL below](docs/images/rumil-banner.jpeg)

[![CI](https://github.com/joangrigorov/rumil/actions/workflows/ci.yml/badge.svg)](https://github.com/joangrigorov/rumil/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/joangrigorov/rumil)](https://github.com/joangrigorov/rumil/releases/latest)
[![Go version](https://img.shields.io/github/go-mod/go-version/joangrigorov/rumil)](https://go.dev/dl/)
[![License](https://img.shields.io/github/license/joangrigorov/rumil)](LICENSE)
[![Container](https://img.shields.io/badge/ghcr.io-joangrigorov%2Frumil-blue)](https://github.com/joangrigorov/rumil/pkgs/container/rumil)

Rumil is a self-hosted GitHub App that reviews pull requests. It reads the change, posts one GitHub review with inline comments, and — with memory turned on — records the engineering rules your team states in review, so a later round stops flagging a deliberate decision as a mistake.

It is read-only in what it does: the review, the replies in the threads it opened, the resolution of its own threads, and the confirmation it posts for a recorded rule are everything its code writes. One permission is wider than that use — GitHub maps closing a review thread to **Contents: Read and write**, so an App whose threads close themselves holds a token that could also write files. An operator who would rather not grant it keeps Contents at Read, and closes the threads a round says are fixed.

| Command | What it does |
| :--- | :--- |
| `rumil init` | Write a configuration file, one question at a time. |
| `rumil review` | Review one pull request and exit. No server, no queue. |
| `rumil serve` | Run the webhook server and the queue that reviews on request. |
| `rumil mod` | List the recorded rules, and approve or reject them. |

## Contents

- [Requirements](#requirements)
- [Install](#install)
- [Configure](#configure)
- [Review one pull request from the command line](#review-one-pull-request-from-the-command-line)
- [Run the webhook server](#run-the-webhook-server)
- [Repository guidelines](#repository-guidelines)
- [Teaching Rumil your conventions](#teaching-rumil-your-conventions)
- [Troubleshooting](#troubleshooting)
- [License](#license)

## Requirements

- **An OpenAI-compatible chat endpoint** and an API key for it. It has to support OpenAI-style tool calling: a review is an agent session that calls tools, and an endpoint that answers chat completions without them produces no review.
- **A GitHub App of your own**, installed on the repositories Rumil should review. [Configure](#configure) creates it.
- **A Postgres 16+ database with the `pgvector` extension** — for `rumil serve` only. The `pgvector/pgvector:pg16` image is the easiest way to get one. Rumil creates its own tables on boot when `database.auto_migrate` is on, so its database user needs the right to create extensions; on a managed Postgres, check that before you start.
- **Optionally, an embedding provider** of its own, for example `text-embedding-3-small`. That is what turns on recorded rules; without it Rumil reviews normally and has no memory. The provider states how wide its vectors are, and memory is stored and searched at that width, so a local model is enough: `embed-gemma` at 768 dimensions works.
- **Inbound HTTPS to your instance** — for `rumil serve` only, so that GitHub can deliver webhooks. Any reverse proxy works; a local run needs a tunnel that GitHub can reach.

Every review spends tokens on your chat provider, and recording or retrieving a rule spends them on your embedding provider. Nobody bills you but your provider.

## Install

Every release publishes static binaries and packages for Linux, macOS and Windows, plus a container image. Downloads are on the [releases page](https://github.com/joangrigorov/rumil/releases/latest). The commands below read the version from these two variables, so set them once:

```bash
VERSION=0.0.8                                    # the current release
BASE=https://github.com/joangrigorov/rumil/releases/download/v$VERSION
```

Download `checksums.txt` from the same base and check what you downloaded:

```bash
curl -LO $BASE/checksums.txt
sha256sum -c checksums.txt --ignore-missing      # shasum -a 256 -c checksums.txt on macOS
```

Packages and macOS binaries are unsigned, so the checksum is what a download is checked against.

### Linux

The packages install the binary to `/usr/bin/rumil` and the example config to `/etc/rumil/rumil.example.yaml`.

Debian and Ubuntu:

```bash
curl -LO $BASE/rumil_${VERSION}_amd64.deb        # arm64 and armhf are published too
sudo dpkg -i rumil_${VERSION}_amd64.deb
```

Fedora, RHEL and openSUSE:

```bash
curl -LO $BASE/rumil-${VERSION}-1.x86_64.rpm     # aarch64 and armv7hl are published too
sudo rpm -i rumil-${VERSION}-1.x86_64.rpm
```

Alpine:

```bash
curl -LO $BASE/rumil_${VERSION}_x86_64.apk       # aarch64 and armv7 are published too
sudo apk add --allow-untrusted rumil_${VERSION}_x86_64.apk
```

Arch:

```bash
curl -LO $BASE/rumil-${VERSION}-1-x86_64.pkg.tar.zst   # aarch64 and armv7h are published too
sudo pacman -U rumil-${VERSION}-1-x86_64.pkg.tar.zst
```

Any other Linux, from a static tarball that holds the binary, the example config and the license:

```bash
curl -LO $BASE/rumil_${VERSION}_linux_x86_64.tar.gz    # arm64 and armv7 are published too
tar -xzf rumil_${VERSION}_linux_x86_64.tar.gz
sudo install -m 0755 rumil /usr/local/bin/rumil
```

### macOS

```bash
curl -LO $BASE/rumil_${VERSION}_darwin_arm64.tar.gz    # darwin_x86_64 for Intel
tar -xzf rumil_${VERSION}_darwin_arm64.tar.gz
sudo install -m 0755 rumil /usr/local/bin/rumil
```

The macOS binaries are unsigned and un-notarized. If macOS refuses a copy that a browser downloaded, clear the quarantine flag:

```bash
xattr -d com.apple.quarantine /usr/local/bin/rumil
```

### Windows

Download `rumil_<version>_windows_x86_64.zip` (or `_windows_arm64.zip`) from the [releases page](https://github.com/joangrigorov/rumil/releases/latest), unzip it, and put the directory holding `rumil.exe` on your `PATH`.

### Container image

```bash
docker run --rm ghcr.io/joangrigorov/rumil:latest --help
```

The image is `linux/amd64` and holds the binary alone. It is the same binary the packages install, so mount a config and pass the environment it reads — [Run the webhook server](#run-the-webhook-server) shows the full command.

## Configure

Rumil reads one YAML file. `--config` or `-c` names it, and so does `RUMIL_CONFIG`; without either, the first file in the search below is read. Before the YAML is parsed, `${VAR}` placeholders are expanded from the environment, so secrets never have to live in the file. `rumil.example.yaml` documents every key: it is in the repository, and a package installs it to `/etc/rumil/rumil.example.yaml`.

### Where the file is found

| Order | Path |
| :--- | :--- |
| 1 | `./rumil.yaml` |
| 2 | `~/.config/rumil/config.yaml` (`$XDG_CONFIG_HOME` is respected) |
| 3 | `~/.config/rumil/rumil.yaml` |
| 4 | `/etc/rumil/config.yaml` |
| 5 | `/etc/rumil/rumil.yaml` |

A file in the working directory wins, which is what a repository clone and `docker compose` rely on. A package install is configured machine-wide in `/etc/rumil/`, beside the example config it ships. Every command logs the path it read, and when there is no file anywhere the error names every path that was tried. A path you name yourself is used as it is: if it is not there, the command says so rather than searching elsewhere.

### Write one with `rumil init`

`rumil init` asks for what a first run needs — the App credentials, the model endpoint, and optionally memory and the webhook server — and writes the file it will then find on its own:

```bash
rumil init
```

Every screen says what it is asking for, including where in GitHub the value comes from, so this README is not needed while it runs. The file is parsed before it is written, so `rumil init` cannot leave behind a configuration that Rumil refuses to start with. `--force` overwrites an existing file, `--print` writes to stdout instead of the filesystem, and `--config` says where the file belongs.

Memory is optional, and it asks for an embeddings model of its own: a provider in the file is either the chat model or the embedding model, so the chat model is refused where the embeddings model is asked for. A chat endpoint that serves no embeddings — DeepSeek's, for one — is therefore paired with an embeddings model from another provider, which is what the separate embedding endpoint and key are for, or with an empty embeddings model, which turns memory off and leaves reviews working without it. When an embeddings model is given, the next question asks how wide its vectors are — 1536 for `text-embedding-3-small`, 768 for a local EmbeddingGemma — because memory is stored and searched at that width.

An answer that is the name of an environment variable — `OPENAI_API_KEY` — is written as `${OPENAI_API_KEY}`, so the file holds no secrets and can be copied or committed. It writes the per-user file described below and creates its directory.

Without a terminal it asks the same questions one line at a time, which is how a script or a container answers them:

```bash
rumil init < answers.txt
```

### Create the GitHub App

Rumil authenticates as a GitHub App you own. Both ways of running it need the App; only the server needs a webhook.

1. Go to **Settings → Developer settings → GitHub Apps → New GitHub App** (for an organization, in the organization's settings). The App's name decides the slug people type in comments: an App named `Acme Rumil` answers to `acme-rumil`.
2. **Homepage URL**: anything, for example your Rumil instance's address.
3. **Repository permissions**:

   | Permission | Access | Why |
   | :--- | :--- | :--- |
   | Contents | Read | Read `RUMIL.md` and the file contents behind a diff hunk. **Read and write** is what GitHub requires before it lets an App installation resolve a review thread, so a read-only App leaves its own threads for a person to close. Raising it on the App is not enough on its own: the installation's owner has to accept the change before the tokens carry it |
   | Issues | Read and write | Read the pull request conversation, and post the reply to a remembered rule |
   | Pull requests | Read and write | Read metadata, diffs, reviews and review threads; submit the review, and reply to review comments |
   | Metadata | Read | Always required by GitHub |

4. Create the App, then **Generate a private key** and download the `.pem`. Point `github.private_key_path` at it, or put its contents in the `GITHUB_PRIVATE_KEY` environment variable.
5. Note the **App ID** on the App's settings page: that is `github.app_id`.
6. **Install** the App on the account that owns the repositories it should review, all of them or a selection. The installation's ID is the number at the end of the URL after installing (`.../installations/<id>`): that is `github.installation_id`.

The webhook URL, the webhook secret and the subscribed events are only needed to run the server. [Run the webhook server](#run-the-webhook-server) covers those.

### The configuration file

A minimal configuration that reviews pull requests with OpenAI:

```yaml
app:
  bot_handle: "acme-rumil"          # your App's slug, as people type it in comments

github:
  app_id: 123456
  installation_id: 98765432
  private_key_path: "${GITHUB_APP_PRIVATE_KEY_PATH}"
  webhook_secret: "${GITHUB_WEBHOOK_SECRET}"   # rumil serve only

models:
  default_chat: "gpt-4o-mini"       # any OpenAI-compatible endpoint
  providers:
    - id: "gpt-4o-mini"
      provider: "openai-compatible"
      base_url: "https://api.openai.com/v1"
      model_name: "gpt-4o-mini"
      api_key: "${OPENAI_API_KEY}"

database:                            # rumil serve only
  dsn: "${DATABASE_URL}"
  auto_migrate: true                # create and update the tables on boot
```

Rumil refuses to start on an invalid configuration and names the key it did not like.

### The values you must set

| Key | Notes |
| :--- | :--- |
| `github.app_id` | The App's ID, from [Create the GitHub App](#create-the-github-app) |
| `github.installation_id` | The installation to serve, from the same section |
| `github.private_key_path` | Path to the App's `.pem`, from the same section. `github.private_key` takes the PEM itself, and the `GITHUB_PRIVATE_KEY` environment variable is used when neither is set |
| `github.webhook_secret` | Required by `rumil serve`. The secret you set on the App, and it has to match exactly |
| `models.providers` | At least one provider; every provider needs `id`, `provider: openai-compatible`, `base_url`, `model_name` and `api_key` |
| `database.dsn` | Required by `rumil serve`. `rumil review` runs without it, doing a one-off review with no memory |
| `database.auto_migrate` | Keep it `true` and Rumil creates and updates its own tables; the tables must already exist if it is off |

### Everything else

| Key | Default | Notes |
| :--- | :--- | :--- |
| `app.bot_handle` | `@rumil` | Your App's slug. A leading `@` and a trailing `[bot]` are ignored, so `acme-rumil`, `@acme-rumil` and `acme-rumil[bot]` all match. `rumil serve` and `rumil review` read `GET /app` at startup and refuse to run when this is not the App's slug, because a handle that names something else silently disables the comment commands, the review requests and thread resolution. The same read warns about the one permission GitHub refuses thread resolution without |
| `app.max_agent_turns` | `25` | How many model steps one session may take. `0` removes the cap, so the session runs until the model stops or the job timeout arrives |
| `app.max_diff_bytes` | `250000` | Larger diffs are paginated instead of handed over in one piece |
| `app.log_level` | `info` | `debug`, `info`, `warn` or `error`. Logs are JSON on stderr |
| `review.guidelines_ref` | `base` | Which revision `RUMIL.md` is read from: `base` (the pull request's base, so a branch cannot rewrite its own rules) or `head` |
| `review.resolve_threads` | `true` | Whether Rumil resolves its own review threads once the finding is gone. From the second review of a pull request on, and at most 20 in one review. GitHub refuses the mutation unless the **installation's** Contents permission is Read and write, whatever its Pull requests permission says; `rumil serve` warns at startup when it is not, and the warning also covers an App whose permission was raised but whose installation has not accepted the change yet, and a round then posts the reason into the thread and names it in the summary |
| `compaction.enabled` | `true` | Whether a session summarizes its older context once it reaches `compaction.trigger_tokens`. On by default; set it to `false` to let the model's context window fill |
| `compaction.trigger_tokens` | `262144` | The estimated context size at which older messages are summarized. Nothing is compacted before this point, so a 256K trigger leaves a large pull request's details in context |
| `compaction.reserve_tokens` | `16384` | Output budget kept below the trigger, so the session can still answer after the context grows |
| `compaction.keep_recent_tokens` | `20000` | The most recent context kept verbatim; older messages are replaced by a summary |
| `memory.repo_scope_boost` | `0.05` | How much a repository's own rule outranks an organization-wide one when both are candidates |
| `worker.concurrency` | `4` | Reviews processed in parallel |
| `worker.job_timeout` | `15m` | Wall-clock budget for one review or command job, as a Go duration such as `30m` or `1h`. It must be positive. `app.max_agent_turns: 0` removes the step cap, but this timeout still bounds the run |
| `server.listen` | `:8080` | Address for the webhook server; `--listen` overrides it |
| `models.default_chat` | first non-embedding provider | Which provider reviews pull requests |
| `models.default_embedding` | unset | The provider that turns memory on. A provider is an embedding provider when it sets `dimensions`, which is the width of the vectors its model returns (1536 for `text-embedding-3-small`, 768 for `embed-gemma`) |
| `models.providers[].thinking_level` | unset | Reasoning effort for a chat provider: `off`, `none`, `minimal`, `low`, `medium`, `high` or `max`. Rumil sends it as `reasoning_effort` on the chat-completions request. `off` leaves the provider's default in place; `max` is for providers that expose it, such as DeepSeek V4 Flash |
| `moderation.accept_all` | `false` | The bypass for the rule approval gate. Leave it `false` |
| `moderation.auto_approve_associations` | empty | GitHub `author_association` values whose comments record rules without approval, for example `["OWNER", "MEMBER"]` |
| `moderation.admin_token` | unset | Bearer token (or basic-auth password) for the moderation UI and API. Unset means both refuse every request |

## Review one pull request from the command line

`rumil review` is the shortest way to a review: no server, no queue, no webhook, no database.

```bash
rumil review --repo acme/payments --pr 42
```

It needs the App credentials and a chat provider, and it submits the review as the App. It reads the file the [search](#where-the-file-is-found) finds, so the `rumil.yaml` you copied in the step above is enough, and `--config` names another one. With `database.dsn` and an embedding provider set, it also applies the rules you have approved; without both, it reviews with no memory and logs a warning saying so. A run that ends without submitting a review exits non-zero; the server records the same outcome as a failed job.

## Run the webhook server

`rumil serve` is what puts Rumil into the pull request flow: it receives GitHub's webhooks, queues them, and reviews when someone requests the App as a reviewer. It needs Postgres and the App's webhook settings, which the command line path above does not.

### Point the App at your server

In the App's settings:

1. **Webhook URL**: `https://<your-rumil-host>/webhooks/github`
2. **Webhook secret**: a long random string, and the same value as `github.webhook_secret`
3. **Subscribe to events**: *Pull request*, *Issue comment* and *Pull request review comment*

### Docker Compose

The repository ships a compose file with Rumil and Postgres with `pgvector`:

```bash
git clone https://github.com/joangrigorov/rumil.git
cd rumil

cp rumil.example.yaml rumil.yaml     # then fill in app_id, installation_id and the rest
export GITHUB_APP_PRIVATE_KEY_PATH=/path/to/your-app.pem
export GITHUB_WEBHOOK_SECRET=...     # the secret you set on the App
export OPENAI_API_KEY=...
export DEEPSEEK_API_KEY=...
export RUMIL_ADMIN_TOKEN=...         # optional: turns on the moderation page and API

docker compose up
```

Compose builds the image from source and starts both services. Rumil listens on `:8080`; the webhook endpoint is `POST /webhooks/github` and the health check is `GET /healthz`.

The compose file insists on both provider keys, because the example config names a DeepSeek chat provider and an OpenAI embedding provider. Drop the one you do not use from `rumil.yaml` and from `docker-compose.yml` together, and put your own endpoint in the `models.providers` block.

### The container image

```bash
docker run --rm -p 8080:8080 \
  -e DATABASE_URL='postgres://rumil:rumil@<db-host>:5432/rumil?sslmode=disable' \
  -e GITHUB_WEBHOOK_SECRET=... \
  -e GITHUB_APP_PRIVATE_KEY_PATH=/run/secrets/github_app.pem \
  -e OPENAI_API_KEY=... \
  -v /etc/rumil/rumil.yaml:/etc/rumil/rumil.yaml:ro \
  -v /path/to/your-app.pem:/run/secrets/github_app.pem:ro \
  ghcr.io/joangrigorov/rumil:latest serve --config /etc/rumil/rumil.yaml
```

The `${VAR}` placeholders are expanded inside the container, so every variable the config names has to arrive with `-e`. An unexpanded one becomes an empty string, and Rumil then refuses to start on the missing value instead of serving without it.

The published image is `linux/amd64`. On another architecture, use `docker compose up`, which compiles Rumil for the host.

### The installed binary

```bash
rumil serve
```

Without `--config` it reads the first file of the [search](#where-the-file-is-found) — `/etc/rumil/rumil.yaml` for a package install, `./rumil.yaml` for an extracted archive. It serves on `server.listen` (`:8080` unless the config or `--listen` says otherwise). The database at `database.dsn` has to exist with the `pgvector` extension in it; with `database.auto_migrate: true` Rumil creates and updates its own tables at startup.

### What a review does

1. GitHub delivers a webhook. Rumil verifies the signature and queues the job in Postgres.
2. A worker claims the job and starts a review session against your model provider. The session reads the pull request description and the files it touches, the annotated diff and the surrounding file contents, the whole conversation with every reply, and any threads earlier rounds left open.
3. It posts one review: a summary of what earlier rounds raised and what this one found, plus inline comments anchored to lines in the diff. Where an earlier thread's finding is genuinely gone from the code, it posts a one-line reason into that thread and resolves it, so a resolution is never silent. An installation whose Contents permission is Read cannot resolve a thread — GitHub refuses the mutation — so in that configuration the reason goes into the thread, the summary names the threads to close, and a person clicks resolve.

To start one, mention the App's handle in a comment with `review` and nothing else:

```text
@<the App's handle> review
```

The comment is the trigger, and the only one. A GitHub App cannot be requested as a reviewer on github.com: `POST /pulls/{n}/requested_reviewers` answers 200 and adds nobody, `gh pr edit --add-reviewer 'slug[bot]'` fails to resolve the login, and the **Reviewers** list offers people, teams and GitHub Copilot, which is special-cased. GitHub's own documentation describes requesting a review from a person, a team or Copilot, and from nothing else. Mentioning the App in a comment reaches it the way a mention reaches any account.

Rumil answers a command with a `+1` reaction on the comment as soon as it has taken it, so a request that was picked up looks different from one that was dropped. The review itself follows a minute later. A later push is not reviewed on its own; ask again to review it, and the next round reads what the earlier ones raised.

## Repository guidelines

If a repository keeps a `RUMIL.md` at its root, Rumil reads it before the review and treats it as that repository's own policy — it outranks everything else Rumil has been told, including remembered rules, unless a maintainer promotes a rule above it (below). It is capped at 32 KB.

This is where a team writes down the conventions it wants enforced: "we do not use an ORM in this service", "every exported function needs a doc comment", "money is always `decimal`, never `float`". Write it as instructions to a reviewer; that is exactly how Rumil reads it.

## Teaching Rumil your conventions

Rumil can record rules from the conversation, so a decision made once in review is not re-litigated on the next pull request.

**This is opt-in.** Set `database.dsn` and `models.default_embedding` at an embedding provider to turn it on. Without them, Rumil reviews normally and records nothing. The provider's `dimensions` is the width of the vectors its model returns, and memory is stored and searched at that width: any width up to 2000 works, so a local embedding server is as good as a hosted one.

### Recording a rule

On a private repository, anyone who can comment on a pull request can write a rule. On a public one the commenter needs a standing in the repository — `OWNER`, `MEMBER`, `COLLABORATOR` or `CONTRIBUTOR` — because a public repository takes comments from anyone, and a stranger's rule would queue work and spend the operator's embedding credits. A `review` command needs the same standing, for the same reason. Mention the App's handle in a comment with `remember` and the rule in your own words:

```text
@rumil remember this service deliberately tolerates 60 seconds of clock skew in the JWT validator
```

Rumil rewrites the note into a self-contained rule — naming the package, path or pattern instead of "this" and "here" — and answers with an ID and a status:

```text
Memory proposal `#3df15f48` queued for maintainer moderation (`status: PENDING`).
```

A rule for the whole organization rather than one repository:

```text
@rumil remember --org all Go services log with log/slog instead of logrus
```

An organization-wide rule is offered to the model as such; keep repository-specific detail out of it, because it reaches every repository in the organization that has nothing more specific to say.

### Approving a rule

A proposed rule does nothing until a maintainer approves it. Nothing retrieves a pending rule, so a comment cannot put text in front of the reviewer on its own.

On a terminal, `rumil mod` opens the rules in a browser: move through the queue with the arrow keys, press enter to open one and read everything recorded about it — the rule, why it was recorded, what it applies to, the comment it came from, who asked — and decide it there. The rule screen offers approving it, editing the wording before approving, rejecting it, and promoting it above the repository's `RUMIL.md`. `--moderator` names who the decision is recorded against. With the input or the error output redirected there is nothing to move around with, so it prints the queue instead:

```bash
rumil mod                                   # the browser, on a terminal
rumil mod list                              # everything, with its status
rumil mod list --status pending             # what is waiting
rumil mod list --status approved --repo acme/payments
rumil mod approve 3df15f48 --moderator alice
rumil mod approve 3df15f48 --rule-text "the JWT validator's 60s clock skew tolerance is intentional" --moderator alice
rumil mod reject 3df15f48 --moderator alice
```

Or from the moderation page, which needs no JavaScript and no session. It answers only when `moderation.admin_token` is set — the example config reads that from `${RUMIL_ADMIN_TOKEN}` — and takes the value as a bearer credential or as the password of a basic-auth credential (any username):

```text
https://<your-rumil-host>/ui/moderation
```

The same decisions are available as JSON for scripting: `POST /api/moderation/<id>/approve` and `POST /api/moderation/<id>/reject`, with form fields `rule_text`, `path_glob`, `scope`, `authority` and `moderator`.

Approving can also narrow a rule: an organization-wide proposal can be recorded for the repository it was proposed in with `--scope REPO`. The other direction is refused, because one decision on a page should not push a rule that reaches a single repository out to every repository in the organization.

Setting `moderation.auto_approve_associations` lets named trusted commenters record rules without approval — for example `["OWNER", "MEMBER"]`. `moderation.accept_all: true` turns the gate off for everyone; it exists for a private instance where every commenter is trusted, and Rumil logs a warning at startup for as long as it is on.

### Raising a rule above the repository guidelines

Everything a maintainer approves is recorded context, and a repository's `RUMIL.md` outranks it. To make one rule win instead — a decision the repository's own guidelines contradict, which the team nonetheless wants enforced — promote it at the gate:

```bash
rumil mod approve 3df15f48 --authority directive --moderator alice
```

A directive outranks `RUMIL.md` for the case it names, and the reviewer is told to say so in its summary when the two conflict. A directive applies to one repository, so an organization-wide rule has to be narrowed to a repository first, in the same decision:

```bash
rumil mod approve 3df15f48 --scope REPO --authority directive --moderator alice
```

`--authority context` puts a promoted rule back to recorded context.

## Troubleshooting

**A review never appears.** Rumil reviews only when the App is requested as a reviewer, so check that the pull request actually asks for it. Then look at the App's **Advanced → Recent Deliveries**: a delivery that is not a review request, comes from another installation, or carries a bad signature is answered `200` or `401` and logged with the reason it was ignored. `docker compose logs -f rumil` shows those lines.

**"review session ended without submitting a review".** The session stopped without saying anything about the pull request. It reached `app.max_agent_turns`, or the chat endpoint does not support tool calling. The run is recorded as failed. Raise the limit, set `app.max_agent_turns: 0` for no cap, or point `models.default_chat` at a larger model.

**A reasoning model answers suspiciously fast.** Set `models.providers[].thinking_level` on the chat provider, for example `thinking_level: "high"` or `thinking_level: "max"` where the provider exposes it. Rumil sends it as `reasoning_effort` on the chat-completions request. `off` leaves the provider's default in place, and a provider that does not accept the field may reject the request.

**Every webhook delivery is rejected.** `github.webhook_secret` must match the App's webhook secret exactly, and the endpoint GitHub reaches is `POST /webhooks/github` on whatever host you configured.

**`/ui/moderation` answers "the moderation surface needs moderation.admin_token".** Set `moderation.admin_token` — the example config takes it from `${RUMIL_ADMIN_TOKEN}` — and restart. Until then the page and the API refuse every request rather than standing open.

**The container will not start on this machine.** The published image is `linux/amd64`. Use `docker compose up`, which compiles Rumil for the host architecture.

**Rules are recorded but never used in a review.** Memory needs `models.default_embedding` pointing at a provider whose `dimensions` match the width its model returns, and the approved status: `rumil mod list --status approved` should show the rule. If the provider's width has changed since the rules were recorded, startup says so and names the existing width, because vectors of two widths cannot be compared.

## License

Apache License 2.0 — see [LICENSE](LICENSE).
