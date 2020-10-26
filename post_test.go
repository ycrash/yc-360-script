package shell

import (
	"io/ioutil"
	"os"
	"testing"
)

func TestLastNLines(t *testing.T) {
	file, err := os.Open("config/testdata/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	_5000Lines = 5
	err = PositionLast5000Lines(file)
	if err != nil {
		t.Fatal(err)
	}
	bytes, err := ioutil.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	result := `    - urlParams: pidstat
      cmd: pidstat
  processTokens:
    - uploadDir
    - buggyApp`
	if string(bytes) != result {
		t.Fatalf("invalid result '%s' != '%s'", bytes, result)
	}
}
