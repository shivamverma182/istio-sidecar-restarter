# Pod Restarter

This Go program is designed to restart Kubernetes pods that have Istio sidecar injection enabled by updating their parent Deployment's pod template annotations.

## Features

- Searches for pods with `sidecar.istio.io/inject: "true"` annotation
- Traces pod ownership from Pod → ReplicaSet → Deployment
- Updates Deployment's pod template with `kubectl.kubernetes.io/restartedAt` annotation to trigger a restart

## Prerequisites

- Go 1.21 or later
- Access to a Kubernetes cluster
- `kubectl` configured with proper cluster access

## Installation

1. Clone the repository:
```bash
git clone https://github.com/shivam/pod_restarter.git
cd pod_restarter
```

2. Install dependencies:
```bash
go mod download
```

## Usage

Build and run the program:

```bash
go build
./pod_restarter --namespace your-namespace
```

### Command-line flags:

- `--kubeconfig`: Path to your kubeconfig file (optional, defaults to ~/.kube/config)
- `--namespace`: Namespace to search for pods (required)

## How it works

1. The program connects to your Kubernetes cluster using your kubeconfig
2. It searches for pods in the specified namespace that have Istio sidecar injection enabled
3. For each matching pod, it:
   - Finds the owning ReplicaSet
   - Finds the owning Deployment
   - Updates the Deployment's pod template annotations to trigger a rolling restart

## Example

```bash
./pod_restarter --namespace my-namespace
```

The program will output information about the pods it finds and any updates it makes to trigger restarts.