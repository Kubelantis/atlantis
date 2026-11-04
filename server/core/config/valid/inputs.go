package valid

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// InputsKey is the allowed_overrides key that lets repo config set inputs.
const InputsKey = "inputs"

// Inputs configures the built-in steps declaratively: variable files and
// values, backend configuration, extra arguments and environment variables.
// It replaces what custom workflows used `extra_args`, `env` and `run`
// steps for. Inputs never add steps; they only shape built-in ones.
type Inputs struct {
	// VarFiles are passed to plan and import as -var-file, in order.
	VarFiles []string
	// Vars are passed to plan and import as -var, sorted by name.
	Vars map[string]string
	// BackendConfig entries (files or key=value) are passed to init as
	// -backend-config. Init then also gets -reconfigure, so projects that
	// switch backends between environments need no cleanup step.
	BackendConfig []string
	// Env is set for every step. Values may reference ${BASE_REPO_OWNER},
	// ${BASE_REPO_NAME}, ${REPO_REL_DIR}, ${WORKSPACE}, ${PROJECT_NAME},
	// ${PULL_NUM} and ${HEAD_COMMIT}.
	Env map[string]string
	// ExtraArgs are appended to the built-in step of the same name.
	ExtraArgs map[string][]string
}

// InputsExtraArgsSteps are the built-in steps extra_args may target.
var InputsExtraArgsSteps = []string{"init", "plan", "apply", "show", "policy_check", "import", "state_rm"}

// Override returns base with the fields set in o applied on top. VarFiles,
// Vars and BackendConfig set in o replace those of base. Env is merged by name,
// o winning. ExtraArgs are merged by step, with the arguments of o appended
// after those of base, so o cannot remove an argument base passes (such as
// -plugin-dir). A nil o returns base unchanged.
//
// o is repo config, which pull request authors control; base is the
// operator's server-side config.
func (base Inputs) Override(o *Inputs) Inputs {
	if o == nil {
		return base
	}
	out := base
	if o.VarFiles != nil {
		out.VarFiles = o.VarFiles
	}
	if o.Vars != nil {
		out.Vars = o.Vars
	}
	if o.BackendConfig != nil {
		out.BackendConfig = o.BackendConfig
	}
	if o.Env != nil {
		out.Env = maps.Clone(base.Env)
		if out.Env == nil {
			out.Env = map[string]string{}
		}
		maps.Copy(out.Env, o.Env)
	}
	if o.ExtraArgs != nil {
		out.ExtraArgs = maps.Clone(base.ExtraArgs)
		if out.ExtraArgs == nil {
			out.ExtraArgs = map[string][]string{}
		}
		for step, args := range o.ExtraArgs {
			if merged := mergeArgs(out.ExtraArgs[step], args); len(merged) > 0 {
				out.ExtraArgs[step] = merged
			}
		}
	}
	return out
}

// mergeArgs appends extra's arguments to base's, leaving out those base
// already has. An argument is a single token (`-upgrade`, `-lock-timeout=5m`)
// or a flag followed by its value (`-target module.a`), so the same flag with
// a different value is kept rather than losing its flag.
func mergeArgs(base, extra []string) []string {
	merged := slices.Clone(base)
	have := map[string]bool{}
	for _, a := range argUnits(base) {
		have[strings.Join(a, "\x00")] = true
	}
	for _, a := range argUnits(extra) {
		key := strings.Join(a, "\x00")
		if have[key] {
			continue
		}
		have[key] = true
		merged = append(merged, a...)
	}
	return merged
}

// argUnits groups args into arguments: a flag without `=` followed by a token
// that isn't a flag is one argument with its value.
func argUnits(args []string) [][]string {
	var units [][]string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-") && !strings.Contains(arg, "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			units = append(units, []string{arg, args[i+1]})
			i++
			continue
		}
		units = append(units, []string{arg})
	}
	return units
}

// IsZero reports whether no input is configured.
func (i Inputs) IsZero() bool {
	return len(i.VarFiles) == 0 && len(i.Vars) == 0 && len(i.BackendConfig) == 0 && len(i.Env) == 0 && len(i.ExtraArgs) == 0
}

// args returns the arguments inputs add to the built-in step stepName.
func (i Inputs) args(stepName string) []string {
	var args []string
	switch stepName {
	case "init":
		if len(i.BackendConfig) > 0 && !slices.Contains(i.ExtraArgs["init"], "-reconfigure") {
			args = append(args, "-reconfigure")
		}
		for _, b := range i.BackendConfig {
			args = append(args, "-backend-config="+b)
		}
	case "plan", "import":
		for _, f := range i.VarFiles {
			args = append(args, "-var-file="+f)
		}
		for _, k := range slices.Sorted(maps.Keys(i.Vars)) {
			args = append(args, "-var", k+"="+i.Vars[k])
		}
	}
	return append(args, i.ExtraArgs[stepName]...)
}

// repoDeniedEnv are environment variables repo-side inputs may not set. They
// change which binaries or plugins run, how the CLI is configured, or carry
// Atlantis's own settings, so setting them would let a pull request undo
// controls the operator put on the server. HOME, TF_DATA_DIR and the XDG data
// directories are here because Terraform reads $HOME/.terraformrc and looks
// for providers in them, which would undo the TF_CLI_CONFIG_FILE and
// -plugin-dir limits. The proxy and CA variables would let a pull request
// read provider and cloud API traffic, AWS_CONFIG_FILE and
// AWS_SHARED_CREDENTIALS_FILE can name a credential_process command, and
// TF_REATTACH_PROVIDERS points Terraform at an already running provider that
// receives the provider configuration.
var repoDeniedEnv = []string{
	"PATH", "BASH_ENV", "ENV", "HOME", "TF_CLI_CONFIG_FILE", "TERRAFORM_CONFIG", "TF_PLUGIN_CACHE_DIR", "TF_DATA_DIR", "XDG_DATA_HOME", "XDG_DATA_DIRS", "TG_TF_PATH", "TERRAGRUNT_TFPATH", "NODE_OPTIONS",
	"HTTPS_PROXY", "HTTP_PROXY", "ALL_PROXY", "NO_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS",
	"AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE", "TF_REATTACH_PROVIDERS",
}

// repoDeniedEnvPrefixes are prefixes of environment variables repo-side
// inputs may not set; see repoDeniedEnv.
var repoDeniedEnvPrefixes = []string{"LD_", "DYLD_", "GIT_", "ATLANTIS_", "TF_CLI_ARGS"}

// ValidateRepoSide checks inputs that came from repo config, which pull
// request authors control. Server-side inputs are trusted and not checked.
func (i Inputs) ValidateRepoSide() error {
	for k := range i.Env {
		name := strings.ToUpper(k)
		if slices.Contains(repoDeniedEnv, name) || slices.ContainsFunc(repoDeniedEnvPrefixes, func(p string) bool { return strings.HasPrefix(name, p) }) {
			return fmt.Errorf("inputs.env.%s cannot be set in repo config; set it in server-side config instead", k)
		}
	}
	if _, ok := i.ExtraArgs["policy_check"]; ok {
		return errors.New("inputs.extra_args.policy_check cannot be set in repo config; set it in server-side config instead")
	}
	for _, arg := range i.ExtraArgs["init"] {
		if strings.HasPrefix(strings.TrimLeft(arg, "-"), "plugin-dir") {
			return fmt.Errorf("inputs.extra_args.init cannot pass %q in repo config; set it in server-side config instead", arg)
		}
	}
	return nil
}

// AllArgs returns every argument the inputs add to any built-in step.
func (i Inputs) AllArgs() []string {
	var all []string
	for _, step := range InputsExtraArgsSteps {
		for _, arg := range i.args(step) {
			if !slices.Contains(all, arg) {
				all = append(all, arg)
			}
		}
	}
	return all
}

// Apply returns a copy of w whose built-in steps carry the inputs'
// arguments. Steps that are not built in are left unchanged.
func (i Inputs) Apply(w Workflow) Workflow {
	if i.IsZero() {
		return w
	}
	apply := func(s Stage) Stage {
		steps := make([]Step, len(s.Steps))
		for n, step := range s.Steps {
			step.ExtraArgs = append(i.args(step.StepName), step.ExtraArgs...)
			if len(step.ExtraArgs) == 0 {
				step.ExtraArgs = nil
			}
			steps[n] = step
		}
		return Stage{Steps: steps}
	}
	w.Plan = apply(w.Plan)
	w.Apply = apply(w.Apply)
	w.PolicyCheck = apply(w.PolicyCheck)
	w.Import = apply(w.Import)
	w.StateRm = apply(w.StateRm)
	return w
}

// InputsContextVars are the names env values may reference.
var InputsContextVars = []string{"BASE_REPO_OWNER", "BASE_REPO_NAME", "REPO_REL_DIR", "WORKSPACE", "PROJECT_NAME", "PULL_NUM", "HEAD_COMMIT"}

// ExpandEnv resolves ${NAME} references to context values in Env.
// Unknown names are left as they are; config validation rejects them.
func (i Inputs) ExpandEnv(context map[string]string) map[string]string {
	if len(i.Env) == 0 {
		return nil
	}
	out := make(map[string]string, len(i.Env))
	for k, v := range i.Env {
		out[k] = expandContext(v, context)
	}
	return out
}

func expandContext(s string, context map[string]string) string {
	var b strings.Builder
	for {
		start := strings.Index(s, "${")
		if start < 0 {
			b.WriteString(s)
			return b.String()
		}
		end := strings.Index(s[start:], "}")
		if end < 0 {
			b.WriteString(s)
			return b.String()
		}
		name := s[start+2 : start+end]
		b.WriteString(s[:start])
		if v, ok := context[name]; ok {
			b.WriteString(v)
		} else {
			b.WriteString(s[start : start+end+1])
		}
		s = s[start+end+1:]
	}
}
