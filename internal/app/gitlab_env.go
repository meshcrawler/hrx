package app

import "os"

// The GitLab launch contract: environment a broker launch gives the harness and
// that HRX's own helper processes (the git credential helper and the glab shim)
// read back. Names are defined only here; writers and readers use these
// constants and gitlabLaunch, never string literals.
const (
	envGitlabToken        = "GITLAB_TOKEN" // read by glab and the git helper
	envAgentIdentity      = "HRX_AGENT_IDENTITY"
	envCredentialHost     = "HRX_GITLAB_CREDENTIAL_HOST"
	envCredentialUser     = "HRX_GITLAB_CREDENTIAL_USER"
	envLaunchTokenSHA256  = "HRX_GITLAB_LAUNCH_TOKEN_SHA256" // digest only, never the token
	envLaunchTokenExpires = "HRX_GITLAB_LAUNCH_TOKEN_EXPIRES"
	envGlabReal           = "HRX_GLAB_REAL"
)

// gitlabLaunch is the contract as a helper process sees it.
type gitlabLaunch struct {
	Identity           string // broker identity the launch was issued for
	Host               string // GitLab host the credential is valid for
	Username           string // service account the credential belongs to
	Token              string // current GITLAB_TOKEN: the launch token or one the caller set
	LaunchTokenSHA256  string // recognises the launch token without keeping a copy
	LaunchTokenExpires string // YYYY-MM-DD, as issued by the broker
	RealGlab           string // absolute path the glab shim execs
}

func currentGitlabLaunch() gitlabLaunch {
	return gitlabLaunch{
		Identity:           os.Getenv(envAgentIdentity),
		Host:               os.Getenv(envCredentialHost),
		Username:           os.Getenv(envCredentialUser),
		Token:              os.Getenv(envGitlabToken),
		LaunchTokenSHA256:  os.Getenv(envLaunchTokenSHA256),
		LaunchTokenExpires: os.Getenv(envLaunchTokenExpires),
		RealGlab:           os.Getenv(envGlabReal),
	}
}
