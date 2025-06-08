package soaktest

import (
	"context"
	"embed"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	api_corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
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

	// network policies
	netPolFilePath    = "manifests/network_policy.yaml"
	APIserverFilePath = "manifests/allow_apiserver_np.yaml"

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

		//deploy target pods
		if err := m.deployTargetPods("start"); err != nil {
			return nil, err
		}

		if m.enableNetworkPolicy {
			// deploy the network policy to allow traffic from client to API server only in start phase
			if err := m.deployAPIServerNetworkPolicy(); err != nil {
				return nil, err
			}
		}
	}

	if m.enableNetworkPolicy {
		// deploy the network policy to allow traffic from client to target pods
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
				if err := m.framework.ApplyTemplatedManifests(manifestsFS, targetFilePath, batchTemplateMap); err != nil {
					return fmt.Errorf("phase: %s, %s NS: %s, failed to apply target deployment manifest: %v", phase, m.String(), ns, err)
				}

			} else {
				if err := m.framework.UpdateTemplatedManifests(manifestsFS, targetFilePath, batchTemplateMap); err != nil {
					return fmt.Errorf("phase: %s, %s NS: %s, failed to update target deployment manifest: %v", phase, m.String(), ns, err)
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

	if m.enableNetworkPolicy {
		//delete cnps & or ccnps
		m.deleteNetworkPolicies()
	}

	// delete client pods
	if err := m.k8sClient.AppsV1().Deployments(clientNamespace).DeleteCollection(context.TODO(), metav1.DeleteOptions{}, metav1.ListOptions{}); err != nil {
		klog.Errorf("phase: gather, %s: failed to delete client deployments: %v", m.String(), err)
	}

	// delte client namespace
	if err := m.k8sClient.CoreV1().Namespaces().Delete(context.TODO(), clientNamespace, metav1.DeleteOptions{}); err != nil {
		klog.Errorf("phase: gather, %s: failed to delete namespace %s: %v", m.String(), clientNamespace, err)
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

	// stop gatherers
	if m.gatherers != nil {
		m.gatherers.Dispose()
	}
}

func (m *ConnectivitySoakMeasurement) String() string {
	return connectivitySoakMeasurementName
}
