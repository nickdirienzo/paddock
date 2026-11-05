# Paddock

Paddock lets you work with Codex and Claude Code from your terminal and GitHub pull requests.

It includes a user-level skill that teaches agents how to use Paddock. Install
it once, then ask an agent to start tasks, delegate work, or follow up on PRs
across your repositories. You can also use the CLI directly:

```sh
paddock run --repo ~/code/my-app --agent claude "Fix the login redirect"
paddock tasks
paddock attach TASK_ID
```

Each task gets its own branch, Git worktree, and terminal. Delegated tasks open
as tabs under their parent's Rex section. You can
reconnect from another device through [Rex](https://www.superlogical.com/rex),
or resume the same agent conversation later.

I built Paddock after using Rex for a few days. I wanted to reach my coding
agents from whichever device I had with me and keep working with them directly
on PRs. Paddock brings Rex, OpenShell, and GitHub together around the agent you
choose.

## Get started

Paste this into Codex or Claude Code:

```text
Read https://github.com/nickdirienzo/paddock/blob/main/README.md and help me set up Paddock.
Install Paddock and its user-level skill for Codex and Claude Code, then help me
configure the runtime and start my first task.
```

## Install manually

You'll need Git, [mise](https://mise.jdx.dev/), and a running Rex server.
macOS is the tested platform.

```sh
git clone https://github.com/nickdirienzo/paddock.git
cd paddock
mise trust
mise install
mise run build

mkdir -p ~/.local/bin
install -m 755 bin/paddock ~/.local/bin/paddock
~/.local/bin/paddock skill setup
```

Keep `~/.local/bin` on your PATH.

New tasks run in OpenShell by default. Follow the [OpenShell setup
guide](docs/openshell.md) to configure the sandbox, agent authentication, and
GitHub access before starting a task.

The default OpenShell runtime requires a GitHub App installed on each repository
you want agents to work in, with these **Repository permissions**:

| Permission | Access | Used for |
| --- | --- | --- |
| Contents | Read and write | Clone repositories and push task branches. |
| Pull requests | Read and write | Create and update PRs, read reviews, and reply to review threads. |
| Issues | Read and write | Post PR conversation comments and reactions. |
| Metadata | Read-only (included automatically) | Read repository metadata. |

If you already installed the App, update its **Permissions & events** settings
to match this list, then approve the new permissions for each
installation. Saving the App settings alone does not update existing
installations. See [GitHub App setup](docs/github.md#configure-a-github-app) for
the App ID, private key, installation, and repository routing steps.

You can also use agents already installed and logged in on your Mac:

```sh
paddock runtime default host
```

Enable **Remote Control** in Rex's settings so Paddock can open task tabs.

## Teach your agents Paddock

The `paddock skill setup` command above installs the skill for both Codex and
Claude Code at the user level. It's available across repositories, so you can
ask for work in conversation:

```text
Use Paddock to start a Claude task to fix the login redirect.
```

The skill explains how to launch and inspect tasks, delegate work, connect PRs,
and handle review feedback. Start a new agent session after installation.
You can invoke it explicitly with `$paddock` in Codex or `/paddock` in Claude
Code. See [agent discovery](docs/reference.md#agent-discovery-across-repositories)
for installation details.

## Use the CLI

Start a task from any Git repository, or give `--repo` a path. Choose
`--agent codex` or `--agent claude`. Longer prompts can come from a file:

```sh
paddock run --repo ~/code/my-app --prompt-file task.txt
```

Use `paddock attach TASK_ID` to reopen its terminal. Send a follow-up with
`paddock message`, or use `paddock resume` to continue a stopped conversation:

```sh
paddock message TASK_ID "Add a regression test for the redirect"
paddock resume TASK_ID
```

Inside a sandboxed agent, `! paddock attach` opens a host shell in the live
worktree. On a local OrbStack setup, files are already visible to Mac tools:

```sh
open report.html
```

Paste screenshots directly into sandboxed Codex or Claude. Paddock transfers
the selected clipboard image into the task automatically.

## Keep the conversation on GitHub

Once [GitHub is configured](docs/github.md), connect a PR to its task:

```sh
paddock github watch https://github.com/you/my-app/pull/42 \
  --task TASK_ID --allow YOUR_GITHUB_LOGIN
```

Comments from the people you allow go to the task's agent. They can leave
ordinary review comments; no special mentions or commands are required. The
agent can act on the feedback and reply in the same thread.

Paddock keeps the terminal session and PR conversation connected. You decide
when to commit, open a PR, or merge.

## Let Paddock route the work

Different problems suit different agents. The optional planner can decide
whether a GitHub comment or CLI request belongs in an existing session or
needs a new task, and choose Codex or Claude Code for new work. You choose
which tasks and repositories it can use.

PR comments go straight to the existing agent by default.
[Enable planning](docs/orchestration.md) when you want Paddock to make those
routing decisions.

## Documentation

- [OpenShell setup](docs/openshell.md)
- [GitHub setup and PR conversations](docs/github.md)
- [Planning and routing](docs/orchestration.md)
- [Commands, configuration, and development](docs/reference.md)
- [Task retention and recovery](docs/lifecycle.md)

Paddock is licensed under the [Apache License 2.0](LICENSE).
