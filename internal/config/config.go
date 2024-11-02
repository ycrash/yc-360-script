package config

import (
	"flag"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Version    string
	TimezoneID string // Add TimezoneID field
	Options
}

type Options struct {
	Pid            string `yaml:"p" usage:"The process ID or unique token of the target, for example: 3121, or 'buggyApp'"`
	ApiKey         string `yaml:"k" usage:"The API Key that will be used to make API requests, for example: tier1app@12312-12233-1442134-112"`
	Server         string `yaml:"s" usage:"The server url that will be used to upload data, for example: https://ycrash.companyname.com"`
	AppName        string `yaml:"a" usage:"The APP Name of the target"`
	HeapDump       bool   `yaml:"hd" usage:"Capture heap dump, default is false"`
	HeapDumpPath   string `yaml:"hdPath" usage:"The heap dump file to be uploaded while it exists"`
	ThreadDumpPath string `yaml:"tdPath" usage:"The thread dump file to be uploaded while it exists"`
	GCPath         string `yaml:"gcPath" usage:"The gc log file to be uploaded while it exists"`
	JavaHomePath   string `yaml:"j" usage:"The java home path to be used. Default will try to use os env 'JAVA_HOME' if 'JAVA_HOME' is not empty, for example: /usr/lib/jvm/java-8-openjdk-amd64"`
	DeferDelete    bool   `yaml:"d" usage:"Delete logs folder created during analyse"`

	ShowVersion bool   `arg:"version" yaml:"-" usage:"Show the version of this program"`
	ConfigPath  string `arg:"c" yaml:"-" usage:"The config file path to load"`

	Commands []Command `yaml:"cmds" usage:"Custom commands to be executed"`

	VerifySSL  bool   `yaml:"verifySSL" usage:"Verifying the server SSL certificate"`
	CACertPath string `yaml:"caCertPath" usage:"The CA Cert Path"`

	M3                   bool          `arg:"m3" usage:"Run in m3 mode, default is false"`
	M3Frequency          time.Duration `yaml:"m3Frequency" usage:"Frequency of m3 mode, default is 3 minutes"`
	ProcessTokens        ProcessTokens `yaml:"processTokens" usage:"Process tokens of m3 mode"`
	ExcludeProcessTokens ProcessTokens `yaml:"excludeTokens" usage:"Process exclude tokens of m3 mode"`

	AccessLog string `yaml:"accessLog" usage:"Access log file path which is written by the target application"`

	CaptureCmd string `yaml:"captureCmd" usage:"Capture command line to be executed"`

	Address string `yaml:"address" usage:"Address to serve API service"`
	Port    int    `yaml:"port" usage:"Port to serve API service"`

	GCCaptureCmd string `yaml:"gcCaptureCmd" usage:"GC log capture command line to be executed"`
	TDCaptureCmd string `yaml:"tdCaptureCmd" usage:"Thread dump capture command line to be executed"`
	HDCaptureCmd string `yaml:"hdCaptureCmd" usage:"Heap dump capture command line to be executed"`

	OnlyCapture bool `yaml:"onlyCapture" usage:"Only capture all the artifacts and generate a zip file, default is false"`

	PingHost string `yaml:"pingHost" usage:"Ping to host three times"`
	Tags     string `yaml:"tags" usage:"Comma delimited strings as tags to transmit to server"`

	GCCaptureMode   bool   `yaml:"gcCaptureMode" usage:"Run in GC Capture mode"`
	TDCaptureMode   bool   `yaml:"tdCaptureMode" usage:"Run in Thread Dump Capture mode"`
	HDCaptureMode   bool   `yaml:"hdCaptureMode" usage:"Run in Heap Dump Capture mode"`
	JCmdCaptureMode string `yaml:"jCmdCaptureMode" usage:"Run in JCmd Capture mode"`
	VMStatMode      bool   `yaml:"vmstatMode" usage:"Run in vmstat mode"`
	TopMode         bool   `yaml:"topMode" usage:"Run in top mode"`

	LogFilePath     string `yaml:"logFilePath" usage:"Path to save the log file"`
	LogFileMaxSize  int64  `yaml:"logFileMaxSize" usage:"Max size of the log files"`
	LogFileMaxCount uint   `yaml:"logFileMaxCount" usage:"Max count of the log files"`
	LogLevel        string `yaml:"logLevel" usage:"Log level: trace, debug, info, warn, error, fatal, panic, disable."`

	AppLog          string  `yaml:"appLog" usage:"The target application’s log file path"`
	AppLogs         AppLogs `yaml:"appLogs" usage:"The target application’s log file paths"`
	AppLogLineCount uint    `yaml:"appLogLineCount" usage:"Number of last lines from the log file should be uploaded"`

	StoragePath string `yaml:"storagePath" usage:"The storage path to save the captured files"`

	Kubernetes bool `yaml:"kubernetes" usage:"pass true for Kubernetes field"`

	HealthChecks HealthChecks `yaml:"healthChecks"`
}

type HealthChecks map[string]HealthCheck
type HealthCheck struct {
	Endpoint    string `yaml:"endpoint"`
	HTTPBody    string `yaml:"httpBody"`
	TimeoutSecs int    `yaml:"timeoutSecs"`
}

type Command struct {
	UrlParams UrlParams `yaml:"urlParams" usage:"[DEPRECATED] This option is no longer in use."`
	Cmd       Cmd       `yaml:"cmd" usage:"[DEPRECATED] This option is no longer in use."`
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

type ProcessToken string
type ProcessTokens []ProcessToken

func (p *ProcessTokens) String() string {
	return fmt.Sprintf("%v", *p)
}

func (p *ProcessTokens) Set(s string) error {
	*p = append(*p, ProcessToken(s))
	return nil
}

type AppLog string
type AppLogs []AppLog

func (p *AppLogs) String() string {
	return fmt.Sprintf("%v", *p)
}

func (p *AppLogs) Set(s string) error {
	*p = append(*p, AppLog(s))
	return nil
}

func defaultConfig() Config {
	return Config{
		Options: Options{
			VerifySSL:       true,
			M3Frequency:     3 * time.Minute,
			Address:         "localhost",
			Port:            -1,
			LogFileMaxCount: 7,
			LogFileMaxSize:  512 * 1024 * 1024,
			LogLevel:        zerolog.InfoLevel.String(),
			PingHost:        "google.com",
			DeferDelete:     true,
			AppLogLineCount: 2000,
		},
	}
}

var GlobalConfig = defaultConfig()

func ParseFlags(args []string) error {
	if len(args) < 2 {
		return nil
	}
	flagSet, result := registerFlags(args[0])
	flagSet.Parse(args[1:])

	defer func() {
		if len(GlobalConfig.Server) > 2 && strings.HasSuffix(GlobalConfig.Server, "/") {
			GlobalConfig.Server = GlobalConfig.Server[:len(GlobalConfig.Server)-1]
		}
	}()

	err := copyFlagsValue(&GlobalConfig.Options, result)
	if err != nil {
		return err
	}

	if GlobalConfig.Options.ConfigPath == "" {
		return nil
	}

	file, err := os.Open(GlobalConfig.Options.ConfigPath)
	if err != nil {
		dir, _ := os.Getwd()
		return fmt.Errorf("workdir %s read config file path %s failed: %w", dir, GlobalConfig.Options.ConfigPath, err)
	}
	decoder := yaml.NewDecoder(file)
	err = decoder.Decode(&GlobalConfig)
	if err != nil {
		return fmt.Errorf("decode config file path %s failed: %w", GlobalConfig.Options.ConfigPath, err)
	}

	return copyFlagsValue(&GlobalConfig.Options, result)
}

func copyFlagsValue(dst interface{}, src map[int]interface{}) (err error) {
	value := reflect.ValueOf(dst).Elem()
	typ := value.Type()
	numField := value.NumField()
	for i := 0; i < numField; i++ {
		s, ok := src[i]
		if !ok {
			continue
		}
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
		var x reflect.Value
		fieldValue := value.Field(i)
		name := typ.Field(i).Name
		if name == "VerifySSL" {
			bs := *(s).(*string)
			b, err := strconv.ParseBool(strings.ToLower(bs))
			if err != nil {
				return fmt.Errorf("-verifySSL should be true or false, not %s", bs)
			}
			x = reflect.ValueOf(b)
		} else {
			x = reflect.ValueOf(s).Elem()
		}
		if reflect.DeepEqual(x.Interface(), fieldValue.Interface()) {
			delete(src, i)
			continue
		}
		fieldValue.Set(x)
	}
	return
}

func registerFlags(flagSetName string) (*flag.FlagSet, map[int]interface{}) {
	flagSet := flag.NewFlagSet(flagSetName, flag.ExitOnError)

	result := make(map[int]interface{})
	value := reflect.ValueOf(&GlobalConfig.Options).Elem()
	typ := value.Type()
	for i := 0; i < typ.NumField(); i++ {
		field := value.Field(i)
		fieldType := typ.Field(i)
		name, ok := fieldType.Tag.Lookup("yaml")
		if !ok || name == "-" {
			name, ok = fieldType.Tag.Lookup("arg")
			if !ok {
				continue
			}
		}
		usage := fieldType.Tag.Get("usage")
		if fieldType.Name == "VerifySSL" {
			result[i] = flagSet.String(name, strconv.FormatBool(field.Bool()), usage)
			continue
		}
		switch v := field.Interface().(type) {
		case ProcessTokens:
			var tokens ProcessTokens
			flagSet.Var(&tokens, name, usage)
			result[i] = &tokens
			continue
		case AppLogs:
			var appLogs AppLogs
			flagSet.Var(&appLogs, name, usage)
			result[i] = &appLogs
			continue
		case time.Duration:
			result[i] = flagSet.Duration(name, v, usage)
			continue
		case HealthChecks:
			// Ignore this due to nested structure, we don't support this via CLI for now.
			continue
		}
		switch fieldType.Type.Kind() {
		case reflect.Uint:
			result[i] = flagSet.Uint(name, uint(field.Uint()), usage)
		case reflect.Int:
			result[i] = flagSet.Int(name, int(field.Int()), usage)
		case reflect.Int64:
			result[i] = flagSet.Int64(name, field.Int(), usage)
		case reflect.String:
			result[i] = flagSet.String(name, field.String(), usage)
		case reflect.Bool:
			result[i] = flagSet.Bool(name, field.Bool(), usage)
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
