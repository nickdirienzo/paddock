# Requesting additional access

Submitting a request only asks for review. It does not grant access. Request the narrowest access needed for the authorized task instead of stopping to ask whether you may request it. The operator decides the grant. Never bypass the sandbox or retrieve host credentials.

## Network requests

Read `/etc/openshell/skills/policy_advisor.md` if it exists. If it is missing, use this bundled fallback. OpenShell has had workload guide-installation failures; a missing file does not establish that its agent API is unavailable.

Use Python's standard library when curl is unavailable. Keep the URL fixed to the sandbox-local service and bypass ambient proxy configuration:

```python
import json
import urllib.request

opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
def policy_request(path, body=None, timeout=40):
    data = None if body is None else json.dumps(body).encode()
    request = urllib.request.Request(
        'http://policy.local' + path, data=data,
        headers={'Content-Type': 'application/json'} if data is not None else {})
    with opener.open(request, timeout=timeout) as response:
        return response.read(1024 * 1024).decode()

print(policy_request('/v1/policy/current'))
print(policy_request('/v1/denials?last=10'))
```

Use the actual denied executable, method, host, port, and path. This npm metadata example requires replacing both placeholders from the denial before submission:

```json
{
  "intent_summary": "Read package metadata needed for the requested development work.",
  "operations": [{
    "addRule": {
      "ruleName": "task_npm_metadata",
      "rule": {
        "name": "task_npm_metadata",
        "endpoints": [{
          "host": "registry.npmjs.org",
          "port": 443,
          "protocol": "rest",
          "enforcement": "enforce",
          "rules": [{"allow": {"method": "GET", "path": "/<exact-package-path>"}}]
        }],
        "binaries": [{"path": "<absolute-executable-path-from-denial>"}]
      }
    }
  }]
}
```

Load the adjusted JSON and submit it with `policy_request('/v1/proposals', proposal)`. Inspect the response for accepted IDs and refusal reasons. Tarballs and downloaded binaries can require separate paths or hosts; propose those only when the task and observed denials require them.

Inspect an accepted ID with `GET /v1/proposals/ID`. Poll `GET /v1/proposals/ID/wait?timeout=30` using `policy_request`; repeat bounded waits while pending. On approval, wait for `policy_reloaded: true` before retrying the denied work. Rejection grants nothing; use its reason to adapt and report the remaining limitation. A denied request is evidence of missing access, not evidence that the proposed technical approach failed.

If the API returns `404 feature_disabled` or cannot be reached, report the advisor as unavailable with the request you needed. Do not change gateway/global settings, approval mode, provider credentials, filesystem policy, or substitute another host.

Protocol reference: https://docs.nvidia.com/openshell/v0.1.6/how-it-works/policies/advisor
