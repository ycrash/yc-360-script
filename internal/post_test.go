package internal

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
	defer file.Close()
	err = PositionLastLines(file, 5)
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
		t.Fatalf("invalid result '%x' != '%x'", bytes, result)
	}
}
