package paddock

import (
	"context"
	_ "embed"
	"strings"
)

// Runtime guidance belongs to Paddock, including follow-ups to older saved
// sessions that predate the access guide. Keep authored text unchanged in the
// message receipt; only the frozen native delivery body includes this pointer.
const runtimeAccessGuidance = "For additional network access, read /sandbox/.paddock/access-guide.md (also listed by paddock --help)."

// AuthoredMessageBody adds runtime guidance only to newly authored OpenShell
// input. Internal notifications and outboxes use Message directly and are not
// augmented. Callers with idempotency keys must persist this once at admission.
func AuthoredMessageBody(task Task, body string) string {
	if task.RuntimeName() != "openshell" || strings.TrimSpace(body) == "" {
		return body
	}
	return body + "\n\nPaddock runtime guidance: " + runtimeAccessGuidance
}

//go:embed access_guide.md
var sandboxAccessGuide string

// The directory descriptor anchors installation inside the task's existing
// private control directory. No symlink target is followed and no root-owned
// source, instruction, or host path is overwritten.
const installSandboxAccessGuide = `import os,pathlib,secrets,stat,sys
root=pathlib.Path('/sandbox/.paddock')
if root.parent.resolve()!=root.parent or root.is_symlink():
    raise RuntimeError('Paddock access guide directory must not be redirected')
root.mkdir(mode=0o700,exist_ok=True)
directory=os.open(root,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW)
temp='.access-guide-'+secrets.token_hex(12)
payload=sys.argv[1].encode('utf-8')
try:
    fd=os.open(temp,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600,dir_fd=directory)
    with os.fdopen(fd,'wb') as out:
        out.write(payload)
    os.replace(temp,'access-guide.md',src_dir_fd=directory,dst_dir_fd=directory)
    fd=os.open('access-guide.md',os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK,dir_fd=directory)
    with os.fdopen(fd,'rb') as source:
        if not stat.S_ISREG(os.fstat(source.fileno()).st_mode) or source.read(len(payload)+1)!=payload:
            raise RuntimeError('Paddock access guide is not readable or complete')
    if not os.path.samestat(os.lstat(root),os.fstat(directory)):
        raise RuntimeError('Paddock access guide directory changed during installation')
finally:
    try: os.unlink(temp,dir_fd=directory)
    except FileNotFoundError: pass
    os.close(directory)
`

func installAccessGuide(ctx context.Context, t Task) error {
	_, err := sandboxOutput(ctx, t, "python3", "-c", installSandboxAccessGuide, sandboxAccessGuide)
	return err
}
