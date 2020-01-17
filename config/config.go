package config

import (
	"flag"
	"fmt"
	"os"
	"reflect"

	"gopkg.in/yaml.v2"
)

type Config struct {
	Version string
	Options
}

type Options struct {
	Pid            int    `yaml:"p" usage:"Process Id, for example: 3121"`
	ApiKey         string `yaml:"k" usage:"API Key, for example: tier1app@12312-12233-1442134-112"`
	Server         string `yaml:"s" usage:"YCrash Server URL, for example: https://ycrash.companyname.com"`
	AppName        string `yaml:"a" usage:"APP Name"`
	HeapDump       bool   `yaml:"hd" usage:"Capture heap dumps"`
	HeapDumpPath   string `yaml:"hdPath" usage:"Heap dump log file path"`
	ThreadDumpPath string `yaml:"tdPath" usage:"Thread dump log file path"`
	GCPath         string `yaml:"gcPath" usage:"GC log file path"`
	JavaHomePath   string `yaml:"j" usage:"JAVA_HOME path, for example: /usr/lib/jvm/java-8-openjdk-amd64"`
	ShowVersion    bool   `yaml:"version" usage:"Show version"`
	ConfigPath     string `yaml:"c" usage:"Config file path"`
}

var GlobalConfig Config

func ParseFlags(args []string) error {
	if len(args) < 2 {
		return nil
	}
	flagSet, result := registerFlags(args[0])
	flagSet.Parse(args[1:])

	op := Options{}
	copyFlagsValue(&op, result)

	if op.ConfigPath == "" {
		GlobalConfig.Options = op
		return nil
	}

	file, err := os.Open(op.ConfigPath)
	if err != nil {
		return fmt.Errorf("read config file path %s failed: %w", op.ConfigPath, err)
	}
	decoder := yaml.NewDecoder(file)
	err = decoder.Decode(&GlobalConfig)
	if err != nil {
		return fmt.Errorf("decode config file path %s failed: %w", op.ConfigPath, err)
	}

	copyFlagsValue(&GlobalConfig.Options, result)
	return nil
}

func copyFlagsValue(dst interface{}, src map[int]interface{}) {
	value := reflect.ValueOf(dst).Elem()
	numField := value.NumField()
	for i := 0; i < numField; i++ {
		x := reflect.ValueOf(src[i]).Elem()
		if x.IsZero() {
			continue
		}
		fieldValue := value.Field(i)
		fieldValue.Set(x)
	}
}

func registerFlags(flagSetName string) (*flag.FlagSet, map[int]interface{}) {
	flagSet := flag.NewFlagSet(flagSetName, flag.ExitOnError)

	typ := reflect.TypeOf(GlobalConfig.Options)
	result := make(map[int]interface{})
	for i := 0; i < typ.NumField(); i++ {
		fieldType := typ.Field(i)
		name, ok := fieldType.Tag.Lookup("yaml")
		if !ok {
			continue
		}
		usage := fieldType.Tag.Get("usage")
		switch fieldType.Type.Kind() {
		case reflect.Int:
			result[i] = flagSet.Int(name, 0, usage)
		case reflect.String:
			result[i] = flagSet.String(name, "", usage)
		case reflect.Bool:
			result[i] = flagSet.Bool(name, false, usage)
		default:
			panic(fmt.Sprintf("not supported type %s of field %s", fieldType.Type.Kind().String(), fieldType.Name))
		}
	}
	return flagSet, result
}

func ShowUsage() {
	flagSet, _ := registerFlags(os.Args[0])
	flagSet.Usage()
}
