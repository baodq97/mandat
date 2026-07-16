# CLI first-run survey — how peers do init, read against US-0013

## Method

Twenty-six tools and conventions surveyed across five categories — CI-runner registration (GitLab, GitHub Actions, Azure Pipelines, Buildkite), cloud/auth CLIs (`gcloud init`, `aws configure`, `aws configure sso`, `az login`, `az init`, `doctl`, `gh`, Claude Code, Stripe, flyctl, tailscale), scaffolders (`docker init`, `npm init`, `terraform init`), doctors (`flutter doctor`, `brew doctor`, `minikube start`), and four cross-tool convention studies (commented-config practice, XDG-vs-FHS placement, non-interactive conventions, re-run semantics, install-to-init handoff). Official docs and, where prose was thin, primary source (CommandSettings.cs, ssh-keygen.c, doctor.dart, doctl auth.go) were the evidence base. Read 2026-07-16.

## Pattern catalog

1. **Split verbs: write-config / authenticate / install-service.** GitLab (`register` → `install` → `verify`), GitHub Actions (`config.sh` → `svc.sh`), Azure Pipelines (`config.sh` → `svc.sh`), `aws configure sso` → `aws sso login`, `gh auth login` → `gh auth status`, Claude Code `/login` vs `setup-token`. Mechanism: config-writing, credential-minting, and OS-service enrollment are separate composable commands, never one wizard.

2. **Reviewable plain-text config, not hidden state.** GitLab `config.toml`, Buildkite `.cfg`, `az config` INI, `doctl` YAML, Stripe `config.toml`, Terraform `.terraform.lock.hcl`. Counter-examples: GitHub Actions opaque `.runner`/`.credentials` dotfiles, Claude `.credentials.json`. Mechanism: a diffable file a customer can read in code review.

3. **Config/secret separation.** `az config` touches only CLI behavior, never creds; `gh` puts the token in the OS keyring, not `config.yml`; Claude `setup-token` prints the token and writes it nowhere. Anti-pattern cluster: Stripe (live keys in the same TOML), `doctl` (raw PAT cleartext), `aws configure`, Buildkite (shared token in the reviewable file). Mechanism: the reviewable file holds structure/references only; secret material lives elsewhere.

4. **Non-interactive flag + 1:1 flag per prompt.** `gitlab-runner --non-interactive`, GH `--unattended`, Azure `--unattended`, `npm -y`, `rustup -y`, `terraform -input=false`, `minikube --interactive=false`, `fly launch -y --now`. Mechanism: an explicit opt-out flag plus a flag for every field the wizard would ask.

5. **Env-var-per-flag with documented precedence.** Azure `VSTS_AGENT_INPUT_*`, Buildkite `BUILDKITE_*`, minikube `MINIKUBE_*`, AWS `AWS_*`, Terraform `TF_VAR_*`; clig.dev codifies the chain. Mechanism: `TOOL_*` env vars, precedence flags > env > config file, env never persists back into the file.

6. **Auto-detect TTY before prompting.** clig.dev convention (non-interactive conventions study): prompt only if stdin is a TTY, else skip and require the equivalent flags with a clear "pass `--flag`" error instead of hanging. Real tools that skip this break in CI. Mechanism: interactivity is a property of stdin, gated independently of the `--non-interactive` flag.

7. **Idempotent re-run with bracketed current values.** `aws configure` shows the current value as the default each prompt (Enter keeps it); `npm init` is strictly additive (fills gaps only); `terraform init` documented "always safe to run multiple times"; `minikube start` resumes an existing profile. Counter-examples: GitLab (appends another block), GitHub Actions (fails unless `--replace`), `docker init` (destructive all-or-nothing overwrite), `ssh-keygen` (unconditional un-bypassable overwrite prompt). Mechanism: existing values become defaults; unchanged fields survive.

8. **Detect-existing-state, offer explicit choices — never silent overwrite.** `gcloud init` presents re-init-active / switch+re-init / create-new; `doctl --context` always adds, `auth switch` is the only thing that flips active. Mechanism: on finding prior state, menu the choices rather than clobber.

9. **Verify before write: validate live, or preview the diff.** `doctl auth init` calls the real API (`Validating token: OK`) and refuses to persist an unvalidated token; GH `config.sh --check` probes connectivity pre-registration; `terraform plan`→`apply` previews every change before touching state; `fly launch` renders one whole-config summary screen for a single yes/tweak/no. Mechanism: prove the inputs work, or show what will change, before mutating disk.

10. **Auto-discovery from the environment.** `terraform init` derives everything from `.tf`; `docker init` / `fly launch` scan the project; `minikube start` health-probes candidate drivers; `gcloud init` detects existing configurations; GitHub Actions auto-derives labels from OS/arch. Mechanism: probe for defaults, still surface the result through a picker rather than silently committing.

11. **Mint-at-runtime short-lived credentials, no static PAT.** GitHub Actions 1-hour JIT registration token; Azure Pipelines' own PAT → Service Principal → Entra device-code ladder; `aws configure sso` (short-lived cached SSO token); `az login --identity`/`--federated-token`; Claude `apiKeyHelper` for vault-fetched rotating tokens. Negative examples: Buildkite shared bearer token, `doctl`/`aws configure`/Stripe long-lived keys on disk. Mechanism: a browser/device/federated flow mints a short-lived token; nothing long-lived persists.

12. **Separate doctor/preflight with sharp tri-state + machine-readable exit.** `flutter doctor` (`[✓]`/`[!]`/`[✗]`, inline remediation command, documented exit status); `gh auth status` (exits 1 on broken auth); `minikube --dry-run`; `terraform validate`. Counter-example: `brew doctor` deliberately blurs pass/warn/fail into "ignorable unless debugging" — the wrong model for a security-gated setup. Mechanism: a read-only command, distinct from init, with a decisive verdict per check.

13. **Init ends with a self-check, the next command, and a security-relevant warning.** Docker's convenience script runs `docker version` live then prints the rootless-setup command and a docker-group security warning; Homebrew prints the reviewable `brew shellenv` lines to paste (never silently edits dotfiles) and recommends `brew doctor`; `flutter doctor` prints the exact fix inline. Mechanism: close the loop with a real probe plus a visible, reviewable next step.

## The closest peer: CI runner registration

Mandat `init` is functionally "register this VM against Entra + ADO," so the three self-hosted runner registrars are the tightest analog. Compared prompt-by-prompt against mandat's irreducible set (US-0013.3: tracker org/project; repo url + remit paths + gates; role identity ids/UPNs):

| Tool | Interactive prompts | Sequence | Identity/URL source |
|---|---|---|---|
| `gitlab-runner register` | 6 | URL → token → description → tags → maintenance note → executor | url+token explicit flags |
| GH Actions `config.sh` | 4 | name → work folder → labels → runner group | url+token via flags (`--pat` exchanged for JIT token) |
| ADO `config.sh` | 7 | server URL → auth type → credential → pool → agent name → work folder → TEE EULA | all explicit |
| **mandat init (target)** | **~0 on happy path** | tracker org/project, repo url+remit+gates, role ids/UPNs | **discovered via az-derived token (13.1), prompt only on fallback** |

The peers all prompt for identity/URL explicitly; none auto-discover the org from an existing operator session. Mandat's 13.1 discovery step is a genuine improvement over every runner here — if it lands, the happy-path interactive count drops below all three. The irreducible set that survives failed discovery (tracker org/project, repo url + remit paths + gates, role identity ids/UPNs) is comparable in size to GitLab's 6 and smaller than ADO's 7. All three cleanly separate `config` from service-install (`svc.sh`/`install`), validating US-0013.6's optional-systemd split. None is idempotent the way mandat needs: GitLab appends, GitHub fails-or-replaces, ADO prompts to replace — a gap mandat should close, not copy.

## Adopt / adapt / reject for mandat init

| Pattern | Verdict | Reason (mandat invariant) |
|---|---|---|
| 1. Split write-config / auth / service | **adopt** | init writes config.yaml only; Entra token minting is a separate step, systemd unit optional (13.6). Matches "no PAT" + reviewable-config split; peers universally do this. |
| 2. Reviewable plain-text config | **adopt** | Core invariant: config.yaml is reviewed like code, never hidden state. `az config`/`doctl`/Stripe confirm the format works. |
| 3. Config/secret separation | **adopt** | No minted Entra token ever lands in config.yaml (Stripe/doctl are the anti-pattern). Enforces "no PAT anywhere." |
| 4. Non-interactive flag + flag-per-prompt | **adopt** | 13.x lacks this today; every runner peer has it. CI bring-up + govkit-governed rollout need a `--non-interactive`. |
| 5. `MANDAT_*` env vars, flags>env>file | **adapt** | Adopt the precedence chain for init inputs only; never use env vars to smuggle secrets (design forbids env-var creds), and the written file stays the only runtime source. |
| 6. TTY auto-detect | **adopt** | Cheap; prevents init hanging in CI/hook contexts. clig.dev convention. |
| 7. Idempotent re-run, bracketed defaults | **adopt** | Open gap flagged in the doc. `aws configure`/`npm` show it's low-friction; existing config.yaml values become the kept defaults. |
| 8. Detect-state menu on rerun | **adapt** | `gcloud`'s menu is right, but one VM = one config, so offer edit-in-place / start-fresh / abort, not multi-context. |
| 9. Verify-before-write (validate live / diff) | **adopt** | Validate the az-derived token/tenant against a real endpoint before writing (`doctl`); print a `terraform plan`-style diff of config.yaml changes before committing them ("config reviewed like code"). |
| 10. Auto-discovery | **adopt (bounded)** | 13.1 is exactly this; pin the az mechanism (open gap) and always surface discovered values for confirmation, never silently commit. |
| 11. Mint-at-runtime tokens | **already core** | The no-PAT/hourly-Entra design is this pattern; survey validates it (GH JIT, Azure SP→Entra ladder, `aws sso`). init must not persist any token. |
| 12. Doctor tri-state + exit code | **adopt (reuse)** | doctor already exists; 13.7 has init call it. Hold it to `flutter doctor`'s sharp tri-state, not `brew doctor`'s shrug — this gates Entra identity + worktree isolation. init calls the same validators, does not fork a second set. |
| 13. Init ends with self-check + next-step + warning | **adopt** | 13.7 already runs doctor at the end; add the Docker/Homebrew move — print the next command and a remit/identity-scope security warning. |
| Exhaustive commented-defaults template (postgres/sshd) | **reject** | Roles/remits/ceilings are per-entity like `gitlab-runner register`, not a fixed small option set; a giant commented file drowns real settings. 13.2's per-field default-naming comment is the right middle ground; exhaustive reference belongs in a separate `config schema`/doc. |
| XDG path discovery | **reject** | Every comparable Go daemon (gitlab, k3s, containerd, docker) uses `/etc/<name>/`; XDG env vars may be unset under a root systemd unit. Keep hardcoded `/etc/mandat/config.yaml` + a `--config` override. |
| ssh-keygen unconditional overwrite prompt | **reject** | Undocumented, un-bypassable, no force flag — a known usability gap; mandat needs `--force`/`--yes` for CI reruns. |

## Proposed AC deltas for US-0013

1. **Add: non-interactive mode (AC 13.9).** `mandat init --non-interactive` requires every irreducible field as a flag (`--tracker-org`, `--tracker-project`, `--repo-url`, `--remit-path`, `--gate`, role identity flags) and errors — naming the missing flag — instead of prompting. Additionally TTY-autodetect: when stdin is not a TTY, behave as if `--non-interactive`. *Evidence:* gitlab-runner `--non-interactive`, GH/Azure `--unattended`, clig.dev TTY convention. **Phase-1-cheap.**

2. **Add: `MANDAT_*` env vars with documented precedence (AC 13.10).** Flags > `MANDAT_*` env > `/etc/mandat/config.yaml`; env vars carry non-secret config only (Entra tokens stay runtime-minted). *Evidence:* Azure `VSTS_AGENT_INPUT_*`, minikube `MINIKUBE_*`, AWS `AWS_*`, clig.dev precedence chain. **Phase-1-cheap.**

3. **Add: idempotent re-run with bracketed current values (AC 13.11).** Re-running over an existing config.yaml shows each existing value as the prompt default (Enter keeps it); unchanged fields are preserved, not rewritten. Closes the doc's explicitly-flagged idempotency gap. *Evidence:* `aws configure` bracketed defaults, `npm init` additive rerun, `terraform init` safe-to-rerun. **Phase-1-cheap.**

4. **Add: diff-before-write + explicit confirm, with `--force`/`--yes` bypass (AC 13.12).** On rerun, print a diff of what init will change in config.yaml (new role entries, changed remit paths, changed autonomy ceilings) and require confirmation; `--force`/`--yes` skips it for CI. Do not copy ssh-keygen's un-bypassable prompt. *Evidence:* `terraform plan`→`apply`, `fly launch` single-summary screen, `ssh-keygen` as the anti-pattern. **Phase-1-cheap.**

5. **Modify 13.1: pin the discovery mechanism and validate-before-persist.** Specify the az token chain used for ADO org/project/repo discovery (the doc flags this as unpinned), and require init to validate the discovered token/tenant against a real endpoint — refusing to write config.yaml on failure — before persisting. *Evidence:* `doctl auth init` live-API validation before disk write; `gh config.sh --check` connectivity probe. **Phase-1-cheap** (validation) / discovery-mechanism pin is **phase-1-cheap** but depends on the az chain already in the dogfood infra.

6. **Modify 13.7: hold init's doctor call to a sharp tri-state and reuse doctor's validators.** init's end-of-run check must reuse doctor's existing checks (no second validator set), present the same PASS/FAIL table with a decisive per-check verdict, and surface a non-zero exit on failure — explicitly not `brew doctor`'s advisory framing, given this gates Entra identity + worktree isolation. *Evidence:* `flutter doctor` tri-state + documented exit status vs. `brew doctor` counter-example. **Phase-1-cheap.**

7. **Add: init ends with next-command + remit/identity security warning (AC 13.13).** After the doctor table, print the next command to run and a warning naming the Entra identity scope and remit paths this VM was granted — the reviewable-handoff move. *Evidence:* Docker script's post-install security warning, Homebrew's printed-paste reviewable step. **Phase-1-cheap.**

8. **Add: install.sh → init handoff prints, never silently mutates (AC 13.14).** If mandat ships an install script, it prints the reviewable next step (run `mandat init`) rather than auto-launching a buried wizard or silently editing system state — Homebrew's stance, matching "config reviewed like code." *Evidence:* Homebrew reviewable-paste, uv `UV_NO_MODIFY_PATH`, k3s silent-autostart as the counter-model. **Phase-1-cheap** (install.sh exists; today it ends at `mandat version`).

9. **Modify 13.8 / scope note: keep Entra secrets-acquisition as a distinct phase-2 step.** The survey's strongest security precedents (`aws configure sso` config-write vs `aws sso login` mint; `az login --federated-token`; GH 1-hour JIT) all separate config-writing from token-minting. Mandat should mirror this: init writes config.yaml; a distinct login/refresh step mints the hourly Entra token. The doc already defers Entra provisioning to phase 2 needing a spike/RFC — this delta names the split explicitly as the phase-2 design target. *Evidence:* `aws configure sso`/`aws sso login` split, `az login` identity flags, GH JIT token. **Phase-2** (needs the flagged spike/RFC).

## Sources

- https://docs.gitlab.com/runner/register/
- https://docs.gitlab.com/runner/commands/
- https://docs.gitlab.com/runner/configuration/advanced-configuration/
- https://gitlab.com/gitlab-org/gitlab-runner/-/raw/main/config.toml.example
- https://docs.github.com/actions/hosting-your-own-runners/adding-self-hosted-runners
- https://docs.github.com/actions/hosting-your-own-runners/managing-self-hosted-runners/configuring-the-self-hosted-runner-application-as-a-service
- https://github.com/actions/runner/blob/main/src/Runner.Listener/CommandSettings.cs
- https://learn.microsoft.com/en-us/azure/devops/pipelines/agents/linux-agent?view=azure-devops
- https://learn.microsoft.com/en-us/azure/devops/pipelines/agents/agent-authentication-options?view=azure-devops
- https://buildkite.com/docs/agent/v3/linux
- https://buildkite.com/docs/agent/v3/configuration
- https://buildkite.com/docs/agent/v3/cli-start
- https://docs.cloud.google.com/sdk/gcloud/reference/init
- https://docs.cloud.google.com/sdk/docs/initializing
- https://docs.cloud.google.com/sdk/docs/configurations
- https://docs.aws.amazon.com/cli/latest/userguide/cli-configure-quickstart.html
- https://docs.aws.amazon.com/cli/latest/reference/configure/
- https://docs.aws.amazon.com/cli/latest/reference/configure/list.html
- https://docs.aws.amazon.com/cli/latest/userguide/cli-configure-sso.html
- https://docs.aws.amazon.com/cli/latest/userguide/cli-configure-envvars.html
- https://docs.aws.amazon.com/cli/v1/userguide/cli-configure-envvars.html
- https://learn.microsoft.com/en-us/cli/azure/authenticate-azure-cli-interactively?view=azure-cli-latest
- https://learn.microsoft.com/en-us/cli/azure/get-started-with-azure-cli?view=azure-cli-latest
- https://learn.microsoft.com/en-us/cli/azure/reference-index?view=azure-cli-latest
- https://learn.microsoft.com/en-us/cli/azure/azure-cli-configuration?view=azure-cli-latest
- https://learn.microsoft.com/en-us/cli/azure/config?view=azure-cli-latest
- https://docs.digitalocean.com/reference/doctl/reference/auth/init/
- https://github.com/digitalocean/doctl/blob/main/commands/auth.go
- https://github.com/digitalocean/doctl/blob/main/README.md
- https://cli.github.com/manual/gh_auth_login
- https://cli.github.com/manual/gh_auth_status
- https://cli.github.com/manual/gh_auth
- https://cli.github.com/manual/gh_config
- https://code.claude.com/docs/en/authentication
- https://docs.stripe.com/stripe-cli/install
- https://docs.stripe.com/cli/config
- https://docs.stripe.com/stripe-cli/keys
- https://fly.io/docs/flyctl/launch/
- https://fly.io/docs/reference/fly-launch/
- https://fly.io/docs/flyctl/auth-login/
- https://tailscale.com/docs/reference/tailscale-cli/up
- https://tailscale.com/docs/reference/tailscaled
- https://tailscale.com/docs/reference/tailscale-cli
- https://docs.docker.com/reference/cli/docker/init/
- https://www.docker.com/blog/docker-init-initialize-dockerfiles-and-compose-files-with-a-single-cli-command/
- https://docs.docker.com/reference/cli/docker/
- https://get.docker.com/
- https://docs.docker.com/engine/install/linux-postinstall/
- https://docs.npmjs.com/cli/v11/commands/npm-init/
- https://docs.npmjs.com/creating-a-package-json-file/
- https://docs.npmjs.com/cli/v11/using-npm/config/
- https://developer.hashicorp.com/terraform/cli/commands/init
- https://developer.hashicorp.com/terraform/cli/commands/apply
- https://developer.hashicorp.com/terraform/tutorials/cli/plan
- https://developer.hashicorp.com/terraform/cli/config/environment-variables
- https://docs.flutter.dev/install/troubleshoot
- https://docs.flutter.dev/reference/flutter-cli
- https://github.com/flutter/flutter/blob/master/packages/flutter_tools/lib/src/commands/doctor.dart
- https://docs.brew.sh/Manpage
- https://docs.brew.sh/Troubleshooting
- https://docs.brew.sh/Installation
- https://minikube.sigs.k8s.io/docs/commands/start/
- https://minikube.sigs.k8s.io/docs/drivers/
- https://minikube.sigs.k8s.io/docs/handbook/config/
- https://github.com/kubernetes/minikube/blob/master/site/content/en/docs/handbook/config.md
- https://www.postgresql.org/docs/current/config-setting.html
- https://github.com/postgres/postgres/blob/master/src/backend/utils/misc/postgresql.conf.sample
- https://man7.org/linux/man-pages/man5/sshd_config.5.html
- https://redis.io/docs/latest/operate/oss_and_stack/management/config/
- https://raw.githubusercontent.com/redis/redis/7.0/redis.conf
- https://specifications.freedesktop.org/basedir/latest/
- https://refspecs.linuxfoundation.org/FHS_3.0/fhs/ch03s07.html
- https://docs.k3s.io/installation/configuration
- https://docs.k3s.io/quick-start
- https://github.com/containerd/containerd/blob/main/docs/man/containerd-config.toml.5.md
- https://github.com/spf13/viper/issues/1048
- https://clig.dev/
- https://raw.githubusercontent.com/openssh/openssh-portable/master/ssh-keygen.c
- https://man7.org/linux/man-pages/man1/ssh-keygen.1.html
- https://github.com/PowerShell/Win32-OpenSSH/issues/685
- https://rust-lang.github.io/rustup/installation/other.html
- https://users.rust-lang.org/t/how-to-send-1-proceed-with-installation-default-in-a-single-command-with-curl-proto-https-tlsv1-2-ssf-https-sh-rustup-rs-sh-command/89235
- https://docs.astral.sh/uv/getting-started/installation/
- https://docs.astral.sh/uv/reference/installer/
