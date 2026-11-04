package valid_test

import (
	"testing"

	"github.com/runatlantis/atlantis/server/core/config/valid"
	. "github.com/runatlantis/atlantis/testing"
)

func stepArgs(w valid.Workflow) map[string][]string {
	out := map[string][]string{}
	for _, st := range []valid.Stage{w.Plan, w.Apply, w.PolicyCheck, w.Import, w.StateRm} {
		for _, s := range st.Steps {
			out[s.StepName] = s.ExtraArgs
		}
	}
	return out
}

func defaultWorkflow() valid.Workflow {
	return valid.Workflow{
		Name: "default", Plan: valid.DefaultPlanStage, Apply: valid.DefaultApplyStage,
		PolicyCheck: valid.DefaultPolicyCheckStage, Import: valid.DefaultImportStage, StateRm: valid.DefaultStateRmStage,
	}
}

func TestInputsCompileToBuiltInSteps(t *testing.T) {
	in := valid.Inputs{
		VarFiles:      []string{"env/prod.tfvars", "common.tfvars"},
		Vars:          map[string]string{"region": "eu-west-1", "app": "web"},
		BackendConfig: []string{"prod.backend.hcl", "key=prod/terraform.tfstate"},
		ExtraArgs:     map[string][]string{"plan": {"-refresh=false"}, "apply": {"-parallelism=5"}, "policy_check": {"--no-fail"}},
	}
	args := stepArgs(in.Apply(defaultWorkflow()))
	Equals(t, []string{"-reconfigure", "-backend-config=prod.backend.hcl", "-backend-config=key=prod/terraform.tfstate"}, args["init"])
	// Vars are sorted so plans are reproducible.
	Equals(t, []string{"-var-file=env/prod.tfvars", "-var-file=common.tfvars", "-var", "app=web", "-var", "region=eu-west-1", "-refresh=false"}, args["plan"])
	// A saved plan cannot take variables; apply only gets its extra args.
	Equals(t, []string{"-parallelism=5"}, args["apply"])
	Equals(t, []string{"--no-fail"}, args["policy_check"])
	Equals(t, []string{"-var-file=env/prod.tfvars", "-var-file=common.tfvars", "-var", "app=web", "-var", "region=eu-west-1"}, args["import"])
}

func TestInputsDoNotDuplicateReconfigure(t *testing.T) {
	in := valid.Inputs{BackendConfig: []string{"b.hcl"}, ExtraArgs: map[string][]string{"init": {"-reconfigure", "-upgrade"}}}
	Equals(t, []string{"-backend-config=b.hcl", "-reconfigure", "-upgrade"}, stepArgs(in.Apply(defaultWorkflow()))["init"])
}

func TestEmptyInputsLeaveWorkflowUntouched(t *testing.T) {
	w := defaultWorkflow()
	Equals(t, w, valid.Inputs{}.Apply(w))
}

func TestInputsOverrideReplacesSetFields(t *testing.T) {
	server := valid.Inputs{VarFiles: []string{"server.tfvars"}, Env: map[string]string{"A": "1"}}
	project := &valid.Inputs{VarFiles: []string{"project.tfvars"}}
	got := server.Override(project)
	Equals(t, []string{"project.tfvars"}, got.VarFiles)
	Equals(t, map[string]string{"A": "1"}, got.Env) // unset fields keep the server value
	Equals(t, server, server.Override(nil))
}

func TestInputsOverrideMergesEnvByName(t *testing.T) {
	server := valid.Inputs{Env: map[string]string{"A": "server", "B": "server"}}
	got := server.Override(&valid.Inputs{Env: map[string]string{"B": "repo", "C": "repo"}})
	Equals(t, map[string]string{"A": "server", "B": "repo", "C": "repo"}, got.Env)
	Equals(t, map[string]string{"A": "server", "B": "server"}, server.Env) // base is not modified
}

func TestInputsOverrideKeepsServerExtraArgs(t *testing.T) {
	server := valid.Inputs{ExtraArgs: map[string][]string{
		"init":         {"-plugin-dir=/opt/plugins"},
		"plan":         {"-lock-timeout=5m"},
		"policy_check": {"--all-namespaces"},
	}}
	got := server.Override(&valid.Inputs{ExtraArgs: map[string][]string{
		"init": {"-upgrade"},
		"plan": {"-refresh=false", "-lock-timeout=5m"},
		"show": {"-no-color"},
	}})
	Equals(t, map[string][]string{
		"init":         {"-plugin-dir=/opt/plugins", "-upgrade"},
		"plan":         {"-lock-timeout=5m", "-refresh=false"},
		"show":         {"-no-color"},
		"policy_check": {"--all-namespaces"},
	}, got.ExtraArgs)
	Equals(t, []string{"-plugin-dir=/opt/plugins"}, server.ExtraArgs["init"]) // base is not modified
}

func TestExpandEnvResolvesContext(t *testing.T) {
	in := valid.Inputs{Env: map[string]string{
		"TF_AWS_DEFAULT_TAGS_repository": "github.com/${BASE_REPO_OWNER}/${BASE_REPO_NAME}",
		"DIR":                            "${REPO_REL_DIR}",
		"LITERAL":                        "${NOT_A_CONTEXT_VAR} and $HOME",
	}}
	got := in.ExpandEnv(map[string]string{"BASE_REPO_OWNER": "acme", "BASE_REPO_NAME": "infra", "REPO_REL_DIR": "envs/prod"})
	Equals(t, "github.com/acme/infra", got["TF_AWS_DEFAULT_TAGS_repository"])
	Equals(t, "envs/prod", got["DIR"])
	Equals(t, "${NOT_A_CONTEXT_VAR} and $HOME", got["LITERAL"])
}

// A flag given as two tokens, flag then value, is merged as one argument: a
// repeated flag with different values keeps both, and an identical pair is
// kept once.
func TestInputsOverrideKeepsFlagValuePairs(t *testing.T) {
	server := valid.Inputs{ExtraArgs: map[string][]string{
		"plan": {"-target", "module.a", "-lock-timeout=5m"},
	}}
	got := server.Override(&valid.Inputs{ExtraArgs: map[string][]string{
		"plan": {"-target", "module.b", "-target", "module.a", "-refresh=false"},
	}})
	Equals(t, []string{"-target", "module.a", "-lock-timeout=5m", "-target", "module.b", "-refresh=false"}, got.ExtraArgs["plan"])
}
