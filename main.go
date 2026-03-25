package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

var logger *zap.Logger

// WorkloadType represents the type of Kubernetes workload
type WorkloadType string

const (
	DeploymentType WorkloadType = "deployment"
	DaemonSetType  WorkloadType = "daemonset"
)

// IstioWorkload represents an Istio component and its workload type
type IstioWorkload struct {
	Name string
	Type WorkloadType
}

// istioWorkloads defines the Istio components to be managed
var istioWorkloads = []IstioWorkload{
	{Name: "istiod", Type: DeploymentType},
	{Name: "istio-ingressgateway", Type: DeploymentType},
	{Name: "istio-cni-node", Type: DaemonSetType},
}

var istioNamespace = "istio-system"

var (
	podGVR         = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
	namespaceGVR   = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"}
	deploymentGVR  = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	daemonsetGVR   = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "daemonsets"}
	statefulsetGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}
	replicasetGVR  = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}
	rolloutGVR     = schema.GroupVersionResource{Group: "argoproj.io", Version: "v1alpha1", Resource: "rollouts"}
)

// getKubeConfig creates a Kubernetes config, trying in-cluster first, then falling back to kubeconfig file
func getKubeConfig(kubeconfigPath string) (*rest.Config, error) {
	// If a specific kubeconfig path is provided, use it
	if kubeconfigPath != "" {
		logger.Info("Using kubeconfig file", zap.String("path", kubeconfigPath))
		return clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	}

	// Try in-cluster config first
	config, err := rest.InClusterConfig()
	if err == nil {
		logger.Info("Using in-cluster configuration")
		return config, nil
	}

	logger.Warn("In-cluster config not available, falling back to kubeconfig file", zap.Error(err))

	// Fall back to kubeconfig file
	var kubeconfig string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = filepath.Join(home, ".kube", "config")
	} else {
		return nil, fmt.Errorf("unable to determine home directory and in-cluster config failed")
	}

	// Check if kubeconfig file exists
	if _, err := os.Stat(kubeconfig); os.IsNotExist(err) {
		return nil, fmt.Errorf("kubeconfig file not found at %s and in-cluster config failed", kubeconfig)
	}

	logger.Info("Using kubeconfig file", zap.String("path", kubeconfig))
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}

func main() {
	// Initialize Zap logger with JSON output
	encoderConfig := zap.NewProductionEncoderConfig()
	encoderConfig.TimeKey = "timestamp"
	encoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	encoderConfig.MessageKey = "message"
	encoderConfig.EncodeLevel = zapcore.CapitalLevelEncoder

	core := zapcore.NewCore(
		zapcore.NewJSONEncoder(encoderConfig),
		zapcore.AddSync(os.Stdout),
		zapcore.InfoLevel,
	)
	logger = zap.New(core)
	defer logger.Sync()

	// Parse command line flags
	namespace := flag.String("namespace", "", "namespace to search pods in")
	allNamespaces := flag.Bool("all-namespaces", false, "search pods in all namespaces")
	kubeconfig := flag.String("kubeconfig", "", "path to kubeconfig file (optional, defaults to ~/.kube/config if not running in-cluster)")
	flag.Parse()

	// Validate namespace configuration
	if *namespace == "" && !*allNamespaces {
		logger.Error("Must specify either -namespace or -all-namespaces")
		os.Exit(1)
	}

	// Setup kubernetes client
	config, err := getKubeConfig(*kubeconfig)
	if err != nil {
		logger.Error("Failed to get kubernetes config", zap.Error(err))
		os.Exit(1)
	}

	// Suppress all Kubernetes API server deprecation warnings
	rest.SetDefaultWarningHandler(rest.NoWarnings{})
	config.WarningHandler = rest.NoWarnings{}

	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		logger.Error("Failed to create dynamic client", zap.Error(err))
		os.Exit(1)
	}

	ctx := context.Background()

	// Handle Istio workload restarts first
	logger.Info("Restarting Istio workloads...")
	for _, workload := range istioWorkloads {
		if err := restartIstioWorkload(ctx, dynClient, workload); err != nil {
			logger.Error("Failed to restart Istio workload",
				zap.String("workload_type", string(workload.Type)),
				zap.String("workload_name", workload.Name),
				zap.Error(err))
		}
	}

	// Get list of namespaces to process
	namespaces, err := getNamespaces(ctx, dynClient, *namespace, *allNamespaces)
	if err != nil {
		logger.Error("Failed to get namespaces", zap.Error(err))
		os.Exit(1)
	}

	logger.Info("Processing namespaces for Istio sidecar restarts", zap.Int("count", len(namespaces)))

	// Process pods in each namespace
	processedPods := 0
	for _, ns := range namespaces {
		count, err := processPodsInNamespace(ctx, dynClient, ns)
		if err != nil {
			logger.Error("Error processing pods in namespace", zap.String("namespace", ns), zap.Error(err))
			continue
		}
		processedPods += count
	}

	logger.Info("Successfully processed pods with Istio sidecars", zap.Int("count", processedPods))
}

// restartIstioWorkload handles the restart of a specific Istio workload
func restartIstioWorkload(ctx context.Context, dynClient dynamic.Interface, workload IstioWorkload) error {
	switch workload.Type {
	case DeploymentType:
		return restartWorkload(ctx, dynClient, istioNamespace, workload.Name, "Deployment")
	case DaemonSetType:
		return restartWorkload(ctx, dynClient, istioNamespace, workload.Name, "DaemonSet")
	default:
		return fmt.Errorf("unsupported workload type: %s", workload.Type)
	}
}

// getNamespaces returns the list of namespaces to process
func getNamespaces(ctx context.Context, dynClient dynamic.Interface, namespace string, allNamespaces bool) ([]string, error) {
	if allNamespaces {
		nsList, err := dynClient.Resource(namespaceGVR).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to list namespaces: %v", err)
		}

		namespaces := make([]string, 0, len(nsList.Items))
		for _, ns := range nsList.Items {
			name := ns.GetName()
			// Skip system namespaces that typically don't have user workloads
			if name == "kube-system" || name == "kube-public" || name == "kube-node-lease" {
				continue
			}
			namespaces = append(namespaces, name)
		}
		return namespaces, nil
	}
	return []string{namespace}, nil
}

// hasIstioSidecar checks if a pod has Istio sidecar injection
func hasIstioSidecar(pod *unstructured.Unstructured) bool {
	initContainers, found, err := unstructured.NestedSlice(pod.Object, "spec", "initContainers")
	if !found || err != nil {
		return false
	}
	for _, c := range initContainers {
		container, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(container, "name")
		if name == "istio-init" || name == "istio-validation" {
			return true
		}
	}
	return false
}

// processPodsInNamespace processes all pods in a given namespace and returns count of processed pods
func processPodsInNamespace(ctx context.Context, dynClient dynamic.Interface, namespace string) (int, error) {
	pods, err := dynClient.Resource(podGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, fmt.Errorf("failed to list pods in namespace %s: %v", namespace, err)
	}

	processedCount := 0
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !hasIstioSidecar(pod) {
			continue // Skip pods without Istio sidecar
		}

		if err := processPod(ctx, dynClient, pod); err != nil {
			logger.Error("Error processing pod",
				zap.String("namespace", namespace),
				zap.String("pod", pod.GetName()),
				zap.Error(err))
			continue
		}
		processedCount++
	}

	if processedCount > 0 {
		logger.Info("Processed pods with Istio sidecars",
			zap.String("namespace", namespace),
			zap.Int("count", processedCount))
	}

	return processedCount, nil
}

func processPod(ctx context.Context, dynClient dynamic.Interface, pod *unstructured.Unstructured) error {
	logger.Info("Processing pod with Istio sidecar",
		zap.String("namespace", pod.GetNamespace()),
		zap.String("pod", pod.GetName()))

	ownerRefs := pod.GetOwnerReferences()
	if len(ownerRefs) == 0 {
		logger.Info("Skipping pod: no owner references",
			zap.String("namespace", pod.GetNamespace()),
			zap.String("pod", pod.GetName()))
		return nil
	}

	return traverseOwners(ctx, dynClient, pod.GetNamespace(), ownerRefs[0])
}

func traverseOwners(ctx context.Context, dynClient dynamic.Interface, namespace string, ownerRef metav1.OwnerReference) error {
	switch ownerRef.Kind {
	case "ReplicaSet":
		rs, err := dynClient.Resource(replicasetGVR).Namespace(namespace).Get(ctx, ownerRef.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get ReplicaSet %s: %v", ownerRef.Name, err)
		}

		owners := rs.GetOwnerReferences()
		if len(owners) > 0 {
			return traverseOwners(ctx, dynClient, namespace, owners[0])
		}
		logger.Info("ReplicaSet has no owner references",
			zap.String("namespace", namespace),
			zap.String("replicaset", ownerRef.Name))

	case "Deployment":
		return restartWorkload(ctx, dynClient, namespace, ownerRef.Name, "Deployment")

	case "DaemonSet":
		return restartWorkload(ctx, dynClient, namespace, ownerRef.Name, "DaemonSet")

	case "StatefulSet":
		return restartWorkload(ctx, dynClient, namespace, ownerRef.Name, "StatefulSet")

	case "Rollout":
		return restartRollout(ctx, dynClient, namespace, ownerRef.Name)

	default:
		logger.Warn("Unsupported owner kind",
			zap.String("kind", ownerRef.Kind),
			zap.String("namespace", namespace),
			zap.String("owner", ownerRef.Name))
	}

	return nil
}

// restartWorkload handles restarting different types of workloads
func restartWorkload(ctx context.Context, dynClient dynamic.Interface, namespace, name, kind string) error {
	var gvr schema.GroupVersionResource
	switch kind {
	case "Deployment":
		gvr = deploymentGVR
	case "DaemonSet":
		gvr = daemonsetGVR
	case "StatefulSet":
		gvr = statefulsetGVR
	default:
		return fmt.Errorf("unsupported workload kind: %s", kind)
	}

	obj, err := dynClient.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get %s %s: %v", kind, name, err)
	}

	annotations, _, _ := unstructured.NestedStringMap(obj.Object, "spec", "template", "metadata", "annotations")
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations["kubectl.kubernetes.io/restartedAt"] = time.Now().Format(time.RFC3339)

	if err := unstructured.SetNestedStringMap(obj.Object, annotations, "spec", "template", "metadata", "annotations"); err != nil {
		return fmt.Errorf("failed to set annotations on %s %s: %v", kind, name, err)
	}

	_, err = dynClient.Resource(gvr).Namespace(namespace).Update(ctx, obj, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update %s %s: %v", kind, name, err)
	}

	logger.Info("Successfully restarted workload",
		zap.String("kind", kind),
		zap.String("name", name),
		zap.String("namespace", namespace))
	return nil
}

// restartRollout restarts an Argo Rollout by patching spec.restartAt
func restartRollout(ctx context.Context, dynClient dynamic.Interface, namespace, name string) error {
	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"restartAt": time.Now().Format(time.RFC3339),
		},
	}

	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("failed to marshal patch for Rollout %s: %v", name, err)
	}

	_, err = dynClient.Resource(rolloutGVR).Namespace(namespace).Patch(ctx, name, types.MergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("failed to patch Rollout %s: %v", name, err)
	}

	logger.Info("Successfully restarted Rollout",
		zap.String("name", name),
		zap.String("namespace", namespace))
	return nil
}
