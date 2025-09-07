package soaktest

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	api_corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/util/exec"
	"k8s.io/klog/v2"
	"k8s.io/perf-tests/clusterloader2/pkg/framework"
	"k8s.io/perf-tests/clusterloader2/pkg/framework/client"
	"k8s.io/perf-tests/clusterloader2/pkg/measurement"
	measurementutil "k8s.io/perf-tests/clusterloader2/pkg/measurement/util"
	"k8s.io/perf-tests/clusterloader2/pkg/measurement/util/gatherers"
	"k8s.io/perf-tests/clusterloader2/pkg/util"
)

const (
	// file paths
	connectivitySoakMeasurementName = "ConnectivitySoakMeasurement"
	serviceAccountFilePath          = "manifests/serviceaccount.yaml"
	clusterRoleFilePath             = "manifests/clusterrole.yaml"
	clusterRoleBindingFilePath      = "manifests/clusterrolebinding.yaml"
	clientFilePath                  = "manifests/client_deploy.yaml"
	targetFilePath                  = "manifests/target_deploy.yaml"
	targetServiceFilePath           = "manifests/target_service.yaml"

	// network policies
	netPolFilePath    = "manifests/network_policy.yaml"
	APIserverFilePath = "manifests/allow_apiserver_np.yaml"
	dnsccnpFilePath   = "manifests/dns_ccnp.yaml"

	// DNS and LRP
	nodelocaldnsCMFilePath         = "manifests/nodelocaldns_cm.yaml"
	nodelocaldnsDSFilePath         = "manifests/nodelocaldns_ds.yaml"
	nodelocaldnsCRFilePath         = "manifests/nodelocaldns_clusterrole.yaml"
	nodelocaldnsCRBindingFilePath  = "manifests/nodelocaldns_crbinding.yaml"
	nodelocaldnsServiceAccFilePath = "manifests/nodelocaldns_sa.yaml"
	nodelocaldnsServiceFilePath    = "manifests/nodelocaldns_service.yaml"
	lrpFilePath                    = "manifests/nodelocaldns_lrp.yaml"
	busyboxDaemonSetFilePath       = "manifests/busybox_daemonset.yaml"

	// variables
	clientNamespace = "soak-client"
	clientName      = "soak-client" // all the k8s resources will be prefixed with this name
	targetName      = "soak-target" // all the deployments in target namespaces will be named with soak-target
	apiserverNPName = "allow-egress-apiserver"
)

//go:embed manifests
var manifestsFS embed.FS

type ConnectivitySoakMeasurement struct {
	isRunning            bool
	testDuration         time.Duration
	k8sClient            kubernetes.Interface
	framework            *framework.Framework
	targetNamespaces     []string
	targetLabelKey       string
	targetLabelVal       string
	clientLabelKey       string
	clientLabelVal       string
	targetReplicasPerNs  int
	clientReplicasPerDep int
	targetPort           int
	targetPort2          int
	targetPath           string
	testEndTime          time.Time
	workerPerClient      int
	enableNetworkPolicy  bool
	l7Enabled            bool
	l3l4port             bool
	isRestart            bool
	npType               string
	// gatherers
	gatherers                *gatherers.ContainerResourceGatherer
	resourceGatheringEnabled bool
	// DNS testing fields
	restConfig      *rest.Config
	dnsTestStopChan chan struct{}
	dnsTestWg       sync.WaitGroup
}

func createConnectivitySoakMeasurement() measurement.Measurement {
	return &ConnectivitySoakMeasurement{}
}

func init() {
	measurement.Register(connectivitySoakMeasurementName, createConnectivitySoakMeasurement)
}

func (m *ConnectivitySoakMeasurement) Execute(config *measurement.Config) ([]measurement.Summary, error) {
	action, err := util.GetString(config.Params, "action")
	if err != nil {
		return nil, err
	}

	switch action {
	case "start":
		return m.start(config)
	case "gather":
		return m.gather()
	case "restart":
		return m.restart()
	case "delete k8s nps":
		return m.deleteK8sNPs()
	case "delete-ccnps-cnps":
		return m.deleteNetworkPolicies()
	case "delete-pods":
		return m.deletePods()
	default:
		return nil, fmt.Errorf("unknown action: %s", action)
	}
}

func (m *ConnectivitySoakMeasurement) start(config *measurement.Config) ([]measurement.Summary, error) {
	if m.isRunning && !m.isRestart {
		return nil, fmt.Errorf("phase: start, %s: measurement already running", m.String())
	}

	if err := m.initialize(config, "start"); err != nil {
		return nil, err
	}

	// do this only in the start phase
	if !m.isRestart {
		// create the client namespace
		if err := client.CreateNamespace(m.k8sClient, clientNamespace); err != nil {
			return nil, fmt.Errorf("phase: start, %s: failed to create namespace %s: %v", m.String(), clientNamespace, err)
		}

		// deploy the RBAC resources
		if err := m.deployRBACResources(); err != nil {
			return nil, err
		}

		// deploy NodeLocalDNS DaemonSet and LocalRedirectPolicy
		if err := m.deployNodeLocalDNSAndLRP(); err != nil {
			return nil, err
		}

		// // Deploy busybox DaemonSet (1 pod per node)
		// if err := m.framework.ApplyTemplatedManifests(manifestsFS, busyboxDaemonSetFilePath, templateMap); err != nil {
		// 	return fmt.Errorf("phase: start, %s: failed to apply busybox DaemonSet manifest: %v", m.String(), err)
		// }

		
		// // Wait for busybox pods to come up and test DNS resolution
		// if err := m.waitForBusyboxPodsAndTestDNS(); err != nil {
		// 	return nil, err
		// }

		//deploy target pods
		if err := m.deployTargetPods("start"); err != nil {
			return nil, err
		}

		// if m.enableNetworkPolicy {
		// 	// deploy DNS CCNP to allow client pods DNS access
		// 	if err := m.deployDNSCCNP(); err != nil {
		// 			return nil, err
		// 	}

		// }

	}

	if m.enableNetworkPolicy {
		// deploy the network policy to allow traffic from client to target pods
		klog.Infof(m.npType)
		if err := m.deployNetworkPolicy(); err != nil {
			return nil, err
		}
	}

	// deploy the client pods
	if err := m.deployClientPods("start"); err != nil {
		return nil, err
	}

	// start envoy resource gatherer
	if m.resourceGatheringEnabled && !m.isRestart {
		if err := m.envoyResourceGather(); err != nil {
			return nil, err
		}
	}

	m.isRestart = true
	m.isRunning = true
	return nil, nil
}

func (m *ConnectivitySoakMeasurement) initialize(config *measurement.Config, phase string) error {
	// initialization
	m.k8sClient = config.ClusterFramework.GetClientSets().GetClient()
	m.framework = config.ClusterFramework
	m.restConfig = config.ClusterFramework.GetRestClient()

	// Initialize DNS testing channels
	m.dnsTestStopChan = make(chan struct{})

	namespaceList, err := m.k8sClient.CoreV1().Namespaces().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("phase: %s, %s: failed to list namespaces: %v", phase, m.String(), err)
	}

	// target namespaces are automanagered by the framework
	// capture all the target namespaces
	targetNamespacePrefix := m.framework.GetAutomanagedNamespacePrefix()
	if !m.isRestart {
		for _, ns := range namespaceList.Items {
			if strings.HasPrefix(ns.Name, targetNamespacePrefix) {
				m.targetNamespaces = append(m.targetNamespaces, ns.Name)
			}
		}

	}

	if len(m.targetNamespaces) == 0 {
		return fmt.Errorf("phase: start, %s: no target namespaces found, verify config", m.String())
	}

	// parse the config params
	if m.targetLabelKey, err = util.GetString(config.Params, "targetLabelKey"); err != nil {
		return fmt.Errorf("phase: start, %s: failed to get target label key: %v", m.String(), err)
	}

	if m.targetLabelVal, err = util.GetString(config.Params, "targetLabelValue"); err != nil {
		return fmt.Errorf("phase: start, %s: failed to get target label value: %v", m.String(), err)
	}

	if m.clientLabelKey, err = util.GetString(config.Params, "clientLabelKey"); err != nil {
		return fmt.Errorf("phase: start, %s: failed to get client label key: %v", m.String(), err)
	}

	if m.clientLabelVal, err = util.GetString(config.Params, "clientLabelValue"); err != nil {
		return fmt.Errorf("phase: start, %s: failed to get client label value: %v", m.String(), err)
	}

	if m.targetReplicasPerNs, err = util.GetIntOrDefault(config.Params, "targetReplicasPerNs", 1); err != nil {
		return fmt.Errorf("phase: start, %s: failed to get target replicas per namespace: %v", m.String(), err)
	}

	if m.clientReplicasPerDep, err = util.GetIntOrDefault(config.Params, "clientReplicasPerDep", 1); err != nil {
		return fmt.Errorf("phase: start, %s: failed to get client replicas per deployment: %v", m.String(), err)
	}

	if m.targetPort, err = util.GetIntOrDefault(config.Params, "targetPort", 80); err != nil {
		return fmt.Errorf("phase: start, %s: failed to get target port: %v", m.String(), err)
	}

	if m.targetPort2, err = util.GetIntOrDefault(config.Params, "targetPort2", 90); err != nil {
		return fmt.Errorf("phase: start, %s: failed to get target port 2: %v", m.String(), err)
	}

	if m.l7Enabled, err = util.GetBoolOrDefault(config.Params, "l7Enabled", false); err != nil {
		return fmt.Errorf("phase: start, %s: failed to get l7 enabled: %v", m.String(), err)
	}

	if m.l3l4port, err = util.GetBoolOrDefault(config.Params, "l3l4port", false); err != nil {
		return fmt.Errorf("phase: start, %s: failed to get l3l4port: %v", m.String(), err)
	}

	if m.enableNetworkPolicy, err = util.GetBoolOrDefault(config.Params, "enableNetworkPolicy", false); err != nil {
		return fmt.Errorf("phase: start, %s: failed to get enableNetworkPolicy: %v", m.String(), err)
	}

	if m.targetPath, err = util.GetStringOrDefault(config.Params, "targetPath", "/"); err != nil {
		return fmt.Errorf("phase: start, %s: failed to get target path: %v", m.String(), err)
	}

	if m.testDuration, err = util.GetDuration(config.Params, "testDuration"); err != nil {
		return fmt.Errorf("phase: start, %s: failed to get test duration: %v", m.String(), err)
	}

	if m.workerPerClient, err = util.GetIntOrDefault(config.Params, "workerPerClient", 1); err != nil {
		return fmt.Errorf("phase: start, %s: failed to get worker per client: %v", m.String(), err)
	}

	if m.npType, err = util.GetStringOrDefault(config.Params, "npType", "none"); err != nil {
		return fmt.Errorf("phase: start, %s: failed to get network policy type: %v", m.String(), err)
	}

	if m.resourceGatheringEnabled, err = util.GetBoolOrDefault(config.Params, "resourceGatheringEnabled", false); err != nil {
		return fmt.Errorf("phase: start, %s: failed to get resource gathering enabled: %v", m.String(), err)
	}

	return nil
}

func (m *ConnectivitySoakMeasurement) deployRBACResources() error {
	templateMap := map[string]interface{}{
		"Name":      clientName,
		"Namespace": clientNamespace,
	}

	// create the service account
	if err := m.framework.ApplyTemplatedManifests(manifestsFS, serviceAccountFilePath, templateMap); err != nil {
		return fmt.Errorf("phase: start, %s: failed to apply service account manifest: %v", m.String(), err)
	}

	// create the cluster role
	if err := m.framework.ApplyTemplatedManifests(manifestsFS, clusterRoleFilePath, templateMap); err != nil {
		return fmt.Errorf("phase: start, %s: failed to apply cluster role manifest: %v", m.String(), err)
	}

	// create the cluster role binding
	if err := m.framework.ApplyTemplatedManifests(manifestsFS, clusterRoleBindingFilePath, templateMap); err != nil {
		return fmt.Errorf("phase: start, %s: failed to apply cluster role binding manifest: %v", m.String(), err)
	}

	return nil
}

func (m *ConnectivitySoakMeasurement) deployNodeLocalDNSAndLRP() error {
	klog.Infof("Deploying NodeLocalDNS DaemonSet and LocalRedirectPolicy")

	// Deploy NodeLocalDNS DaemonSet
	templateMap := map[string]interface{}{
		// No template variables needed for the static DaemonSet
	}

	if err := m.framework.ApplyTemplatedManifests(manifestsFS, nodelocaldnsServiceAccFilePath, templateMap); err != nil {
		return fmt.Errorf("phase: start, %s: failed to apply NodeLocalDNS Service Account manifest: %v", m.String(), err)
	}

	if err := m.framework.ApplyTemplatedManifests(manifestsFS, nodelocaldnsServiceFilePath, templateMap); err != nil {
		return fmt.Errorf("phase: start, %s: failed to apply NodeLocalDNS Service  manifest: %v", m.String(), err)
	}

	if err := m.framework.ApplyTemplatedManifests(manifestsFS, nodelocaldnsCMFilePath, templateMap); err != nil {
		return fmt.Errorf("phase: start, %s: failed to apply NodeLocalDNS ConfigMap manifest: %v", m.String(), err)
	}

	if err := m.framework.ApplyTemplatedManifests(manifestsFS, nodelocaldnsDSFilePath, templateMap); err != nil {
		return fmt.Errorf("phase: start, %s: failed to apply NodeLocalDNS DaemonSet manifest: %v", m.String(), err)
	}

	if err := m.framework.ApplyTemplatedManifests(manifestsFS, nodelocaldnsCRFilePath, templateMap); err != nil {
		return fmt.Errorf("phase: start, %s: failed to apply NodeLocalDNS Clusterrole manifest: %v", m.String(), err)
	}

	if err := m.framework.ApplyTemplatedManifests(manifestsFS, nodelocaldnsCRBindingFilePath, templateMap); err != nil {
		return fmt.Errorf("phase: start, %s: failed to apply NodeLocalDNS Clusterrolebinding  manifest: %v", m.String(), err)
	}

	// Deploy LocalRedirectPolicy
	if err := m.framework.ApplyTemplatedManifests(manifestsFS, lrpFilePath, templateMap); err != nil {
		return fmt.Errorf("phase: start, %s: failed to apply LocalRedirectPolicy manifest: %v", m.String(), err)
	}


	klog.Infof("Successfully deployed NodeLocalDNS DaemonSet, LocalRedirectPolicy")
	return nil
}

func (m *ConnectivitySoakMeasurement) deployDNSCCNP() error {
	klog.Infof("Deploying DNS CCNP for client DNS access")

	templateMap := map[string]interface{}{
		"ClientLabelKey":   m.clientLabelKey,
		"ClientLabelValue": m.clientLabelVal,
	}

	if err := m.framework.ApplyTemplatedManifests(manifestsFS, dnsccnpFilePath, templateMap); err != nil {
		return fmt.Errorf("phase: start, %s: failed to apply DNS CCNP manifest: %v", m.String(), err)
	}

	klog.Infof("Successfully deployed DNS CCNP")
	return nil
}

func (m *ConnectivitySoakMeasurement) deployTargetPods(phase string) error {
	// Validate that the replica count is positive
	if m.targetReplicasPerNs <= 0 {
		return fmt.Errorf("phase: %s, %s: invalid target replicas per namespace: %d", phase, m.String(), m.targetReplicasPerNs)
	}

	depBatchSize := 50
	for i := 0; i < len(m.targetNamespaces); i += depBatchSize {
		end := i + depBatchSize
		if end > len(m.targetNamespaces) {
			end = len(m.targetNamespaces)
		}
		// Create a new template map per batch to avoid reuse issues.
		batchTemplateMap := map[string]interface{}{
			"TargetName":       targetName,
			"TargetLabelKey":   m.targetLabelKey,
			"TargetLabelValue": m.targetLabelVal,
			"Replicas":         m.targetReplicasPerNs,
			"TargetPort":       m.targetPort,
			"TargetPort2":      m.targetPort2,
			"DeploymentLabel":  phase,
			// generate unique key and value for each deployment batch
			// this will be used to wait for the pods to be ready by matching the label selector
			"TargetDeploymentLabelKey":   fmt.Sprintf("%s-%d", m.targetLabelKey, i),
			"TargetDeploymentLabelValue": fmt.Sprintf("%s-%d", m.targetLabelVal, i),
		}
		for _, ns := range m.targetNamespaces[i:end] {
			batchTemplateMap["TargetNamespace"] = ns
			if phase == "start" {
				// Deploy target deployment
				if err := m.framework.ApplyTemplatedManifests(manifestsFS, targetFilePath, batchTemplateMap); err != nil {
					return fmt.Errorf("phase: %s, %s NS: %s, failed to apply target deployment manifest: %v", phase, m.String(), ns, err)
				}

				// Deploy target service for DNS resolution
				if err := m.framework.ApplyTemplatedManifests(manifestsFS, targetServiceFilePath, batchTemplateMap); err != nil {
					return fmt.Errorf("phase: %s, %s NS: %s, failed to apply target service manifest: %v", phase, m.String(), ns, err)
				}

			} else {
				// Update target deployment
				if err := m.framework.UpdateTemplatedManifests(manifestsFS, targetFilePath, batchTemplateMap); err != nil {
					return fmt.Errorf("phase: %s, %s NS: %s, failed to update target deployment manifest: %v", phase, m.String(), ns, err)
				}

				// Update target service
				if err := m.framework.UpdateTemplatedManifests(manifestsFS, targetServiceFilePath, batchTemplateMap); err != nil {
					return fmt.Errorf("phase: %s, %s NS: %s, failed to update target service manifest: %v", phase, m.String(), ns, err)
				}

			}
		}
		// Wait for the current batch deployments to be ready.
		labelSelector := fmt.Sprintf("%s=%s", batchTemplateMap["TargetDeploymentLabelKey"], batchTemplateMap["TargetDeploymentLabelValue"])
		desiredBatchPodCount := (end - i) * m.targetReplicasPerNs
		waitSeconds := math.Max(60.0, float64(desiredBatchPodCount)*0.5)
		targetWaitCtx, targetWaitCancel := context.WithTimeout(context.TODO(), time.Duration(waitSeconds)*time.Second)
		if err := m.waitForDeploymentPodsReady(targetWaitCtx, desiredBatchPodCount, labelSelector); err != nil {
			klog.Warningf("phase: %s, %s: failed to wait for target pods to be ready: %v", phase, m.String(), err)
		}
		targetWaitCancel() // Explicitly cancel the context immediately after waiting.
	}

	return nil
}

func (m *ConnectivitySoakMeasurement) deployAPIServerNetworkPolicy() error {

	if !m.enableNetworkPolicy {
		return nil
	}

	if policy, err := m.k8sClient.NetworkingV1().NetworkPolicies(clientNamespace).Get(context.TODO(), apiserverNPName, metav1.GetOptions{}); err == nil && policy != nil {
		// network policy already exists
		klog.Warningf("Network policy %s already exists, skipping deployment", apiserverNPName)
		return nil
	}

	// get the API server IP address
	var kubeAPIServerIP string
	if endpoints, err := m.k8sClient.CoreV1().Endpoints(api_corev1.NamespaceDefault).Get(context.TODO(), "kubernetes", metav1.GetOptions{}); err != nil {
		return fmt.Errorf("phase: start, %s: failed to get API server endpoint: %v", m.String(), err)
	} else {
		if len(endpoints.Subsets) == 0 || len(endpoints.Subsets[0].Addresses) == 0 {
			return fmt.Errorf("phase: start, %s: failed to get API server IP address", m.String())
		}
		kubeAPIServerIP = endpoints.Subsets[0].Addresses[0].IP
	}

	templateMap := map[string]interface{}{
		"Name":             apiserverNPName,
		"Namespace":        clientNamespace,
		"ClientLabelKey":   m.clientLabelKey,
		"ClientLabelValue": m.clientLabelVal,
		"KubeAPIServerIP":  kubeAPIServerIP,
	}

	if err := m.framework.ApplyTemplatedManifests(manifestsFS, APIserverFilePath, templateMap); err != nil {
		return fmt.Errorf("phase: start, %s: failed to apply API server network policy manifest: %v", m.String(), err)
	}
	return nil
}

func (m *ConnectivitySoakMeasurement) deployNetworkPolicy() error {

	if !m.enableNetworkPolicy {
		return nil
	}

	klog.Infof("l7Enabled: %v", m.l7Enabled)

	templateMap := map[string]interface{}{
		"ClientNamespace":    clientNamespace,
		"ClientLabelKey":     m.clientLabelKey,
		"ClientLabelValue":   m.clientLabelVal,
		"TargetLabelKey":     m.targetLabelKey,
		"TargetLabelValue":   m.targetLabelVal,
		"TargetPort":         strconv.Itoa(m.targetPort),
		"L7Enabled":          m.l7Enabled,
		"L3L4Port":           m.l3l4port,
		"TargetPath":         m.targetPath,
		"NetworkPolicy_Type": m.npType,
	}
	for _, ns := range m.targetNamespaces {
		templateMap["TargetNamespace"] = ns
		templateMap["Name"] = ns // use the target namespace name as the network policy name

		if err := m.framework.ApplyTemplatedManifests(manifestsFS, netPolFilePath, templateMap); err != nil {
			return fmt.Errorf("phase: start, %s NS: %s, failed to apply network policy manifest: %v", m.String(), err, ns)
		}
	}
	return nil
}

func (m *ConnectivitySoakMeasurement) deployClientPods(phase string) error {
	// Usually server/target pods replicas are not large, so they should be up and running in a short time
	klog.Infof("Deploying client pods")

	// convert the test duration to seconds
	duration := int(m.testDuration.Seconds())
	clientBatchSize := 50
	for i := 0; i < len(m.targetNamespaces); i += clientBatchSize {
		end := i + clientBatchSize
		if end > len(m.targetNamespaces) {
			end = len(m.targetNamespaces)
		}
		// Create a new template map per batch to avoid reuse issues.
		batchTemplateMap := map[string]interface{}{
			"ClientName":       clientName,
			"ClientNamespace":  clientNamespace,
			"ClientLabelKey":   m.clientLabelKey,
			"ClientLabelValue": m.clientLabelVal,
			"TargetLabelKey":   m.targetLabelKey,
			"TargetLabelValue": m.targetLabelVal,
			"TargetPort":       m.targetPort,
			"TargetPort2":      m.targetPort2,
			"DeploymentLabel":  phase,
			"TargetPath":       m.targetPath,
			"Duration":         duration,
			"Replicas":         m.clientReplicasPerDep,
			"Workers":          m.workerPerClient,
			// generate unique key and value for each deployment batch
			// this will be used to wait for the pods to be ready by matching the label selector
			"ClientDeploymentLabelKey":   fmt.Sprintf("%s-%d", m.clientLabelKey, i),
			"ClientDeploymentLabelValue": fmt.Sprintf("%s-%d", m.clientLabelVal, i),
		}
		for _, ns := range m.targetNamespaces[i:end] {
			batchTemplateMap["TargetNamespace"] = ns
			batchTemplateMap["UniqueName"] = ns // use the target namespace name as the deployment name
			if phase == "start" {
				if err := m.framework.ApplyTemplatedManifests(manifestsFS, clientFilePath, batchTemplateMap); err != nil {
					return fmt.Errorf("phase: %s, %s NS: %s, failed to apply client deployment manifest: %v", phase, m.String(), ns, err)
				}
			} else {
				if err := m.framework.UpdateTemplatedManifests(manifestsFS, clientFilePath, batchTemplateMap); err != nil {
					return fmt.Errorf("phase: %s, %s NS: %s, failed to apply updated client deployment manifest: %v", phase, m.String(), ns, err)
				}
			}
		}

		// Wait for the current batch client pods to be ready.
		labelSelector := fmt.Sprintf("%s=%s", batchTemplateMap["ClientDeploymentLabelKey"], batchTemplateMap["ClientDeploymentLabelValue"])
		desiredBatchPodCount := (end - i) * m.clientReplicasPerDep
		waitDuration := math.Max(60.0, float64(desiredBatchPodCount)*0.5)
		clientWaitCtx, clientWaitCancel := context.WithTimeout(context.TODO(), time.Duration(waitDuration)*time.Second)
		if err := m.waitForDeploymentPodsReady(clientWaitCtx, desiredBatchPodCount, labelSelector); err != nil {
			klog.Warningf("phase: %s, %s: failed to wait for client pods to be ready: %v", phase, m.String(), err)
		}
		clientWaitCancel() // cancel context immediately after waiting
	}

	m.testEndTime = time.Now().Add(m.testDuration)
	return nil
}

// Wait for the deployment pods be to be ready
func (m *ConnectivitySoakMeasurement) waitForDeploymentPodsReady(ctx context.Context, desiredPodCount int, labelSelector string) error {
	// get the selector for the pods
	selector := util.NewObjectSelector()
	if labelSelector == "" {
		return fmt.Errorf("label selector is empty")
	}
	selector.LabelSelector = labelSelector

	options := &measurementutil.WaitForPodOptions{
		DesiredPodCount:     func() int { return desiredPodCount },
		CallerName:          m.String(),
		WaitForPodsInterval: 2 * time.Second,
	}

	podStore, err := measurementutil.NewPodStore(m.k8sClient, selector)
	if err != nil {
		return err
	}

	_, err = measurementutil.WaitForPods(ctx, podStore, options)
	if err != nil {
		return err
	}

	return nil
}

// waitForBusyboxPodsAndTestDNS waits for busybox DaemonSet pods to be ready and starts continuous DNS testing
func (m *ConnectivitySoakMeasurement) waitForBusyboxPodsAndTestDNS() error {
	klog.Infof("Waiting for busybox DaemonSet pods to be ready...")

	// Wait for DaemonSet to be ready
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
	defer cancel()

	daemonSetName := "busybox-daemonset"
	namespace := "kube-system"

	// Wait for DaemonSet to have all desired pods ready
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for busybox DaemonSet to be ready")
		default:
			ds, err := m.k8sClient.AppsV1().DaemonSets(namespace).Get(context.TODO(), daemonSetName, metav1.GetOptions{})
			if err != nil {
				klog.Warningf("Failed to get busybox DaemonSet: %v", err)
				time.Sleep(5 * time.Second)
				continue
			}

			klog.V(3).Infof("DaemonSet status: NumberReady=%d, DesiredNumberScheduled=%d", ds.Status.NumberReady, ds.Status.DesiredNumberScheduled)

			if ds.Status.NumberReady == ds.Status.DesiredNumberScheduled && ds.Status.NumberReady > 0 {
				klog.Infof("Busybox DaemonSet is ready with %d pods", ds.Status.NumberReady)
				goto daemonSetReady
			}

			klog.Infof("Waiting for busybox DaemonSet - Ready: %d/%d", ds.Status.NumberReady, ds.Status.DesiredNumberScheduled)
			time.Sleep(5 * time.Second)
		}
	}

daemonSetReady:

	// Start continuous DNS testing in a separate goroutine
	klog.Infof("Starting continuous DNS testing on all busybox pods...")
	m.dnsTestWg.Add(1)
	go m.continuousDNSTest(namespace)

	return nil
}

// continuousDNSTest runs DNS resolution tests continuously until stopped
func (m *ConnectivitySoakMeasurement) continuousDNSTest(namespace string) {
	defer m.dnsTestWg.Done()

	labelSelector := "app=busybox"
	testInterval := 30 * time.Second // Run DNS test every 30 seconds

	klog.Infof("Starting continuous DNS testing loop...")

	for {
		select {
		case <-m.dnsTestStopChan:
			klog.Infof("Stopping continuous DNS testing...")
			return
		default:
			// Get current busybox pods
			podList, err := m.k8sClient.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{
				LabelSelector: labelSelector,
			})
			if err != nil {
				klog.Warningf("Failed to list busybox pods: %v", err)
				time.Sleep(testInterval)
				continue
			}

			if len(podList.Items) == 0 {
				klog.Infof("No busybox pods found, DNS testing loop ending...")
				return
			}

			// Test DNS resolution on each running pod
			successCount := 0
			for _, pod := range podList.Items {
				if pod.Status.Phase != api_corev1.PodRunning {
					continue
				}

				// Check if all containers are ready
				allReady := true
				for _, containerStatus := range pod.Status.ContainerStatuses {
					if !containerStatus.Ready {
						allReady = false
						break
					}
				}
				if !allReady {
					continue
				}

				// Perform DNS test
				if err := m.execDNSTestOnPod(pod.Name, namespace); err != nil {
					klog.V(4).Infof("DNS test failed on pod %s: %v", pod.Name, err)
				} else {
					successCount++
					klog.V(4).Infof("DNS test successful on pod %s", pod.Name)
				}
			}

			if successCount > 0 {
				klog.V(2).Infof("DNS resolution test successful on %d/%d running busybox pods", successCount, len(podList.Items))
			} else {
				klog.Warningf("DNS resolution failed on all %d busybox pods", len(podList.Items))
			}

			// Wait before next test cycle
			time.Sleep(testInterval)
		}
	}
}

// execDNSTestOnPod performs actual nslookup command on a specific pod using Kubernetes exec API
func (m *ConnectivitySoakMeasurement) execDNSTestOnPod(podName, namespace string) error {
	cmd := []string{"nslookup", "www.google.com"}

	// Create the exec request
	req := m.k8sClient.CoreV1().RESTClient().
		Post().
		Namespace(namespace).
		Resource("pods").
		Name(podName).
		SubResource("exec").
		VersionedParams(&api_corev1.PodExecOptions{
			Container: "busybox", // busybox container name
			Command:   cmd,
			Stdin:     false,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec)

	// Create the executor
	executor, err := remotecommand.NewSPDYExecutor(m.restConfig, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("failed to create executor: %v", err)
	}

	// Capture stdout and stderr
	var stdoutBuf, stderrBuf bytes.Buffer

	// Execute the command with a timeout
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	execDone := make(chan error, 1)
	go func() {
		err := executor.Stream(remotecommand.StreamOptions{
			Stdout: &stdoutBuf,
			Stderr: &stderrBuf,
		})
		execDone <- err
	}()

	select {
	case err := <-execDone:
		if err != nil {
			// Check if it's a non-zero exit code
			if exitErr, ok := err.(exec.CodeExitError); ok {
				return fmt.Errorf("nslookup failed with exit code %d: stdout=%s stderr=%s",
					exitErr.ExitStatus(), stdoutBuf.String(), stderrBuf.String())
			}
			return fmt.Errorf("exec error: %v, stderr=%s", err, stderrBuf.String())
		}

		// Command succeeded
		klog.V(5).Infof("nslookup output for pod %s: %s", podName, stdoutBuf.String())
		return nil

	case <-ctx.Done():
		return fmt.Errorf("nslookup command timed out after 10 seconds")
	}
}

func (m *ConnectivitySoakMeasurement) gather() ([]measurement.Summary, error) {
	if !m.isRunning {
		return nil, fmt.Errorf("phase: gather, %s: measurement not running", m.String())
	}

	// wait for the test to complete
	klog.Infof("phase: gather, %s: waiting for the test run to complete...", m.String())
	// Instead of: time.Sleep(time.Until(m.testEndTime))
	timer := time.NewTimer(time.Until(m.testEndTime))
	<-timer.C
	// Optionally, call timer.Stop() if needed.
	klog.Infof("phase: gather, %s: test run completed", m.String())

	// if resource gathering is not enabled, skip the gathering
	klog.Infof("rsource geathering, %v", m.resourceGatheringEnabled)
	if !m.resourceGatheringEnabled {
		klog.Infof("phase: gather, %s: resource gathering not enabled, skipping...", m.String())
		return nil, nil
	}

	// stop gathering resource usage
	if m.gatherers == nil {
		klog.Warningf("phase: gather, %s: gatherer not initialized. Envoy resource usage not collected", m.String())
	}

	// stop gathering resource usage
	summary, err := m.gatherers.StopAndSummarize([]int{50, 90, 99, 100})
	if err != nil {
		return nil, fmt.Errorf("phase: gather, %s: failed to stop gathering resource usage: %v", m.String(), err)
	}

	content, err := util.PrettyPrintJSON(summary)
	if err != nil {
		return nil, fmt.Errorf("phase: gather, %s: failed to pretty print resource usage summary: %v", m.String(), err)
	}

	resourceSummary := measurement.CreateSummary(connectivitySoakMeasurementName, "json", content)
	return []measurement.Summary{resourceSummary}, nil
}

func (m *ConnectivitySoakMeasurement) envoyResourceGather() error {
	if m.gatherers != nil {
		return fmt.Errorf("phase: gather, %s: resource gatherer already initialized, not expected", m.String())
	}

	// api server IP address
	host := m.framework.GetClusterConfig().GetMasterIP()

	// namespace
	namespace := "kube-system"

	// label selector
	labelSelector := "name=cilium-envoy"

	// resource gatherer options
	options := gatherers.ResourceGathererOptions{
		InKubemark:                        m.framework.GetClusterConfig().Provider.Features().IsKubemarkProvider,
		ResourceDataGatheringPeriod:       120 * time.Second,
		MasterResourceDataGatheringPeriod: 120 * time.Second,
		Nodes:                             gatherers.AllNodes,
	}

	gatherers, err := gatherers.NewResourceUsageGatherer(m.k8sClient,
		host,
		m.framework.GetClusterConfig().KubeletPort,
		m.framework.GetClusterConfig().Provider,
		options,
		namespace,
		labelSelector)
	if err != nil {
		return fmt.Errorf("phase: gather, %s: failed to create resource gatherer: %v", m.String(), err)
	}
	m.gatherers = gatherers

	// start gathering resource usage
	go m.gatherers.StartGatheringData()

	return nil
}

func (nps *ConnectivitySoakMeasurement) deleteK8sNPs() ([]measurement.Summary, error) {
	dynamicClient := nps.framework.GetDynamicClients().GetClient()

	k8sGVR := schema.GroupVersionResource{
		Group:    "networking.k8s.io",
		Version:  "v1",
		Resource: "networkpolicies",
	}

	// List all NetworkPolicies in all namespaces
	npsList, err := dynamicClient.Resource(k8sGVR).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		klog.Errorf("failed to list NetworkPolicies: %v", err)
		return nil, err
	}

	// Define a set of NetworkPolicy names you want to keep
	keepNames := map[string]bool{
		"konnectivity-agent":     true,
		"allow-egress-apiserver": true,
		// Add more names as needed
	}

	for _, item := range npsList.Items {
		name := item.GetName()
		namespace := item.GetNamespace()
		if keepNames[name] {
			klog.Infof("Skipping NetworkPolicy %s/%s", namespace, name)
			continue
		}
		// Delete the NetworkPolicy
		err := dynamicClient.Resource(k8sGVR).Namespace(namespace).Delete(context.TODO(), name, metav1.DeleteOptions{})
		if err != nil {
			klog.Errorf("failed to delete NetworkPolicy %s/%s: %v", namespace, name, err)
		} else {
			klog.Infof("Deleted NetworkPolicy %s/%s", namespace, name)
		}
	}
	return nil, nil
}

func (nps *ConnectivitySoakMeasurement) deleteNetworkPolicies() ([]measurement.Summary, error) {

	if !nps.enableNetworkPolicy {
		return nil, nil
	}

	dynamicClient := nps.framework.GetDynamicClients().GetClient()

	switch nps.npType {
	case "k8s", "none":
		return nil, nil
	case "ccnp":
		// Define the GVR for CiliumClusterwideNetworkPolicy
		ccnpGVR := schema.GroupVersionResource{
			Group:    "cilium.io",
			Version:  "v2",
			Resource: "ciliumclusterwidenetworkpolicies",
		}

		if err := dynamicClient.Resource(ccnpGVR).DeleteCollection(context.TODO(), metav1.DeleteOptions{}, metav1.ListOptions{}); err != nil {
			klog.Errorf("failed to delete CiliumClusterwideNetworkPolicy, error: %v", err)
		}

		// Wait for CCNPs to be fully deleted
		klog.Info("Waiting for CiliumClusterwideNetworkPolicies to be fully deleted...")
		if err := nps.waitForNetworkPoliciesDeleted(dynamicClient, ccnpGVR, ""); err != nil {
			klog.Errorf("failed to wait for CiliumClusterwideNetworkPolicies to be deleted: %v", err)
			return nil, err
		}

	case "cnp":
		// Define the GVR for CiliumNetworkPolicy
		cnpGVR := schema.GroupVersionResource{
			Group:    "cilium.io",
			Version:  "v2",
			Resource: "ciliumnetworkpolicies",
		}

		if err := dynamicClient.Resource(cnpGVR).Namespace(clientNamespace).DeleteCollection(context.TODO(), metav1.DeleteOptions{}, metav1.ListOptions{}); err != nil {
			klog.Errorf("failed to delete CiliumNetworkPolicy in ns:%s, error: %v", clientNamespace, err)
		}

		// Wait for CNPs to be fully deleted
		klog.Info("Waiting for CiliumNetworkPolicies to be fully deleted...")
		if err := nps.waitForNetworkPoliciesDeleted(dynamicClient, cnpGVR, clientNamespace); err != nil {
			klog.Errorf("failed to wait for CiliumNetworkPolicies to be deleted: %v", err)
			return nil, err
		}
	}
	return nil, nil
}

func (nps *ConnectivitySoakMeasurement) waitForNetworkPoliciesDeleted(dynamicClient dynamic.Interface, gvr schema.GroupVersionResource, namespace string) error {
	if !nps.enableNetworkPolicy {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute) // Adjust timeout as needed
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for network policies to be deleted")
		default:
			var list *unstructured.UnstructuredList
			var err error

			if namespace == "" {
				list, err = dynamicClient.Resource(gvr).List(context.TODO(), metav1.ListOptions{})
			} else {
				list, err = dynamicClient.Resource(gvr).Namespace(namespace).List(context.TODO(), metav1.ListOptions{})
			}

			if err != nil {
				return fmt.Errorf("failed to list network policies: %v", err)
			}

			if len(list.Items) == 0 {
				klog.Infof("All network policies of type %s have been deleted", gvr.Resource)
				return nil
			}

			klog.Infof("Waiting for %d network policies of type %s to be deleted...", len(list.Items), gvr.Resource)
			time.Sleep(1 * time.Second) // Polling interval
		}
	}
}

func (m *ConnectivitySoakMeasurement) deletePods() ([]measurement.Summary, error) {

	// delete client pods
	if err := m.k8sClient.AppsV1().Deployments(clientNamespace).DeleteCollection(context.TODO(), metav1.DeleteOptions{}, metav1.ListOptions{}); err != nil {
		klog.Errorf("phase: delete-pods, %s: failed to delete client deployments: %v", m.String(), err)
	}

	// Wait for client pods to be fully deleted
	klog.Info("Waiting for client pods to be fully deleted...")
	err := m.waitForPodsDeleted(clientNamespace, m.clientLabelKey, m.clientLabelVal)
	if err != nil {
		klog.Errorf("phase: delete-pods, %s: failed to wait for client pods to be deleted: %v", m.String(), err)
		return nil, err
	}

	return nil, nil
}

func (m *ConnectivitySoakMeasurement) waitForPodsDeleted(namespace, labelKey, labelValue string) error {
	labelSelector := fmt.Sprintf("%s=%s", labelKey, labelValue)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute) // Adjust timeout as needed
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for pods in namespace %s with label %s=%s to be deleted", namespace, labelKey, labelValue)
		default:
			podList, err := m.k8sClient.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{LabelSelector: labelSelector})
			if err != nil {
				return fmt.Errorf("failed to list pods in namespace %s with label %s=%s: %v", namespace, labelKey, labelValue, err)
			}

			if len(podList.Items) == 0 {
				klog.Infof("All pods in namespace %s with label %s=%s have been deleted", namespace, labelKey, labelValue)
				return nil
			}

			klog.Infof("Waiting for %d pods in namespace %s with label %s=%s to be deleted...", len(podList.Items), namespace, labelKey, labelValue)
			time.Sleep(1 * time.Second) // Polling interval
		}
	}
}

func (m *ConnectivitySoakMeasurement) restart() ([]measurement.Summary, error) {

	time.Sleep(300 * time.Second) //5 minute wait so requests can occur

	// deploy the client pods
	if err := m.deployClientPods("restart"); err != nil {
		return nil, err
	}

	time.Sleep(300 * time.Second) //5 minute wait so requests can occur

	return nil, nil

}

func (m *ConnectivitySoakMeasurement) cleanupDNSInfrastructure() error {
	klog.Infof("Cleaning up DNS infrastructure (NodeLocalDNS, LocalRedirectPolicy, and DNS CCNP)")

	// // First stop the continuous DNS testing
	// klog.Infof("Stopping continuous DNS testing...")
	// select {
	// case <-m.dnsTestStopChan:
	// 	// Channel already closed
	// default:
	// 	close(m.dnsTestStopChan)
	// }
	// m.dnsTestWg.Wait()
	// klog.Infof("DNS testing stopped")

	dynamicClient := m.framework.GetDynamicClients().GetClient()

	// First delete NodeLocalDNS DaemonSet (remove cache pods)
	if err := m.k8sClient.AppsV1().DaemonSets("kube-system").Delete(context.TODO(), "node-local-dns", metav1.DeleteOptions{}); err != nil {
		klog.Errorf("failed to delete NodeLocalDNS DaemonSet: %v", err)
	} else {
		klog.Infof("Successfully deleted NodeLocalDNS DaemonSet")
	}

	// // Delete busybox DaemonSet
	// if err := m.k8sClient.AppsV1().DaemonSets("kube-system").Delete(context.TODO(), "busybox-daemonset", metav1.DeleteOptions{}); err != nil {
	// 	klog.Errorf("failed to delete busybox DaemonSet: %v", err)
	// } else {
	// 	klog.Infof("Successfully deleted busybox DaemonSet")
	// }

	// Then delete LocalRedirectPolicy (stop redirecting to non-existent pods)
	lrpGVR := schema.GroupVersionResource{
		Group:    "cilium.io",
		Version:  "v2",
		Resource: "ciliumlocalredirectpolicies",
	}

	if err := dynamicClient.Resource(lrpGVR).Namespace("kube-system").Delete(context.TODO(), "nodelocaldns-cache-redirect", metav1.DeleteOptions{}); err != nil {
		klog.Errorf("failed to delete LocalRedirectPolicy: %v", err)
	} else {
		klog.Infof("Successfully deleted LocalRedirectPolicy")
	}

	// Wait for LRP to be fully deleted
	if err := m.waitForResourcesDeleted(dynamicClient, lrpGVR, ""); err != nil {
		klog.Errorf("failed to wait for LocalRedirectPolicy to be deleted: %v", err)
	}

	// Clean up remaining NodeLocalDNS resources
	// Delete NodeLocalDNS ConfigMap
	if err := m.k8sClient.CoreV1().ConfigMaps("kube-system").Delete(context.TODO(), "node-local-dns", metav1.DeleteOptions{}); err != nil {
		klog.Errorf("failed to delete NodeLocalDNS ConfigMap: %v", err)
	}

	// Delete NodeLocalDNS ServiceAccount
	if err := m.k8sClient.CoreV1().ServiceAccounts("kube-system").Delete(context.TODO(), "node-local-dns", metav1.DeleteOptions{}); err != nil {
		klog.Errorf("failed to delete NodeLocalDNS ServiceAccount: %v", err)
	}

	// Delete NodeLocalDNS Service
	if err := m.k8sClient.CoreV1().Services("kube-system").Delete(context.TODO(), "kube-dns-upstream", metav1.DeleteOptions{}); err != nil {
		klog.Errorf("failed to delete NodeLocalDNS Service: %v", err)
	}

	// Delete NodeLocalDNS ClusterRole
	if err := m.k8sClient.RbacV1().ClusterRoles().Delete(context.TODO(), "system:node-local-dns", metav1.DeleteOptions{}); err != nil {
		klog.Errorf("failed to delete NodeLocalDNS ClusterRole: %v", err)
	}

	// Delete NodeLocalDNS ClusterRoleBinding
	if err := m.k8sClient.RbacV1().ClusterRoleBindings().Delete(context.TODO(), "system:node-local-dns", metav1.DeleteOptions{}); err != nil {
		klog.Errorf("failed to delete NodeLocalDNS ClusterRoleBinding: %v", err)
	}

	// Finally delete DNS CCNP (allow fallback to regular kube-dns)
	dnsccnpGVR := schema.GroupVersionResource{
		Group:    "cilium.io",
		Version:  "v2",
		Resource: "ciliumclusterwidenetworkpolicies",
	}

	// Delete the specific DNS CCNP by name if it exists
	if err := dynamicClient.Resource(dnsccnpGVR).Delete(context.TODO(), "allow-client-dns-access", metav1.DeleteOptions{}); err != nil {
		klog.Errorf("failed to delete DNS CCNP: %v", err)
	} else {
		klog.Infof("Successfully deleted DNS CCNP")
	}

	klog.Infof("DNS infrastructure cleanup completed")
	return nil
}

func (m *ConnectivitySoakMeasurement) waitForResourcesDeleted(dynamicClient dynamic.Interface, gvr schema.GroupVersionResource, namespace string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for resources of type %s to be deleted", gvr.Resource)
		default:
			var list *unstructured.UnstructuredList
			var err error

			if namespace == "" {
				list, err = dynamicClient.Resource(gvr).List(context.TODO(), metav1.ListOptions{})
			} else {
				list, err = dynamicClient.Resource(gvr).Namespace(namespace).List(context.TODO(), metav1.ListOptions{})
			}

			if err != nil {
				return fmt.Errorf("failed to list resources of type %s: %v", gvr.Resource, err)
			}

			if len(list.Items) == 0 {
				klog.Infof("All resources of type %s have been deleted", gvr.Resource)
				return nil
			}

			klog.Infof("Waiting for %d resources of type %s to be deleted...", len(list.Items), gvr.Resource)
			time.Sleep(1 * time.Second)
		}
	}
}

func (m *ConnectivitySoakMeasurement) Dispose() {
	// delete RBAC resources
	if err := m.k8sClient.RbacV1().ClusterRoleBindings().Delete(context.TODO(), fmt.Sprintf("%s-crb", clientName), metav1.DeleteOptions{}); err != nil {
		klog.Errorf("phase: gather, %s: failed to delete cluster role binding: %v", m.String(), err)
	}

	if err := m.k8sClient.RbacV1().ClusterRoles().Delete(context.TODO(), fmt.Sprintf("%s-cr", clientName), metav1.DeleteOptions{}); err != nil {
		klog.Errorf("phase: gather, %s: failed to delete cluster role: %v", m.String(), err)
	}

	if err := m.k8sClient.CoreV1().ServiceAccounts(clientNamespace).Delete(context.TODO(), fmt.Sprintf("%s-sa", clientName), metav1.DeleteOptions{}); err != nil {
		klog.Errorf("phase: gather, %s: failed to delete service account: %v", m.String(), err)
	}

	// delete client pods first (stop generating traffic)
	if err := m.k8sClient.AppsV1().Deployments(clientNamespace).DeleteCollection(context.TODO(), metav1.DeleteOptions{}, metav1.ListOptions{}); err != nil {
		klog.Errorf("phase: gather, %s: failed to delete client deployments: %v", m.String(), err)
	}

	// clear target deployments from all the target namespaces using label selector
	labelSelector := fmt.Sprintf("%s=%s", m.targetLabelKey, m.targetLabelVal)
	for _, ns := range m.targetNamespaces {
		if err := m.k8sClient.AppsV1().Deployments(ns).DeleteCollection(context.TODO(), metav1.DeleteOptions{}, metav1.ListOptions{LabelSelector: labelSelector}); err != nil {
			klog.Errorf("phase: gather, %s NS: %s, failed to delete target deployments: %v", m.String(), ns, err)
		}
		// add a delay to avoid API server throttling,
		// wait for 500ms before deleting the next deployment
		time.Sleep(500 * time.Millisecond)
	}

	// Clean up DNS infrastructure after pods are deleted (safe cleanup)
	if err := m.cleanupDNSInfrastructure(); err != nil {
		klog.Errorf("phase: dispose, %s: failed to cleanup DNS infrastructure: %v", m.String(), err)
	}

	if m.enableNetworkPolicy {
		//delete cnps & or ccnps
		m.deleteNetworkPolicies()
	}

	// delte client namespace
	if err := m.k8sClient.CoreV1().Namespaces().Delete(context.TODO(), clientNamespace, metav1.DeleteOptions{}); err != nil {
		klog.Errorf("phase: gather, %s: failed to delete namespace %s: %v", m.String(), clientNamespace, err)
	}

	// stop gatherers
	if m.gatherers != nil {
		m.gatherers.Dispose()
	}
}

func (m *ConnectivitySoakMeasurement) String() string {
	return connectivitySoakMeasurementName
}
