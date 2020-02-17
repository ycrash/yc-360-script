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
	DeferDelete    bool   `yaml:"d" usage:"Delete logs folder after complete successfully"`

	Commands []Command `yaml:"cmds" usage:"Custom commands to be executed"`
}

type Command struct {
	UrlParams UrlParams `yaml:"urlParams" usage:"Params to be added at the end of url, should be used with -cmd together"`
	Cmd       Cmd       `yaml:"cmd" usage:"Command to be executed, should be used with -urlParams together"`
}

type UrlParams string
type UrlParamsSlice []UrlParams

func (u *UrlParamsSlice) String() string {
	return fmt.Sprintf("%v", *u)
}

func (u *UrlParamsSlice) Set(p string) error {
	*u = append(*u, UrlParams(p))
	return nil
}

type Cmd string
type CmdSlice []Cmd

func (c *CmdSlice) String() string {
	return fmt.Sprintf("%v", *c)
}

func (c *CmdSlice) Set(cmd string) error {
	*c = append(*c, Cmd(cmd))
	return nil
}

type CommandsFlagSetPair struct {
	UrlParamsSlice
	CmdSlice
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
		s := src[i]
		if x, ok := s.(*CommandsFlagSetPair); ok {
			if len(x.UrlParamsSlice) != len(x.CmdSlice) {
				panic("num of urlParams should be same as num of cmd")
			}
			cmds := make([]Command, len(x.UrlParamsSlice))
			for i, params := range x.UrlParamsSlice {
				cmds[i] = Command{
					UrlParams: params,
					Cmd:       x.CmdSlice[i],
				}
			}
			fieldValue := value.Field(i)
			fieldValue.Set(reflect.ValueOf(cmds))
			continue
		}
		x := reflect.ValueOf(s).Elem()
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
		case reflect.Slice:
			if fieldType.Type.AssignableTo(reflect.TypeOf([]Command{})) {
				tp := reflect.TypeOf(Command{})
				pair := &CommandsFlagSetPair{}
				ft := tp.Field(0)
				name, ok := ft.Tag.Lookup("yaml")
				if !ok {
					panic(fmt.Sprintf("failed to lookup tag 'yaml' for %s in %s", tp.Field(0).Type, tp))
				}
				usage := ft.Tag.Get("usage")
				flagSet.Var(&pair.UrlParamsSlice, name, usage)
				ft = tp.Field(1)
				name, ok = ft.Tag.Lookup("yaml")
				if !ok {
					panic(fmt.Sprintf("failed to lookup tag 'yaml' for %s in %s", tp.Field(1).Type, tp))
				}
				usage = ft.Tag.Get("usage")
				flagSet.Var(&pair.CmdSlice, name, usage)

				result[i] = pair
			}
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
