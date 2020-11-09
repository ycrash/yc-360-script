package shell

import (
	"testing"
)

func TestCheckProcessExists(t *testing.T) {
	e := IsProcessExists(65535)
	if e {
		t.Fatal("process 65535 should not exists")
	}

	noGC, err := CommandStartInBackground(Command{"java", "-cp", "./capture/testdata/", "MyClass"})
	if err != nil {
		t.Fatal(err)
	}
	defer noGC.KillAndWait()

	e = IsProcessExists(noGC.GetPid())
	if !e {
		t.Fatal("process should be exists")
	}
}

func TestGetTopProcess(t *testing.T) {
	noGC, err := CommandStartInBackground(Command{"java", "-cp", "./capture/testdata/", "MyClass"})
	if err != nil {
		t.Fatal(err)
	}
	defer noGC.KillAndWait()
	t.Run("cpu", func(t *testing.T) {
		id, err := GetTopCpu()
		if err != nil {
			t.Fatal(err)
		}
		t.Log(id)
		if id < 1 {
			t.Fatal("can not get pid of java process")
		}
	})
	t.Run("mem", func(t *testing.T) {
		id, err := GetTopMem()
		if err != nil {
			t.Fatal(err)
		}
		t.Log(id)
		if id < 1 {
			t.Fatal("can not get pid of java process")
		}
	})
}
