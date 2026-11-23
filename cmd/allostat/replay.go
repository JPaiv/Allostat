package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"sigs.k8s.io/yaml"

	"github.com/JPaiv/Allostat/api/v1alpha1"
)

// replay reads decision records and release records and plays each through the decision core
// again. It needs no cluster. It returns an error if a record does not
// give its recorded answer.
//
//	kubectl get decisionrecords,releaserecords -o yaml | allostat replay
//	allostat replay record.yaml
func replay(args []string, stdin io.Reader, out io.Writer) error {
	var data []byte
	var err error
	switch len(args) {
	case 0:
		data, err = io.ReadAll(stdin)
	case 1:
		data, err = os.ReadFile(args[0])
	default:
		return errors.New("usage: allostat replay [file]   (reads standard input without a file)")
	}
	if err != nil {
		return err
	}

	// One record, or a list as `kubectl get -o yaml` prints it.
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	data, err = yaml.YAMLToJSON(data)
	if err != nil {
		return fmt.Errorf("read the records: %w", err)
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return fmt.Errorf("read the records: %w", err)
	}
	if len(list.Items) == 0 {
		list.Items = []json.RawMessage{data}
	}

	different := 0
	for _, item := range list.Items {
		what, noReplay, err := verify(item)
		switch {
		case what == "":
			return err
		case err != nil:
			different++
			fmt.Fprintf(out, "DIFFERENT  %s: %v\n", what, err)
		case noReplay:
			fmt.Fprintf(out, "no replay  %s: refused without a contract\n", what)
		default:
			fmt.Fprintf(out, "same       %s\n", what)
		}
	}
	if different > 0 {
		return fmt.Errorf("%d of %d records do not give their recorded answer", different, len(list.Items))
	}
	return nil
}

// verify plays one record again. It returns a description of the record
// and the difference, if there is one. Without a description the input is
// not a record and the error says why.
func verify(item []byte) (what string, noReplay bool, err error) {
	var kind struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(item, &kind); err != nil {
		return "", false, fmt.Errorf("read the record: %w", err)
	}
	switch kind.Kind {
	case "DecisionRecord":
		var r v1alpha1.DecisionRecord
		if err := strict(item, &r); err != nil {
			return "", false, err
		}
		what = fmt.Sprintf("%s (request %s, %s)", r.Name, r.Spec.Request.Name, r.Spec.Decision.Decision)
		return what, r.Spec.Contract == nil, r.Verify()
	case "ReleaseRecord":
		var r v1alpha1.ReleaseRecord
		if err := strict(item, &r); err != nil {
			return "", false, err
		}
		what = fmt.Sprintf("%s (request %s, released)", r.Name, r.Spec.Request.Name)
		return what, false, r.Verify()
	case "", "List":
		return "", false, errors.New("no record found in the input")
	default:
		return "", false, fmt.Errorf("the input has an object of kind %q: want DecisionRecord or ReleaseRecord", kind.Kind)
	}
}

func strict(data []byte, into any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(into); err != nil {
		return fmt.Errorf("read the record: %w", err)
	}
	return nil
}
