package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"sigs.k8s.io/yaml"

	"github.com/JPaiv/Allostat/api/v1alpha1"
)

// replay reads decision records and plays each through the decision core
// again. It needs no cluster. It returns an error if a record does not
// give its recorded answer.
//
//	kubectl get decisionrecords -o yaml | allostat replay
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
		Items []v1alpha1.DecisionRecord `json:"items"`
	}
	if err := yaml.Unmarshal(data, &list); err != nil {
		return fmt.Errorf("read the records: %w", err)
	}
	records := list.Items
	if len(records) == 0 {
		var one v1alpha1.DecisionRecord
		if err := yaml.UnmarshalStrict(data, &one); err != nil {
			return fmt.Errorf("read the record: %w", err)
		}
		if one.Spec.Request.Name == "" {
			return errors.New("no decision record found in the input")
		}
		records = []v1alpha1.DecisionRecord{one}
	}

	different := 0
	for i := range records {
		r := &records[i]
		what := fmt.Sprintf("%s (request %s, %s)", r.Name, r.Spec.Request.Name, r.Spec.Decision.Decision)
		switch err := r.Verify(); {
		case err != nil:
			different++
			fmt.Fprintf(out, "DIFFERENT  %s: %v\n", what, err)
		case r.Spec.Contract == nil:
			fmt.Fprintf(out, "no replay  %s: refused without a contract\n", what)
		default:
			fmt.Fprintf(out, "same       %s\n", what)
		}
	}
	if different > 0 {
		return fmt.Errorf("%d of %d records do not give their recorded answer", different, len(records))
	}
	return nil
}
