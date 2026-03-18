package eval

import (
	"encoding/json"
	"fmt"
	"os"
)

func LoadDataset(path string) (Dataset, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Dataset{}, fmt.Errorf("read dataset: %w", err)
	}
	var ds Dataset
	if err := json.Unmarshal(data, &ds); err != nil {
		return Dataset{}, fmt.Errorf("parse dataset json: %w", err)
	}
	if ds.ID == "" {
		ds.ID = "heldout"
	}
	return ds, nil
}

