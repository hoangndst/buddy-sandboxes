#!/usr/bin/env bash
set -euo pipefail

echo "=== Running Buddy Sandboxes Smoke Test ==="

echo "[1/3] Applying CRDs to cluster..."
kubectl apply -k config/crd

echo "[2/3] Creating sample Sandbox..."
kubectl apply -f config/samples/sandboxes_v1alpha1_sandbox.yaml

echo "[3/3] Verifying Sandbox resource..."
kubectl get sandboxes -o wide

echo "Cleaning up smoke test sandbox..."
kubectl delete -f config/samples/sandboxes_v1alpha1_sandbox.yaml --wait=false

echo "=== Smoke test completed successfully ==="
