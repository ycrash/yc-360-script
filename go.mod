module shell

go 1.16

require (
	github.com/bmatcuk/doublestar/v4 v4.6.1
	github.com/cakturk/go-netstat v0.0.0-20200220111822-e5b49efee7a5
	github.com/gentlemanautomaton/cmdline v0.0.0-20190611233644-681aa5e68f1c
	github.com/jonboulle/clockwork v0.2.2 // indirect
	github.com/lestrrat-go/file-rotatelogs v2.4.0+incompatible
	github.com/lestrrat-go/strftime v1.0.4 // indirect
	github.com/mattn/go-zglob v0.0.4
	github.com/mitchellh/go-ps v1.0.0
	github.com/pterm/pterm v0.12.8
	github.com/rs/zerolog v1.20.0
	github.com/shirou/gopsutil/v3 v3.22.7
	github.com/stretchr/testify v1.8.2
	gopkg.in/yaml.v3 v3.0.1
)

replace gopkg.in/yaml.v2 v2.3.0 => ./yaml.v2@v2.3.0/
