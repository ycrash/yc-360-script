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
	Pid            int    `yaml:"p" usage:"The process Id of the target, for example: 3121"`
	ApiKey         string `yaml:"k" usage:"The API Key that will be used to make API requests, for example: tier1app@12312-12233-1442134-112"`
	Server         string `yaml:"s" usage:"The server url that will be used to upload data, for example: https://ycrash.companyname.com"`
	AppName        string `yaml:"a" usage:"The APP Name of the target"`
	HeapDump       bool   `yaml:"hd" usage:"Capture heap dump, default is false"`
	HeapDumpPath   string `yaml:"hdPath" usage:"The heap dump file to be uploaded while it exists"`
	ThreadDumpPath string `yaml:"tdPath" usage:"The thread dump file to be uploaded while it exists"`
	GCPath         string `yaml:"gcPath" usage:"The gc log file to be uploaded while it exists"`
	JavaHomePath   string `yaml:"j" usage:"The java home path to be used. Default will try to use os env 'JAVA_HOME' if 'JAVA_HOME' is not empty, for example: /usr/lib/jvm/java-8-openjdk-amd64"`
	DeferDelete    bool   `yaml:"d" usage:"Delete logs folder created during analyse, default is false"`

	ShowVersion bool   `arg:"version" yaml:"-" usage:"Show the version of this program"`
	ConfigPath  string `arg:"c" yaml:"-" usage:"The config file path to load"`

	Commands []Command `yaml:"cmds" usage:"Custom commands to be executed"`
}

type Command struct {
	UrlParams UrlParams `yaml:"urlParams" usage:"The params to be added at the end of upload request url, should be paired with '-cmd' together"`
	Cmd       Cmd       `yaml:"cmd" usage:"The command to be executed, should be paired with '-urlParams' together"`
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
		if !ok || name == "-" {
			name, ok = fieldType.Tag.Lookup("arg")
			if !ok {
				continue
			}
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
