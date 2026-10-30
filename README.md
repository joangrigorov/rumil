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
| `rumil sessions` | Continue a failed review session so it does not start from the beginning. |
| `rumil findings` | Report what happened to the findings Rumil posted, grouped by model. |
| `rumil eval` | Run the reviewer against a local fixture set and score the findings. |
| `rumil mod` | List the recorded rules, and approve or reject them. |

## Contents

- [Requirements](#requirements)
- [Install](#install)
- [Configure](#configure)
- [Review one pull request from the command line](#review-one-pull-request-from-the-command-line)
- [Run the webhook server](#run-the-webhook-server)
- [Custom MCP servers](#custom-mcp-servers)
- [Token usage and cost](#token-usage-and-cost)
- [The queue and cancelling a review](#the-queue-and-cancelling-a-review)
- [Metrics](#metrics)
- [Asking why about a finding](#asking-why-about-a-finding)
- [Provider fallback](#provider-fallback)
- [Repository guidelines](#repository-guidelines)
- [Teaching Rumil your conventions](#teaching-rumil-your-conventions)
- [Measuring review quality](#measuring-review-quality)
- [Checking an installation with `rumil doctor`](#checking-an-installation-with-rumil-doctor)
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
   | Checks | Read and write | Report each review's progress as a check run. **Required for the check run:** with Read alone, GitHub refuses the create and update calls. Rumil logs the refusal and posts the review without a check, so the feature is missing rather than the review failing |
   | Issues | Read and write | Read the pull request conversation, and post the reply to a remembered rule |
   | Pull requests | Read and write | Read metadata, diffs, reviews and review threads; submit the review, and reply to review comments |
   | Metadata | Read | Always required by GitHub |

   Checks must be Read and write for the check run. Take the permission in the App settings, then accept the updated permissions on every installation: a token minted before the installation's owner accepted the change does not carry it, and the check is skipped until a fresh token is minted.

4. Create the App, then **Generate a private key** and download the `.pem`. Point `github.private_key_path` at it, or put its contents in the `GITHUB_PRIVATE_KEY` environment variable.
5. Note the **App ID** on the App's settings page: that is `github.app_id`.
6. **Install** the App on the account that owns the repositories it should review, all of them or a selection. The installation's ID is the number at the end of the URL after installing (`.../installations/<id>`): that is `github.installation_id`. A deployment that serves several accounts of the same App lists the others under `github.installations` ([Serving several installations](#serving-several-installations)).

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
| `github.installation_id` | The installation to serve, from the same section. It stays the unnamed first installation when `github.installations` is set |
| `github.installations` | The other installations of the same App, each with `id` and a unique `name`, plus optional `chat_provider`, `worker_concurrency`, `max_total_tokens` and `max_cost_usd`. At most 50 installations in total. See [Serving several installations](#serving-several-installations) |
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
| `app.max_diff_bytes` | `250000` | Larger diffs are paginated instead of handed over in one piece. A pull request above GitHub's 300-file diff limit is read through the files API instead, so it still gets a review and still accepts inline comments |
| `app.log_level` | `info` | `debug`, `info`, `warn` or `error`. Logs are JSON on stderr |
| `mcp_servers` | empty | Operator-provided MCP servers whose tools a review may call. Each needs a `name` and either `command` (stdio) or `url` (http), and `tools.allow` names the tools that are exposed. See [Custom MCP servers](#custom-mcp-servers) |
| `review.guidelines_ref` | `base` | Which revision `RUMIL.md` is read from: `base` (the pull request's base, so a branch cannot rewrite its own rules) or `head` |
| `review.resolve_threads` | `true` | Whether Rumil resolves its own review threads once the finding is gone. From the second review of a pull request on, and at most 20 in one review. GitHub refuses the mutation unless the **installation's** Contents permission is Read and write, whatever its Pull requests permission says; `rumil serve` warns at startup when it is not, and the warning also covers an App whose permission was raised but whose installation has not accepted the change yet, and a round then posts the reason into the thread and names it in the summary |
| `review.max_total_tokens` | unset | Per-session token ceiling, counting input, output and cache tokens together. The session stops before its next step once the reported usage passes it. `0` or unset is no limit; see [Token usage and cost](#token-usage-and-cost) |
| `review.max_cost_usd` | unset | Per-session estimated-cost ceiling. Requires `price_input_per_mtok` and `price_output_per_mtok` on the chat provider. `0` or unset is no limit |
| `compaction.enabled` | `true` | Whether a session summarizes its older context once it reaches `compaction.trigger_tokens`. On by default; set it to `false` to let the model's context window fill |
| `compaction.trigger_tokens` | `262144` | The estimated context size at which older messages are summarized. Nothing is compacted before this point, so a 256K trigger leaves a large pull request's details in context |
| `compaction.reserve_tokens` | `16384` | Output budget kept below the trigger, so the session can still answer after the context grows |
| `compaction.keep_recent_tokens` | `20000` | The most recent context kept verbatim; older messages are replaced by a summary |
| `snapshot.enabled` | `true` | Whether a session materializes the reviewed commit as a read-only working tree in a temporary directory. It backs the listing, search and read tools; turning it off leaves the GitHub API tools, which cost a request per lookup |
| `snapshot.max_bytes` | `536870912` | The largest working tree a session extracts. A bigger repository falls back to the GitHub API tools instead of filling the disk |
| `snapshot.timeout` | `2m` | How long the archive download and extraction may take, as a Go duration. The time comes out of the job's own `worker.job_timeout` |
| `snapshot.dir` | operating system temp directory | Where the per-session working trees are created. Set it when the temp directory has no room for the repository |
| `memory.repo_scope_boost` | `0.05` | How much a repository's own rule outranks an organization-wide one when both are candidates |
| `worker.concurrency` | `4` | Reviews processed in parallel. With `github.installations`, each installation gets its own pool of this size unless its `worker_concurrency` says otherwise |
| `worker.job_timeout` | `15m` | Wall-clock budget for one review or command job, as a Go duration such as `30m` or `1h`. It must be positive. `app.max_agent_turns: 0` removes the step cap, but this timeout still bounds the run |
| `server.listen` | `:8080` | Address for the webhook server; `--listen` overrides it |
| `server.public_url` | unset | Public base address of the deck, used as the details link on the review's GitHub check run. Unset leaves the check without a link |
| `server.metrics_listen` | unset | Address for a separate Prometheus listener, such as `127.0.0.1:9090`. Unset leaves `/metrics` on the main listener behind `moderation.admin_token`; setting it moves the endpoint there without a credential, so bind it to loopback or a firewalled interface |
| `models.default_chat` | first non-embedding provider | Which provider reviews pull requests |
| `models.default_embedding` | unset | The provider that turns memory on. A provider is an embedding provider when it sets `dimensions`, which is the width of the vectors its model returns (1536 for `text-embedding-3-small`, 768 for `embed-gemma`) |
| `models.providers[].thinking_level` | unset | Reasoning effort for a chat provider: `off`, `none`, `minimal`, `low`, `medium`, `high` or `max`. Rumil sends it as `reasoning_effort` on the chat-completions request. `off` leaves the provider's default in place; `max` is for providers that expose it, such as DeepSeek V4 Flash |
| `models.providers[].price_input_per_mtok` | unset | What a million plain input tokens cost on this chat provider, for the estimated cost. Unset means no estimate |
| `models.providers[].price_output_per_mtok` | unset | What a million output tokens cost. `price_input_per_mtok` and this one turn the estimate on |
| `models.providers[].price_cache_read_per_mtok` | unset | What a million input tokens served from the provider's cache cost. Unset counts cached input at zero |
| `models.providers[].max_tokens` | unset | The most tokens one model response may generate, reasoning included. Unset leaves the provider's default, which can be far below the context window: DeepSeek's chat endpoint generates at most 8192 unless asked for more, so `thinking_level: max` can spend the whole budget on reasoning and never reach the tool call. Set it above the reasoning a step needs |
| `models.providers[].embedding_timeout` | `30s` | The budget for one embeddings call, as a Go duration. Only an embedding provider (one with `dimensions`) may set it. A local backend can spend a minute or more on its first call while it loads the model, so raise it when the embeddings server starts on demand; the review then keeps its pre-retrieved rules instead of logging `memory_failed` |
| `models.providers[].request_timeout` | `2m` | How long a chat provider may take to send its response headers, as a Go duration. Only a chat provider may set it. A provider that queues a request can sit silent for minutes; the attempt fails at this budget so the retry policy can try again, rather than holding a worker until `worker.job_timeout`. The streamed answer is not cut off, because only the wait before its first byte is bounded |
| `moderation.accept_all` | `false` | The bypass for the rule approval gate. Leave it `false` |
| `moderation.auto_approve_associations` | empty | GitHub `author_association` values whose comments record rules without approval, for example `["OWNER", "MEMBER"]` |
| `moderation.admin_token` | unset | The single admin credential, kept for a configuration written before named credentials. It is accepted as an entry named `legacy`. A bearer token or basic-auth password for the moderation UI, the deck, cancellation and `/metrics`; the pages set a same-site cookie that authenticates reads only, so the session live stream can connect where `EventSource` cannot send a header. Unset, with no `admin_tokens`, means every operator surface refuses every request |
| `moderation.admin_tokens` | empty | Named admin credentials: `[{name, token}]`. Every entry is accepted, and the name of the matching entry is what the audit trail records. Names are 1–64 letters, digits, `.`, `_` or `-`; names and tokens must be unique. At most 16 entries |

## Review one pull request from the command line

`rumil review` is the shortest way to a review: no server, no queue, no webhook, no database.

```bash
rumil review --repo acme/payments --pr 42
```

It needs the App credentials and a chat provider, and it submits the review as the App. It reads the file the [search](#where-the-file-is-found) finds, so the `rumil.yaml` you copied in the step above is enough, and `--config` names another one. With several installations configured, it uses the first one that can read the repository; `--installation` names one explicitly. With `database.dsn` and an embedding provider set, it also applies the rules you have approved; without both, it reviews with no memory and logs a warning saying so. A run that ends without submitting a review exits non-zero; the server records the same outcome as a failed job.

## Run the webhook server

`rumil serve` is what puts Rumil into the pull request flow: it receives GitHub's webhooks, queues them, and reviews when someone requests the App as a reviewer. It needs Postgres and the App's webhook settings, which the command line path above does not.

### Point the App at your server

In the App's settings:

1. **Webhook URL**: `https://<your-rumil-host>/webhooks/github`
2. **Webhook secret**: a long random string, and the same value as `github.webhook_secret`
3. **Subscribe to events**: *Pull request*, *Issue comment* and *Pull request review comment*. Add *Pull request review* and *Reaction* if you want the finding-quality report to fill in

### Serving several installations

One Rumil process can serve several installations of the same App, which is one deployment for a set of accounts rather than one process each. List them under `github.installations`; `github.installation_id` keeps working as the unnamed first installation, so a configuration written before this option needs no change.

```yaml
github:
  app_id: 123456
  installation_id: 98765432     # the first installation, unnamed
  installations:
    - id: 55555555
      name: "acme-eu"           # shown in logs and on the deck
      chat_provider: "deepseek" # optional: this account's default review model
      worker_concurrency: 2     # optional: this account's workers, else worker.concurrency
      max_total_tokens: 2000000 # optional: overrides review.max_total_tokens
      max_cost_usd: 5.0         # optional: overrides review.max_cost_usd
  private_key_path: "${GITHUB_APP_PRIVATE_KEY_PATH}"
```

Every installation belongs to the same App, so they share the private key and the App-wide webhook secret. Install the App on each account and use that account's installation id.

The webhook accepts a delivery through any listed installation and ignores one through an account the configuration does not name, before anything is written. Each installation gets its own worker pool, which claims only its account's jobs, so a busy account cannot take another's workers. A repository rule (`review` settings in `.rumil.yaml`) applies to its repository; `chat_provider` and the budgets apply when that account's repository does not set them.

`rumil review` resolves the installation from `--repo` by asking each configured installation in turn which one can read it, and `--installation` names one explicitly. The moderation page, the session deck and the finding report are not per-tenant: they read and write with the primary installation, which is `github.installation_id` when it is set and otherwise the first listed installation.

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
2. A worker claims the job and starts a review session against your model provider. Before the first turn it downloads the reviewed commit as a tarball into a temporary directory and removes it when the session ends. The session reads the pull request description and the files it touches, the annotated diff and the surrounding file contents, the whole conversation with every reply, and any threads earlier rounds left open. That working tree backs `list_repository_tree`, `list_directory`, `search_code` and the default file read, so the reviewer finds exact paths and searches the code locally instead of spending a round on each guessed path. A repository larger than `snapshot.max_bytes`, or a download that fails, falls back to the GitHub API tools and the review continues.
3. It posts one review: a summary of what earlier rounds raised and what this one found, plus inline comments anchored to lines in the diff. Where an earlier thread's finding is genuinely gone from the code, it posts a one-line reason into that thread and resolves it, so a resolution is never silent. An installation whose Contents permission is Read cannot resolve a thread — GitHub refuses the mutation — so in that configuration the reason goes into the thread, the summary names the threads to close, and a person clicks resolve.

To start one, mention the App's handle in a comment with `review`:

```text
@<the App's handle> review
```

The words after the verb are the review's focus: Rumil puts them in front of the reviewer, so it spends its effort where you point it. A sentence reads naturally, and a closing period or colon after the verb is fine:

```text
@<the App's handle> review. Focus on migration consistency and deployment instructions
```

The focus is capped at 2 KB and cut at a character boundary. It narrows where the reviewer looks; the review still covers the rest of the diff.

The comment is the trigger, and the only one. A GitHub App cannot be requested as a reviewer on github.com: `POST /pulls/{n}/requested_reviewers` answers 200 and adds nobody, `gh pr edit --add-reviewer 'slug[bot]'` fails to resolve the login, and the **Reviewers** list offers people, teams and GitHub Copilot, which is special-cased. GitHub's own documentation describes requesting a review from a person, a team or Copilot, and from nothing else. Mentioning the App in a comment reaches it the way a mention reaches any account.

Rumil answers a command with a `+1` reaction on the comment as soon as it has taken it, so a request that was picked up looks different from one that was dropped. The review itself follows a minute later. A later push is not reviewed on its own; ask again to review it, and the next round reads what the earlier ones raised.

Each review also opens a **Rumil review** check run on the reviewed commit. It shows as in progress while the session runs and completes as success for an approval, neutral for a comment-only review, and action required for requested changes. A failed session completes it as a failure with the reason. One check run covers a commit, so a continuation updates it rather than adding another. This feature needs the App's **Checks: Read and write** repository permission, accepted on the installation: with Checks on Read only, or off, GitHub refuses the check-run call, Rumil logs the refusal, and the review still posts without a check. Set `server.public_url` to link the check to the session page.

### Continuing a failed review

A session that ends without calling `submit_pr_review` — because it used its turn budget, hit a provider error, or had a response cut off at the model's output limit — is recorded as failed. Its transcript is kept, so an admin can give it another attempt without paying to read the pull request from the first byte:

```bash
rumil sessions continue <session-id> --turns 25 --requested-by alice
```

`--turns` is the new attempt's own step budget. Without it the attempt uses `app.max_agent_turns`, and a configured `0` still means unlimited. The command queues a job and prints the session it will continue; `rumil serve` runs it. The new attempt starts with the earlier attempt's tool results, partial summary and the same commit, and is recorded as a separate session that points at its parent, with who asked and when. A session that already submitted a review is refused, and so is one whose pull request has moved since the earlier attempt.

The session page at `/ui/sessions/<session-id>` has the same action: **Continue review** appears for any failed session that has not been continued yet, and uses the configured budget. It shares `moderation.admin_token` with the moderation surface, so the page records the credential as the requester and the CLI records `cli`; `--requested-by` and the API's `requested_by` field name somebody explicitly. The CLI needs the database; neither surface needs a GitHub credential of its own.

## Provider fallback

`models.fallback` lists chat providers, in order, that Rumil tries when the selected provider fails before a review is submitted:

```yaml
models:
  default_chat: "primary"
  fallback: ["secondary", "third"]
```

A provider is tried when it exhausts its attempts on transport errors, 5xx responses, repeated 429s, or a response-header timeout. Authentication and invalid-request failures stop the attempt instead: another provider would answer the same request the same way. Embedding providers are not part of the chain, because an embedding failure already degrades memory without failing the review.

A fallback starts a new attempt on the same commit with the earlier attempt's tool calls and partial text, capped the same way a continuation is. A review that already reached GitHub is never followed by another attempt, so a pull request is reviewed at most once. The session records the provider that produced the review, and the deck shows a **Falling back to another provider** event naming the two providers. The list holds at most five providers, and each must be a chat provider the operator configured. A provider a repository chose in `.rumil.yaml` falls back through the same operator list.

## Custom MCP servers

Rumil's own tools read the pull request and the reviewed commit. An [MCP](https://modelcontextprotocol.io) server adds capabilities it does not ship, for example a workspace that checks the commit out and runs its test suite, or an internal documentation search. The reviewer sees the allowed tools next to its own and can call them during a review.

```yaml
mcp_servers:
  - name: "workspace"                 # lowercase handle, letters, digits, - and _
    transport: "stdio"                # stdio (default) or http
    command: "rumil-workspace"        # stdio: the program Rumil starts
    args: ["--root", "/srv/checkout"]
    env:                              # merged over the service's own environment
      WORKSPACE_TOKEN: "${WORKSPACE_TOKEN}"
    timeout: "60s"                    # one connect, list or call
    max_output_bytes: 65536           # one tool result, cut with a marker
    tools:
      allow: ["run_tests", "list_checks"]   # the trust boundary; empty exposes none

  - name: "docs"
    transport: "http"
    url: "https://mcp.example.com/mcp"
    headers:
      Authorization: "Bearer ${DOCS_TOKEN}"
    tools:
      allow: ["search_docs"]
```

**The allow list is the trust boundary.** An MCP server runs with the service account's privileges, outside any sandbox: a tool can run commands, write files, or make network requests. Rumil exposes only the tools named in `tools.allow`, and names each one `mcp__<server>__<tool>` so the prompt can tell it from a built-in tool. Review the server and the tools you allow as you would review any code that runs in production.

A stdio server receives the session as environment variables, in addition to the service's own environment and its configured `env`:

| Variable | Value |
| :--- | :--- |
| `RUMIL_REPO` | `owner/name` |
| `RUMIL_PR` | The pull request number |
| `RUMIL_HEAD_SHA` | The commit under review |
| `RUMIL_SESSION_ID` | The recorded session id, when there is one |
| `RUMIL_WORKSPACE` | The extracted working tree at the reviewed commit, when the snapshot is on |

`RUMIL_WORKSPACE` is what lets a test-runner server act on the same commit the reviewer is reading. It is removed when the session ends.

A server that fails to connect, or a tool that errors, costs that server's tools for that review; the review continues with its own. Tool output is capped at `max_output_bytes` (1 MiB at most) and treated as untrusted data by the reviewer's rules.

## Token usage and cost

Every review session records what the provider reported for each step: input, output, cache-read and cache-creation tokens. The session page shows the totals with the cache categories kept apart, and the session list totals the filtered sessions. When the provider reports no usage, Rumil records nothing and the deck says **usage not reported**; it does not show a free review.

Prices are optional. Set them on the chat provider to see an estimated cost:

```yaml
models:
  providers:
    - id: "deepseek-v4-flash"
      # ...
      price_input_per_mtok: 0.27
      price_output_per_mtok: 1.10
      price_cache_read_per_mtok: 0.07
```

A million tokens costs `price_input_per_mtok` for plain input, `price_output_per_mtok` for output, and `price_cache_read_per_mtok` for input the provider served from its cache. Cache-creation tokens are priced at the input rate. The estimate appears only when the input and output rates are set; the tokens remain the source of truth.

Two per-session budgets bound spending:

```yaml
review:
  max_total_tokens: 2000000    # input + output + cache tokens; 0 is no limit
  max_cost_usd: 2.50           # requires the chat provider's input and output prices
```

Rumil checks the budget between steps, after a step's usage is reported and before the next model request. A session over its budget stops with an error naming the limit, is recorded FAILED, and does not post a review it had not already submitted. Spending exactly the limit is allowed; the next token is over it.

## The queue and cancelling a review

`/ui/queue` shows the deliveries that have not finished: what is waiting for a worker and what is running now, with the delivery ID, repository, pull request, event type, age, the worker that claimed a running job, and a link to its session. The page refreshes every five seconds while there is work.

Each row links to a confirmation page. Cancelling a **queued** job moves it to `CANCELLED` before a worker claims it, so nothing runs. Cancelling a **running review** sets a flag the worker polls; the engine checks it between steps and before a tool call, stops the session, records it as `CANCELLED`, and completes the GitHub check run as cancelled. A review that already reached GitHub before the flag was read stays posted and the session is recorded as a success: the cancel stops work that is still going, and does not retract a review.

The same action appears on a running session's page. It needs an admin credential — `moderation.admin_token`, or any entry of `moderation.admin_tokens`: the pages may read with the same-site cookie, and every cancel POST needs the header and the same-site check, like the other decisions on the operator surfaces.

## Metrics

`rumil serve` exposes Prometheus metrics at `/metrics`. By default the endpoint sits on the main listener and needs an admin credential — `moderation.admin_token` or any entry of `moderation.admin_tokens` — as a bearer token or as the password of a basic credential, like the deck and the moderation page. A scraper that cannot hold the credential can use a separate listener instead:

```yaml
server:
  metrics_listen: "127.0.0.1:9090"
```

With that set, `/metrics` is served on `127.0.0.1:9090` without authentication and is not mounted on the main listener. Bind it to loopback or a firewalled interface: it is unauthenticated by design so a local scraper needs no secret. Restart after changing the address or the admin token.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `rumil_webhook_jobs_total` | counter | `event`, `outcome` | Deliveries by event and outcome: `received`, `queued`, `completed`, `failed` |
| `rumil_queue_jobs` | gauge | `status` | Jobs waiting (`queued`) and being worked on (`running`), read from the database at scrape time |
| `rumil_queue_oldest_job_age_seconds` | gauge | | Age of the oldest queued job; zero when the queue is empty |
| `rumil_sessions` | gauge | `status` | Review sessions `running`, `succeeded` and `failed`, read at scrape time |
| `rumil_review_duration_seconds` | histogram | `outcome` | Wall-clock time of a review: `submitted`, `turn_limit`, `output_limit`, `no_submission`, `cancelled` or `failed` |
| `rumil_provider_requests_total` | counter | `class` | Model steps: `ok`, or the error class `auth`, `rate_limit`, `unavailable`, `invalid_request`, `context_overflow`, `other` |
| `rumil_provider_retries_total` | counter | | Provider requests the retry policy scheduled again |
| `rumil_provider_header_timeouts_total` | counter | | Provider calls that timed out before the response headers arrived |
| `rumil_reviews_without_submission_total` | counter | `reason` | Sessions that said nothing about the pull request, using the same reasons as the duration histogram |
| `rumil_snapshot_fallbacks_total` | counter | | Sessions that used the GitHub API tools because the working tree was unavailable |
| `rumil_memory_retrieval_failures_total` | counter | | Pre-retrieval attempts that could not read the recorded rules |
| `rumil_tokens_total` | counter | `kind` | Tokens used: `input`, `output`, `cache_read` and `cache_creation` |
| `rumil_estimated_cost_usd_total` | counter | | Estimated provider cost of completed sessions, when the chat provider is priced |
| `rumil_metrics_scrape_errors_total` | counter | | Database reads behind `/metrics` that failed; the gauges are then left out of that scrape |

Labels stay low-cardinality: no repository, pull request, session id, tool argument or error text is a label. A delivery for another installation, or an event Rumil does not handle, is counted under its event name or `other`; the queue and session gauges come from the database, so a restarted process reports the current state rather than the counters it remembered.

## Asking why about a finding

A reply of `@rumil why` (or `@rumil explain`) in a review thread asks Rumil to say more about the finding there:

```text
@rumil why is the settlement call safe to leave without a timeout?
```

Rumil reads the thread, the diff hunk, and the file at the reviewed commit, and answers in the same thread. The words after the command are the question, so `@rumil why exactly is the gateway left unprotected?` asks about that aspect, while a bare `@rumil why` asks for the reasoning behind the finding. The answer is one model call and one reply: it explains the reasoning and points at the code it rests on. It does not open a review, add a finding, or resolve the thread. The reply is capped at 4,000 characters.

The command works in a review thread only, because that is where the hunk and the thread are, and it needs the same standing as `review` and `remember` on a public repository. A question in a thread somebody else started is answered from that thread's context, and Rumil does not claim to have written the comment or the finding. The answer carries an invisible marker naming the comment it replies to, so a redelivered webhook or a rerun after an interruption does not answer twice.

## Repository guidelines

If a repository keeps a `RUMIL.md` at its root, Rumil reads it before the review and treats it as that repository's own policy — it outranks everything else Rumil has been told, including remembered rules, unless a maintainer promotes a rule above it (below). It is capped at 32 KB.

This is where a team writes down the conventions it wants enforced: "we do not use an ORM in this service", "every exported function needs a doc comment", "money is always `decimal`, never `float`". Write it as policy about the code; that is how Rumil reads it.

A pull request that adds or edits `RUMIL.md` does not rewrite the policy judging it. The copy at the configured ref still governs, and the branch version is attached separately as a proposal, so the reviewer can judge the change on its merits without following it. Set `review.guidelines_ref: head` to let the branch version govern instead.

Everything Rumil reads from the pull request is data, not instructions. The prompt says so for the title and description, comments, review threads, diff, file contents, commit messages, `RUMIL.md`, and every tool result: they can inform a finding or narrow a review, but text inside them cannot change the reviewer's rules, tools, or task. A comment that asks for an approval, a skipped check, or the system prompt is reported when it matters, and a block cannot be closed from inside its own content.

### Repository settings in `.rumil.yaml`

A repository can narrow how Rumil reviews it with a `.rumil.yaml` at its root. It is read from the same ref as `RUMIL.md` — the base branch by default — so a pull request cannot change the settings that review it. Every key is optional:

```yaml
review:
  resolve_threads: false          # may this review close its own threads
  request_changes: false          # may this review request changes
  ignore_paths: ["docs/**", "**/*.md"]
  default_focus: ["migrations", "deployment"]
  max_inline_comments: 20
models:
  default_chat: "cheap-model"     # a chat provider the operator configured
```

The layers, from the operator's file down: `review.*` and `models.*` in the operator's configuration are the baseline, `.rumil.yaml` narrows or replaces that baseline for one repository, and the built-in defaults fill what neither sets. A repository may choose only a chat provider the operator already configured; it cannot add a provider.

The file is validated strictly. An unknown key, a credential (`api_key`, `token`, `password`, `secret`, `private_key`, `dsn`), or a provider definition is refused. So are more than 50 ignore paths, more than 5 focus entries, an entry over 200 bytes, an absolute path or one containing `..`, and an inline comment cap outside 1–200. An invalid file fails the review with the file's problem instead of silently using the operator's settings.

The prompt carries the effective settings, and the tool enforces them: a finding on an ignored path is dropped, the inline cap moves the extra findings into the summary, and a requested-changes review becomes a comment when the repository does not allow one. The session page records a `repository settings` event with the values the review used, so a surprising review is explainable.

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

Or from the moderation page, which needs no JavaScript and no session. It answers only when at least one admin credential is set — `moderation.admin_token`, or `moderation.admin_tokens` — and takes the value as a bearer credential or as the password of a basic-auth credential (any username):

```text
https://<your-rumil-host>/ui/moderation
```

The same decisions are available as JSON for scripting: `POST /api/moderation/<id>/approve` and `POST /api/moderation/<id>/reject`, with form fields `rule_text`, `path_glob`, `scope`, `authority` and `moderator`.

### Named admin credentials

One shared token cannot say who decided, and revoking one person means changing it for everybody. `moderation.admin_tokens` names each holder:

```yaml
moderation:
  admin_tokens:
    - name: alice
      token: ${ALICE_ADMIN_TOKEN}
    - name: ci-bot
      token: ${CI_ADMIN_TOKEN}
```

Every configured token is compared on every request, and the matching name is what the audit trail records: `reviewed_by` on a moderation decision, `requested_by` on a continuation, and the log line when a session stream opens. A form may still name somebody with `moderator` or `requested_by`, which an API client uses when the credential is a service account. To rotate, add the new entry and restart, move the holder to it, then remove the old entry and restart; removing an entry stops its header and its cookie on the next request. A lone `moderation.admin_token` keeps working as an entry named `legacy`.

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

## Measuring review quality

Every inline comment Rumil posts is recorded with the GitHub comment id it was created as. Later deliveries attach what happened to it: a reply in the thread, a thread Rumil closed after verifying the finding is gone, a reaction on the comment, a dismissed review, or a push after the round. The report groups the counts by the model that produced the review:

```bash
rumil findings --repo acme/payments --days 30
```

`--json` prints the same rows for a script, and `--days 0` reads every finding on record. A finding with no signal is not a failure; it is a finding nobody has looked at yet.

The feedback arrives by webhook: replies with *Pull request review comment*, dismissals with *Pull request review*, reactions with *Reaction*, and pushes with *Pull request*. Subscribe to the first three to fill the report in; the push signal is already part of the required set.

### Running the reviewer against a fixture set

`rumil findings` measures reviews that happened. `rumil eval` measures the reviewer itself: it runs the configured model against a set of local pull requests and scores the findings against what each one should produce.

```bash
rumil eval --fixtures fixtures
```

GitHub is replaced by an in-process fake, so nothing is posted and no repository is read. The chat provider is real, so the command spends tokens. A fixture is a YAML file:

```yaml
name: unbounded-retry
title: Retry the settlement call
diff: |
  diff --git a/worker.go b/worker.go
  ...
files:
  worker.go: |
    package worker
    ...
expected:                 # a good review finds these
  - path: worker.go
    line: 6
false_positives:          # a review may say these, and they are wrong
  - path: worker.go
    line: 13
    match: unused
```

A finding matches an expectation when the path is equal, the line is within two, and the comment body contains `match`, case-insensitively; an empty `match` accepts any body on that line. Each finding counts once, so repeating an expectation cannot inflate the score. The report prints per fixture and in total: expected, matched, missed, known false positives, unexpected findings, and recall. `--json` prints the same data for a script that compares two runs.

The repository ships `fixtures/` as a starter set. `--fixture NAME` runs one, `--model ID` reviews with another configured chat provider, and `--timeout` bounds one fixture (the default is `worker.job_timeout`). The run disables the snapshot and operator MCP servers: the comparison is about the prompt and the model, not the tools around them.

## Checking an installation with `rumil doctor`

`rumil doctor` tests the configuration against every service it names, so a wrong key or a schema behind the binary is found before the first review:

```bash
rumil doctor
rumil doctor --repo acme/payments
rumil doctor --snapshot acme/payments --json
```

It prints the effective non-secret settings and one line per check, then exits non-zero when a configured component failed. A warning, such as memory being off, exits zero. Secrets — provider keys, the admin token, the webhook secret, the App's private key and a password in the database URL — are never printed, including in a provider's own error text. The command writes nothing to Postgres and starts no review.

| Check | What it covers | Failure |
|---|---|---|
| `config` | The file loaded and validated; the default chat provider, model count and MCP server count | The command stops before the checks |
| `database` | Postgres answers; the applied schema version matches the migrations embedded in the binary; pgvector is installed when memory is on | A configured database that is unreachable, behind, ahead of the binary, or missing pgvector |
| `github` | The private key signs an App JWT, the installation token is minted, the installation's own record is read, and with `--repo` the repository is visible | A wrong key, a missing installation, or an unreadable repository |
| `chat` | One small completion, with its latency and the usage the provider reported | The provider does not answer, or answers without a completion |
| `embedding` | One call to the embedding provider, and the vector width it returned | The provider is configured and returns another width or no vector |
| `snapshot` | With `--snapshot owner/name`: the default branch's archive is downloaded, extracted into a temporary directory, and removed | The archive or the extraction fails, or the configured directory is unusable |
| `server` | `github.webhook_secret` and an admin credential (`moderation.admin_token` or an entry of `moderation.admin_tokens`) are set when `database.dsn` configures `rumil serve` | A missing webhook secret; a missing admin credential is a warning |

`--json` writes the same report as an object with `config_path`, `settings` and `checks` (each with `name`, `status` and `detail`), for a script or a monitoring probe. A provider that answers without reporting usage is a warning: reviews still run, but the token and cost budgets cannot count that provider.

## Troubleshooting

**A review never appears.** Rumil reviews only when the App is requested as a reviewer, so check that the pull request actually asks for it. Then look at the App's **Advanced → Recent Deliveries**: a delivery that is not a review request, comes from another installation, or carries a bad signature is answered `200` or `401` and logged with the reason it was ignored. `docker compose logs -f rumil` shows those lines.

**No check run appears on the commit.** The App's **Checks** repository permission is not Read and write, or the installation has not accepted the updated permissions. Grant Read and write on Checks in the App settings, then accept the change on the installation; a token minted before the acceptance does not carry it. Restart Rumil after the installation accepts so the next review mints a fresh token. The review itself still posts, and the logged check-run refusal names the cause.

**"review session ended without submitting a review".** The session stopped without saying anything about the pull request. It reached `app.max_agent_turns`, a turn was cut off at the model's output limit, or the chat endpoint does not support tool calling. The run is recorded as failed. Raise the limit, set `app.max_agent_turns: 0` for no cap, or point `models.default_chat` at a larger model. A message that ends with "(model output limit reached)" means the provider's response budget ran out before the tool call: set `models.providers[].max_tokens` on the chat provider, because reasoning counts against that budget and `thinking_level: max` can use all of it. Continue the session from `/ui/sessions` or with `rumil sessions continue <session-id>`: the new attempt reuses its tool results and gets its own budget. When a turn alone is cut off, Rumil asks the model once more for the tool call before failing.

**A reasoning model answers suspiciously fast.** Set `models.providers[].thinking_level` on the chat provider, for example `thinking_level: "high"` or `thinking_level: "max"` where the provider exposes it. Rumil sends it as `reasoning_effort` on the chat-completions request. `off` leaves the provider's default in place, and a provider that does not accept the field may reject the request.

**"Repository snapshot unavailable".** The session could not download or extract the reviewed commit, so it continues with the GitHub API tools: listing still works, but `search_code` is absent and each read is a request. The session log names the cause. The usual ones are a repository above `snapshot.max_bytes`, no room in `snapshot.dir` or the temp directory, a network policy that blocks the archive host, or a job that reached `snapshot.timeout`.

**Every webhook delivery is rejected.** `github.webhook_secret` must match the App's webhook secret exactly, and the endpoint GitHub reaches is `POST /webhooks/github` on whatever host you configured.

**`/ui/moderation` answers "the moderation surface needs an admin token".** Set `moderation.admin_token`, or add an entry to `moderation.admin_tokens` — the example config takes the single token from `${RUMIL_ADMIN_TOKEN}` — and restart. Until then the page, the deck and `/metrics` refuse every request rather than standing open.

**The container will not start on this machine.** The published image is `linux/amd64`. Use `docker compose up`, which compiles Rumil for the host architecture.

**Rules are recorded but never used in a review.** Memory needs `models.default_embedding` pointing at a provider whose `dimensions` match the width its model returns, and the approved status: `rumil mod list --status approved` should show the rule. If the provider's width has changed since the rules were recorded, startup says so and names the existing width, because vectors of two widths cannot be compared.

## License

Apache License 2.0 — see [LICENSE](LICENSE).
