package integration

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestFrozenFixturesAndIndependentOracle(t *testing.T) {
	b, err := os.ReadFile("testdata/oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	var oracle struct {
		Files   []struct{ File, SHA256 string }
		Queries []Query
	}
	if err = json.Unmarshal(b, &oracle); err != nil {
		t.Fatal(err)
	}
	if len(oracle.Files) != 2 || !reflect.DeepEqual(oracle.Queries, fixedQueries) {
		t.Fatal("frozen question set drift")
	}
	for _, f := range oracle.Files {
		b, err = os.ReadFile("testdata/" + f.File)
		if err != nil {
			t.Fatal(err)
		}
		if digest(b) != f.SHA256 || !strings.HasSuffix(string(b), "\n") || strings.Contains(string(b), "\r") {
			t.Fatalf("fixture %s changed", f.File)
		}
	}
}
