# Team Principal (legacy)

The Team Principal ran an agent on the host. It is unavailable because Paddock
now requires every agent turn and tool call to run inside OpenShell. Existing
Team Principal task records remain inspectable with `paddock tasks --json` and
`paddock show TASK_ID --json`, but they cannot resume.

Use a named identity and an OpenShell task for new work. The
[integration interface](integrations.md) supports deterministic dispatch and
follow-up, and the [OpenShell guide](openshell.md) covers execution profiles.

The [former Team Principal design](legacy/tp.md) remains available as historical
reference.
