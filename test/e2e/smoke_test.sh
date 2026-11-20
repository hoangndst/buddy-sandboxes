#!/usr/bin/env bash
set -euo pipefail

NAMESPACE="default"
SAMPLE_NAME="sandbox-sample"

echo "=== Running Buddy Sandboxes Smoke Test ==="

echo "[1/4] Applying CRDs to cluster..."
kubectl apply -k config/crd

echo "[2/4] Creating sample Sandbox..."
kubectl apply -f config/samples/sandboxes_v1alpha1_sandbox.yaml

echo "[3/4] Verifying Sandbox resource exists..."
FOUND_NAME=$(kubectl get sandbox "${SAMPLE_NAME}" -n "${NAMESPACE}" -o jsonpath='{.metadata.name}')
if [ "${FOUND_NAME}" != "${SAMPLE_NAME}" ]; then
  echo "ERROR: Sandbox ${SAMPLE_NAME} was not found in namespace ${NAMESPACE}" >&2
  exit 1
fi

echo "Verified Sandbox ${FOUND_NAME} is registered in namespace ${NAMESPACE}."

echo "[4/4] Cleaning up smoke test sandbox..."
kubectl delete sandbox "${SAMPLE_NAME}" -n "${NAMESPACE}" --wait=true

echo "=== Smoke test completed successfully ==="
