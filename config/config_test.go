package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v2"
)

func TestConfig(t *testing.T) {
	t.Run("encode", func(t *testing.T) {
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

				Commands: []Command{
					{
						UrlParams: "vmstat",
						Cmd:       "vmstat",
					},
					{
						UrlParams: "pidstat",
						Cmd:       "pidstat",
					},
				},
			},
		}
		out, err := yaml.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		t.Log(string(out))
	})

	// https://tier1app.atlassian.net/browse/GCEA-1781
	// https://tier1app.atlassian.net/browse/GCEA-1782
	t.Run("ParseArgs", func(t *testing.T) {
		args := []string{"yc", "-c", "testdata/config.yaml", "-s", "https://test.gceasy.io"}
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
		if len(GlobalConfig.ProcessTokens) != 2 || GlobalConfig.ProcessTokens[0] != "uploadDir" || GlobalConfig.ProcessTokens[1] != "buggyApp" {
			t.Fatal("valid processTokens")
		}
	})

	t.Run("Parse Cmd Args", func(t *testing.T) {
		args := []string{"yc", "-urlParams", "tp=pidstat", "-cmd", "pidstat", "-urlParams", "tp=vmstat", "-cmd", "vmstat"}
		err := ParseFlags(args)
		if err != nil {
			t.Fatal(err)
		}
		t.Log(GlobalConfig)
		command := GlobalConfig.Commands[0]
		if command.UrlParams != "tp=pidstat" {
			t.Fatalf("expect %s == 'tp=pidstat'", command.UrlParams)
		}
		if command.Cmd != "pidstat" {
			t.Fatalf("expect %s == 'pidstat'", command.Cmd)
		}

		command = GlobalConfig.Commands[1]
		if command.UrlParams != "tp=vmstat" {
			t.Fatalf("expect %s == 'tp=vmstat'", command.UrlParams)
		}
		if command.Cmd != "vmstat" {
			t.Fatalf("expect %s == 'vmstat'", command.Cmd)
		}
	})

	t.Run("Parse verifySSL flag", func(t *testing.T) {
		args := []string{"yc", "-verifySSL", "false"}
		err := ParseFlags(args)
		if err != nil {
			t.Fatal(err)
		}
		if GlobalConfig.VerifySSL {
			t.Fail()
		}
		t.Log(GlobalConfig)
	})

	t.Run("ParseArgs", func(t *testing.T) {
		args := []string{"yc", "-c", "testdata/bug.yaml", "-verifySSL", "false"}
		err := ParseFlags(args)
		if err != nil {
			t.Fatal(err)
		}
		if GlobalConfig.VerifySSL {
			t.Fail()
		}
		if GlobalConfig.ApiKey != "buggycompany@e094a34e-c3eb-4c9a-8254-f0dd107245cc" {
			t.Fatalf("expect %s == buggycompany@e094a34e-c3eb-4c9a-8254-f0dd107245cc", GlobalConfig.ApiKey)
		}
		if GlobalConfig.Server != "http://localhost:8080/" {
			t.Fatalf("expect %s == http://localhost:8080/", GlobalConfig.Server)
		}
	})

	t.Run("ParseAPArgs", func(t *testing.T) {
		args := []string{"yc", "-m3Frequency", "5m", "-processTokens", "abc", "-processTokens", "cba", "-m3"}
		err := ParseFlags(args)
		if err != nil {
			t.Fatal(err)
		}
		if !GlobalConfig.M3 {
			t.Fail()
		}
		if len(GlobalConfig.ProcessTokens) != 2 {
			t.Fail()
		}
		if GlobalConfig.M3Frequency != 5*time.Minute {
			t.Fail()
		}
	})
}
