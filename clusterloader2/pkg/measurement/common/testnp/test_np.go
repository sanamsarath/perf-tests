package testnp

import (
	"embed"

	"k8s.io/klog/v2"
	"k8s.io/perf-tests/clusterloader2/pkg/measurement"
)

const (
	testNpMeasurmentName           = "TestNetworkPolicy"
	policyEgressTargetPodsFilePath = "manifests/policy-egress-allow-target-pods.yaml"
)

//go:embed manifests
var manifestsFS embed.FS

// init
func init() {
	// Register the TestNetworkPolicyMeasurement
	if err := measurement.Register(testNpMeasurmentName, createTestNetworkPolicyMeasurement); err != nil {
		klog.Fatalf("Failed to register the TestNetworkPolicyMeasurement: %v", err)
	}
}

// TestNetworkPolicyMeasurement is a struct that implements the Measurement interface
type TestNetworkPolicyMeasurement struct {
}

func createTestNetworkPolicyMeasurement() measurement.Measurement {
	return &TestNetworkPolicyMeasurement{}
}

// Name returns the name of the measurement
func (tnpm *TestNetworkPolicyMeasurement) Name() string {
	return testNpMeasurmentName
}

func (tnpm *TestNetworkPolicyMeasurement) Execute(config *measurement.Config) ([]measurement.Summary, error) {
	klog.Infof("Executing TestNetworkPolicyMeasurement")
	templateMap := map[string]interface{}{
		"Networking_Type":     "cilium",
		"OnlyTargetNamespace": true,
		"L7Enabled":           true,
	}
	if err := config.ClusterFramework.ApplyTemplatedManifests(manifestsFS, policyEgressTargetPodsFilePath, templateMap); err != nil {
		return nil, err
	}
	return nil, nil
}

func (tnpm *TestNetworkPolicyMeasurement) Dispose() {
}

func (tnpm *TestNetworkPolicyMeasurement) String() string {
	return testNpMeasurmentName
}
