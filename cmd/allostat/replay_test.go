package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/JPaiv/Allostat/admission"
	"github.com/JPaiv/Allostat/api/v1alpha1"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func record(t *testing.T, name string, status metav1.ConditionStatus) *v1alpha1.DecisionRecord {
	t.Helper()
	ar := &v1alpha1.ActionRequest{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "fleet", UID: "uid-" + "1"},
		Spec: v1alpha1.ActionRequestSpec{
			Action: "restart", Requester: "upgrade",
			Target: v1alpha1.Target{Name: "a", Labels: map[string]string{"shard": "1"}},
		},
	}
	contract := &v1alpha1.Contract{
		ObjectMeta: metav1.ObjectMeta{Name: "restart"},
		Spec: v1alpha1.ContractSpec{
			Action:   "restart",
			Requires: []v1alpha1.Requirement{{Condition: "PeerCanServe", MaxAge: metav1.Duration{Duration: time.Minute}}},
			Budgets:  []v1alpha1.Budget{{Label: "shard", Max: 1}},
			GrantTTL: metav1.Duration{Duration: 10 * time.Minute},
		},
	}
	obs := []v1alpha1.Observation{{Spec: v1alpha1.ObservationSpec{
		Condition: "PeerCanServe", Subject: "a", Status: status, Source: "replica-health",
		ObservedAt: metav1.NewMicroTime(t0.Add(-10 * time.Second)),
	}}}
	d := admission.Decide(contract.ToAdmission(), ar.ToAdmission(), []admission.Observation{obs[0].ToAdmission()}, nil, t0)
	r, err := v1alpha1.NewDecisionRecord(ar, contract, obs, nil, t0, d)
	if err != nil {
		t.Fatal(err)
	}
	r.TypeMeta = metav1.TypeMeta{APIVersion: "allostat.io/v1alpha1", Kind: "DecisionRecord"}
	return r
}

func write(t *testing.T, v any) string {
	t.Helper()
	data, err := yaml.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "records.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReplayOfRecordsThatHold(t *testing.T) {
	granted, refused := record(t, "r1", metav1.ConditionTrue), record(t, "r2", metav1.ConditionFalse)

	var out bytes.Buffer
	if err := replay([]string{write(t, granted)}, nil, &out); err != nil {
		t.Fatalf("one record: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "same") || !strings.Contains(out.String(), "request r1, Granted") {
		t.Fatalf("unexpected output: %s", out.String())
	}

	// A list, as `kubectl get -o yaml` prints it, from standard input.
	list := map[string]any{"apiVersion": "v1", "kind": "List", "items": []any{granted, refused}}
	data, err := os.ReadFile(write(t, list))
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := replay(nil, bytes.NewReader(data), &out); err != nil {
		t.Fatalf("a list: %v\n%s", err, out.String())
	}
	if strings.Count(out.String(), "same") != 2 || !strings.Contains(out.String(), "request r2, Refused") {
		t.Fatalf("unexpected output: %s", out.String())
	}
}

func TestReplayFindsARecordThatWasChanged(t *testing.T) {
	// The record says "granted", but its observation now says False.
	changed := record(t, "r1", metav1.ConditionTrue)
	changed.Spec.Observations[0].Status = metav1.ConditionFalse

	var out bytes.Buffer
	err := replay([]string{write(t, changed)}, nil, &out)
	if err == nil || !strings.Contains(out.String(), "DIFFERENT") || !strings.Contains(out.String(), "the record says Granted, the replay says Refused") {
		t.Fatalf("want a difference, got err=%v, output: %s", err, out.String())
	}
}

func TestReplayRefusesInputWithoutARecord(t *testing.T) {
	var out bytes.Buffer
	if err := replay(nil, strings.NewReader("kind: ConfigMap\n"), &out); err == nil {
		t.Fatal("want an error for input without a record")
	}
	if err := replay([]string{"a", "b"}, nil, &out); err == nil {
		t.Fatal("want an error for two files")
	}
}
