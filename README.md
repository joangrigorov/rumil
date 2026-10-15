# Rumil

Rumil is a self-hosted GitHub App that reviews your pull requests. It reads a pull request, posts a normal GitHub review with inline comments, and — if you turn it on — remembers the engineering rules your team states in the conversation, so later reviews stop flagging deliberate decisions as mistakes.

It is **read-only**: the only things it writes are the review itself, the replies and resolutions on the review threads it opened, and the confirmation it posts when it records a rule. It cannot push, edit files, or change repository settings.

It **never reviews on its own**: nothing happens when a pull request opens or gains commits. A review starts when the App is *requested as a reviewer*, and that is the only trigger it accepts.

It ships **no hosted service and no shared App**. You register your own GitHub App, so your code, your rules, and your tokens stay in your account and on your infrastructure. You also bring your own model provider: any OpenAI-compatible chat endpoint that supports tool calling works.

## Contents

- [How a review happens](#how-a-review-happens)
- [Requirements](#requirements)
- [Install](#install)
- [Create the GitHub App](#create-the-github-app)
- [Configuration](#configuration)
- [Using Rumil](#using-rumil)
- [Teaching Rumil your conventions](#teaching-rumil-your-conventions)
- [Troubleshooting](#troubleshooting)
- [License](#license)

## How a review happens

1. GitHub delivers a webhook to your Rumil instance. Rumil verifies the signature and queues the job in Postgres.
2. A worker claims the job and starts a review session against your model provider. The session reads the pull request description and the files it touches, the annotated diff and surrounding file contents, the whole conversation with every reply, and any threads earlier rounds left open.
3. It posts one review: a summary of what earlier rounds raised and what this one found, plus inline comments anchored to lines in the diff. Where an earlier thread's finding is genuinely gone from the code, it resolves that thread with a one-line reason, so a resolution is never silent.

The review is a single agent session with a tool set scoped to that one pull request. The model can read the pull request and the repository, and search the rules you have approved; it has no tool that writes to the repository.

## Requirements

- **A Postgres 16+ database with the `pgvector` extension.** The `pgvector/pgvector:pg16` image is the easiest way to get one. Rumil creates its own tables on boot when `database.auto_migrate` is on, so its database user needs the right to create extensions. On a managed Postgres, check that before you start; the failure appears on the first boot.
- **A GitHub App of your own**, installed on the repositories Rumil should review.
- **An OpenAI-compatible chat endpoint** and an API key for it. The endpoint has to support OpenAI-style tool calling: a review is an agent session that calls tools, and an endpoint that answers chat completions without them produces no review.
- **Optionally, an embedding provider** with 1536-dimension embeddings. That is what turns on recorded rules; without it Rumil reviews normally and has no memory.
- **Inbound HTTPS** from GitHub to your instance, for webhook deliveries. Any reverse proxy works; a local run needs a tunnel that GitHub can reach.

Every review spends tokens on your chat provider, and recording or retrieving a rule spends them on your embedding provider. Nobody bills you but your provider.

## Install

Whichever way you install it, Rumil needs the values that [Create the GitHub App](#create-the-github-app) produces: the App ID, the installation ID, the private key and the webhook secret. Create the App first, or read that section alongside this one.

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

### Released binaries and packages

Every release publishes static binaries and packages for Linux, macOS and Windows:

- `rumil_<version>_linux_x86_64.tar.gz`, `_linux_arm64`, `_linux_armv7`, `_darwin_x86_64`, `_darwin_arm64`, `_windows_x86_64.zip`, `_windows_arm64.zip` — each archive holds the `rumil` binary, a commented `rumil.example.yaml` and the license text
- `.deb`, `.rpm`, `.apk` and `.pkg.tar.zst` packages for the common Linux distributions, which install the binary to `/usr/bin/rumil`, the example config to `/etc/rumil/rumil.example.yaml` and the license to `/usr/share/licenses/rumil/LICENSE`
- `checksums.txt` with the SHA-256 of every artifact

Download them from the [releases page](https://github.com/joangrigorov/rumil/releases/latest) and verify against `checksums.txt`. From an extracted archive:

```bash
cp rumil.example.yaml rumil.yaml     # then fill it in
./rumil serve --config ./rumil.yaml
```

From a package, `rumil` is on `PATH` already and the example config is installed for you:

```bash
cp /etc/rumil/rumil.example.yaml /etc/rumil/rumil.yaml   # then fill it in
rumil serve --config /etc/rumil/rumil.yaml
```

### Container image

A prebuilt image is published to `ghcr.io/joangrigorov/rumil`, tagged with the version, the minor version and `latest`:

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

## Create the GitHub App

Rumil authenticates as a GitHub App you own. Nothing here is shared with anyone else.

1. Go to **Settings → Developer settings → GitHub Apps → New GitHub App** (for an organization, use the organization's settings). Give it a name; its slug becomes the handle people type in comments (for example an App named `Acme Rumil` answers to `acme-rumil`).
2. **Homepage URL**: anything, for example your Rumil instance's address.
3. **Webhook URL**: `https://<your-rumil-host>/webhooks/github`.
4. **Webhook secret**: a long random string. Put the same value in `github.webhook_secret`.
5. **Repository permissions**:

   | Permission | Access | Why |
   | :--- | :--- | :--- |
   | Contents | Read | Read `RUMIL.md` and the file contents behind a diff hunk |
   | Issues | Read and write | Read the pull request conversation, and post the reply to a remembered rule |
   | Pull requests | Read and write | Read metadata, diffs, reviews and review threads; submit the review, reply to review comments, and resolve threads |
   | Metadata | Read | Always required by GitHub |

6. **Subscribe to events**: *Pull request*, *Issue comment*, and *Pull request review comment*.
7. Create the App, then **Generate a private key** and download the `.pem`. Point `github.private_key_path` at it, or put its contents in the `GITHUB_PRIVATE_KEY` environment variable.
8. Note the **App ID** on the App's settings page, which is `github.app_id`.
9. **Install** the App on the account that owns the repositories it should review, choosing either all repositories or a selection. The installation's ID is the number at the end of the URL after installing (`.../installations/<id>`); that is `github.installation_id`.

To review a pull request, open it and use **Reviewers → request the App**. Rumil starts working within a second or two and posts the review when the session finishes.

## Configuration

Rumil reads one YAML file, `rumil.yaml` by default; `--config` or `-c` points somewhere else. Before the YAML is parsed, `${VAR}` placeholders are expanded from the environment, so secrets never have to live in the file. `rumil.example.yaml` documents every key: it is in the repository, and an installed package puts it at `/etc/rumil/rumil.example.yaml`.

A minimal configuration that reviews pull requests with OpenAI:

```yaml
app:
  bot_handle: "acme-rumil"          # your App's slug, as people type it in comments

github:
  app_id: 123456
  installation_id: 98765432
  private_key_path: "${GITHUB_APP_PRIVATE_KEY_PATH}"
  webhook_secret: "${GITHUB_WEBHOOK_SECRET}"

models:
  default_chat: "gpt-4o-mini"       # any OpenAI-compatible endpoint
  providers:
    - id: "gpt-4o-mini"
      provider: "openai-compatible"
      base_url: "https://api.openai.com/v1"
      model_name: "gpt-4o-mini"
      api_key: "${OPENAI_API_KEY}"

database:
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
| `app.bot_handle` | `@rumil` | Your App's slug. A leading `@` and a trailing `[bot]` are ignored, so `acme-rumil`, `@acme-rumil` and `acme-rumil[bot]` all match |
| `app.max_agent_turns` | `25` | How many model turns one session may take. A session that runs out submits nothing, and the log says so |
| `app.max_diff_bytes` | `250000` | Larger diffs are paginated instead of handed over in one piece |
| `app.log_level` | `info` | `debug`, `info`, `warn` or `error`. Logs are JSON on stderr |
| `review.guidelines_ref` | `base` | Which revision `RUMIL.md` is read from: `base` (the pull request's base, so a branch cannot rewrite its own rules) or `head` |
| `review.resolve_threads` | `true` | Whether Rumil resolves its own review threads once the finding is gone. From the second review of a pull request on, and at most 20 in one review |
| `memory.repo_scope_boost` | `0.05` | How much a repository's own rule outranks an organization-wide one when both are candidates |
| `worker.concurrency` | `4` | Reviews processed in parallel |
| `server.listen` | `:8080` | Address for the webhook server; `--listen` overrides it |
| `models.default_chat` | first non-embedding provider | Which provider reviews pull requests |
| `models.default_embedding` | unset | The provider that turns memory on. A provider is an embedding provider when it sets `dimensions: 1536` |
| `moderation.accept_all` | `false` | The bypass for the rule approval gate. Leave it `false` |
| `moderation.auto_approve_associations` | empty | GitHub `author_association` values whose comments record rules without approval, for example `["OWNER", "MEMBER"]` |
| `moderation.admin_token` | unset | Bearer token (or basic-auth password) for the moderation UI and API. Unset means both refuse every request |

## Using Rumil

### Request a review

Open a pull request and request the App under **Reviewers**. Rumil reviews the pull request as it stands and posts one review. To review a new push, request it again.

Rumil answers each round with the state of the review: what earlier rounds raised, what is still open, what this round found, and what it withdrew and why. It does not repeat a finding an open thread already covers.

### Repository guidelines

If a repository keeps a `RUMIL.md` at its root, Rumil reads it before the review and treats it as that repository's own policy — it outranks everything else Rumil has been told, including remembered rules, unless a maintainer promotes a rule above it (below). It is capped at 32 KB.

This is where a team writes down the conventions it wants enforced: "we do not use an ORM in this service", "every exported function needs a doc comment", "money is always `decimal`, never `float`". Write it as instructions to a reviewer; that is exactly how Rumil reads it.

### One-off review from the command line

`rumil review` reviews a single pull request and submits the review, with no webhook and no queue:

```bash
rumil review --config rumil.yaml --repo acme/payments --pr 42
```

It needs the App credentials and a chat provider. With `database.dsn` and an embedding provider set, it also uses the rules you have approved; without both, it reviews with no memory and logs a warning saying so.

## Teaching Rumil your conventions

Rumil can record rules from the conversation, so a decision made once in review is not re-litigated on the next pull request.

**This is opt-in.** Set `database.dsn` and `models.default_embedding` with a 1536-dimension provider to turn it on. Without them, Rumil reviews normally and records nothing.

### Recording a rule

On a private repository, anyone who can comment on a pull request can write a rule. On a public one the commenter needs a standing in the repository — `OWNER`, `MEMBER`, `COLLABORATOR` or `CONTRIBUTOR` — because a public repository takes comments from anyone, and a stranger's rule would queue work and spend the operator's embedding credits. Mention the App's handle in a comment with `remember` and the rule in your own words:

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

Approve from the command line, using the short ID from the reply:

```bash
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

**"review session ended without submitting a review".** The session reached `app.max_agent_turns` before it submitted anything, or the chat endpoint does not support tool calling. Raise the limit, or point `models.default_chat` at a larger model.

**Every webhook delivery is rejected.** `github.webhook_secret` must match the App's webhook secret exactly, and the endpoint GitHub reaches is `POST /webhooks/github` on whatever host you configured.

**`/ui/moderation` answers "the moderation surface needs moderation.admin_token".** Set `moderation.admin_token` — the example config takes it from `${RUMIL_ADMIN_TOKEN}` — and restart. Until then the page and the API refuse every request rather than standing open.

**The container will not start on this machine.** The published image is `linux/amd64`. Use `docker compose up`, which compiles Rumil for the host architecture.

**Rules are recorded but never used in a review.** Memory needs `models.default_embedding` pointing at a provider with `dimensions: 1536`, and the approved status: `rumil mod list --status approved` should show the rule.

## License

Apache License 2.0 — see [LICENSE](LICENSE).
