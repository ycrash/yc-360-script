package config

import (
	"testing"

	"gopkg.in/yaml.v2"
)

func TestConfig(t *testing.T) {
	t.Run("encode", func(t *testing.T) {
		t.SkipNow()
		c := &Config{
			Version: "1",
			Options: Options{
				Pid:            0,
				ApiKey:         "buggycompany@e094aasdsa-c3eb-4c9a-8254-f0dd107245cc",
				Server:         "https://test.gceasy.io",
				AppName:        "aps",
				HeapDump:       true,
				HeapDumpPath:   "",
				ThreadDumpPath: "",
				GCPath:         "",
				JavaHomePath:   "",
			},
		}
		out, err := yaml.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		t.Log(string(out))
	})

	t.Run("ParseArgs", func(t *testing.T) {
		args := []string{"yc", "-properties", "testdata/config.yaml", "-s", "https://test.gceasy.io"}
		err := ParseFlags(args)
		if err != nil {
			t.Fatal(err)
		}
		if GlobalConfig.ApiKey != "buggycompany@e094aasdsa-c3eb-4c9a-8254-f0dd107245cc" {
			t.Fatalf("expect %s == buggycompany@e094aasdsa-c3eb-4c9a-8254-f0dd107245cc", GlobalConfig.ApiKey)
		}
		if GlobalConfig.Server != "https://test.gceasy.io" {
			t.Fatalf("expect %s == https://test.gceasy.io", GlobalConfig.Server)
		}
	})
}
