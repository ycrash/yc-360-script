package shell

import (
	"io"
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

func TestLast1000Lines(t *testing.T) {
	src, err := os.Open("testdata/tier1app.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	err = PositionLastLines(src, 1000)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := os.Create("testdata/applog.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	_, err = io.Copy(dst, src)
	if err != nil {
		t.Fatal(err)
	}
	err = dst.Sync()
	if err != nil {
		t.Fatal(err)
	}
}
