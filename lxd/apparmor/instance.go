package apparmor

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/lxc/lxd/lxd/cgroup"
	"github.com/lxc/lxd/lxd/project"
	"github.com/lxc/lxd/lxd/state"
	"github.com/lxc/lxd/shared"
)

// Internal copy of the instance interface.
type instance interface {
	Project() string
	Name() string
	ExpandedConfig() map[string]string
}

// InstanceProfileName returns the instance's AppArmor profile name.
func InstanceProfileName(inst instance) string {
	path := shared.VarPath("")
	name := fmt.Sprintf("%s_<%s>", project.Instance(inst.Project(), inst.Name()), path)
	return profileName("", name)
}

// InstanceNamespaceName returns the instance's AppArmor namespace.
func InstanceNamespaceName(inst instance) string {
	// Unlike in profile names, / isn't an allowed character so replace with a -.
	path := strings.Replace(strings.Trim(shared.VarPath(""), "/"), "/", "-", -1)
	name := fmt.Sprintf("%s_<%s>", project.Instance(inst.Project(), inst.Name()), path)
	return profileName("", name)
}

// instanceProfileFilename returns the name of the on-disk profile name.
func instanceProfileFilename(inst instance) string {
	name := project.Instance(inst.Project(), inst.Name())
	return profileName("", name)
}

// InstanceLoad ensures that the instances's policy is loaded into the kernel so the it can boot.
func InstanceLoad(state *state.State, inst instance) error {
	err := createNamespace(state, InstanceNamespaceName(inst))
	if err != nil {
		return err
	}

	/* In order to avoid forcing a profile parse (potentially slow) on
	 * every container start, let's use AppArmor's binary policy cache,
	 * which checks mtime of the files to figure out if the policy needs to
	 * be regenerated.
	 *
	 * Since it uses mtimes, we shouldn't just always write out our local
	 * AppArmor template; instead we should check to see whether the
	 * template is the same as ours. If it isn't we should write our
	 * version out so that the new changes are reflected and we definitely
	 * force a recompile.
	 */
	profile := filepath.Join(aaPath, "profiles", instanceProfileFilename(inst))
	content, err := ioutil.ReadFile(profile)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	updated, err := instanceProfile(state, inst)
	if err != nil {
		return err
	}

	if string(content) != string(updated) {
		err = ioutil.WriteFile(profile, []byte(updated), 0600)
		if err != nil {
			return err
		}
	}

	err = loadProfile(state, instanceProfileFilename(inst))
	if err != nil {
		return err
	}

	return nil
}

// InstanceUnload ensures that the instances's policy namespace is unloaded to free kernel memory.
// This does not delete the policy from disk or cache.
func InstanceUnload(state *state.State, inst instance) error {
	err := deleteNamespace(state, InstanceNamespaceName(inst))
	if err != nil {
		return err
	}

	err = unloadProfile(state, instanceProfileFilename(inst))
	if err != nil {
		return err
	}

	return nil
}

// InstanceParse validates the instance profile.
func InstanceParse(state *state.State, inst instance) error {
	return parseProfile(state, instanceProfileFilename(inst))
}

// InstanceDelete removes the policy from cache/disk.
func InstanceDelete(state *state.State, inst instance) error {
	return deleteProfile(state, instanceProfileFilename(inst))
}

// instanceProfile generates the AppArmor profile template from the given instance.
func instanceProfile(state *state.State, inst instance) (string, error) {
	// Prepare raw.apparmor.
	rawContent := ""
	rawApparmor, ok := inst.ExpandedConfig()["raw.apparmor"]
	if ok {
		for _, line := range strings.Split(strings.Trim(rawApparmor, "\n"), "\n") {
			rawContent += fmt.Sprintf("  %s\n", line)
		}
	}

	// Check for features.
	unixSupported, err := parserSupports(state, "unix")
	if err != nil {
		return "", err
	}

	// Render the profile.
	var sb *strings.Builder = &strings.Builder{}
	err = lxcProfileTpl.Execute(sb, map[string]interface{}{
		"feature_unix":     unixSupported,
		"feature_cgns":     state.OS.CGInfo.Namespacing,
		"feature_cgroup2":  state.OS.CGInfo.Layout == cgroup.CgroupsUnified || state.OS.CGInfo.Layout == cgroup.CgroupsHybrid,
		"feature_stacking": state.OS.AppArmorStacking && !state.OS.AppArmorStacked,
		"namespace":        InstanceNamespaceName(inst),
		"nesting":          shared.IsTrue(inst.ExpandedConfig()["security.nesting"]),
		"name":             InstanceProfileName(inst),
		"unprivileged":     !shared.IsTrue(inst.ExpandedConfig()["security.privileged"]) || state.OS.RunningInUserNS,
		"raw":              rawContent,
	})
	if err != nil {
		return "", err
	}

	return sb.String(), nil
}
