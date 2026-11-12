package paddock

import (
	"context"
	"fmt"
)

// Claude Code 2.1.286 has no CLI option for project trust. Its isolated
// OpenShell home starts with a small, image-owned config, so we record only the
// trust decision explicitly granted through `paddock repo trust` before launch.
// Tool permissions remain under Claude's normal approval policy.
const claudeProjectTrustScript = `import json,pathlib,sys
config=pathlib.Path(sys.argv[1])
worktree=sys.argv[2]
data=json.loads(config.read_text())
if not isinstance(data,dict): raise ValueError('invalid Claude config')
projects=data.setdefault('projects',{})
if not isinstance(projects,dict): raise ValueError('invalid Claude projects')
project=projects.setdefault(worktree,{})
if not isinstance(project,dict): raise ValueError('invalid Claude project')
project['hasTrustDialogAccepted']=True
temp=config.with_name(config.name+'.paddock-trust-tmp')
temp.write_text(json.dumps(data)); temp.chmod(0o600); temp.replace(config)
`

func installClaudeProjectTrust(ctx context.Context, task Task) error {
	if task.Session.Agent != "claude" || !task.Session.TrustProject {
		return nil
	}
	if task.RuntimeName() != "openshell" || task.Worktree != "/sandbox/worktree" {
		return fmt.Errorf("Claude project trust requires the isolated OpenShell worktree")
	}
	_, err := sandboxOutput(ctx, task, "/usr/bin/python3", "-c", claudeProjectTrustScript,
		"/home/sandbox/.claude/.claude.json", task.Worktree)
	if err != nil {
		return fmt.Errorf("record Claude project trust: %w", err)
	}
	return nil
}
