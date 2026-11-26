# Image URL to use all building/pushing image targets
IMG ?= controller:latest

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

# TODO(user): To use a different vendor for e2e tests, modify the setup under 'tests/e2e'.
# The default setup assumes Kind is pre-installed and builds/loads the Manager Docker image locally.
# CertManager is installed by default; skip with:
# - CERT_MANAGER_INSTALL_SKIP=true
.PHONY: test-e2e
test-e2e: manifests generate fmt vet ## Run the e2e tests. Expected an isolated environment using Kind.
	@command -v kind >/dev/null 2>&1 || { \
		echo "Kind is not installed. Please install Kind manually."; \
		exit 1; \
	}
	@kind get clusters | grep -q 'kind' || { \
		echo "No Kind cluster is running. Please start a Kind cluster before running the e2e tests."; \
		exit 1; \
	}
	go test ./test/e2e/ -v -ginkgo.v

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	$(GOLANGCI_LINT) run --fix

.PHONY: lint-config
lint-config: golangci-lint ## Verify golangci-lint linter configuration
	$(GOLANGCI_LINT) config verify

##@ Documentation

.PHONY: generate-cli-docs
generate-cli-docs: ## Generate Hugo CLI reference documentation from Cobra command tree.
	@mkdir -p site/content/docs/reference/cli
	@go run ./cmd/docgen --type=cli --out-dir=site/content/docs/reference/cli

.PHONY: generate-crd-docs
generate-crd-docs: crd-ref-docs ## Generate Hugo CRD API reference documentation from Go types.
	@mkdir -p site/content/docs/reference/crd
	@$(CRD_REF_DOCS) \
		--source-path=./api \
		--config=hack/crd-ref-docs/config.yaml \
		--templates-dir=hack/crd-ref-docs/templates \
		--renderer=markdown \
		--output-path=site/content/docs/reference/crd/_index.md

.PHONY: generate-openapi-docs
generate-openapi-docs: ## Generate OpenAPI 3.1 specification JSON.
	@mkdir -p docs
	@go run ./cmd/docgen --type=openapi --out-dir=docs

.PHONY: openapi
openapi: generate-openapi-docs ## Alias for generate-openapi-docs.

.PHONY: generate-docs
generate-docs: generate-cli-docs generate-crd-docs generate-openapi-docs ## Generate all reference documentation.

.PHONY: verify-cli-docs
verify-cli-docs: generate-cli-docs ## Verify CLI documentation is up-to-date.
	@git diff --exit-code site/content/docs/reference/cli || { \
		echo "Error: CLI reference documentation is out of date. Run 'make generate-cli-docs'."; \
		exit 1; \
	}
	@test -z "$$(git status --porcelain site/content/docs/reference/cli | grep '^??' || true)" || { \
		echo "Error: Untracked CLI reference documentation found. Run 'make generate-cli-docs' and track the files."; \
		git status --porcelain site/content/docs/reference/cli; \
		exit 1; \
	}

.PHONY: verify-crd-docs
verify-crd-docs: generate-crd-docs ## Verify CRD API reference documentation is up-to-date.
	@git diff --exit-code site/content/docs/reference/crd || { \
		echo "Error: CRD API reference documentation is out of date. Run 'make generate-crd-docs'."; \
		exit 1; \
	}
	@test -z "$$(git status --porcelain site/content/docs/reference/crd | grep '^??' || true)" || { \
		echo "Error: Untracked CRD API reference documentation found. Run 'make generate-crd-docs' and track the files."; \
		git status --porcelain site/content/docs/reference/crd; \
		exit 1; \
	}

.PHONY: verify-openapi-docs
verify-openapi-docs: generate-openapi-docs ## Verify OpenAPI specification is up-to-date.
	@git diff --exit-code docs/openapi.json || { \
		echo "Error: OpenAPI reference documentation is out of date. Run 'make generate-openapi-docs'."; \
		exit 1; \
	}

.PHONY: verify-docs
verify-docs: verify-cli-docs verify-crd-docs verify-openapi-docs ## Verify all reference documentation is up-to-date.
	@go run ./cmd/docgen --type=verify-all --ref-dir=site/content/docs/reference

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: build-api
build-api: fmt vet ## Build buddy-api gateway binary.
	go build -o bin/buddy-api ./cmd/buddy-api

.PHONY: build-cli
build-cli: fmt vet ## Build buddy-sbx CLI binary.
	go build -o bin/buddy-sbx ./cmd/buddy-sbx

.PHONY: build-egress-gateway
build-egress-gateway: fmt vet ## Build buddy-egress-gateway binary.
	go build -o bin/buddy-egress-gateway ./cmd/buddy-egress-gateway

.PHONY: build-all
build-all: build build-api build-cli build-egress-gateway ## Build all binaries.

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-build-buddy-api
docker-build-buddy-api: ## Build the buddy-api gateway image.
	$(CONTAINER_TOOL) build -f images/buddy-api/Dockerfile -t ${IMG} .

.PHONY: docker-build-git-init
docker-build-git-init: ## Build the Git init image with the credential transport dependency.
	$(CONTAINER_TOOL) build -f images/git-init/Dockerfile -t ${IMG} .

KIND_CLUSTER ?= dell-cluster
REGISTRY ?= ghcr.io/hoangndst
BASE_IMG ?= $(REGISTRY)/buddy-sandboxes-base:latest
CODEX_IMG ?= $(REGISTRY)/buddy-sandboxes-codex:0.171.0
CLAUDE_IMG ?= $(REGISTRY)/buddy-sandboxes-claude-code:2.1.330

.PHONY: docker-build-agent-base
docker-build-agent-base: ## Build the hardened agent base image.
	$(CONTAINER_TOOL) build -f images/agent-base/Dockerfile -t $(BASE_IMG) -t buddy-sandboxes-base:local .

.PHONY: docker-build-agent-codex
docker-build-agent-codex: docker-build-agent-base ## Build the in-tree Codex agent image.
	$(CONTAINER_TOOL) build -f images/agent-codex/Dockerfile --build-arg BASE_IMAGE=buddy-sandboxes-base:local -t $(CODEX_IMG) .

.PHONY: docker-build-agent-claude
docker-build-agent-claude: docker-build-agent-base ## Build the in-tree Claude Code agent image.
	$(CONTAINER_TOOL) build -f images/agent-claude/Dockerfile --build-arg BASE_IMAGE=buddy-sandboxes-base:local -t $(CLAUDE_IMG) .

.PHONY: docker-build-agent-images
docker-build-agent-images: docker-build-agent-codex docker-build-agent-claude ## Build all in-tree agent images.

.PHONY: kind-load-agent-images
kind-load-agent-images: docker-build-agent-images ## Build and load agent images into KinD cluster.
	kind load docker-image $(CODEX_IMG) --name $(KIND_CLUSTER)
	kind load docker-image $(CLAUDE_IMG) --name $(KIND_CLUSTER)

.PHONY: gateway-migrate
gateway-migrate: ## Run the buddy-api schema migration Job in the current Kubernetes context.
	$(KUBECTL) delete job buddy-api-migrate -n buddy-sandboxes-system --ignore-not-found
	$(KUBECTL) apply -f config/local-gateway/migrate-job.yaml
	$(KUBECTL) wait --for=condition=complete job/buddy-api-migrate -n buddy-sandboxes-system --timeout=180s

.PHONY: gateway-deploy
gateway-deploy: ## Build and deploy buddy-api plus local PostgreSQL to the active cluster.
	$(CONTAINER_TOOL) build -f images/buddy-api/Dockerfile -t buddy-api:local .
	$(CONTAINER_TOOL) build -f images/git-init/Dockerfile -t buddy-git-init:local .
	kind load docker-image buddy-api:local --name dell-cluster
	kind load docker-image buddy-git-init:local --name dell-cluster
	$(KUBECTL) apply -k config/local-gateway
	$(KUBECTL) rollout status statefulset/buddy-postgres -n buddy-sandboxes-system --timeout=180s
	$(MAKE) gateway-migrate
	$(KUBECTL) rollout restart deployment/buddy-api -n buddy-sandboxes-system
	$(KUBECTL) rollout status deployment/buddy-api -n buddy-sandboxes-system --timeout=180s

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name buddy-sandboxes-builder
	$(CONTAINER_TOOL) buildx use buddy-sandboxes-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm buddy-sandboxes-builder
	rm Dockerfile.cross

.PHONY: build-installer
build-installer: manifests generate kustomize generate-gateway-secret ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	cd config/egress-gateway && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default > dist/install.yaml

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: generate-gateway-secret
generate-gateway-secret: ## Generate a unique private HMAC signing secret for egress gateway
	@if [ ! -f config/egress-gateway/secret.yaml ] || grep -q "REPLACE_WITH_HMAC_SECRET_KEY_32BYTES" config/egress-gateway/secret.yaml 2>/dev/null; then \
		echo "Generating fresh private egress gateway HMAC secret..."; \
		RANDOM_SECRET=$$(head -c 32 /dev/urandom | base64 | tr -d '\n'); \
		sed -i "s|hmac-secret:.*|hmac-secret: $$RANDOM_SECRET|" config/egress-gateway/secret.yaml; \
	fi

.PHONY: deploy
deploy: manifests kustomize generate-gateway-secret ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	cd config/egress-gateway && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default | $(KUBECTL) apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/default | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
KUBECTL ?= kubectl
KUSTOMIZE ?= $(LOCALBIN)/kustomize
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

##@ Cleanup

.PHONY: clean-cache
clean-cache: ## Clean temporary build artifacts, Go build cache, Docker build cache, and prune dangling KinD images.
	@rm -f bin/manager bin/buddy-api bin/buddy-sbx bin/buddy-egress-gateway /tmp/images-tar* /tmp/go-build* /tmp/buddy-* 2>/dev/null || true
	@go clean -cache
	@docker builder prune -f
	@docker image prune -f
	@for node in $$(kind get nodes --name dell-cluster 2>/dev/null); do \
		docker exec $$node crictl rmi --prune 2>/dev/null || true; \
	done
	@echo "Cleanup complete."

## Tool Versions
KUSTOMIZE_VERSION ?= v5.5.0
CONTROLLER_TOOLS_VERSION ?= v0.17.2
#ENVTEST_VERSION is the version of controller-runtime release branch to fetch the envtest setup script (i.e. release-0.20)
ENVTEST_VERSION ?= $(shell go list -m -f "{{ .Version }}" sigs.k8s.io/controller-runtime | awk -F'[v.]' '{printf "release-%d.%d", $$2, $$3}')
#ENVTEST_K8S_VERSION is the version of Kubernetes to use for setting up ENVTEST binaries (i.e. 1.31)
ENVTEST_K8S_VERSION ?= $(shell go list -m -f "{{ .Version }}" k8s.io/api | awk -F'[v.]' '{printf "1.%d", $$3}')
GOLANGCI_LINT_VERSION ?= v1.63.4
CRD_REF_DOCS ?= $(LOCALBIN)/crd-ref-docs
CRD_REF_DOCS_VERSION ?= v0.3.0

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: setup-envtest
setup-envtest: envtest ## Download the binaries required for ENVTEST in the local bin directory.
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@$(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

.PHONY: crd-ref-docs
crd-ref-docs: $(CRD_REF_DOCS) ## Download crd-ref-docs locally if necessary.
$(CRD_REF_DOCS): $(LOCALBIN)
	$(call go-install-tool,$(CRD_REF_DOCS),github.com/elastic/crd-ref-docs,$(CRD_REF_DOCS_VERSION))

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f $(1) || true ;\
GOBIN=$(LOCALBIN) go install $${package} ;\
mv $(1) $(1)-$(3) ;\
} ;\
ln -sf $(1)-$(3) $(1)
endef
