# redis-operator Makefile

# Image URL to use for building/pushing the operator image
IMG ?= redis-operator:latest
# Binary name
BINARY ?= manager

# Tool versions
CONTROLLER_GEN_VERSION ?= v0.20.1
GOLANGCI_LINT_VERSION ?= v2.10.1

# Get the currently used golang install path
GOBIN ?= $(shell go env GOPATH)/bin

# Setting SHELL to bash allows bash commands in recipes
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: generate manifests build

##@ General

.PHONY: help
help: ## Display this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: generate
generate: controller-gen ## Generate DeepCopy methods (zz_generated.deepcopy.go)
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: manifests
manifests: controller-gen ## Generate CRD manifests into config/crd/bases/
	$(CONTROLLER_GEN) crd paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: fmt
fmt: ## Run go fmt against code
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code
	go vet ./...

.PHONY: lint
lint: golangci-lint ## Run golangci-lint
	$(GOLANGCI_LINT) run

.PHONY: test
test: generate fmt vet ## Run all unit tests
	go test ./... -coverprofile cover.out

##@ Build

.PHONY: build
build: fmt vet ## Build the operator binary
	go build -o bin/$(BINARY) ./cmd/manager

.PHONY: run
run: generate fmt vet ## Run the operator locally against the current kubeconfig cluster
	go run ./cmd/manager controller

.PHONY: docker-build
docker-build: ## Build the Docker image
	docker build -t $(IMG) .

.PHONY: docker-push
docker-push: ## Push the Docker image
	docker push $(IMG)

##@ Deployment

.PHONY: install
install: manifests ## Install CRDs into the current cluster
	kubectl apply -f config/crd/bases/

.PHONY: uninstall
uninstall: manifests ## Uninstall CRDs from the current cluster
	kubectl delete -f config/crd/bases/

.PHONY: deploy
deploy: manifests ## Deploy operator to the current cluster
	cd config && kustomize build . | kubectl apply -f -

.PHONY: undeploy
undeploy: ## Undeploy operator from the current cluster
	cd config && kustomize build . | kubectl delete -f -

##@ Testing

# Directory for downloaded tool binaries (e.g. envtest).
LOCALBIN ?= $(shell pwd)/bin
ENVTEST_K8S_VERSION ?= 1.31.0

.PHONY: test-e2e
test-e2e: ## Run e2e tests using envtest (requires: make setup-envtest first)
	KUBEBUILDER_ASSETS="$(shell setup-envtest use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path 2>/dev/null || echo '')" \
	go test ./test/e2e/... -v -timeout 120s

.PHONY: setup-envtest
setup-envtest: ## Download envtest binaries for local e2e testing
	go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
	setup-envtest use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN)

##@ Tool Dependencies

CONTROLLER_GEN = $(GOBIN)/controller-gen
.PHONY: controller-gen
controller-gen: ## Download controller-gen if necessary
	@test -s $(CONTROLLER_GEN) || go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)

GOLANGCI_LINT = $(GOBIN)/golangci-lint
.PHONY: golangci-lint
golangci-lint: ## Download golangci-lint if necessary
	@test -s $(GOLANGCI_LINT) || go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
