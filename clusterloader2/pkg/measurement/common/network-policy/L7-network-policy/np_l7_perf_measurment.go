// L7 network policy performance measurement tests capture the performance of the L7 network policy enforcement
// in k8s cluster.
// Setup (includes): deployment of target (server) pods
//
//	L7 network polcies.
//
// run: Send traffic from client pods to server pods and measure the performance of the L7 network policy enforcement.
// gather: Gather the performance metrics.
package l7networkpolicy

import (
	"embed"
	"fmt"

	"k8s.io/perf-tests/clusterloader2/pkg/measurement"
	"k8s.io/perf-tests/clusterloader2/pkg/util"
)

const (
	l7NetworkPolicyMeasurementName = "L7NetworkPolicyPerfMeasurement"
	serviceAccountFilePath         = "manifests/setup/serviceaccount.yaml"
	clusterRoleFilePath            = "manifests/setup/clusterrole.yaml"
	clusterRoleBindingFilePath     = "manifests/setup/clusterrolebinding.yaml"
	netPolFilePath                 = "manifests/setup/networkpolicy.yaml"
	depTargetPodFilePath           = "manifests/setup/deployment-target-pod.yaml"
	depClientPodFilePath           = "manifests/run/deployment-client-pod.yaml"
)

//go:embed manifests
var manifestsFS embed.FS

type L7NetworkPolicyPerfMeasurement struct {
	isRunning bool
}

func createL7NetworkPolicyPerfMeasurement() measurement.Measurement {
	return &L7NetworkPolicyPerfMeasurement{}
}

func init() {
	measurement.Register(l7NetworkPolicyMeasurementName, createL7NetworkPolicyPerfMeasurement)
}

func (m *L7NetworkPolicyPerfMeasurement) Execute(config *measurement.Config) ([]measurement.Summary, error) {
	action, err := util.GetString(config.Params, "action")
	if err != nil {
		return nil, err
	}

	switch action {
	case "setup":
		return nil, nil
	case "run":
		return nil, nil
	case "gather":
		return nil, nil
	default:
		return nil, fmt.Errorf("unknown action: %s", action)
	}
}

func (m *L7NetworkPolicyPerfMeasurement) Setup(config *measurement.Config) error {
	if m.isRunning {
		return fmt.Errorf("Phase: Setup, %s: measurement already running", m)
	}

	// initialize the k8s client
	// create the cluster role
	// create the service account
	// create the cluster role binding
	// create the network policy
	// create target pod namespace
	// create the target pods

	return nil
}

func (m *L7NetworkPolicyPerfMeasurement) Dispose() {}

func (m *L7NetworkPolicyPerfMeasurement) String() string {
	return l7NetworkPolicyMeasurementName
}
