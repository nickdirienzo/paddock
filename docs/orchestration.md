# Model-backed orchestration (legacy)

Model-backed planning is unavailable. Its former planner ran on the host, while
Model-backed planning remains unavailable. Existing signal records can be inspected with `paddock orchestrate show SOURCE ID`, but
new model-backed plans cannot run.

For deterministic integration dispatch, use the [structured CLI
interface](integrations.md): `run`, `show`, `artifacts`, and `message` with stable
external IDs. That interface keeps authorization and task state with the
trusted caller and Paddock rather than a planner model.

The [retired planner design](legacy/orchestration.md) is retained as historical
reference, not as setup instructions.
