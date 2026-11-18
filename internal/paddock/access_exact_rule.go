package paddock

import (
	"path"
	"strings"

	api "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	sandboxapi "github.com/NVIDIA/OpenShell/sdk/go/proto/sandboxv1"
)

// qualifyExactNetworkRule limits the initial interactive network path to a
// complete credential-free REST AddRule. The captured token binds that rule;
// independently authorized provider/global policy changes remain independent.
func qualifyExactNetworkRule(chunk *api.PolicyChunk) string {
	if chunk == nil || chunk.GetProposedRule() == nil || hasUnknownAccessFields(chunk.ProtoReflect()) {
		return "The complete known network proposal is required."
	}
	rule := chunk.GetProposedRule()
	name := chunk.GetRuleName()
	if name == "" || rule.GetName() != name || strings.HasPrefix(strings.ToLower(name), "_provider_") {
		return "Reserved or inconsistent network rule names cannot be approved here."
	}
	if len(rule.GetEndpoints()) == 0 || len(rule.GetBinaries()) == 0 {
		return "Explicit endpoint and executable scope is required."
	}
	for _, binary := range rule.GetBinaries() {
		if binary == nil || !path.IsAbs(binary.GetPath()) || strings.ContainsRune(binary.GetPath(), '\x00') {
			return "An absolute executable identity is required."
		}
	}
	if chunk.GetCandidateEffectivePolicy() == nil {
		return "The evaluated candidate policy is required to check credential authority."
	}
	if chunk.GetSecurityNotes() != "" {
		return "Flagged proposals require a separate operator review; this interface accepts credential-free rules only."
	}
	validation := strings.ToLower(chunk.GetValidationResult())
	for _, finding := range []string{"credential_reach_expansion", "l7_bypass_credentialed", "capability_expansion", "validation unavailable"} {
		if strings.Contains(validation, finding) {
			return "The proposal cannot establish credential-free inspected access."
		}
	}
	for _, endpoint := range rule.GetEndpoints() {
		if endpoint == nil || endpoint.GetHost() == "" || strings.ContainsAny(endpoint.GetHost(), "*?[]") ||
			endpoint.GetProtocol() != "rest" || endpoint.GetTls() != sandboxapi.NetworkTlsMode_NETWORK_TLS_MODE_UNSPECIFIED ||
			endpoint.GetEnforcement() != sandboxapi.NetworkEnforcementMode_NETWORK_ENFORCEMENT_MODE_ENFORCE {
			return "Only exact-host REST endpoints with inspection and enforcement can be approved here."
		}
		if endpointCredentialAuthority(endpoint) || endpoint.GetPersistedQueries() != "" || len(endpoint.GetGraphqlPersistedQueries()) != 0 ||
			endpoint.GetGraphqlMaxBodyBytes() != 0 || endpoint.GetJsonRpcMaxBodyBytes() != 0 || endpoint.GetMcp() != nil {
			return "Credential, signing, provider, and non-REST authority is unavailable through network approval."
		}
		ports := endpointPorts(endpoint)
		if len(ports) == 0 {
			return "An explicit destination port is required."
		}
		for _, port := range ports {
			if port == 0 || port > 65535 {
				return "A valid destination port is required."
			}
		}
		if endpoint.GetAccess() == sandboxapi.NetworkAccessPreset_NETWORK_ACCESS_PRESET_UNSPECIFIED && len(endpoint.GetRules()) == 0 {
			return "An explicit REST method/path policy is required."
		}
		for _, l7 := range endpoint.GetRules() {
			allow := l7.GetAllow()
			if allow == nil || allow.GetMethod() == "" || allow.GetCommand() != "" || allow.GetOperationType() != "" ||
				allow.GetOperationName() != "" || len(allow.GetFields()) != 0 || len(allow.GetParams()) != 0 {
				return "Only disclosed REST allow operations are supported."
			}
		}
		// Provider credentials can authorize traffic independently of the new
		// rule's own fields. Reject any destination overlapping that authority.
		for _, effectiveRule := range chunk.GetCandidateEffectivePolicy().GetNetworkPolicies() {
			for _, existing := range effectiveRule.GetEndpoints() {
				if endpointCredentialAuthority(existing) && endpointDestinationsOverlap(endpoint, existing) {
					return "This destination overlaps provider credential or token-grant authority."
				}
			}
		}
	}
	return ""
}

func endpointCredentialAuthority(e *sandboxapi.NetworkEndpoint) bool {
	return e != nil && (e.GetCredentialBinding() != nil || e.GetCredentialSigning() != "" || e.GetSigningService() != "" || e.GetSigningRegion() != "" ||
		e.GetWebsocketCredentialRewrite() || e.GetRequestBodyCredentialRewrite() || e.GetAllowUninspectedCredentials() ||
		e.GetProviderCredentialed() || e.GetTokenGrantOwner() != "")
}
func endpointPorts(e *sandboxapi.NetworkEndpoint) []uint32 {
	if len(e.GetPorts()) != 0 {
		return e.GetPorts()
	}
	if e.GetPort() != 0 {
		return []uint32{e.GetPort()}
	}
	return nil
}
func endpointDestinationsOverlap(proposed, authority *sandboxapi.NetworkEndpoint) bool {
	host := strings.TrimSuffix(strings.ToLower(proposed.GetHost()), ".")
	pattern := strings.TrimSuffix(strings.ToLower(authority.GetHost()), ".")
	if pattern != "" && !strings.ContainsAny(pattern, "*?[]\\") {
		if host != pattern {
			return false
		}
	} else if pattern != "" && !strings.ContainsRune(pattern, '\\') {
		// Keep only invariant literal edges. This deliberately overapproximates
		// DNS *, ** (including zero labels), and character-pattern syntax.
		first, last := strings.IndexAny(pattern, "*?[]"), strings.LastIndexAny(pattern, "*?[]")
		if first >= 0 && (!strings.HasPrefix(host, strings.TrimRight(pattern[:first], ".")) || !strings.HasSuffix(host, strings.TrimLeft(pattern[last+1:], "."))) {
			return false
		}
	}
	// An empty or unsupported credential host is conservatively broad.
	a, b := endpointPorts(proposed), endpointPorts(authority)
	if len(b) == 0 {
		return true
	}
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}
