package rke2

import (
	"encoding/json"
	"reflect"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/collections"

	bootstrapv1 "github.com/rancher/cluster-api-provider-rke2/bootstrap/api/v1beta1"
	controlplanev1 "github.com/rancher/cluster-api-provider-rke2/controlplane/api/v1beta1"
	bsutil "github.com/rancher/cluster-api-provider-rke2/pkg/util"
)

// matchesRCPConfiguration returns a filter to find all machines that matches with RCP config and do not require any rollout.
// Kubernetes version, infrastructure template, and RKE2Config field need to be equivalent.
func matchesRCPConfiguration(
	infraConfigs map[string]*unstructured.Unstructured,
	machineConfigs map[string]*bootstrapv1.RKE2Config,
	rcp *controlplanev1.RKE2ControlPlane,
) func(machine *clusterv1.Machine) bool {
	return collections.And(
		matchesKubernetesOrRKE2Version(rcp.GetDesiredVersion()),
		matchesRKE2BootstrapConfig(machineConfigs, rcp),
		matchesTemplateClonedFrom(infraConfigs, rcp),
	)
}

// matchesRKE2BootstrapConfig checks if machine's RKE2ConfigSpec is equivalent with RCP's RKE2ConfigSpec.
func matchesRKE2BootstrapConfig(machineConfigs map[string]*bootstrapv1.RKE2Config, rcp *controlplanev1.RKE2ControlPlane) collections.Func {
	return func(machine *clusterv1.Machine) bool {
		if machine == nil {
			return true
		}

		// Check if RCP and machine RKE2Config matches, if not return
		if match := matchServerConfig(rcp, machine); !match {
			return false
		}

		bootstrapRef := machine.Spec.Bootstrap.ConfigRef
		if bootstrapRef == nil {
			// Missing bootstrap reference should not be considered as unmatching.
			// This is a safety precaution to avoid selecting machines that are broken, which in the future should be remediated separately.
			return true
		}

		machineConfig, found := machineConfigs[machine.Name]
		if !found {
			// Return true here because failing to get KubeadmConfig should not be considered as unmatching.
			// This is a safety precaution to avoid rolling out machines if the client or the api-server is misbehaving.
			return true
		}

		if _, ok := machineConfig.Annotations["cluster-api.cattle.io/turtles-system-agent"]; ok {
			files := []bootstrapv1.File{}

			for _, file := range machineConfig.Spec.Files {
				switch file.Path {
				case "/etc/rancher/agent/connect-info-config.json", "/opt/system-agent-install.sh",
					"/etc/rancher/agent/config.yaml": // Filter out files that are injected by the Rancher Turtles webhook
					continue
				}

				files = append(files, file)
			}

			if len(files) == 0 {
				machineConfig.Spec.Files = nil // Set to nil because rcp.Spec.RKE2ConfigSpec.Files will be nil if no files are present
			} else {
				machineConfig.Spec.Files = files
			}

			cmds := []string{}

			for _, cmd := range machineConfig.Spec.PostRKE2Commands { // Filter out commands that are injected by the Rancher Turtles webhook
				if cmd == "sh /opt/system-agent-install.sh" {
					continue
				}

				cmds = append(cmds, cmd)
			}

			if len(cmds) == 0 {
				machineConfig.Spec.PostRKE2Commands = nil // Set to nil because rcp.Spec.RKE2ConfigSpec.PostRKE2Commands will be nil if no commands are present
			} else {
				machineConfig.Spec.PostRKE2Commands = cmds
			}
		}

		// Check if RCP AgentConfig and machineBootstrapConfig matches
		return reflect.DeepEqual(machineConfig.Spec, rcp.Spec.RKE2ConfigSpec)
	}
}

// matchServerConfig checks if RKE2Configs in the ControlPlane object and the machine annotation match.
func matchServerConfig(rcp *controlplanev1.RKE2ControlPlane, machine *clusterv1.Machine) bool {
	machineServerConfigStr, ok := machine.GetAnnotations()[controlplanev1.RKE2ServerConfigurationAnnotation]
	if !ok {
		// We don't have enough information to make a decision; don't' trigger a roll out.
		return true
	}

	machineServerConfig := &controlplanev1.RKE2ServerConfig{}
	// RKE2ServerConfig annotation is not correct, need to rollout new machine
	if err := json.Unmarshal([]byte(machineServerConfigStr), &machineServerConfig); err != nil {
		return false
	}

	if machineServerConfig == nil {
		machineServerConfig = &controlplanev1.RKE2ServerConfig{}
	}

	var rcpServerConfig *controlplanev1.RKE2ServerConfig
	if reflect.DeepEqual(rcp.Spec.ServerConfig, controlplanev1.RKE2ServerConfig{}) {
		// If the ServerConfig is empty, initialize a new RKE2ServerConfig
		rcpServerConfig = &controlplanev1.RKE2ServerConfig{}
	} else {
		// Otherwise, take the address of the existing ServerConfig
		rcpServerConfig = &rcp.Spec.ServerConfig
	}

	// Compare and return
	return reflect.DeepEqual(machineServerConfig, rcpServerConfig)
}

// matchesTemplateClonedFrom returns a filter to find all machines that match a given RCP infra template.
func matchesTemplateClonedFrom(infraConfigs map[string]*unstructured.Unstructured, rcp *controlplanev1.RKE2ControlPlane) collections.Func {
	return func(machine *clusterv1.Machine) bool {
		if machine == nil {
			return false
		}

		infraObj, found := infraConfigs[machine.Name]
		if !found {
			// Return true here because failing to get infrastructure machine should not be considered as unmatching.
			return true
		}

		clonedFromName, ok1 := infraObj.GetAnnotations()[clusterv1.TemplateClonedFromNameAnnotation]
		clonedFromGroupKind, ok2 := infraObj.GetAnnotations()[clusterv1.TemplateClonedFromGroupKindAnnotation]

		if !ok1 || !ok2 {
			// All rcp cloned infra machines should have this annotation.
			// Missing the annotation may be due to older version machines or adopted machines.
			// Should not be considered as mismatch.
			return true
		}

		// Check if the machine's infrastructure reference has been created from the current RCP infrastructure template.
		if clonedFromName != rcp.Spec.MachineTemplate.InfrastructureRef.Name ||
			clonedFromGroupKind != rcp.Spec.MachineTemplate.InfrastructureRef.GroupVersionKind().GroupKind().String() {
			return false
		}

		return true
	}
}

// matchesKubernetesVersion returns a filter to find all machines that match a given Kubernetes or RKE2 version.
func matchesKubernetesOrRKE2Version(rke2Version string) func(*clusterv1.Machine) bool {
	return func(machine *clusterv1.Machine) bool {
		if machine == nil {
			return false
		}

		if machine.Spec.Version == nil {
			return false
		}

		if bsutil.IsRKE2Version(*machine.Spec.Version) {
			return bsutil.CompareVersions(*machine.Spec.Version, rke2Version)
		}

		rcpKubeVersion, err := bsutil.Rke2ToKubeVersion(rke2Version)
		if err != nil {
			return true
		}

		return bsutil.CompareVersions(*machine.Spec.Version, rcpKubeVersion)
	}
}
