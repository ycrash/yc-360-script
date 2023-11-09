### Usage of argument options:
```bash
Usage of yc:
  -a string
        The APP Name of the target
  -c string
        The config file path to load
  -d Delete logs folder created during analyse, default is false
  -gcPath string
        The gc log file to be uploaded while it exists
  -hd
        Capture heap dump, default is false
  -hdPath string
        The heap dump file to be uploaded while it exists
  -j string
        The java home path to be used. Default will try to use os env 'JAVA_HOME' if 'JAVA_HOME' is not empty, for example: /usr/lib/jvm/java-8-openjdk-amd64
  -k string
        The API Key that will be used to make API requests, for example: tier1app@12312-12233-1442134-112
  -p int
        The process Id of the target, for example: 3121
  -s string
        The server url that will be used to upload data, for example: https://ycrash.companyname.com
  -tdPath string
        The thread dump file to be uploaded while it exists
  -version
        Show the version of this program

```
### Example of config file:
```yaml
version: "1"
options:
  a: name
  d: false
  hd: false
  j: /usr/lib/jvm/java-8-openjdk-amd64
  k: buggycompany@e094aasdsa-c3eb-4c9a-8254-f0dd107245cc
  p: 3121
  s: https://gceasy.io
  gcPath: /var/log/gc.log
  hdPath: /var/log/heapdump.log
  tdPath: /var/log/threaddump.log
```

The config file is using yaml format. The name of the option keys is same as the name of argument options.

'-s': the server url that will be used to upload data.  
'-k': the API key that will be used to make API requests.
'-j': the java home path to be used. Default will try to use os env 'JAVA_HOME' if 'JAVA_HOME' is not empty.
'-a': the app name of the target.  
'-p': the pid of the target.  
'-d': delete logs folder created during analyse, default is false.  
'-hd': capture heap dump, default is false.  
'-gcPath': the gc log file to be uploaded while it exists, otherwise it will captures one if failed to get the path from '-Xlog:gc' or '-Xloggc'.  
'-hdPath': the heap dump file to be uploaded while it exists.  
'-tdPath': the thread dump file to be uploaded while it exists, otherwise it will captures one.  

Only for argument options:  
'-version' show the version of this program.  
'-c': the config file path to load.  

### Example to capture info from target with pid 3121:

`yc -p 3121 -s https://gceasy.io -k testCompany@e094a34e-c3eb-4c9a-8254-f0dd107245cc -j /usr/lib/jvm/java-11-openjdk-amd64 -c ./config.yaml`
