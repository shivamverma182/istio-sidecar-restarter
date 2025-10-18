package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"path/filepath"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
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
	// {Name: "istiod", Type: DeploymentType},
	// {Name: "istio-ingressgateway", Type: DeploymentType},
	// {Name: "istio-cni-node", Type: DaemonSetType},
}

var istioNamespace = "istio-system"
var namespaces []string

// setupClient initializes the Kubernetes client configuration
func setupClient(kubeconfigPath string) (*kubernetes.Clientset, error) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("error building kubeconfig: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("error creating kubernetes client: %v", err)
	}

	return clientset, nil
}

func main() {
	// Get kubeconfig path
	var kubeconfig string
	if home := homedir.HomeDir(); home != "" {
		flag.StringVar(&kubeconfig, "kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		flag.StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	}

	namespace := flag.String("namespace", "", "namespace to search pods in")
	allNamespaces := flag.Bool("all-namespaces", false, "search pods in all namespaces")
	flag.Parse()

	// Setup kubernetes client
	clientset, err := setupClient(kubeconfig)
	if err != nil {
		log.Fatalf("Failed to setup kubernetes client: %v", err)
	}

	// Handle Istio workload restarts
	for _, workload := range istioWorkloads {
		if err := restartIstioWorkload(clientset, workload); err != nil {
			log.Printf("Failed to restart %s %s: %v", workload.Type, workload.Name, err)
		}
	}

	// Get list of namespaces to process
	namespaces, err := getNamespaces(clientset, *namespace, *allNamespaces)
	if err != nil {
		log.Fatalf("Failed to get namespaces: %v", err)
	}
	log.Printf("Processing namespaces: %v", namespaces)
	// Process pods in each namespace
	for _, ns := range namespaces {
		pods, err := clientset.CoreV1().Pods(ns).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			log.Printf("Error listing pods in namespace %s: %v", ns, err)
			continue
		}

		for _, pod := range pods.Items {
			if err := processPod(clientset, &pod); err != nil {
				log.Printf("Error processing pod %s/%s: %v", ns, pod.Name, err)
			}
		}
	}
}

// restartIstioWorkload handles the restart of a specific Istio workload
func restartIstioWorkload(clientset *kubernetes.Clientset, workload IstioWorkload) error {
	switch workload.Type {
	case DeploymentType:
		dep, err := clientset.AppsV1().Deployments(istioNamespace).Get(context.TODO(), workload.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get deployment: %v", err)
		}

		dep.Spec.Template.Annotations = addRestartAnnotation(dep.Spec.Template.Annotations)
		_, err = clientset.AppsV1().Deployments(istioNamespace).Update(context.TODO(), dep, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to update deployment: %v", err)
		}
		log.Printf("Triggered restart for Deployment %s", workload.Name)

	case DaemonSetType:
		ds, err := clientset.AppsV1().DaemonSets(istioNamespace).Get(context.TODO(), workload.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("failed to get daemonset: %v", err)
		}

		ds.Spec.Template.Annotations = addRestartAnnotation(ds.Spec.Template.Annotations)
		_, err = clientset.AppsV1().DaemonSets(istioNamespace).Update(context.TODO(), ds, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to update daemonset: %v", err)
		}
		log.Printf("Triggered restart for DaemonSet %s", workload.Name)

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
func getNamespaces(clientset *kubernetes.Clientset, namespace string, allNamespaces bool) ([]string, error) {
	if allNamespaces {
		namespaceList, err := clientset.CoreV1().Namespaces().List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("error listing namespaces: %v", err)
		}
		var namespaces []string
		for _, ns := range namespaceList.Items {
			namespaces = append(namespaces, ns.Name)
		}
		return namespaces, nil
	}
	return []string{namespace}, nil
}

// hasIstioSidecar checks if a pod has Istio sidecar injection
func hasIstioSidecar(pod *corev1.Pod) bool {
	for _, container := range pod.Spec.InitContainers {
		if container.Name == "istio-validation" {
			return true
		}
	}
	return false
}

func processPod(clientset *kubernetes.Clientset, pod *corev1.Pod) error {
	if !hasIstioSidecar(pod) {
		return nil
	}

	log.Printf("Found pod %s with istio sidecar injection enabled", pod.Name)

	if len(pod.OwnerReferences) == 0 {
		return fmt.Errorf("pod %s has no owner references", pod.Name)
	}

	return traverseOwners(clientset, pod.Namespace, pod.OwnerReferences[0])
}

func traverseOwners(clientset *kubernetes.Clientset, namespace string, ownerRef metav1.OwnerReference) error {
	switch ownerRef.Kind {
	case "ReplicaSet":
		// Get the ReplicaSet
		rs, err := clientset.AppsV1().ReplicaSets(namespace).Get(context.TODO(), ownerRef.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("error getting ReplicaSet: %v", err)
		}

		// Check if ReplicaSet has an owner (Deployment)
		if len(rs.OwnerReferences) > 0 {
			return traverseOwners(clientset, namespace, rs.OwnerReferences[0])
		}

	case "Deployment":
		// Get the Deployment
		deployment, err := clientset.AppsV1().Deployments(namespace).Get(context.TODO(), ownerRef.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("error getting Deployment: %v", err)
		}

		// Update the Deployment's pod template annotations using the helper
		deployment.Spec.Template.Annotations = addRestartAnnotation(deployment.Spec.Template.Annotations)

		// Update the Deployment
		_, err = clientset.AppsV1().Deployments(namespace).Update(context.TODO(), deployment, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("error updating Deployment: %v", err)
		}

		fmt.Printf("Successfully updated Deployment %s to trigger pod restart in namespace %s\n", deployment.Name, deployment.Namespace)

	case "DaemonSet":
		// Get the DaemonSet
		daemonset, err := clientset.AppsV1().DaemonSets(namespace).Get(context.TODO(), ownerRef.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("error getting DaemonSet: %v", err)
		}

		// Update the DaemonSet's pod template annotations using the helper
		daemonset.Spec.Template.Annotations = addRestartAnnotation(daemonset.Spec.Template.Annotations)

		// Update the DaemonSet
		_, err = clientset.AppsV1().DaemonSets(namespace).Update(context.TODO(), daemonset, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("error updating DaemonSet: %v", err)
		}

		fmt.Printf("Successfully updated DaemonSet %s to trigger pod restart in namespace %s\n", daemonset.Name, daemonset.Namespace)

	default:
		fmt.Printf("Unsupported owner kind: %s\n", ownerRef.Kind)
	}

	return nil
}
