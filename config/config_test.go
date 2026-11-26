// Package config_test checks the files that install Allostat.
package config_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

// objects returns every object in the YAML files of a directory.
func objects(t *testing.T, dir string) []*unstructured.Unstructured {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no YAML files in %s: %v", dir, err)
	}
	var out []*unstructured.Unstructured
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, doc := range bytes.Split(data, []byte("\n---\n")) {
			obj := &unstructured.Unstructured{}
			if err := yaml.Unmarshal(doc, &obj.Object); err != nil {
				t.Fatalf("%s: %v", file, err)
			}
			if len(obj.Object) > 0 {
				out = append(out, obj)
			}
		}
	}
	return out
}

// A real API server must accept every install file as it is written, and
// the parts must fit: the deployment runs as the account that the binding
// gives the coordinator's role to.
func TestInstallFiles(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set; run `make test`")
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{"crd"}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start the API server: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	c, err := client.New(cfg, client.Options{Scheme: clientgoscheme.Scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	var deployment appsv1.Deployment
	var binding rbacv1.ClusterRoleBinding
	var role rbacv1.ClusterRole
	// The namespace first, then what lives in it.
	all := append(objects(t, "rbac"), objects(t, "manager")...)
	for _, wantNamespace := range []bool{true, false} {
		for _, obj := range all {
			if (obj.GetKind() == "Namespace") != wantNamespace {
				continue
			}
			if err := c.Create(ctx, obj.DeepCopy(), client.FieldValidation("Strict")); err != nil {
				t.Fatalf("%s %q is rejected: %v", obj.GetKind(), obj.GetName(), err)
			}
			var into any
			switch obj.GetKind() {
			case "Deployment":
				into = &deployment
			case "ClusterRoleBinding":
				into = &binding
			case "ClusterRole":
				into = &role
			default:
				continue
			}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, into); err != nil {
				t.Fatal(err)
			}
		}
	}

	if deployment.Name == "" || binding.Name == "" || role.Name == "" {
		t.Fatalf("want a deployment, a binding and a role; got %q, %q, %q", deployment.Name, binding.Name, role.Name)
	}
	if binding.RoleRef.Name != role.Name {
		t.Errorf("the binding gives the role %q, the role is %q", binding.RoleRef.Name, role.Name)
	}
	account := deployment.Spec.Template.Spec.ServiceAccountName
	if len(binding.Subjects) != 1 || binding.Subjects[0].Name != account || binding.Subjects[0].Namespace != deployment.Namespace {
		t.Errorf("the deployment runs as %s/%s, the binding is for %+v", deployment.Namespace, account, binding.Subjects)
	}

	// More than one process is safe only with leader election.
	container := deployment.Spec.Template.Spec.Containers[0]
	args := strings.Join(container.Args, " ")
	if *deployment.Spec.Replicas > 1 && !strings.Contains(args, "--leader-elect=true") {
		t.Errorf("%d replicas without --leader-elect=true: %s", *deployment.Spec.Replicas, args)
	}
	sc := container.SecurityContext
	if sc == nil || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem ||
		sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Errorf("the container must have a read-only root and no privilege escalation: %+v", sc)
	}
}

// The coordinator must know every flag the deployment gives it.
func TestDeploymentFlagsExist(t *testing.T) {
	var deployment appsv1.Deployment
	for _, obj := range objects(t, "manager") {
		if obj.GetKind() == "Deployment" {
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, &deployment); err != nil {
				t.Fatal(err)
			}
		}
	}
	args := deployment.Spec.Template.Spec.Containers[0].Args
	if len(args) == 0 {
		t.Fatal("the deployment gives no flags")
	}
	// -h prints the flags the binary has.
	cmd := exec.Command("go", "run", "../cmd/allostat", "-h")
	out, _ := cmd.CombinedOutput()
	if !bytes.Contains(out, []byte("-leader-elect")) {
		t.Fatalf("cannot read the flags of the coordinator:\n%s", out)
	}
	for _, arg := range args {
		name, _, _ := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if !bytes.Contains(out, []byte("\n  -"+name+"\n")) && !bytes.Contains(out, []byte("\n  -"+name+" ")) {
			t.Errorf("the deployment gives --%s, the coordinator has no such flag", name)
		}
	}
}
