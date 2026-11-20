CONTROLLER_GEN = go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.23.0
SETUP_ENVTEST  = go run sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.3
# Kubernetes version of the API server that the tests start.
ENVTEST_K8S    = 1.37.0

.PHONY: generate test check

# Write the deep copy code and the CRD files from the Go types.
generate:
	$(CONTROLLER_GEN) object paths=./api/...
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:dir=config/crd
	$(CONTROLLER_GEN) rbac:roleName=allostat-coordinator paths=./internal/... output:rbac:dir=config/rbac

# Run all tests. This downloads a test API server on the first run.
test:
	go vet ./...
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S) -p path)" go test -race ./...

# Fail if the generated files do not match the Go types.
check: generate
	git diff --exit-code -- api config
