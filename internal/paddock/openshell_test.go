package paddock

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRepositoryTokenIsNarrowAndUncached(t *testing.T) {
	_, key := appKeyFixture(t)
	t.Setenv("PADDOCK_GITHUB_PRIVATE_KEY", string(key))
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "POST" || r.URL.Path != "/app/installations/2/access_tokens" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			Repositories []string          `json:"repositories"`
			Permissions  map[string]string `json:"permissions"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.Repositories) != 1 || body.Repositories[0] != "paddock" || len(body.Permissions) != 3 || body.Permissions["contents"] != "write" {
			t.Errorf("incorrect scope: %+v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "ghs_test", "expires_at": time.Now().Add(time.Hour), "repositories": []any{map[string]string{"full_name": "nickdirienzo/paddock"}}, "permissions": map[string]string{"contents": "write", "pull_requests": "write", "issues": "write", "metadata": "read"}})
	}))
	defer server.Close()
	a := newGitHubAppAuth(GitHubAppConfig{AppID: 1, InstallationID: 2, KeyEnvironment: true})
	a.baseURL = server.URL
	for range 2 {
		token, _, e := a.repositoryToken(context.Background(), "nickdirienzo/paddock")
		if e != nil || token != "ghs_test" {
			t.Fatalf("%q %v", token, e)
		}
	}
	if calls != 2 || a.token != "" {
		t.Fatal("task tokens must not reuse installation-wide auth cache")
	}
}
func TestRepositoryTokenRejectsBroaderGrant(t *testing.T) {
	_, key := appKeyFixture(t)
	t.Setenv("PADDOCK_GITHUB_PRIVATE_KEY", string(key))
	for _, bad := range []string{"repo", "permissions", "expiry"} {
		t.Run(bad, func(t *testing.T) {
			response := map[string]any{"token": "ghs_secret", "expires_at": time.Now().Add(time.Hour), "repositories": []any{map[string]string{"full_name": "nickdirienzo/paddock"}}, "permissions": map[string]string{"contents": "write", "pull_requests": "write", "issues": "write"}}
			switch bad {
			case "repo":
				response["repositories"] = []any{map[string]string{"full_name": "mirage-security/mirage"}}
			case "permissions":
				response["permissions"].(map[string]string)["administration"] = "write"
			case "expiry":
				response["expires_at"] = time.Now()
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(response) }))
			defer server.Close()
			a := newGitHubAppAuth(GitHubAppConfig{AppID: 1, InstallationID: 2, KeyEnvironment: true})
			a.baseURL = server.URL
			token, _, e := a.repositoryToken(context.Background(), "nickdirienzo/paddock")
			if e == nil || token != "" || strings.Contains(e.Error(), "ghs_secret") {
				t.Fatalf("accepted bad grant or exposed secret: %v", e)
			}
		})
	}
}
func TestOpenShellDefaultFailsClosedBeforeLaunch(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	repo := repoFixture(t)
	_, e := (Service{Store: s}).Run(context.Background(), RunOptions{Repo: repo, Prompt: "hello"})
	if e == nil || !strings.Contains(e.Error(), "default runtime") {
		t.Fatalf("missing setup must not run on host: %v", e)
	}
	tasks, _ := s.List("")
	if len(tasks) != 0 {
		t.Fatal("preflight created a task")
	}
	if (Task{}).RuntimeName() != "host" {
		t.Fatal("legacy task migrated implicitly")
	}
}
func TestSandboxCommandNeverInheritsHostCredentials(t *testing.T) {
	for k, v := range map[string]string{"GH_TOKEN": "personal-secret", "GITHUB_TOKEN": "personal-secret", "SSH_AUTH_SOCK": "/personal/ssh", "OPENAI_API_KEY": "personal-secret", "PADDOCK_GITHUB_PRIVATE_KEY": "private-secret", "CODEX_HOME": "/personal/.codex", "OPENSHELL_GATEWAY": "wrong-gateway"} {
		t.Setenv(k, v)
	}
	task := Task{ID: "012345678901234567890123", Runtime: "openshell", Worktree: "/sandbox/worktree", OpenShell: &OpenShellTask{Name: "pd-test", Config: OpenShellConfig{Binary: "/bin/echo", Gateway: "pinned", Workspace: "default"}}, Session: Session{Agent: "codex", AgentPath: "/usr/local/bin/codex"}}
	cmd := AgentCommand(task, "/host/state", "/host/paddock")
	for _, v := range cmd.Env {
		if strings.Contains(v, "personal") || strings.Contains(v, "secret") || strings.Contains(v, "wrong-gateway") {
			t.Fatalf("credential leak: %s", strings.SplitN(v, "=", 2)[0])
		}
	}
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "--gateway pinned") || !strings.Contains(args, "paddock-agent") || strings.Contains(args, "/host/state") || strings.Contains(args, "/host/paddock") {
		t.Fatalf("wrong execution boundary: %s", args)
	}
}
func TestSandboxOwnershipAndExitProof(t *testing.T) {
	for _, tc := range []struct {
		name, label, marker string
		want                bool
	}{
		{"confirmed", "task", `{"generation":3,"code":0}`, true},
		{"wrong-owner", "other", `{"generation":3,"code":0}`, false},
		{"stale-exit", "task", `{"generation":2,"code":0}`, false},
		{"transport-lost", "task", ``, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "calls")
			bin := script(t, `case "$5 $6" in
'sandbox get') printf '%s' '{"name":"pd-fixture","phase":"Ready","labels":{"paddock.task":"`+tc.label+`"}}';;
'sandbox exec') printf '%s' '`+tc.marker+`';;
'sandbox stop') printf stopped > `+quote(log)+`;;
esac`)
			task := Task{ID: "task", Runtime: "openshell", Worktree: "/sandbox/worktree", Session: Session{Generation: 3}, OpenShell: &OpenShellTask{Name: "pd-fixture", Config: OpenShellConfig{Binary: bin, Gateway: "fixture", Workspace: "default"}}}
			e := stopCompletedSandbox(context.Background(), task)
			_, stat := os.Stat(log)
			if (e == nil) != tc.want || (stat == nil) != tc.want {
				t.Fatalf("unsafe stop: error=%v stopped=%v", e, stat == nil)
			}
		})
	}
}
func TestGitHubProfileUsesStableReference(t *testing.T) {
	b, _ := json.Marshal(githubProfile("nickdirienzo/paddock", "task-profile"))
	if !strings.Contains(string(b), `"stable_placeholder":true`) || strings.Contains(string(b), "mirage") {
		t.Fatal("wrong provider boundary")
	}
}

func TestOpenShellUsesOuterSandboxWithApprovals(t *testing.T) {
	task := Task{Runtime: "openshell", Worktree: "/sandbox/worktree", OpenShell: &OpenShellTask{Config: OpenShellConfig{Binary: "/bin/echo"}}, Session: Session{Agent: "codex", AgentPath: "/usr/local/bin/codex"}}
	c := AgentCommand(task, "/host/state", "/host/paddock")
	args := strings.Join(c.Args, " ")
	if !strings.Contains(args, "--sandbox danger-full-access --ask-for-approval on-request") || strings.Contains(args, "--dangerously-bypass") || c.Args[0] != "codex" || c.Path != "/bin/echo" {
		t.Fatalf("wrong outer-sandbox invocation: %v", c.Args)
	}
	task.Session.Resume = true
	task.Session.ConversationID = "saved-conversation"
	c = AgentCommand(task, "/host/state", "/host/paddock")
	args = strings.Join(c.Args, " ")
	if !strings.Contains(args, "resume saved-conversation") || !strings.Contains(args, "--remote unix://") || strings.Contains(args, "--sandbox") || strings.Contains(args, "--ask-for-approval") {
		t.Fatalf("remote resume must restore saved permissions: %v", c.Args)
	}
	task.Runtime = "host"
	task.Session.AgentPath = "/bin/echo"
	c = AgentCommand(task, "/host/state", "/host/paddock")
	if strings.Contains(strings.Join(c.Args, " "), "--sandbox danger-full-access") {
		t.Fatal("host isolation changed")
	}
}
func TestSandboxGCRequiresStoppedCompute(t *testing.T) {
	s := openTestStore(t, t.TempDir())
	task := taskFixture()
	task.Runtime = "openshell"
	task.Worktree = "/sandbox/worktree"
	task.Status = "stopped"
	task.LastActivityAt = time.Now().Add(-8 * 24 * time.Hour).Format(time.RFC3339Nano)
	task.Session.Backend = "screen"
	task.Session.TerminalPath = script(t, "echo 'No Sockets found in /tmp.'\n")
	task.OpenShell = &OpenShellTask{Name: "pd-fixture", Ready: true, Config: OpenShellConfig{Gateway: "fixture", Workspace: "default", Binary: script(t, `printf '%s' '{"name":"pd-fixture","phase":"Ready","labels":{"paddock.task":"`+task.ID+`"}}'`)}}
	if e := s.Insert(task); e != nil {
		t.Fatal(e)
	}
	results, e := (Service{Store: s}).GC(context.Background(), true)
	if e != nil || len(results) != 1 || results[0].Removed || results[0].Eligible || !strings.Contains(results[0].Reason, "confirmed stopped") {
		t.Fatalf("unsafe sandbox collection: %+v %v", results, e)
	}
}

func TestNativeOwnerStatusGuards(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python is only required by the sandbox image")
	}
	helper, err := filepath.Abs("../../runtime/openshell/paddock-codex-state")
	if err != nil {
		t.Fatal(err)
	}
	fixture := `import json,os,runpy,socket,sys,types
helper,mode,status,queued=sys.argv[1:]
os.environ['CODEX_HOME']='/fixture'
class Sock:
 def settimeout(self,*args):pass
 def connect(self,*args):pass
socket.socket=lambda *args:Sock()
class WS:
 request=None
 started=False
 def send(self,data):self.request=json.loads(data)
 def recv(self):
  r=self.request;m=r['method']
  if m=='initialize':
   assert r['params']['capabilities']['experimentalApi'] is True
   result={}
  elif m=='thread/loaded/list':result={'data':['thread'],'nextCursor':None}
  elif m=='thread/read':result={'thread':{'ephemeral':queued=='ephemeral','status':{'type':status,'activeFlags':['waitingOnApproval'] if status=='active' else []}}}
  elif m=='thread/resume':
   assert status in ('idle','notLoaded')
   assert r['params']['sandbox']=='danger-full-access' and r['params']['approvalPolicy']=='on-request'
   result={'sandbox':{'type':'dangerFullAccess' if queued=='no' else 'readOnly'},'approvalPolicy':'on-request'}
  elif m=='thread/queue/list':
   assert queued!='ephemeral'
   result={'data':[{'id':'queued'}] if queued=='yes' else [],'nextCursor':None}
  elif m=='thread/queue/start':
   assert status=='idle'
   self.started=True;result={}
  else:raise AssertionError(m)
  return json.dumps({'id':r['id'],'result':result})
 def close(self):print('started='+str(self.started))
ws=WS();sys.modules['websocket']=types.SimpleNamespace(create_connection=lambda *a,**k:ws)
sys.argv=[helper,mode,'thread'];runpy.run_path(helper,run_name='__main__')
`
	for _, tc := range []struct {
		mode, status, queued string
		ok, start            bool
	}{
		{"idle", "idle", "no", true, false},
		{"idle", "active", "no", false, false},
		{"idle", "idle", "yes", false, false},
		{"idle", "systemError", "no", false, false},
		{"idle", "idle", "ephemeral", true, false},
		{"idle", "active", "ephemeral", false, false},
		{"owner", "notLoaded", "no", false, false},
		{"resume", "notLoaded", "no", true, false},
		{"resume", "idle", "yes", false, false},
		{"resume", "active", "no", false, false},
		{"start-queued", "idle", "yes", true, true},
		{"start-queued", "active", "yes", true, false},
	} {
		t.Run(tc.mode+tc.status+tc.queued, func(t *testing.T) {
			b, e := exec.Command(python, "-c", fixture, helper, tc.mode, tc.status, tc.queued).CombinedOutput()
			if (e == nil) != tc.ok || (strings.Contains(string(b), "started=True")) != tc.start {
				t.Fatalf("native owner guard: %s %v", b, e)
			}
		})
	}
}

func TestAgentTransportIdentity(t *testing.T) {
	home := t.TempDir()
	source := filepath.Join(t.TempDir(), "openshell")
	if e := os.WriteFile(source, []byte("pinned transport"), 0700); e != nil {
		t.Fatal(e)
	}
	proxy, e := sandboxAgentProxy(home, source, "codex")
	if e != nil {
		t.Fatal(e)
	}
	if filepath.Base(proxy) != "codex" {
		t.Fatal(proxy)
	}
	if e = os.WriteFile(source, []byte("upgraded transport"), 0700); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(proxy)
	if e != nil || string(b) != "pinned transport" {
		t.Fatal("proxy changed with source")
	}
	next, e := sandboxAgentProxy(home, source, "codex")
	if e != nil || next == proxy {
		t.Fatal("upgrade reused stale transport")
	}
	if e = os.WriteFile(next, []byte("tampered"), 0700); e != nil {
		t.Fatal(e)
	}
	if _, e = sandboxAgentProxy(home, source, "codex"); e == nil {
		t.Fatal("accepted modified transport")
	}
}
