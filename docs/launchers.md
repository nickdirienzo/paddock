# Launcher configurations

An identity names a caller's agent and optional GitHub App. Its `launcher` field
selects a named execution configuration. Launcher names have no special meaning;
their settings determine execution behavior.

```sh
paddock launcher configure local --runtime host --agent claude --terminal rex
paddock identity create me --launcher local
paddock run --identity me --repo project 'Investigate the issue'

# Change an existing identity's mapping.
paddock identity configure me --launcher implementation
paddock launcher list --json
paddock launcher show local --json
```

Launchers are saved in `$PADDOCK_HOME/launchers.json`. The initial configurations
are `default` (no settings), `host` (`runtime: host`), `implementation`
(`runtime: openshell`), and `investigation` (`runtime: openshell`,
`read_only: true`). All are editable; custom names work the same way.

Selection and defaults:

1. `run --launcher NAME` selects that configuration; otherwise use the selected
   identity's mapping, then `default` if there is no mapping. Repository identity
   routes supply the identity when `--identity` is omitted.
2. Explicit `--runtime`, `--agent`, and `--terminal` flags override config values.
3. An omitted runtime uses `paddock runtime default` (OpenShell when unset).
   An omitted agent uses Codex; an omitted terminal uses automatic selection.

The interactive `paddock new` form uses the same mappings. Leave its agent set to
**Configured agent** to use the launcher setting. It also accepts `--identity`
and `--launcher`.

Each task saves its resolved launcher settings. Changing a config or identity
mapping affects new tasks; follow-ups and resumes retain their saved runtime,
agent, terminal, and OpenShell settings. Idempotent dispatch returns the original
task. Explicitly changed launch arguments are rejected when reusing an external ID.

`read_only: true` enables the guarded checkout and read-only GitHub policy and
token. This setting requires OpenShell; it is independent of the config name.
Child tasks cannot broaden a read-only parent's permissions.
Host configs use the local agent's native authentication and permissions.
They do not inject an identity's GitHub App credentials or claim that App as the
agent's GitHub actor. OpenShell App-backed tasks retain their scoped credentials.
Idle TTL currently requires OpenShell.

For separate gateways, images, providers, or policies, load a full configuration:

```sh
paddock launcher configure service --file /path/to/launcher.json
```

The file accepts `runtime`, `agent`, `terminal`, `read_only`, and an optional
`openshell` object containing the same fields as `paddock runtime show`. Without
that object, the launcher uses the shared OpenShell runtime configuration. Runtime
setup failures are errors; OpenShell launches never silently fall back to host.

`paddock tp` explicitly selects host execution and reconnects to its retained
Team Principal conversation without changing the default launcher or runtime.

## Codex terminal mode

Choose the view for each launch, for example scrollback when connected remotely
and fullscreen when working locally:

```sh
paddock run --agent codex --codex-tui scrollback 'Investigate the issue'
paddock run --agent codex --codex-tui fullscreen 'Implement the change'
paddock resume TASK_ID --codex-tui scrollback
paddock tp --codex-tui scrollback
```

The flag also works with `new` and `delegate`. Save a launcher preference with
`paddock launcher configure remote --codex-tui scrollback`; its JSON field is
`codex_tui`. Explicit launch flags override that preference. Omit the setting or
choose `default` to follow Codex's own configuration. `fullscreen` explicitly
enables Codex's fullscreen transcript; `scrollback` uses terminal scrollback and
selection. Claude receives no Codex display flags.

Paddock passes the setting as invocation arguments and leaves Codex's global and
project configuration files intact. A task remembers its view across resumes;
`resume --codex-tui` changes the view for a stopped task while preserving its native
conversation. A running task must exit before a different view can be applied at
resume. Use `/tui` inside Codex to select its native preference instead. Switching
between local and remote Rex clients does not automatically change a running TUI.
