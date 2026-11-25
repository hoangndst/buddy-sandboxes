---
title: "Getting Started"
linkTitle: "Getting Started"
description: "Prerequisites, quickstart instructions, and first sandbox deployment"
weight: 10
---

# Getting Started with Buddy Sandboxes

This guide covers prerequisites and step-by-step instructions for deploying Buddy Sandboxes to a Kubernetes cluster and running your first agent sandbox.

## Prerequisites

- A Kubernetes cluster (v1.28 or later, e.g. local KinD, k3s, or managed cloud cluster)
- `kubectl` configured with cluster administrator privileges
- `buddy-sbx` CLI installed on your local workstation

## Installation

### 1. Install Custom Resource Definitions

```bash
kubectl apply -f https://github.com/hoangndst/buddy-sandboxes/releases/latest/download/crds.yaml
```

### 2. Deploy the Operator

```bash
kubectl apply -f https://github.com/hoangndst/buddy-sandboxes/releases/latest/download/operator.yaml
```

Verify that the operator pod is running:

```bash
kubectl get pods -n buddy-sandboxes-system
```

### 3. Connect to the Gateway

Expose the Buddy Gateway locally:

```bash
kubectl port-forward -n buddy-sandboxes-system svc/buddy-api 8080:8080
```

Authenticate your local CLI session:

```bash
buddy-sbx login http://localhost:8080 --token <token>
```

## Running Your First Sandbox

Run an interactive sandbox with the developer CLI:

```bash
buddy-sbx run --name quickstart-box --rm alpine:latest -- /bin/sh
```

Inspect sandbox status:

```bash
buddy-sbx get sbx
buddy-sbx describe sbx quickstart-box
```
