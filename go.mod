module shell

go 1.16

require (
	github.com/cakturk/go-netstat v0.0.0-20200220111822-e5b49efee7a5
	github.com/gentlemanautomaton/cmdline v0.0.0-20190611233644-681aa5e68f1c
	github.com/jonboulle/clockwork v0.2.2 // indirect
	github.com/lestrrat-go/file-rotatelogs v2.4.0+incompatible
	github.com/lestrrat-go/strftime v1.0.4 // indirect
	github.com/mitchellh/go-ps v1.0.0
	github.com/pterm/pterm v0.12.8
	github.com/rs/zerolog v1.20.0
	github.com/shirou/gopsutil/v3 v3.22.3
	golang.org/x/sys v0.0.0-20220319134239-a9b59b0215f8 // indirect
	gopkg.in/yaml.v2 v2.3.0
)

replace (
	github.com/shirou/gopsutil/v3 v3.22.3 => ./gopsutil/v3@v3.22.3
	gopkg.in/yaml.v2 v2.3.0 => ./yaml.v2@v2.3.0/
)
