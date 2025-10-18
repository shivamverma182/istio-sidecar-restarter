package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
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

func main() {
	// Parse command line flags
	namespace := flag.String("namespace", "", "namespace to search pods in")
	allNamespaces := flag.Bool("all-namespaces", false, "search pods in all namespaces")
	flag.Parse()

	// Validate namespace configuration
	if *namespace == "" && !*allNamespaces {
		log.Fatalf("Must specify either -namespace or -all-namespaces")
	}

	// Setup kubernetes client
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatalf("Failed to get in-cluster config: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("Failed to create kubernetes client: %v", err)
	}

	ctx := context.Background()

	// Handle Istio workload restarts first
	log.Println("Restarting Istio workloads...")
	for _, workload := range istioWorkloads {
		if err := restartIstioWorkload(ctx, clientset, workload); err != nil {
			log.Printf("Failed to restart %s %s: %v", workload.Type, workload.Name, err)
		}
	}

	// Get list of namespaces to process
	namespaces, err := getNamespaces(ctx, clientset, *namespace, *allNamespaces)
	if err != nil {
		log.Fatalf("Failed to get namespaces: %v", err)
	}

	log.Printf("Processing %d namespaces for Istio sidecar restarts", len(namespaces))

	// Process pods in each namespace
	processedPods := 0
	for _, ns := range namespaces {
		count, err := processPodsInNamespace(ctx, clientset, ns)
		if err != nil {
			log.Printf("Error processing pods in namespace %s: %v", ns, err)
			continue
		}
		processedPods += count
	}

	log.Printf("Successfully processed %d pods with Istio sidecars", processedPods)
}

// restartIstioWorkload handles the restart of a specific Istio workload
func restartIstioWorkload(ctx context.Context, clientset *kubernetes.Clientset, workload IstioWorkload) error {
	switch workload.Type {
	case DeploymentType:
		dep, err := clientset.AppsV1().Deployments(istioNamespace).Get(ctx, workload.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get deployment %s: %v", workload.Name, err)
		}

		dep.Spec.Template.Annotations = addRestartAnnotation(dep.Spec.Template.Annotations)
		_, err = clientset.AppsV1().Deployments(istioNamespace).Update(ctx, dep, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to update deployment %s: %v", workload.Name, err)
		}
		log.Printf("Successfully restarted Deployment %s in namespace %s", workload.Name, istioNamespace)

	case DaemonSetType:
		ds, err := clientset.AppsV1().DaemonSets(istioNamespace).Get(ctx, workload.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get daemonset %s: %v", workload.Name, err)
		}

		ds.Spec.Template.Annotations = addRestartAnnotation(ds.Spec.Template.Annotations)
		_, err = clientset.AppsV1().DaemonSets(istioNamespace).Update(ctx, ds, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to update daemonset %s: %v", workload.Name, err)
		}
		log.Printf("Successfully restarted DaemonSet %s in namespace %s", workload.Name, istioNamespace)

	default:
		return fmt.Errorf("unsupported workload type: %s", workload.Type)
	}

	return nil
}

// addRestartAnnotation adds or updates the restart annotation with current timestamp
func addRestartAnnotation(annotations map[string]string) map[string]string {
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations["kubectl.kubernetes.io/restartedAt"] = time.Now().Format(time.RFC3339)
	return annotations
}

// getNamespaces returns the list of namespaces to process
func getNamespaces(ctx context.Context, clientset *kubernetes.Clientset, namespace string, allNamespaces bool) ([]string, error) {
	if allNamespaces {
		namespaceList, err := clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to list namespaces: %v", err)
		}

		namespaces := make([]string, 0, len(namespaceList.Items))
		for _, ns := range namespaceList.Items {
			// Skip system namespaces that typically don't have user workloads
			if ns.Name == "kube-system" || ns.Name == "kube-public" || ns.Name == "kube-node-lease" {
				continue
			}
			namespaces = append(namespaces, ns.Name)
		}
		return namespaces, nil
	}
	return []string{namespace}, nil
}

// hasIstioSidecar checks if a pod has Istio sidecar injection
func hasIstioSidecar(pod *corev1.Pod) bool {
	// Check for istio-init init container
	for _, container := range pod.Spec.InitContainers {
		if container.Name == "istio-init" || container.Name == "istio-validation" {
			return true
		}
	}

	return false
}

// processPodsInNamespace processes all pods in a given namespace and returns count of processed pods
func processPodsInNamespace(ctx context.Context, clientset *kubernetes.Clientset, namespace string) (int, error) {
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, fmt.Errorf("failed to list pods in namespace %s: %v", namespace, err)
	}

	processedCount := 0
	for _, pod := range pods.Items {
		if !hasIstioSidecar(&pod) {
			continue // Skip pods without Istio sidecar
		}

		if err := processPod(ctx, clientset, &pod); err != nil {
			log.Printf("Error processing pod %s/%s: %v", namespace, pod.Name, err)
			continue
		}
		processedCount++
	}

	if processedCount > 0 {
		log.Printf("Processed %d pods with Istio sidecars in namespace %s", processedCount, namespace)
	}

	return processedCount, nil
}

func processPod(ctx context.Context, clientset *kubernetes.Clientset, pod *corev1.Pod) error {
	log.Printf("Processing pod %s/%s with Istio sidecar", pod.Namespace, pod.Name)

	if len(pod.OwnerReferences) == 0 {
		log.Printf("Skipping pod %s/%s: no owner references", pod.Namespace, pod.Name)
		return nil
	}

	return traverseOwners(ctx, clientset, pod.Namespace, pod.OwnerReferences[0])
}

func traverseOwners(ctx context.Context, clientset *kubernetes.Clientset, namespace string, ownerRef metav1.OwnerReference) error {
	switch ownerRef.Kind {
	case "ReplicaSet":
		// Get the ReplicaSet
		rs, err := clientset.AppsV1().ReplicaSets(namespace).Get(ctx, ownerRef.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get ReplicaSet %s: %v", ownerRef.Name, err)
		}

		// Check if ReplicaSet has an owner (Deployment)
		if len(rs.OwnerReferences) > 0 {
			return traverseOwners(ctx, clientset, namespace, rs.OwnerReferences[0])
		}
		log.Printf("ReplicaSet %s/%s has no owner references", namespace, ownerRef.Name)

	case "Deployment":
		return restartWorkload(ctx, clientset, namespace, ownerRef.Name, "Deployment")

	case "DaemonSet":
		return restartWorkload(ctx, clientset, namespace, ownerRef.Name, "DaemonSet")

	case "StatefulSet":
		return restartWorkload(ctx, clientset, namespace, ownerRef.Name, "StatefulSet")

	default:
		log.Printf("Unsupported owner kind: %s for %s/%s", ownerRef.Kind, namespace, ownerRef.Name)
	}

	return nil
}

// restartWorkload handles restarting different types of workloads
func restartWorkload(ctx context.Context, clientset *kubernetes.Clientset, namespace, name, kind string) error {
	switch kind {
	case "Deployment":
		deployment, err := clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get Deployment %s: %v", name, err)
		}

		deployment.Spec.Template.Annotations = addRestartAnnotation(deployment.Spec.Template.Annotations)
		_, err = clientset.AppsV1().Deployments(namespace).Update(ctx, deployment, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to update Deployment %s: %v", name, err)
		}

		log.Printf("Successfully restarted Deployment %s in namespace %s", name, namespace)

	case "DaemonSet":
		daemonset, err := clientset.AppsV1().DaemonSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get DaemonSet %s: %v", name, err)
		}

		daemonset.Spec.Template.Annotations = addRestartAnnotation(daemonset.Spec.Template.Annotations)
		_, err = clientset.AppsV1().DaemonSets(namespace).Update(ctx, daemonset, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to update DaemonSet %s: %v", name, err)
		}

		log.Printf("Successfully restarted DaemonSet %s in namespace %s", name, namespace)

	case "StatefulSet":
		statefulset, err := clientset.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get StatefulSet %s: %v", name, err)
		}

		statefulset.Spec.Template.Annotations = addRestartAnnotation(statefulset.Spec.Template.Annotations)
		_, err = clientset.AppsV1().StatefulSets(namespace).Update(ctx, statefulset, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to update StatefulSet %s: %v", name, err)
		}

		log.Printf("Successfully restarted StatefulSet %s in namespace %s", name, namespace)

	default:
		return fmt.Errorf("unsupported workload kind: %s", kind)
	}

	return nil
}
