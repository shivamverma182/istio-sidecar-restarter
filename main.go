package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

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
		log.Printf("Using kubeconfig file: %s", kubeconfigPath)
		return clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	}

	// Try in-cluster config first
	config, err := rest.InClusterConfig()
	if err == nil {
		log.Println("Using in-cluster configuration")
		return config, nil
	}

	log.Printf("In-cluster config not available: %v", err)
	log.Println("Falling back to kubeconfig file")

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

	log.Printf("Using kubeconfig file: %s", kubeconfig)
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}

func main() {
	// Parse command line flags
	namespace := flag.String("namespace", "", "namespace to search pods in")
	allNamespaces := flag.Bool("all-namespaces", false, "search pods in all namespaces")
	kubeconfig := flag.String("kubeconfig", "", "path to kubeconfig file (optional, defaults to ~/.kube/config if not running in-cluster)")
	flag.Parse()

	// Validate namespace configuration
	if *namespace == "" && !*allNamespaces {
		log.Fatalf("Must specify either -namespace or -all-namespaces")
	}

	// Setup kubernetes client
	config, err := getKubeConfig(*kubeconfig)
	if err != nil {
		log.Fatalf("Failed to get kubernetes config: %v", err)
	}

	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create dynamic client: %v", err)
	}

	ctx := context.Background()

	// Handle Istio workload restarts first
	log.Println("Restarting Istio workloads...")
	for _, workload := range istioWorkloads {
		if err := restartIstioWorkload(ctx, dynClient, workload); err != nil {
			log.Printf("Failed to restart %s %s: %v", workload.Type, workload.Name, err)
		}
	}

	// Get list of namespaces to process
	namespaces, err := getNamespaces(ctx, dynClient, *namespace, *allNamespaces)
	if err != nil {
		log.Fatalf("Failed to get namespaces: %v", err)
	}

	log.Printf("Processing %d namespaces for Istio sidecar restarts", len(namespaces))

	// Process pods in each namespace
	processedPods := 0
	for _, ns := range namespaces {
		count, err := processPodsInNamespace(ctx, dynClient, ns)
		if err != nil {
			log.Printf("Error processing pods in namespace %s: %v", ns, err)
			continue
		}
		processedPods += count
	}

	log.Printf("Successfully processed %d pods with Istio sidecars", processedPods)
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
			log.Printf("Error processing pod %s/%s: %v", namespace, pod.GetName(), err)
			continue
		}
		processedCount++
	}

	if processedCount > 0 {
		log.Printf("Processed %d pods with Istio sidecars in namespace %s", processedCount, namespace)
	}

	return processedCount, nil
}

func processPod(ctx context.Context, dynClient dynamic.Interface, pod *unstructured.Unstructured) error {
	log.Printf("Processing pod %s/%s with Istio sidecar", pod.GetNamespace(), pod.GetName())

	ownerRefs := pod.GetOwnerReferences()
	if len(ownerRefs) == 0 {
		log.Printf("Skipping pod %s/%s: no owner references", pod.GetNamespace(), pod.GetName())
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
		log.Printf("ReplicaSet %s/%s has no owner references", namespace, ownerRef.Name)

	case "Deployment":
		return restartWorkload(ctx, dynClient, namespace, ownerRef.Name, "Deployment")

	case "DaemonSet":
		return restartWorkload(ctx, dynClient, namespace, ownerRef.Name, "DaemonSet")

	case "StatefulSet":
		return restartWorkload(ctx, dynClient, namespace, ownerRef.Name, "StatefulSet")

	case "Rollout":
		return restartRollout(ctx, dynClient, namespace, ownerRef.Name)

	default:
		log.Printf("Unsupported owner kind: %s for %s/%s", ownerRef.Kind, namespace, ownerRef.Name)
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

	log.Printf("Successfully restarted %s %s in namespace %s", kind, name, namespace)
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

	log.Printf("Successfully restarted Rollout %s in namespace %s", name, namespace)
	return nil
}
